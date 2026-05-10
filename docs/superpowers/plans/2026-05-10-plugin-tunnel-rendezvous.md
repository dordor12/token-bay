# Plugin Tunnel — NAT Rendezvous Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add NAT-rendezvous orchestration to `plugin/internal/tunnel` — a `WithRendezvous` option on Dial/Listen that calls the existing `stun_allocate` RPC, attempts a hole-punch against the peer's reflexive address, and falls back to the `turn_relay_open` path on failure. Tunnel stays import-free of `trackerclient`.

**Architecture:** A small `Rendezvous` interface in the `tunnel` package is the seam: the orchestration layer (Tasks A/B) implements it by adapting `*trackerclient.Client`. When `Config.Rendezvous` is non-nil, `Dial` learns the local reflexive address (kept for symmetry / future telemetry), sends a few raw UDP punch packets to the peer's reflexive address, then attempts a `quic.Transport.Dial` against that same address; on timeout or refused, it calls `Rendezvous.OpenRelay` and dials the returned coords with the same TLS-pinned handshake. `Listen` with rendezvous calls `AllocateReflexive` on bind so the caller can advertise the address through the broker; the listen accept loop is unchanged because both punched and relayed sessions arrive on the same UDP socket.

**Tech Stack:** Go 1.23+, `github.com/quic-go/quic-go` v0.59.0, stdlib `net` / `net/netip`. No new third-party deps.

---

## File Structure

**Create:**
- `plugin/internal/tunnel/rendezvous.go` — `Rendezvous` interface, `RelayCoords` type, `ParseRelayCoords` / `FormatRelayCoords` helpers (this is the "rendezvous_adapter" file referenced in the prompt; lives in `tunnel` but does NOT import `trackerclient`)
- `plugin/internal/tunnel/rendezvous_test.go` — unit tests for the type/helpers
- `plugin/internal/tunnel/holepunch.go` — `dialRendezvous` function: AllocateReflexive → punch → quic.Dial → relay fallback
- `plugin/internal/tunnel/holepunch_test.go` — unit + integration tests for the rendezvous dial path
- `plugin/internal/tunnel/listen_rendezvous_test.go` — listen-side rendezvous tests

**Modify:**
- `plugin/internal/tunnel/config.go` — add `Rendezvous`, `SessionID`, `HolePunchTimeout` fields + defaults + validate
- `plugin/internal/tunnel/dial.go` — branch on `cfg.Rendezvous` at top of `Dial`
- `plugin/internal/tunnel/listen.go` — branch on `cfg.Rendezvous` after bind; add `(*Listener).ReflexiveAddr()` method
- `plugin/internal/tunnel/errors.go` — add `ErrHolePunchFailed`, `ErrRelayFailed`
- `plugin/internal/tunnel/doc.go` — append a "Rendezvous" subsection

**Tests reference:** `plugin/internal/tunnel/e2e_test.go` (existing direct path; do not modify, used as a regression check that the direct path is untouched).

---

## Task 1: Define Rendezvous interface, RelayCoords, sentinels, adapter helpers

**Files:**
- Create: `plugin/internal/tunnel/rendezvous.go`
- Create: `plugin/internal/tunnel/rendezvous_test.go`
- Modify: `plugin/internal/tunnel/errors.go` (add `ErrHolePunchFailed`, `ErrRelayFailed`)

- [ ] **Step 1: Write the failing tests**

Write `plugin/internal/tunnel/rendezvous_test.go`:

```go
package tunnel

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRelayCoords_Valid(t *testing.T) {
	rc, err := ParseRelayCoords("127.0.0.1:51820", []byte{1, 2, 3, 4})
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddrPort("127.0.0.1:51820"), rc.Endpoint)
	assert.Equal(t, []byte{1, 2, 3, 4}, rc.Token)
}

func TestParseRelayCoords_Invalid(t *testing.T) {
	_, err := ParseRelayCoords("not-an-addr", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestParseRelayCoords_EmptyEndpoint(t *testing.T) {
	_, err := ParseRelayCoords("", []byte{1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestFormatRelayCoords_RoundTrip(t *testing.T) {
	original := RelayCoords{
		Endpoint: netip.MustParseAddrPort("[::1]:51820"),
		Token:    []byte("opaque-token"),
	}
	endpoint, token := FormatRelayCoords(original)
	rc, err := ParseRelayCoords(endpoint, token)
	require.NoError(t, err)
	assert.Equal(t, original, rc)
}

func TestRendezvousInterface_Compile(t *testing.T) {
	// Compile-time check: the noop implementation in this package satisfies
	// the interface. If this file fails to compile, the interface signature
	// changed in a backwards-incompatible way.
	var _ Rendezvous = (*noopRendezvous)(nil)
}

type noopRendezvous struct{}

func (noopRendezvous) AllocateReflexive(_ context.Context) (netip.AddrPort, error) {
	return netip.AddrPort{}, nil
}
func (noopRendezvous) OpenRelay(_ context.Context, _ [16]byte) (RelayCoords, error) {
	return RelayCoords{}, nil
}
```

(Add the `import "context"` at the top.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/tunnel/ -run 'Rendezvous|RelayCoords' -count=1`
Expected: FAIL with "undefined: Rendezvous", "undefined: RelayCoords", "undefined: ParseRelayCoords", "undefined: FormatRelayCoords".

- [ ] **Step 3: Add sentinels to errors.go**

Append to `plugin/internal/tunnel/errors.go` (inside the existing `var (...)` block, after `ErrTunnelClosed`):

```go
	// ErrHolePunchFailed is returned by Dial when a rendezvous-mode dial
	// could not establish a direct UDP path to the peer's reflexive address
	// before the configured HolePunchTimeout. The wrapped error is the last
	// underlying QUIC handshake error.
	ErrHolePunchFailed = errors.New("tunnel: hole-punch failed")

	// ErrRelayFailed is returned by Dial when the tracker's relay coords
	// could not be obtained (Rendezvous.OpenRelay error) or the QUIC
	// handshake against the relay endpoint failed.
	ErrRelayFailed = errors.New("tunnel: relay dial failed")
```

- [ ] **Step 4: Write rendezvous.go**

Create `plugin/internal/tunnel/rendezvous.go`:

```go
package tunnel

import (
	"context"
	"fmt"
	"net/netip"
)

// Rendezvous is the orchestration seam between this package and the tracker
// client. The plugin's data-path coordinator implements it by adapting
// *trackerclient.Client; this package never imports trackerclient.
//
// Method semantics:
//
//   - AllocateReflexive reflects the caller's external address by invoking
//     the tracker's stun_allocate RPC. Returned address is the peer-visible
//     UDP endpoint of whatever socket the underlying transport observed the
//     RPC arrive from. Errors propagate verbatim.
//
//   - OpenRelay requests a TURN-style relay allocation for the given session
//     id (the brokered request id). On success the caller dials the returned
//     RelayCoords.Endpoint and authenticates with the same identity-pin as
//     the direct path; the Token is held by the caller for billing/accounting
//     correlation but is opaque to this package.
type Rendezvous interface {
	AllocateReflexive(ctx context.Context) (netip.AddrPort, error)
	OpenRelay(ctx context.Context, sessionID [16]byte) (RelayCoords, error)
}

// RelayCoords names a TURN-style relay endpoint and the opaque session token
// that authorizes its use. Endpoint is a UDP address the consumer dials with
// the same TLS-pinned handshake as the direct path; Token is held by the
// caller for billing correlation and is not transmitted on the data channel
// by this package.
type RelayCoords struct {
	Endpoint netip.AddrPort
	Token    []byte
}

// ParseRelayCoords converts the string-keyed wire form of a relay handle
// (as returned by trackerclient.RelayHandle{Endpoint, Token}) into a typed
// RelayCoords. The orchestration layer calls this immediately after
// trackerclient.TurnRelayOpen returns.
//
// Returns ErrInvalidConfig wrapping the parse error on an unparsable
// endpoint string.
func ParseRelayCoords(endpoint string, token []byte) (RelayCoords, error) {
	if endpoint == "" {
		return RelayCoords{}, fmt.Errorf("%w: empty relay endpoint", ErrInvalidConfig)
	}
	addr, err := netip.ParseAddrPort(endpoint)
	if err != nil {
		return RelayCoords{}, fmt.Errorf("%w: parse relay endpoint %q: %v", ErrInvalidConfig, endpoint, err)
	}
	return RelayCoords{Endpoint: addr, Token: token}, nil
}

// FormatRelayCoords renders a RelayCoords back to the string-keyed wire form
// suitable for trackerclient.RelayHandle. Inverse of ParseRelayCoords.
func FormatRelayCoords(rc RelayCoords) (endpoint string, token []byte) {
	return rc.Endpoint.String(), rc.Token
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./plugin/internal/tunnel/ -run 'Rendezvous|RelayCoords' -count=1`
Expected: PASS (4 tests).

- [ ] **Step 6: Commit**

```bash
git add plugin/internal/tunnel/rendezvous.go plugin/internal/tunnel/rendezvous_test.go plugin/internal/tunnel/errors.go
git commit -m "feat(plugin/tunnel): add Rendezvous interface, RelayCoords, adapter helpers"
```

---

## Task 2: Add Config fields for rendezvous mode

**Files:**
- Modify: `plugin/internal/tunnel/config.go`
- Test: `plugin/internal/tunnel/config_test.go` (existing — extend)

- [ ] **Step 1: Write the failing tests**

Append to `plugin/internal/tunnel/config_test.go` (new test functions; do not modify existing):

```go
func TestConfig_RendezvousDefaults(t *testing.T) {
	c := &Config{
		EphemeralPriv: mustGenPriv(t),
		PeerPin:       mustGenPub(t),
		Rendezvous:    noopRendezvous{},
	}
	c.applyDefaults()
	require.NoError(t, c.validate())
	assert.Equal(t, defaultHolePunchTimeout, c.HolePunchTimeout)
}

func TestConfig_RendezvousNilSessionIDOK(t *testing.T) {
	// SessionID is allowed to be all-zero; it's the caller's job to set it
	// to the brokered request id when relay fallback might fire. Validation
	// does not enforce non-zero because direct-path tests with rendezvous
	// stubbed never exercise OpenRelay.
	c := &Config{
		EphemeralPriv:    mustGenPriv(t),
		PeerPin:          mustGenPub(t),
		Rendezvous:       noopRendezvous{},
		HolePunchTimeout: 1 * time.Second,
	}
	c.applyDefaults()
	require.NoError(t, c.validate())
}

func TestConfig_HolePunchTimeoutNegative(t *testing.T) {
	c := &Config{
		EphemeralPriv:    mustGenPriv(t),
		PeerPin:          mustGenPub(t),
		HolePunchTimeout: -1 * time.Second,
	}
	err := c.validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}
```

(If `mustGenPriv` / `mustGenPub` helpers don't already exist in `config_test.go`, add them.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/tunnel/ -run 'TestConfig_Rendezvous|TestConfig_HolePunchTimeoutNegative' -count=1`
Expected: FAIL with "undefined: defaultHolePunchTimeout" / "Rendezvous undefined".

- [ ] **Step 3: Modify config.go**

Modify `plugin/internal/tunnel/config.go`. Add the constant near the top:

```go
// defaultHolePunchTimeout is the per-attempt budget for the consumer's
// QUIC handshake against the peer's reflexive address before falling back
// to the relay path. 3s is comfortably above typical home-NAT propagation
// (~150ms) and well below the user-visible 2s "next message" budget at
// the ccproxy layer (which times the broker request and relay fallback
// together, not just the punch).
defaultHolePunchTimeout = 3 * time.Second
```

Add fields to `Config` struct:

```go
	// Rendezvous, when non-nil, switches Dial and Listen into rendezvous
	// mode. The caller (orchestration layer) implements the interface by
	// adapting *trackerclient.Client. nil → direct mode (existing behavior).
	Rendezvous Rendezvous

	// SessionID is the brokered request id used when Rendezvous.OpenRelay
	// is invoked on hole-punch failure. Ignored when Rendezvous is nil or
	// when the hole-punch attempt succeeds.
	SessionID [16]byte

	// HolePunchTimeout caps the rendezvous-mode QUIC handshake against the
	// peer's reflexive address. On expiry, Dial falls back to the relay
	// path. 0 → defaultHolePunchTimeout. Ignored when Rendezvous is nil.
	HolePunchTimeout time.Duration
```

Extend `applyDefaults()`:

```go
	if c.HolePunchTimeout == 0 {
		c.HolePunchTimeout = defaultHolePunchTimeout
	}
```

Extend `validate()`:

```go
	if c.HolePunchTimeout < 0 {
		return fmt.Errorf("%w: HolePunchTimeout %v < 0",
			ErrInvalidConfig, c.HolePunchTimeout)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/internal/tunnel/ -count=1`
Expected: PASS (existing + 3 new).

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/tunnel/config.go plugin/internal/tunnel/config_test.go
git commit -m "feat(plugin/tunnel): add Rendezvous, SessionID, HolePunchTimeout config fields"
```

---

## Task 3: Implement rendezvous Dial (hole-punch path, no fallback yet)

**Files:**
- Create: `plugin/internal/tunnel/holepunch.go`
- Create: `plugin/internal/tunnel/holepunch_test.go`
- Modify: `plugin/internal/tunnel/dial.go` (top-of-function branch)

- [ ] **Step 1: Write the failing test (hole-punch happy path)**

Create `plugin/internal/tunnel/holepunch_test.go`:

```go
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRendezvous is a fakable Rendezvous for tests. It records calls and
// can be configured to fail.
type stubRendezvous struct {
	reflexive    netip.AddrPort
	allocErr     error
	relay        RelayCoords
	openErr      error
	allocCalls   atomic.Int64
	openCalls    atomic.Int64
}

func (s *stubRendezvous) AllocateReflexive(_ context.Context) (netip.AddrPort, error) {
	s.allocCalls.Add(1)
	return s.reflexive, s.allocErr
}
func (s *stubRendezvous) OpenRelay(_ context.Context, _ [16]byte) (RelayCoords, error) {
	s.openCalls.Add(1)
	return s.relay, s.openErr
}

// holepunchListener is a minimal seeder-side QUIC listener using an explicit
// quic.Transport, returning the bound AddrPort. Used by tests that need to
// exercise rendezvous dial without invoking Listen's rendezvous mode.
func holepunchListener(t *testing.T, seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (netip.AddrPort, *quic.Listener, func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	tlsCfg, err := newTLSConfig(seederPriv, consumerPub, now, true)
	require.NoError(t, err)
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
	})
	require.NoError(t, err)
	addr := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	cleanup := func() {
		_ = ln.Close()
		_ = tr.Close()
	}
	return addr, ln, cleanup
}

func TestDial_RendezvousHolePunchSucceeds(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	seederAddr, ln, cleanup := holepunchListener(t, seederPriv, consumerPub)
	defer cleanup()

	// Drain one connection so accept does not stall.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		_, _ = conn.AcceptStream(ctx)
		_ = conn.CloseWithError(0, "ok")
	}()

	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
	}
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tun, err := Dial(ctx, seederAddr, cfg)
	require.NoError(t, err)
	defer tun.Close()

	assert.EqualValues(t, 1, rdv.allocCalls.Load(), "AllocateReflexive should be called exactly once")
	assert.EqualValues(t, 0, rdv.openCalls.Load(), "OpenRelay must NOT be called on hole-punch success")

	wg.Wait()
}

func TestDial_RendezvousAllocateReflexiveError(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	seederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	seederAddr, _, cleanup := holepunchListener(t, seederPriv, consumerPub)
	defer cleanup()

	stubErr := errors.New("stun rpc broken")
	rdv := &stubRendezvous{allocErr: stubErr}
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Dial(ctx, seederAddr, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, stubErr)
}

// keep io.Discard import live for later tasks
var _ = io.Discard
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/tunnel/ -run 'TestDial_Rendezvous' -count=1`
Expected: FAIL — `Dial` doesn't yet honor `Rendezvous`.

- [ ] **Step 3: Create holepunch.go**

Create `plugin/internal/tunnel/holepunch.go`:

```go
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/quic-go/quic-go"
)

// holePunchProbeBytes are the harmless payload of UDP punch packets. The
// content is irrelevant; what matters is that the packets traverse the
// caller's NAT outbound, opening a 5-tuple state entry that lets the peer's
// QUIC INITIAL packet arrive.
var holePunchProbeBytes = []byte{0}

// holePunchProbeCount is how many redundant punch packets to send.
// Three packets cover the typical loss-tolerance for a single NAT
// state-establishment exchange; tests on 127.0.0.1 don't actually need
// any but the loop is cheap.
const holePunchProbeCount = 3

// holePunchProbeInterval paces the probes. Loose enough that a slow NAT
// has time to establish state between bursts; tight enough that 3 probes
// cost <100ms total.
const holePunchProbeInterval = 25 * time.Millisecond

// dialRendezvous is the rendezvous-mode entry point invoked by Dial when
// cfg.Rendezvous is non-nil. It binds an ephemeral UDP socket, asks the
// rendezvous oracle for the local reflexive address (currently used for
// telemetry / future broker advertisement; failure is fatal because the
// caller depends on the peer learning this address out-of-band), sends a
// short burst of UDP punch packets, then attempts a QUIC handshake against
// peerAddr. On timeout it falls back to the relay path (Task 4).
func dialRendezvous(ctx context.Context, peerAddr netip.AddrPort, cfg Config) (*Tunnel, error) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("%w: bind udp: %v", ErrInvalidConfig, err)
	}

	if _, err := cfg.Rendezvous.AllocateReflexive(ctx); err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("rendezvous: allocate reflexive: %w", err)
	}

	transport := &quic.Transport{Conn: udp}

	// Punch: a short burst to open the local NAT mapping toward peerAddr.
	// Failures are swallowed — the actual handshake below is the success
	// signal.
	punchTo := net.UDPAddrFromAddrPort(peerAddr)
	for i := 0; i < holePunchProbeCount; i++ {
		_, _ = transport.WriteTo(holePunchProbeBytes, punchTo)
		select {
		case <-time.After(holePunchProbeInterval):
		case <-ctx.Done():
			_ = transport.Close()
			_ = udp.Close()
			return nil, fmt.Errorf("%w: %v", ErrHolePunchFailed, ctx.Err())
		}
	}

	tun, err := dialOverTransport(ctx, transport, peerAddr, cfg, cfg.HolePunchTimeout)
	if err == nil {
		return tun, nil
	}

	// Hole-punch attempt failed — wrap, then fall through to relay fallback
	// (added in Task 4).
	_ = transport.Close()
	_ = udp.Close()
	return nil, fmt.Errorf("%w: %v", ErrHolePunchFailed, err)
}

// dialOverTransport runs the QUIC + TLS handshake against addr using the
// provided transport, then opens the bidirectional stream. Returns a
// *Tunnel that owns transport; caller must not close transport on success.
func dialOverTransport(ctx context.Context, transport *quic.Transport, addr netip.AddrPort, cfg Config, timeout time.Duration) (*Tunnel, error) {
	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), false)
	if err != nil {
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: timeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}
	hsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := transport.Dial(hsCtx, net.UDPAddrFromAddrPort(addr), tlsCfg, quicCfg)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}
	stream, err := conn.OpenStreamSync(hsCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("%w: open stream: %v", ErrHandshakeFailed, err)
	}
	return &Tunnel{conn: conn, stream: stream, cfg: cfg, ownsTransport: transport}, nil
}

// silenceUnusedErrors keeps imports referenced when the relay path lands.
var _ = errors.Is
```

- [ ] **Step 4: Modify dial.go to dispatch on rendezvous**

In `plugin/internal/tunnel/dial.go`, replace the body of `Dial`:

```go
func Dial(ctx context.Context, addr netip.AddrPort, cfg Config) (*Tunnel, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Rendezvous != nil {
		return dialRendezvous(ctx, addr, cfg)
	}
	return dialDirect(ctx, addr, cfg)
}

func dialDirect(ctx context.Context, addr netip.AddrPort, cfg Config) (*Tunnel, error) {
	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), false)
	if err != nil {
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}
	hsCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()

	udp := &net.UDPAddr{IP: addr.Addr().AsSlice(), Port: int(addr.Port())}
	conn, err := quic.DialAddr(hsCtx, udp.String(), tlsCfg, quicCfg)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}
	stream, err := conn.OpenStreamSync(hsCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("%w: open stream: %v", ErrHandshakeFailed, err)
	}
	return &Tunnel{conn: conn, stream: stream, cfg: cfg}, nil
}
```

(Existing `mapHandshakeErr` function is untouched.)

- [ ] **Step 5: Add `ownsTransport` field to Tunnel + close it in Close**

In `plugin/internal/tunnel/tunnel.go`, add field to Tunnel struct (after `cfg`):

```go
	// ownsTransport is set when this Tunnel created its own quic.Transport
	// (rendezvous path). Direct-dial tunnels leave it nil; quic-go's DialAddr
	// owns the transport in that case.
	ownsTransport *quic.Transport
```

Modify the `Close()` method to also close `ownsTransport` after the conn close:

```go
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	if t.stream != nil {
		_ = t.stream.Close()
	}
	if t.conn != nil {
		_ = t.conn.CloseWithError(0, "tunnel closed")
	}
	if t.ownsTransport != nil {
		return t.ownsTransport.Close()
	}
	return nil
}
```

(The previous `return t.conn.CloseWithError(...)` becomes `_ = t.conn.CloseWithError(...)`.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./plugin/internal/tunnel/ -count=1 -race`
Expected: PASS — including the new rendezvous tests AND the existing direct-path tests (regression-protected).

- [ ] **Step 7: Commit**

```bash
git add plugin/internal/tunnel/holepunch.go plugin/internal/tunnel/holepunch_test.go plugin/internal/tunnel/dial.go plugin/internal/tunnel/tunnel.go
git commit -m "feat(plugin/tunnel): rendezvous-mode Dial — hole-punch path"
```

---

## Task 4: Add TURN-relay fallback to Dial

**Files:**
- Modify: `plugin/internal/tunnel/holepunch.go` (replace the "fall through to relay fallback" stub)
- Modify: `plugin/internal/tunnel/holepunch_test.go` (add fallback test)

- [ ] **Step 1: Write the failing test**

Append to `plugin/internal/tunnel/holepunch_test.go`:

```go
// blackholeUDPAddr returns a 127.0.0.1 UDP port that is bound but never
// reads. QUIC packets sent there get queued in the kernel and never reply,
// guaranteeing the consumer's hole-punch attempt times out.
func blackholeUDPAddr(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	addr := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	return addr, func() { _ = udp.Close() }
}

func TestDial_RendezvousFallsBackToRelay(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Real seeder listening on a real address — this is what the relay
	// "forwards to" in our test (we don't run an actual UDP relay).
	seederAddr, ln, cleanupLn := holepunchListener(t, seederPriv, consumerPub)
	defer cleanupLn()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		_, _ = conn.AcceptStream(ctx)
		_ = conn.CloseWithError(0, "ok")
	}()

	// Hole-punch target: a black-hole address that swallows packets so the
	// QUIC handshake against it definitely times out.
	blackhole, cleanupBh := blackholeUDPAddr(t)
	defer cleanupBh()

	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
		relay: RelayCoords{
			Endpoint: seederAddr, // production: real relay endpoint; test: real seeder
			Token:    []byte("opaque-token"),
		},
	}

	cfg := Config{
		EphemeralPriv:    consumerPriv,
		PeerPin:          seederPub,
		Rendezvous:       rdv,
		HolePunchTimeout: 250 * time.Millisecond, // tight; we want the fallback fast
		Now:              func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tun, err := Dial(ctx, blackhole, cfg)
	require.NoError(t, err)
	defer tun.Close()

	assert.EqualValues(t, 1, rdv.allocCalls.Load(), "AllocateReflexive called once")
	assert.EqualValues(t, 1, rdv.openCalls.Load(), "OpenRelay called exactly once after hole-punch failure")

	wg.Wait()
}

func TestDial_RendezvousRelayPinMismatch(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrongPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = seederPub

	seederAddr, ln, cleanupLn := holepunchListener(t, seederPriv, consumerPub)
	defer cleanupLn()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		_ = conn.CloseWithError(0, "ok")
	}()

	blackhole, cleanupBh := blackholeUDPAddr(t)
	defer cleanupBh()

	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
		relay:     RelayCoords{Endpoint: seederAddr, Token: nil},
	}
	cfg := Config{
		EphemeralPriv:    consumerPriv,
		PeerPin:          wrongPub, // intentionally wrong
		Rendezvous:       rdv,
		HolePunchTimeout: 250 * time.Millisecond,
		Now:              func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Dial(ctx, blackhole, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPeerPinMismatch, "TLS pin must be enforced on the relay path")
}

func TestDial_RendezvousOpenRelayError(t *testing.T) {
	seederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	blackhole, cleanupBh := blackholeUDPAddr(t)
	defer cleanupBh()

	stubErr := errors.New("relay allocation refused")
	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
		openErr:   stubErr,
	}
	cfg := Config{
		EphemeralPriv:    consumerPriv,
		PeerPin:          seederPub,
		Rendezvous:       rdv,
		HolePunchTimeout: 250 * time.Millisecond,
		Now:              func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Dial(ctx, blackhole, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRelayFailed)
	assert.ErrorIs(t, err, stubErr)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/tunnel/ -run 'TestDial_RendezvousFallsBack|TestDial_RendezvousRelayPin|TestDial_RendezvousOpenRelayError' -count=1`
Expected: FAIL — fallback path is not yet implemented; `dialRendezvous` returns `ErrHolePunchFailed`.

- [ ] **Step 3: Replace dialRendezvous to add the fallback**

In `plugin/internal/tunnel/holepunch.go`, rewrite `dialRendezvous`:

```go
func dialRendezvous(ctx context.Context, peerAddr netip.AddrPort, cfg Config) (*Tunnel, error) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("%w: bind udp: %v", ErrInvalidConfig, err)
	}

	if _, err := cfg.Rendezvous.AllocateReflexive(ctx); err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("rendezvous: allocate reflexive: %w", err)
	}

	transport := &quic.Transport{Conn: udp}

	punchTo := net.UDPAddrFromAddrPort(peerAddr)
	for i := 0; i < holePunchProbeCount; i++ {
		_, _ = transport.WriteTo(holePunchProbeBytes, punchTo)
		select {
		case <-time.After(holePunchProbeInterval):
		case <-ctx.Done():
			_ = transport.Close()
			return nil, fmt.Errorf("%w: %v", ErrHolePunchFailed, ctx.Err())
		}
	}

	tun, err := dialOverTransport(ctx, transport, peerAddr, cfg, cfg.HolePunchTimeout)
	if err == nil {
		return tun, nil
	}
	hpErr := err

	// Hole-punch failed — fall back to relay.
	relay, err := cfg.Rendezvous.OpenRelay(ctx, cfg.SessionID)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("%w: %v", ErrRelayFailed, err)
	}
	if !relay.Endpoint.IsValid() {
		_ = transport.Close()
		return nil, fmt.Errorf("%w: relay endpoint invalid (zero AddrPort)", ErrRelayFailed)
	}

	// Send punch packets to the relay endpoint to open the NAT mapping
	// toward the tracker (in production paths the relay is the tracker).
	for i := 0; i < holePunchProbeCount; i++ {
		_, _ = transport.WriteTo(holePunchProbeBytes, net.UDPAddrFromAddrPort(relay.Endpoint))
		select {
		case <-time.After(holePunchProbeInterval):
		case <-ctx.Done():
			_ = transport.Close()
			return nil, fmt.Errorf("%w: %v", ErrRelayFailed, ctx.Err())
		}
	}

	tun, err = dialOverTransport(ctx, transport, relay.Endpoint, cfg, cfg.HandshakeTimeout)
	if err != nil {
		_ = transport.Close()
		// Surface ErrPeerPinMismatch / ErrALPNMismatch verbatim — those are
		// security signals that must not be hidden by the relay-fallback
		// wrapper. Other errors get wrapped as ErrRelayFailed and joined
		// with the original hole-punch failure for diagnostic surface.
		if errors.Is(err, ErrPeerPinMismatch) || errors.Is(err, ErrALPNMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: hole-punch: %v; relay: %v", ErrRelayFailed, hpErr, err)
	}
	return tun, nil
}
```

(Remove the `var _ = errors.Is` filler — `errors.Is` is now used in earnest.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/internal/tunnel/ -count=1 -race -timeout 60s`
Expected: PASS, all rendezvous tests + existing direct path.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/tunnel/holepunch.go plugin/internal/tunnel/holepunch_test.go
git commit -m "feat(plugin/tunnel): TURN-relay fallback in rendezvous Dial"
```

---

## Task 5: Listen rendezvous mode (AllocateReflexive on bind, ReflexiveAddr accessor)

**Files:**
- Modify: `plugin/internal/tunnel/listen.go`
- Create: `plugin/internal/tunnel/listen_rendezvous_test.go`

- [ ] **Step 1: Write the failing tests**

Create `plugin/internal/tunnel/listen_rendezvous_test.go`:

```go
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListen_RendezvousCallsAllocateReflexive(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	wantReflexive := netip.MustParseAddrPort("203.0.113.7:51820")
	rdv := &stubRendezvous{reflexive: wantReflexive}

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	defer ln.Close()

	assert.EqualValues(t, 1, rdv.allocCalls.Load())
	assert.Equal(t, wantReflexive, ln.ReflexiveAddr(), "Listener must expose what AllocateReflexive returned")
}

func TestListen_RendezvousAllocateError(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	stubErr := errors.New("stun rpc broken")
	rdv := &stubRendezvous{allocErr: stubErr}

	_, err = Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, stubErr)
}

func TestListen_NoRendezvousSkipsAllocate(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	rdv := &stubRendezvous{} // would error if called

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		// Rendezvous intentionally nil
		Now: func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	defer ln.Close()

	assert.EqualValues(t, 0, rdv.allocCalls.Load(), "Listen without Rendezvous must not call into the stub")
	assert.False(t, ln.ReflexiveAddr().IsValid(), "ReflexiveAddr must be zero when rendezvous unused")
}

// TestE2E_RendezvousHolePunchToListenSucceeds boots a Listener in rendezvous
// mode and dials it via Dial in rendezvous mode; both AllocateReflexive
// calls fire, no relay needed.
func TestE2E_RendezvousHolePunchToListenSucceeds(t *testing.T) {
	clk := func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) }

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Bind seeder Listener with rendezvous; reflexive value is whatever
	// the stub says — the actual local addr is from LocalAddr().
	seederRdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("198.51.100.1:7"),
	}
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Rendezvous:    seederRdv,
		Now:           clk,
	})
	require.NoError(t, err)
	defer ln.Close()

	// Consumer dials the seeder's actual local addr (in production this
	// would be ln.ReflexiveAddr() learned via broker assignment; on the
	// loopback test there's no NAT, so direct addr == reflexive addr).
	chunks := "event: message_stop\ndata: {}\n\n"
	requestBody := []byte(`{"model":"claude-opus-4-7"}`)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		seederTun, err := ln.Accept(ctx)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		body, err := seederTun.ReadRequest()
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		if string(body) != string(requestBody) {
			t.Errorf("body mismatch")
			return
		}
		if err := seederTun.SendOK(); err != nil {
			t.Errorf("sendok: %v", err)
			return
		}
		_, _ = seederTun.ResponseWriter().Write([]byte(chunks))
		_ = seederTun.CloseWrite()
	}()

	consumerRdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("198.51.100.2:9"),
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := Dial(dialCtx, ln.LocalAddr(), Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Rendezvous:    consumerRdv,
		Now:           clk,
	})
	require.NoError(t, err)
	defer tun.Close()

	require.NoError(t, tun.Send(requestBody))
	st, r, err := tun.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusOK, st)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, chunks, string(got))

	wg.Wait()

	assert.EqualValues(t, 1, seederRdv.allocCalls.Load())
	assert.EqualValues(t, 1, consumerRdv.allocCalls.Load())
	assert.EqualValues(t, 0, consumerRdv.openCalls.Load(), "no relay fallback expected")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/tunnel/ -run 'TestListen_Rendezvous|TestE2E_RendezvousHolePunch|TestListen_NoRendezvous' -count=1`
Expected: FAIL — `ReflexiveAddr` undefined; AllocateReflexive isn't called by Listen.

- [ ] **Step 3: Modify listen.go**

In `plugin/internal/tunnel/listen.go`:

Add a field to `Listener`:

```go
	reflexive netip.AddrPort
```

Add a method:

```go
// ReflexiveAddr returns the address the configured Rendezvous reported when
// Listen called AllocateReflexive. Zero AddrPort if Listen was invoked
// without a Rendezvous; the caller (orchestration layer) advertises this
// address through the broker so consumers can dial it during hole-punch.
func (l *Listener) ReflexiveAddr() netip.AddrPort { return l.reflexive }
```

In `Listen()`, after the existing UDP bind / TLS setup / `ln, err := tr.Listen(...)` block — after the `resolved := udp.LocalAddr().(*net.UDPAddr).AddrPort()` line — insert the rendezvous step **after** Listen has succeeded but **before** the `return &Listener{...}`:

```go
	var reflexive netip.AddrPort
	if cfg.Rendezvous != nil {
		ctx, cancel := context.WithTimeout(context.Background(), defaultStunAllocateTimeout)
		defer cancel()
		addr, err := cfg.Rendezvous.AllocateReflexive(ctx)
		if err != nil {
			_ = ln.Close()
			_ = tr.Close()
			_ = udp.Close()
			return nil, fmt.Errorf("rendezvous: allocate reflexive: %w", err)
		}
		reflexive = addr
	}
```

Add the constant near the top of the file (or in `config.go`):

```go
const defaultStunAllocateTimeout = 5 * time.Second
```

Update the constructor return:

```go
	return &Listener{
		cfg:       cfg,
		transport: tr,
		udp:       udp,
		ln:        ln,
		addr:      resolved,
		reflexive: reflexive,
	}, nil
```

Update imports to include `context` and `time` if not already present.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/internal/tunnel/ -count=1 -race -timeout 60s`
Expected: PASS — all four new tests + everything previous.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/tunnel/listen.go plugin/internal/tunnel/listen_rendezvous_test.go
git commit -m "feat(plugin/tunnel): rendezvous-mode Listen — bind + AllocateReflexive"
```

---

## Task 6: Update doc.go to describe rendezvous mode

**Files:**
- Modify: `plugin/internal/tunnel/doc.go`

- [ ] **Step 1: Append rendezvous subsection**

In `plugin/internal/tunnel/doc.go`, replace the "Out of scope" stanza with:

```go
// # Rendezvous mode (opt-in)
//
// When Config.Rendezvous is non-nil, Dial and Listen switch to rendezvous
// mode:
//
//   - Listen binds an ephemeral UDP socket as before, then calls
//     Rendezvous.AllocateReflexive(ctx) and exposes the result via
//     (*Listener).ReflexiveAddr(). The orchestration layer advertises that
//     address through the broker so consumers can dial it during the
//     simultaneous-open phase.
//
//   - Dial binds an ephemeral UDP socket on a quic.Transport, calls
//     AllocateReflexive (kept for symmetry / forward-compat with broker
//     advertisement), sends a short burst of UDP punch packets to the
//     peer's reflexive address, then attempts a QUIC handshake against
//     that address bounded by Config.HolePunchTimeout. On timeout it
//     calls Rendezvous.OpenRelay(ctx, SessionID) and dials the returned
//     RelayCoords.Endpoint with the same TLS-pinned handshake.
//
// The pin is enforced end-to-end on both paths — the tracker can relay
// bytes but cannot decrypt them. The Token in RelayCoords is held by the
// caller for billing correlation; this package does not transmit it on
// the data channel.
//
// # Out of scope
//
// This package does NOT speak STUN, does NOT implement TURN's RFC framing,
// does NOT call into trackerclient (Rendezvous is the seam), and does NOT
// interpret the seeder's stream-json output. Callers supply Rendezvous (or
// nil for direct mode) and receive bytes; orchestration belongs to the
// sidecar's data-path coordinator.
```

- [ ] **Step 2: Verify docs build**

Run: `go vet ./plugin/internal/tunnel/...`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add plugin/internal/tunnel/doc.go
git commit -m "docs(plugin/tunnel): document rendezvous mode"
```

---

## Task 7: Final regression sweep + lint

**Files:** none (verification step)

- [ ] **Step 1: Run full plugin test suite under race**

Run: `cd plugin && go test ./... -race -count=1 -timeout 180s`
Expected: PASS across all plugin packages. Particularly: no regression in `internal/tunnel` direct-path tests (`TestE2E_ConsumerSeederRoundTrip`, `TestDial_HandshakesAndOpensStream`).

- [ ] **Step 2: Run lint**

Run: `cd plugin && golangci-lint run ./...`
Expected: clean.

- [ ] **Step 3: Verify the constraint that tunnel does not import trackerclient**

Run: `grep -r "trackerclient" plugin/internal/tunnel/ || echo "OK: tunnel free of trackerclient imports"`
Expected: only matches in test stubs that mention the type by name in comments (none should actually import the package). If any import line appears, the seam was broken — fix before continuing.

- [ ] **Step 4: Final integration sanity — repo-wide check**

Run from repo root: `make check`
Expected: PASS (test + lint).

- [ ] **Step 5: Commit any cleanup if needed**

```bash
git status
# If clean, this task is done. If anything else surfaces (lint nits etc),
# commit with: chore(plugin/tunnel): post-rendezvous cleanup
```

---

## Self-Review

**Spec coverage:**
- Plugin spec §7.1 (Setup — STUN hole-punch + TURN fallback) → Tasks 3, 4
- Plugin spec §7.3 (Tunnel failure semantics — hole-punch fail → retry once with TURN) → Task 4
- Tracker spec §5.4 (STUN/TURN — reflexive address discovery; lazy-allocated relay per request_id) → Tasks 3, 5 (via the existing `stun_allocate` and `turn_relay_open` RPCs that the Rendezvous interface adapts)
- Architecture §1.3 (data-path choice — P2P first, tracker-relay fallback) → Tasks 3, 4
- Architecture §6 (TLS pinning on relay path so tracker can't see content) → Tasks 3, 4 (`newTLSConfig` + `verifyPeerPin` reused on both paths) and Task 4's pin-mismatch test
- Constraint: tunnel must not import trackerclient → Task 1's `Rendezvous` interface + Task 7 grep verification
- Constraint: existing direct-dial code path unchanged → Task 3 splits `Dial` into `dialDirect` + `dialRendezvous`; existing tests are regression checks

**Placeholder scan:** None remaining. Every task has full code in steps; no TBDs.

**Type consistency:**
- `Rendezvous` interface defined Task 1, used Tasks 3-5
- `RelayCoords{Endpoint netip.AddrPort; Token []byte}` defined Task 1, used in stub Task 3, Tasks 4-5
- `stubRendezvous` defined Task 3, reused Tasks 4-5 (same package)
- `Config.HolePunchTimeout` introduced Task 2, consumed Task 3
- `(*Listener).ReflexiveAddr()` introduced Task 5
- `ErrHolePunchFailed` / `ErrRelayFailed` introduced Task 1, asserted Tasks 3-4

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-10-plugin-tunnel-rendezvous.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
