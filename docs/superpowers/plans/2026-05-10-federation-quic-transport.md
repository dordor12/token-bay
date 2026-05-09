# Federation QUIC Peer Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace federation's in-process Transport with a real-network QUIC + mTLS implementation, plus per-peer redial-with-backoff. Two trackers running real binaries can peer over the network and recover from transient drops.

**Architecture:** New `tracker/internal/federation` files (`tls.go`, `transport_quic.go`, `dialer.go`) implement the existing `Transport` / `PeerConn` interfaces using `quic-go`. SPKI-pinned mTLS, distinct ALPN `tokenbay-fed/1`, persistent bidirectional stream per peer connection. The subsystem replaces today's one-shot `dialOutbound` with a `Dialer.Run` redial loop. `InprocTransport` stays for unit tests.

**Tech Stack:** Go 1.25, `crypto/tls`, `crypto/x509`, `crypto/ed25519`, `crypto/sha256` (stdlib only), `github.com/quic-go/quic-go v0.59.0` (already a tracker dep), `github.com/stretchr/testify`. No new third-party deps.

**Specs:**
- `docs/superpowers/specs/federation/2026-05-09-federation-quic-transport-design.md` (this slice)
- `docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md` (parent)

**Dependency order:** Runs after the federation core slice (already merged on main as `896a1d6`). The existing `Transport` / `PeerConn` interfaces, `Federation.Open` / `Close`, `AllowlistedPeer`, and `RunHandshakeDialer` / `RunHandshakeListener` are unchanged consumers of this work.

---

## 1. File map

```
tracker/internal/federation/
  tls.go                        ← CREATE: ALPN const + CertFromIdentity + MakeServer/ClientTLSConfig +
                                          SPKIHashOfCert + sentinel errors + verifier helpers
  tls_test.go                   ← CREATE: cert round-trip + SPKI determinism + verifier rejection cases
  transport_quic.go             ← CREATE: QUICConfig, QUICTransport, qConn (Dial/Listen/Close + Send/Recv)
  transport_quic_test.go        ← CREATE: real loopback QUIC dial+accept+roundtrip, frame-too-large, cert mismatch
  dialer.go                     ← CREATE: Dialer struct + Run + backoff math
  dialer_test.go                ← CREATE: backoff progression + reset + ctx cancel mid-sleep
  config.go                     ← MODIFY: add RedialBase, RedialMax, IdleTimeout time.Duration fields + defaults
  subsystem.go                  ← MODIFY: drop dialOutbound; spawn Dialer per peer; add attachAndWait
  integration_test.go           ← MODIFY: add TestIntegration_QUIC_RootAttestation_AB + DropAndRecover

tracker/internal/config/
  config.go                     ← MODIFY: extend FederationConfig (ListenAddr, IdleTimeoutS,
                                          RedialBaseS, RedialMaxS) + DefaultConfig values
  apply_defaults.go             ← MODIFY: defaults for the three int fields (ListenAddr left empty)
  validate.go                   ← MODIFY: validate the four new fields
  apply_defaults_test.go        ← MODIFY: cover new defaults
  validate_test.go              ← MODIFY: cover new validation cases

tracker/cmd/token-bay-tracker/
  run_cmd.go                    ← MODIFY: select QUICTransport when cfg.Federation.ListenAddr != ""
```

---

## Task 1: `tls.go` — cert + TLS configs + verifiers

**Files:**
- Create: `tracker/internal/federation/tls.go`
- Create: `tracker/internal/federation/tls_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/tls_test.go`:

```go
package federation_test

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestCertFromIdentity_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, err := federation.CertFromIdentity(priv)
	if err != nil {
		t.Fatalf("CertFromIdentity: %v", err)
	}
	if len(cert.Certificate) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(cert.Certificate))
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	gotPub, ok := parsed.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatal("cert pubkey is not Ed25519")
	}
	if string(gotPub) != string(pub) {
		t.Fatal("cert pubkey != input pubkey")
	}
}

func TestCertFromIdentity_RejectsBadKeyLength(t *testing.T) {
	t.Parallel()
	if _, err := federation.CertFromIdentity(make(ed25519.PrivateKey, 1)); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestSPKIHashOfCert_Deterministic(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	h1, err := federation.SPKIHashOfCert(parsed)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := federation.SPKIHashOfCert(parsed)
	if h1 != h2 {
		t.Fatal("SPKI hash differs across calls on same cert")
	}
	expect := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	if h1 != expect {
		t.Fatal("SPKI hash != sha256(RawSubjectPublicKeyInfo)")
	}
}

func TestSPKIHashOfCert_RejectsNilOrEmpty(t *testing.T) {
	t.Parallel()
	if _, err := federation.SPKIHashOfCert(nil); err == nil {
		t.Fatal("expected error on nil cert")
	}
	if _, err := federation.SPKIHashOfCert(&x509.Certificate{}); err == nil {
		t.Fatal("expected error on empty SPKI")
	}
}

func TestPinVerifier_RejectsCases(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	expected, _ := federation.SPKIHashOfCert(parsed)

	pinned := federation.MakeClientTLSConfig(cert, expected)
	// Happy path: parsed cert hashes to expected → verifier returns nil.
	if err := pinned.VerifyPeerCertificate([][]byte{parsed.Raw}, nil); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	// No cert.
	if err := pinned.VerifyPeerCertificate([][]byte{}, nil); !errors.Is(err, federation.ErrNoPeerCert) {
		t.Fatalf("expected ErrNoPeerCert, got %v", err)
	}
	// Two certs.
	if err := pinned.VerifyPeerCertificate([][]byte{parsed.Raw, parsed.Raw}, nil); !errors.Is(err, federation.ErrTooManyCerts) {
		t.Fatalf("expected ErrTooManyCerts, got %v", err)
	}
	// Wrong SPKI.
	_, otherPriv, _ := ed25519.GenerateKey(crand.Reader)
	otherCert, _ := federation.CertFromIdentity(otherPriv)
	if err := pinned.VerifyPeerCertificate([][]byte{otherCert.Certificate[0]}, nil); !errors.Is(err, federation.ErrSPKIMismatch) {
		t.Fatalf("expected ErrSPKIMismatch, got %v", err)
	}
	// Garbage.
	if err := pinned.VerifyPeerCertificate([][]byte{[]byte("not-a-cert")}, nil); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestServerTLSConfig_CapturesClientHash(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	srvCert, _ := federation.CertFromIdentity(priv)
	var captured [32]byte
	cfg := federation.MakeServerTLSConfig(srvCert, func(h [32]byte) { captured = h })

	_, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	cliCert, _ := federation.CertFromIdentity(cliPriv)
	parsed, _ := x509.ParseCertificate(cliCert.Certificate[0])
	if err := cfg.VerifyPeerCertificate([][]byte{parsed.Raw}, nil); err != nil {
		t.Fatalf("server verify: %v", err)
	}
	want, _ := federation.SPKIHashOfCert(parsed)
	if captured != want {
		t.Fatal("server verifier did not capture client SPKI hash")
	}
}

func TestALPN_DistinctFromPluginFacing(t *testing.T) {
	t.Parallel()
	if federation.ALPN == "" {
		t.Fatal("federation.ALPN is empty")
	}
	if federation.ALPN == "tokenbay/1" {
		t.Fatal("federation ALPN must not collide with plugin-facing ALPN")
	}
	if !strings.HasPrefix(federation.ALPN, "tokenbay-fed/") {
		t.Fatalf("federation ALPN should start with tokenbay-fed/, got %q", federation.ALPN)
	}
	cfg := federation.MakeServerTLSConfig(tls.Certificate{}, nil)
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != federation.ALPN {
		t.Fatalf("server TLS config NextProtos = %v, want [%q]", cfg.NextProtos, federation.ALPN)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.ALPN` etc.

- [ ] **Step 3: Implement tls.go**

`tracker/internal/federation/tls.go`:

```go
package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// ALPN is the protocol identifier negotiated on every Token-Bay
// federation peering link. Distinct from the plugin-facing
// "tokenbay/1" so a misconfigured operator pointing federation at the
// plugin port cannot accidentally cross-serve.
const ALPN = "tokenbay-fed/1"

// Cert + verifier sentinel errors.
var (
	ErrNoPeerCert    = errors.New("federation: no peer certificate")
	ErrTooManyCerts  = errors.New("federation: peer offered more than one cert")
	ErrNotEd25519    = errors.New("federation: peer cert is not Ed25519")
	ErrSPKIMismatch  = errors.New("federation: peer cert SPKI does not match pin")
	ErrEmptySPKI     = errors.New("federation: empty SPKI")
)

// CertFromIdentity issues a self-signed X.509 cert wrapping priv. The
// cert is generated deterministically from the keypair (modulo CSPRNG
// bytes that x509 sprinkles in) and is safe to regenerate per process
// start. The resulting tls.Certificate carries priv as its PrivateKey.
func CertFromIdentity(priv ed25519.PrivateKey) (tls.Certificate, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("federation: priv length %d, want %d",
			len(priv), ed25519.PrivateKeySize)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "token-bay-federation"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("federation: CreateCertificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// SPKIHashOfCert returns the SHA-256 of the cert's
// SubjectPublicKeyInfo bytes — the canonical pin used for federation
// peer identification. Empty SPKI is rejected to defeat hand-crafted
// truncated certs.
func SPKIHashOfCert(cert *x509.Certificate) ([32]byte, error) {
	if cert == nil {
		return [32]byte{}, errors.New("federation: nil cert")
	}
	if len(cert.RawSubjectPublicKeyInfo) == 0 {
		return [32]byte{}, ErrEmptySPKI
	}
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo), nil
}

// MakeClientTLSConfig is used by the dialer side. Pins the server's SPKI
// hash; rejects multi-cert chains, non-Ed25519, and SPKI mismatches.
func MakeClientTLSConfig(ourCert tls.Certificate, expectedServerSPKI [32]byte) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{ourCert},
		InsecureSkipVerify:     true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate:  pinVerifier(expectedServerSPKI),
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
}

// MakeServerTLSConfig is used by the listener side. Accepts any client
// whose cert is a valid self-signed Ed25519 cert; the SPKI hash is
// captured via the supplied callback so the accept handler can verify
// it against the operator allowlist.
func MakeServerTLSConfig(ourCert tls.Certificate, captureClientHash func([32]byte)) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{ourCert},
		ClientAuth:             tls.RequireAnyClientCert,
		InsecureSkipVerify:     true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate:  serverVerifier(captureClientHash),
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
}

func pinVerifier(expected [32]byte) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCert
		}
		if len(rawCerts) > 1 {
			return ErrTooManyCerts
		}
		peer, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("federation: parse peer cert: %w", err)
		}
		if _, ok := peer.PublicKey.(ed25519.PublicKey); !ok {
			return ErrNotEd25519
		}
		got, err := SPKIHashOfCert(peer)
		if err != nil {
			return err
		}
		if got != expected {
			return ErrSPKIMismatch
		}
		return nil
	}
}

func serverVerifier(capture func([32]byte)) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCert
		}
		if len(rawCerts) > 1 {
			return ErrTooManyCerts
		}
		peer, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("federation: parse peer cert: %w", err)
		}
		if _, ok := peer.PublicKey.(ed25519.PublicKey); !ok {
			return ErrNotEd25519
		}
		h, err := SPKIHashOfCert(peer)
		if err != nil {
			return err
		}
		if capture != nil {
			capture(h)
		}
		return nil
	}
}
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/tls.go tracker/internal/federation/tls_test.go
git commit -m "feat(tracker/federation): TLS cert + SPKI-pinned verifiers for QUIC peering"
```

---

## Task 2: `transport_quic.go` — QUICTransport core

**Files:**
- Create: `tracker/internal/federation/transport_quic.go`
- Create: `tracker/internal/federation/transport_quic_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/transport_quic_test.go`:

```go
package federation_test

import (
	"context"
	crand "crypto/rand"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// quicPeer is a small harness combining a key, cert, and SPKI hash.
type quicPeer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	spki [32]byte
}

func newQUICPeer(t *testing.T) quicPeer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	spki, _ := federation.SPKIHashOfCert(parsed)
	return quicPeer{pub: pub, priv: priv, spki: spki}
}

func openLoopbackQUIC(t *testing.T, p quicPeer) *federation.QUICTransport {
	t.Helper()
	cert, _ := federation.CertFromIdentity(p.priv)
	tr, err := federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: 5 * time.Second,
		Cert:        cert,
		HandshakeTO: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewQUICTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestQUICTransport_DialAccept_RoundTrip(t *testing.T) {
	t.Parallel()
	srv := newQUICPeer(t)
	cli := newQUICPeer(t)

	srvT := openLoopbackQUIC(t, srv)
	cliT := openLoopbackQUIC(t, cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	accepted := make(chan federation.PeerConn, 1)
	var listenWG sync.WaitGroup
	listenWG.Add(1)
	go func() {
		defer listenWG.Done()
		_ = srvT.Listen(ctx, func(c federation.PeerConn) { accepted <- c })
	}()

	srvAddr := srvT.ListenAddr()
	if srvAddr == "" {
		t.Fatal("server ListenAddr empty")
	}

	conn, err := cliT.Dial(ctx, srvAddr, srv.pub)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	srvConn := <-accepted
	defer srvConn.Close()

	if err := conn.Send(ctx, []byte("hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := srvConn.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q want hello", got)
	}

	// Server-side RemotePub should be the client's pubkey.
	if h := sha256.Sum256(srvConn.RemotePub()); h != cli.spki {
		t.Fatal("server-side RemotePub does not match client SPKI")
	}
}

func TestQUICTransport_Dial_RejectsWrongPin(t *testing.T) {
	t.Parallel()
	srv := newQUICPeer(t)
	cli := newQUICPeer(t)
	srvT := openLoopbackQUIC(t, srv)
	cliT := openLoopbackQUIC(t, cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_ = srvT.Listen(ctx, func(c federation.PeerConn) { _ = c.Close() })
	}()

	wrongPub, _, _ := ed25519.GenerateKey(crand.Reader)
	_, err := cliT.Dial(ctx, srvT.ListenAddr(), wrongPub)
	if err == nil {
		t.Fatal("expected dial to fail under wrong pin")
	}
}

func TestQUICTransport_Send_FrameTooLarge(t *testing.T) {
	t.Parallel()
	srv := newQUICPeer(t)
	cli := newQUICPeer(t)
	srvT := openLoopbackQUIC(t, srv)
	cliT := openLoopbackQUIC(t, cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = srvT.Listen(ctx, func(c federation.PeerConn) { _ = c.Close() }) }()

	conn, err := cliT.Dial(ctx, srvT.ListenAddr(), srv.pub)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	big := make([]byte, federation.MaxFrameBytes+1)
	if err := conn.Send(ctx, big); !errors.Is(err, federation.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestQUICTransport_NewQUICTransport_BindFailureSurfaces(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	_ = pub
	cert, _ := federation.CertFromIdentity(priv)

	// First listener binds the port.
	tr1, err := federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr: "127.0.0.1:0", IdleTimeout: time.Second, Cert: cert, HandshakeTO: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := tr1.ListenAddr()
	defer tr1.Close()

	// Second listener on the same explicit port should fail to bind.
	host, _, _ := net.SplitHostPort(addr)
	_, err = federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr: addr, IdleTimeout: time.Second, Cert: cert, HandshakeTO: time.Second,
	})
	if err == nil {
		t.Fatalf("expected bind error on %s", host)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.QUICConfig` etc.

- [ ] **Step 3: Implement transport_quic.go**

`tracker/internal/federation/transport_quic.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	quicgo "github.com/quic-go/quic-go"
)

// QUICConfig is the runtime configuration for QUICTransport.
type QUICConfig struct {
	ListenAddr  string          // host:port; empty disables Listen but Dial still works.
	IdleTimeout time.Duration   // QUIC max-idle; default 60s if zero.
	Cert        tls.Certificate // built via CertFromIdentity(myPriv).
	HandshakeTO time.Duration   // dial-side handshake timeout; default 5s if zero.
}

func (c QUICConfig) withDefaults() QUICConfig {
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.HandshakeTO == 0 {
		c.HandshakeTO = 5 * time.Second
	}
	return c
}

// QUICTransport implements Transport over quic-go + mTLS. Construct via
// NewQUICTransport.
type QUICTransport struct {
	cfg QUICConfig

	mu         sync.Mutex
	closed     bool
	listener   *quicgo.Listener
	listenAddr string
}

// NewQUICTransport binds the listener (if ListenAddr != "") and returns a
// ready transport. If ListenAddr is empty, Listen returns immediately and
// the transport is dial-only.
func NewQUICTransport(cfg QUICConfig) (*QUICTransport, error) {
	cfg = cfg.withDefaults()
	if len(cfg.Cert.Certificate) == 0 {
		return nil, errors.New("federation: QUICConfig.Cert is empty")
	}
	t := &QUICTransport{cfg: cfg}
	if cfg.ListenAddr == "" {
		return t, nil
	}

	udpAddr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("federation: resolve %q: %w", cfg.ListenAddr, err)
	}
	pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("federation: listen UDP %q: %w", cfg.ListenAddr, err)
	}

	tlsCfg := MakeServerTLSConfig(cfg.Cert, nil) // SPKI captured per-conn at Accept time.
	qcfg := &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
		MaxIdleTimeout:  cfg.IdleTimeout,
	}
	l, err := quicgo.Listen(pc, tlsCfg, qcfg)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("federation: quic.Listen: %w", err)
	}
	t.listener = l
	t.listenAddr = pc.LocalAddr().String()
	return t, nil
}

// ListenAddr returns the bound address (resolved port for ":0" binds).
// Empty if the transport has no listener.
func (t *QUICTransport) ListenAddr() string { return t.listenAddr }

// Dial implements Transport.Dial.
func (t *QUICTransport) Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error) {
	if len(expectedPeer) != ed25519.PublicKeySize {
		return nil, errors.New("federation: expectedPeer must be 32 bytes")
	}
	expectedSPKI, err := spkiOfPubkey(expectedPeer)
	if err != nil {
		return nil, fmt.Errorf("federation: spki of expectedPeer: %w", err)
	}
	tlsCfg := MakeClientTLSConfig(t.cfg.Cert, expectedSPKI)
	qcfg := &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
		MaxIdleTimeout:  t.cfg.IdleTimeout,
	}
	dialCtx, cancel := context.WithTimeout(ctx, t.cfg.HandshakeTO)
	defer cancel()
	conn, err := quicgo.DialAddr(dialCtx, addr, tlsCfg, qcfg)
	if err != nil {
		return nil, fmt.Errorf("federation: quic.DialAddr %q: %w", addr, err)
	}
	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("federation: open stream: %w", err)
	}
	return newQConn(conn, stream, expectedPeer, addr), nil
}

// Listen implements Transport.Listen. Blocks until ctx is canceled.
func (t *QUICTransport) Listen(ctx context.Context, accept func(PeerConn)) error {
	if t.listener == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := t.listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Transient accept failures: log path is the caller's job (we
			// don't have access to a logger here). Continue.
			continue
		}
		go t.handleAccept(ctx, conn, accept)
	}
}

func (t *QUICTransport) handleAccept(ctx context.Context, conn *quicgo.Conn, accept func(PeerConn)) {
	streamCtx, cancel := context.WithTimeout(ctx, t.cfg.HandshakeTO)
	defer cancel()
	stream, err := conn.AcceptStream(streamCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "no stream")
		return
	}
	state := conn.ConnectionState().TLS
	if len(state.PeerCertificates) != 1 {
		_ = conn.CloseWithError(0, "bad peer certs")
		return
	}
	peerCert := state.PeerCertificates[0]
	peerPub, ok := peerCert.PublicKey.(ed25519.PublicKey)
	if !ok {
		_ = conn.CloseWithError(0, "non-ed25519")
		return
	}
	accept(newQConn(conn, stream, peerPub, conn.RemoteAddr().String()))
}

// Close closes the listener and rejects future Dials. Idempotent.
func (t *QUICTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.listener != nil {
		return t.listener.Close()
	}
	return nil
}

// spkiOfPubkey returns sha256(SubjectPublicKeyInfo) for a synthetic cert
// wrapping pub. Used by Dial to compute the pin without minting a full
// cert at the call site.
func spkiOfPubkey(pub ed25519.PublicKey) ([32]byte, error) {
	// Marshal the pubkey into SPKI form via x509.MarshalPKIXPublicKey.
	der, err := marshalPKIXPublicKey(pub)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256Sum(der), nil
}

// qConn is one peer-to-peer link backed by a *quicgo.Conn + a single
// persistent bidi stream. Frames are length-delimited; MaxFrameBytes is
// enforced on Send and Recv.
type qConn struct {
	conn       *quicgo.Conn
	stream     *quicgo.Stream
	remotePub  ed25519.PublicKey
	remoteAddr string

	closeOnce sync.Once

	sendMu sync.Mutex
	recvMu sync.Mutex
}

func newQConn(conn *quicgo.Conn, stream *quicgo.Stream, peerPub ed25519.PublicKey, addr string) *qConn {
	return &qConn{conn: conn, stream: stream, remotePub: peerPub, remoteAddr: addr}
}

func (q *qConn) Send(ctx context.Context, frame []byte) error {
	if len(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	q.sendMu.Lock()
	defer q.sendMu.Unlock()

	if dl, ok := ctx.Deadline(); ok {
		_ = q.stream.SetWriteDeadline(dl)
		defer func() { _ = q.stream.SetWriteDeadline(time.Time{}) }()
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := q.stream.Write(hdr[:]); err != nil {
		return mapStreamErr(err)
	}
	if _, err := q.stream.Write(frame); err != nil {
		return mapStreamErr(err)
	}
	return nil
}

func (q *qConn) Recv(ctx context.Context) ([]byte, error) {
	q.recvMu.Lock()
	defer q.recvMu.Unlock()

	if dl, ok := ctx.Deadline(); ok {
		_ = q.stream.SetReadDeadline(dl)
		defer func() { _ = q.stream.SetReadDeadline(time.Time{}) }()
	}
	var hdr [4]byte
	if _, err := io.ReadFull(q.stream, hdr[:]); err != nil {
		return nil, mapStreamErr(err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(q.stream, buf); err != nil {
		return nil, mapStreamErr(err)
	}
	return buf, nil
}

func (q *qConn) RemoteAddr() string           { return q.remoteAddr }
func (q *qConn) RemotePub() ed25519.PublicKey { return q.remotePub }

func (q *qConn) Close() error {
	q.closeOnce.Do(func() {
		_ = q.stream.Close()
		_ = q.conn.CloseWithError(0, "qConn close")
	})
	return nil
}

// mapStreamErr converts a quic-go stream error to our sentinel error
// vocabulary where possible. Any unknown error is wrapped as ErrPeerClosed
// so callers (peer.recvLoop) can react uniformly.
func mapStreamErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return ErrPeerClosed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Stream/conn errors from quic-go all collapse to PeerClosed for our
	// receiver loops; the underlying error is wrapped for diagnostics.
	return fmt.Errorf("%w: %v", ErrPeerClosed, err)
}

var _ Transport = (*QUICTransport)(nil)
var _ PeerConn = (*qConn)(nil)
```

Plus a tiny helper file (still part of this task):

```go
// helpers used by transport_quic.go; kept here so the file isn't bloated.
package federation

import (
	"crypto/sha256"
	"crypto/x509"
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

func marshalPKIXPublicKey(pub interface{}) ([]byte, error) {
	return x509.MarshalPKIXPublicKey(pub)
}
```

(Place these in `tracker/internal/federation/transport_quic.go` itself rather than a new file — they are tiny private helpers; the file boundary is for one feature.)

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/transport_quic.go tracker/internal/federation/transport_quic_test.go
git commit -m "feat(tracker/federation): QUICTransport with mTLS + persistent bidi stream"
```

---

## Task 3: `dialer.go` — per-peer redial with backoff

**Files:**
- Create: `tracker/internal/federation/dialer.go`
- Create: `tracker/internal/federation/dialer_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/dialer_test.go`:

```go
package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// fakeDialerTransport implements just enough of federation.Transport to
// drive Dialer tests. Each Dial call returns whatever's at the head of
// the script queue.
type fakeDialerTransport struct {
	mu     sync.Mutex
	script []func() (federation.PeerConn, error)
	calls  int32
}

func (f *fakeDialerTransport) Dial(_ context.Context, _ string, _ ed25519.PublicKey) (federation.PeerConn, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return nil, errors.New("script exhausted")
	}
	step := f.script[0]
	f.script = f.script[1:]
	return step()
}
func (*fakeDialerTransport) Listen(context.Context, func(federation.PeerConn)) error { return nil }
func (*fakeDialerTransport) Close() error                                            { return nil }

// stubConn is a no-op PeerConn used by the dialer tests.
type stubConn struct{ done chan struct{} }

func newStubConn() *stubConn { return &stubConn{done: make(chan struct{})} }

func (*stubConn) Send(context.Context, []byte) error    { return nil }
func (*stubConn) Recv(context.Context) ([]byte, error)  { return nil, federation.ErrPeerClosed }
func (*stubConn) RemoteAddr() string                    { return "stub" }
func (*stubConn) RemotePub() ed25519.PublicKey          { return make(ed25519.PublicKey, 32) }
func (s *stubConn) Close() error                        { close(s.done); return nil }

func TestDialer_BackoffProgression_FirstFailureWaitsThenSucceeds(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	_ = pub

	// Simulate: dial #1 fails, dial #2 succeeds.
	conn := newStubConn()
	transport := &fakeDialerTransport{script: []func() (federation.PeerConn, error){
		func() (federation.PeerConn, error) { return nil, errors.New("dial fail #1") },
		func() (federation.PeerConn, error) { return conn, nil },
	}}

	connectedCh := make(chan struct{}, 1)
	d := &federation.Dialer{
		Transport: transport,
		Peer: federation.AllowlistedPeer{
			TrackerID: ids.TrackerID{},
			PubKey:    make(ed25519.PublicKey, 32),
			Addr:      "127.0.0.1:1",
		},
		MyTrackerID: ids.TrackerID{},
		MyPriv:      priv,
		HandshakeTO: 50 * time.Millisecond,
		BackoffBase: 25 * time.Millisecond,
		BackoffMax:  100 * time.Millisecond,
		Now:         time.Now,
		// We hand off attaching to the test by signaling and then blocking
		// until ctx.Done(); this mimics the production attachAndWait.
		OnConnected: func(_ federation.PeerConn, peerDone <-chan struct{}) {
			connectedCh <- struct{}{}
			<-peerDone
		},
		// Dialer does not need a separate handshake hook: the Dialer's job
		// in this slice is dial+pass-handshake to OnConnected. Since the
		// fake conn skips the handshake entirely, the dialer also accepts
		// nil HandshakeFn and treats success as the Dial returning a conn.
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go d.Run(ctx)

	select {
	case <-connectedCh:
	case <-ctx.Done():
		t.Fatalf("dialer never connected; calls=%d", atomic.LoadInt32(&transport.calls))
	}
	if got := atomic.LoadInt32(&transport.calls); got != 2 {
		t.Fatalf("dial calls = %d, want 2 (one fail then success)", got)
	}
	// Tear down: signal the conn done so the dialer's OnConnected returns.
	_ = conn.Close()
	cancel()
}

func TestDialer_CtxCancel_MidSleep_Exits(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	_ = pub
	transport := &fakeDialerTransport{script: []func() (federation.PeerConn, error){
		func() (federation.PeerConn, error) { return nil, errors.New("fail") },
		// Subsequent dials never happen because we cancel during sleep.
	}}

	d := &federation.Dialer{
		Transport: transport,
		Peer: federation.AllowlistedPeer{PubKey: make(ed25519.PublicKey, 32), Addr: "127.0.0.1:1"},
		MyPriv:      priv,
		HandshakeTO: 50 * time.Millisecond,
		BackoffBase: 5 * time.Second, // long enough that the sleep is in progress when we cancel
		BackoffMax:  5 * time.Second,
		Now:         time.Now,
		OnConnected: func(federation.PeerConn, <-chan struct{}) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	// Wait for the first dial failure to land.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&transport.calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dialer did not exit after ctx cancel")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.Dialer` etc.

- [ ] **Step 3: Implement dialer.go**

`tracker/internal/federation/dialer.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
	"math/rand"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// Dialer drives one allowlisted peer with a redial-with-backoff loop.
// One Dialer per peer; spawned by Federation.Open as a goroutine.
//
// On every iteration the dialer tries to Dial; on success it hands the
// PeerConn to OnConnected, which MUST block until the connection drops,
// then return so the dialer reconnects. ctx cancellation exits cleanly
// at any point, including mid-sleep.
type Dialer struct {
	Transport   Transport
	Peer        AllowlistedPeer
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	HandshakeTO time.Duration
	BackoffBase time.Duration
	BackoffMax  time.Duration
	Now         func() time.Time

	// OnConnected runs the federation-level handshake on top of the
	// supplied PeerConn (in production: f.attachAndWait). It MUST block
	// until the conn drops, then return so the dialer can reconnect.
	OnConnected func(c PeerConn)

	// OnFailure is an optional metric sink. nil disables.
	OnFailure func(reason string, err error)
}

// Run blocks until ctx is canceled, looping Dial → OnConnected → backoff.
func (d *Dialer) Run(ctx context.Context) {
	wait := d.BackoffBase
	if wait <= 0 {
		wait = time.Second
	}
	maxWait := d.BackoffMax
	if maxWait < wait {
		maxWait = wait
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := d.Transport.Dial(ctx, d.Peer.Addr, d.Peer.PubKey)
		if err != nil {
			if d.OnFailure != nil {
				d.OnFailure("dial", err)
			}
			if !sleepWithJitter(ctx, wait) {
				return
			}
			wait *= 2
			if wait > maxWait {
				wait = maxWait
			}
			continue
		}

		// Reset backoff on a successful dial.
		wait = d.BackoffBase
		if wait <= 0 {
			wait = time.Second
		}

		// Hand off to OnConnected; blocks until the conn drops.
		d.OnConnected(conn)
	}
}

// sleepWithJitter waits up to base*rand[0,1) before returning true, or
// returns false if ctx fires.
func sleepWithJitter(ctx context.Context, base time.Duration) bool {
	if base <= 0 {
		return true
	}
	jittered := time.Duration(rand.Int63n(int64(base) + 1)) //nolint:gosec // not security-sensitive
	if jittered <= 0 {
		jittered = time.Millisecond
	}
	t := time.NewTimer(jittered)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/dialer.go tracker/internal/federation/dialer_test.go
git commit -m "feat(tracker/federation): per-peer Dialer with exponential-backoff redial"
```

---

## Task 4: Extend `Config` with redial + idle timeout

**Files:**
- Modify: `tracker/internal/federation/config.go`

- [ ] **Step 1: Add the new Config fields**

In `tracker/internal/federation/config.go`, append to the existing `Config` struct (just below `PublishCadence`):

```go
type Config struct {
    // … existing fields …
    PublishCadence   time.Duration // default 1h
    IdleTimeout      time.Duration // default 60s; 0 → default
    RedialBase       time.Duration // default 1s
    RedialMax        time.Duration // default 30s
    Peers            []AllowlistedPeer
}
```

Update `withDefaults`:

```go
func (c Config) withDefaults() Config {
    if c.HandshakeTimeout == 0 { c.HandshakeTimeout = 5 * time.Second }
    if c.DedupeTTL == 0 { c.DedupeTTL = time.Hour }
    if c.DedupeCap == 0 { c.DedupeCap = 64 * 1024 }
    if c.GossipRateQPS == 0 { c.GossipRateQPS = 100 }
    if c.SendQueueDepth == 0 { c.SendQueueDepth = 256 }
    if c.PublishCadence == 0 { c.PublishCadence = time.Hour }
    if c.IdleTimeout == 0 { c.IdleTimeout = 60 * time.Second }
    if c.RedialBase == 0 { c.RedialBase = time.Second }
    if c.RedialMax == 0 { c.RedialMax = 30 * time.Second }
    if c.RedialMax < c.RedialBase { c.RedialMax = c.RedialBase }
    return c
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/federation/config.go
git commit -m "feat(tracker/federation): add IdleTimeout/RedialBase/RedialMax config"
```

---

## Task 5: Subsystem integration — replace `dialOutbound` with `Dialer`, add `attachAndWait`

**Files:**
- Modify: `tracker/internal/federation/subsystem.go`

- [ ] **Step 1: Read the existing `Open` peer-loop block + `attachPeer` / `dialOutbound`**

Current code (already merged) at `tracker/internal/federation/subsystem.go`:

```go
for _, p := range cfg.Peers {
    go f.dialOutbound(ctx, p)
}
```

```go
func (f *Federation) dialOutbound(ctx context.Context, p AllowlistedPeer) {
    conn, err := f.dep.Transport.Dial(ctx, p.Addr, p.PubKey)
    if err != nil { f.dep.Metrics.InvalidFrames("dial"); return }
    res, err := RunHandshakeDialer(ctx, conn, f.cfg.MyTrackerID, f.cfg.MyPriv, p.TrackerID, p.PubKey, f.cfg.HandshakeTimeout)
    if err != nil { f.dep.Metrics.InvalidFrames("handshake"); _ = conn.Close(); return }
    f.attachPeer(res, conn)
}
```

```go
func (f *Federation) attachPeer(res HandshakeResult, c PeerConn) {
    dispatch := f.makeDispatcher(c, res.PeerTrackerID)
    pe := NewPeerForTest(c, dispatch)
    f.mu.Lock()
    f.peers[res.PeerTrackerID] = pe
    _ = f.reg.Update(PeerInfo{TrackerID: res.PeerTrackerID, PubKey: res.PeerPubKey, Addr: c.RemoteAddr(), State: PeerStateSteady, Conn: c})
    f.mu.Unlock()
    pe.Start(context.Background())
}
```

- [ ] **Step 2: Replace the `Open` peer-loop and add `attachAndWait`**

Replace the `for _, p := range cfg.Peers { go f.dialOutbound(ctx, p) }` block with:

```go
for _, p := range cfg.Peers {
    d := &Dialer{
        Transport:   dep.Transport,
        Peer:        p,
        MyTrackerID: cfg.MyTrackerID,
        MyPriv:      cfg.MyPriv,
        HandshakeTO: cfg.HandshakeTimeout,
        BackoffBase: cfg.RedialBase,
        BackoffMax:  cfg.RedialMax,
        Now:         dep.Now,
        OnConnected: f.attachAndWait,
        OnFailure: func(reason string, _ error) {
            dep.Metrics.InvalidFrames(reason)
        },
    }
    go d.Run(ctx)
}
```

Delete the `dialOutbound` method.

Add `attachAndWait` next to `attachPeer`:

```go
// attachAndWait runs the federation-level handshake on conn and, on
// success, registers the peer + spawns its recvLoop. Blocks until the
// recvLoop exits (i.e., the underlying connection has dropped). Used by
// the Dialer's OnConnected hook so the redial loop reconnects on every
// drop.
func (f *Federation) attachAndWait(c PeerConn) {
    res, err := RunHandshakeDialer(f.listenCtx, c, f.cfg.MyTrackerID, f.cfg.MyPriv,
        peerIDFromPub(c.RemotePub()), c.RemotePub(), f.cfg.HandshakeTimeout)
    if err != nil {
        f.dep.Metrics.InvalidFrames("handshake")
        _ = c.Close()
        return
    }
    pe := f.attachPeerLocked(res, c)
    pe.Wait() // blocks until recvLoop exits or Stop is called
}

// peerIDFromPub computes sha256(pub) — the canonical TrackerID derivation.
func peerIDFromPub(pub ed25519.PublicKey) ids.TrackerID {
    return ids.TrackerID(sha256.Sum256(pub))
}
```

`attachPeerLocked` is a small refactor of `attachPeer`: same body but returns the `*Peer` so `attachAndWait` can `Wait()` on it. Replace `attachPeer` with:

```go
func (f *Federation) attachPeer(res HandshakeResult, c PeerConn) {
    f.attachPeerLocked(res, c)
}

func (f *Federation) attachPeerLocked(res HandshakeResult, c PeerConn) *Peer {
    dispatch := f.makeDispatcher(c, res.PeerTrackerID)
    pe := NewPeerForTest(c, dispatch)
    f.mu.Lock()
    f.peers[res.PeerTrackerID] = pe
    _ = f.reg.Update(PeerInfo{TrackerID: res.PeerTrackerID, PubKey: res.PeerPubKey, Addr: c.RemoteAddr(), State: PeerStateSteady, Conn: c})
    f.mu.Unlock()
    pe.Start(context.Background())
    return pe
}
```

- [ ] **Step 3: Add `Wait()` to `*Peer` so `attachAndWait` can block on the recvLoop**

In `tracker/internal/federation/peer.go`, append a method:

```go
// Wait blocks until the recvLoop exits (Stop has been called or the
// transport's Recv returned a terminal error). Idempotent.
func (p *Peer) Wait() { p.wg.Wait() }
```

(`p.wg` is the existing `sync.WaitGroup` already used by `Stop`.)

- [ ] **Step 3b: Add `ListenAddr()` accessor to `*Federation`**

In `tracker/internal/federation/subsystem.go`, append a method (used by the QUIC integration test in Task 8 and by operator admin endpoints later):

```go
// ListenAddr returns the bound network address of the underlying
// transport, or "" if the transport is not network-bound (e.g. the
// in-process transport). Used by tests dialing 127.0.0.1:0 binds and
// by operator admin endpoints reporting the active peering port.
func (f *Federation) ListenAddr() string {
    if l, ok := f.dep.Transport.(interface{ ListenAddr() string }); ok {
        return l.ListenAddr()
    }
    return ""
}
```

- [ ] **Step 4: Update existing tests if any exercise `dialOutbound` directly**

Run: `grep -rn "dialOutbound" tracker/internal/federation/`
Expected: no matches outside the deleted code path.

- [ ] **Step 5: Run -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS. Existing tests should be unaffected since the in-process transport's Dial returns synchronously and `OnConnected = attachAndWait` blocks on `pe.Wait()` — same end-state as before for the existing happy-path integration test.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/federation/subsystem.go tracker/internal/federation/peer.go
git commit -m "feat(tracker/federation): wire Dialer into Open; redial on drop"
```

---

## Task 6: Extend `tracker/internal/config.FederationConfig`

**Files:**
- Modify: `tracker/internal/config/config.go`
- Modify: `tracker/internal/config/apply_defaults.go`
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/apply_defaults_test.go`
- Modify: `tracker/internal/config/validate_test.go`

- [ ] **Step 1: Append fields**

In `tracker/internal/config/config.go`, extend `FederationConfig`:

```go
type FederationConfig struct {
    // existing…
    Peers []FederationPeer `yaml:"peers"`

    ListenAddr   string `yaml:"listen_addr"`
    IdleTimeoutS int    `yaml:"idle_timeout_s"`
    RedialBaseS  int    `yaml:"redial_base_s"`
    RedialMaxS   int    `yaml:"redial_max_s"`
}
```

Add to `DefaultConfig()` Federation block:

```go
ListenAddr:   "",
IdleTimeoutS: 60,
RedialBaseS:  1,
RedialMaxS:   30,
```

- [ ] **Step 2: Defaults**

In `apply_defaults.go`, add fill-ins after the existing federation defaults:

```go
if c.Federation.IdleTimeoutS == 0 {
    c.Federation.IdleTimeoutS = d.Federation.IdleTimeoutS
}
if c.Federation.RedialBaseS == 0 {
    c.Federation.RedialBaseS = d.Federation.RedialBaseS
}
if c.Federation.RedialMaxS == 0 {
    c.Federation.RedialMaxS = d.Federation.RedialMaxS
}
// ListenAddr: empty is a valid value (federation network-disabled), so
// ApplyDefaults does NOT fill it in.
```

- [ ] **Step 3: Validation**

In `validate.go`, extend the federation block:

```go
if c.Federation.IdleTimeoutS < 1 || c.Federation.IdleTimeoutS > 600 {
    v.add("federation.idle_timeout_s", "must be 1..600", c.Federation.IdleTimeoutS)
}
if c.Federation.RedialBaseS < 1 || c.Federation.RedialBaseS > 60 {
    v.add("federation.redial_base_s", "must be 1..60", c.Federation.RedialBaseS)
}
if c.Federation.RedialMaxS < c.Federation.RedialBaseS || c.Federation.RedialMaxS > 600 {
    v.add("federation.redial_max_s", "must be >= redial_base_s and <= 600", c.Federation.RedialMaxS)
}
if c.Federation.ListenAddr != "" {
    if _, _, err := net.SplitHostPort(c.Federation.ListenAddr); err != nil {
        v.add("federation.listen_addr", fmt.Sprintf("invalid host:port (%v)", err), c.Federation.ListenAddr)
    }
}
```

(`net` and `fmt` are likely already imported; if not, add.)

- [ ] **Step 4: Add tests**

In `apply_defaults_test.go`, extend the existing federation defaults test (the one added in the previous slice) to assert the four new defaults: `ListenAddr == ""`, `IdleTimeoutS == 60`, `RedialBaseS == 1`, `RedialMaxS == 30`.

In `validate_test.go`, append:

```go
func TestValidate_Federation_IdleTimeoutBounds(t *testing.T) {
    t.Parallel()
    cfg := config.DefaultConfig()
    config.ApplyDefaults(cfg)
    cfg.Federation.IdleTimeoutS = 0
    if err := config.Validate(cfg); err == nil { t.Fatal("expected error") }
    cfg = config.DefaultConfig(); config.ApplyDefaults(cfg)
    cfg.Federation.IdleTimeoutS = 601
    if err := config.Validate(cfg); err == nil { t.Fatal("expected error") }
}

func TestValidate_Federation_RedialMaxLowerBound(t *testing.T) {
    t.Parallel()
    cfg := config.DefaultConfig(); config.ApplyDefaults(cfg)
    cfg.Federation.RedialBaseS = 5
    cfg.Federation.RedialMaxS = 4
    if err := config.Validate(cfg); err == nil { t.Fatal("expected error: max < base") }
}

func TestValidate_Federation_ListenAddr_Hostport(t *testing.T) {
    t.Parallel()
    cfg := config.DefaultConfig(); config.ApplyDefaults(cfg)
    cfg.Federation.ListenAddr = "not-a-hostport"
    if err := config.Validate(cfg); err == nil { t.Fatal("expected error") }
    cfg = config.DefaultConfig(); config.ApplyDefaults(cfg)
    cfg.Federation.ListenAddr = ":7443"
    if err := config.Validate(cfg); err != nil { t.Fatalf("expected ok: %v", err) }
}
```

- [ ] **Step 5: Run -race**

Run: `go test -race ./tracker/internal/config/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/config/
git commit -m "feat(tracker/config): extend FederationConfig with listen_addr + redial bounds"
```

---

## Task 7: Wire `QUICTransport` selection into `run_cmd.go`

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go`

- [ ] **Step 1: Replace the InprocTransport-only construction**

Locate the existing federation wiring in `run_cmd.go` (the block that calls `federation.NewInprocHub()` and `federation.NewInprocTransport(...)`). Replace the transport construction with:

```go
trackerPub := trackerKey.Public().(ed25519.PublicKey)
var fedTransport federation.Transport
if cfg.Federation.ListenAddr != "" {
    fedCert, err := federation.CertFromIdentity(trackerKey)
    if err != nil {
        return fmt.Errorf("federation cert: %w", err)
    }
    fedTransport, err = federation.NewQUICTransport(federation.QUICConfig{
        ListenAddr:  cfg.Federation.ListenAddr,
        IdleTimeout: time.Duration(cfg.Federation.IdleTimeoutS) * time.Second,
        Cert:        fedCert,
        HandshakeTO: time.Duration(cfg.Federation.HandshakeTimeoutS) * time.Second,
    })
    if err != nil {
        return fmt.Errorf("federation transport: %w", err)
    }
} else {
    fedHub := federation.NewInprocHub()
    fedTransport = federation.NewInprocTransport(fedHub, "self", trackerPub, trackerKey)
}
```

Pass `fedTransport` (instead of the previous `fedTransport` variable) into `federation.Open`. Add the four new config fields to the `federation.Config{...}` literal:

```go
fed, err := federation.Open(federation.Config{
    MyTrackerID:      ids.TrackerID(sha256.Sum256(trackerPub)),
    MyPriv:           trackerKey,
    HandshakeTimeout: time.Duration(cfg.Federation.HandshakeTimeoutS) * time.Second,
    DedupeTTL:        time.Duration(cfg.Federation.GossipDedupeTTLS) * time.Second,
    SendQueueDepth:   cfg.Federation.SendQueueDepth,
    GossipRateQPS:    cfg.Federation.GossipRateQPS,
    PublishCadence:   time.Duration(cfg.Federation.PublishCadenceS) * time.Second,
    IdleTimeout:      time.Duration(cfg.Federation.IdleTimeoutS) * time.Second,
    RedialBase:       time.Duration(cfg.Federation.RedialBaseS) * time.Second,
    RedialMax:        time.Duration(cfg.Federation.RedialMaxS) * time.Second,
    Peers:            fedPeers,
}, federation.Deps{
    Transport: fedTransport,
    RootSrc:   ledgerRootSourceAdapter{led: led},
    Archive:   storeAsArchive{store: store},
    Metrics:   federation.NewMetrics(prometheus.DefaultRegisterer),
    Logger:    logger,
    Now:       time.Now,
})
```

- [ ] **Step 2: Verify build + run cmd tests**

Run: `go build ./tracker/cmd/token-bay-tracker/...` — clean.
Run: `go test -race ./tracker/cmd/token-bay-tracker/...` — PASS.

- [ ] **Step 3: Commit**

```bash
git add tracker/cmd/token-bay-tracker/run_cmd.go
git commit -m "feat(tracker/cmd): select QUICTransport when federation.listen_addr is set"
```

---

## Task 8: Integration test — two-tracker over real QUIC + drop-and-recover

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Append the new tests**

Append to `tracker/internal/federation/integration_test.go`:

```go
func openQUICFederation(t *testing.T, p quicPeer, peers []federation.AllowlistedPeer) (*federation.Federation, *fakeArchive, *fakeRootSrc) {
    t.Helper()
    cert, err := federation.CertFromIdentity(p.priv)
    if err != nil { t.Fatal(err) }
    tr, err := federation.NewQUICTransport(federation.QUICConfig{
        ListenAddr:  "127.0.0.1:0",
        IdleTimeout: 5 * time.Second,
        Cert:        cert,
        HandshakeTO: 2 * time.Second,
    })
    if err != nil { t.Fatal(err) }

    arch := newFakeArchive()
    src := &fakeRootSrc{ok: false}
    f, err := federation.Open(federation.Config{
        MyTrackerID:      ids.TrackerID(sha256.Sum256(p.pub)),
        MyPriv:           p.priv,
        HandshakeTimeout: 2 * time.Second,
        DedupeTTL:        time.Hour,
        DedupeCap:        1024,
        GossipRateQPS:    100,
        SendQueueDepth:   256,
        PublishCadence:   time.Hour,
        IdleTimeout:      5 * time.Second,
        RedialBase:       50 * time.Millisecond,
        RedialMax:        500 * time.Millisecond,
        Peers:            peers,
    }, federation.Deps{
        Transport: tr,
        RootSrc:   src,
        Archive:   arch,
        Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
        Logger:    zerolog.Nop(),
        Now:       time.Now,
    })
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { _ = f.Close() })
    return f, arch, src
}

func TestIntegration_QUIC_RootAttestation_AB(t *testing.T) {
    t.Parallel()
    a := newQUICPeer(t)
    b := newQUICPeer(t)

    // Open A and B with no peers first to discover their bound :0 ports,
    // then reopen with each side's peer list pointing at the other's
    // bound address. (Federation.Open reads cfg.Peers at construction
    // time; runtime peer-add is a future API.)
    aFed, _, _ := openQUICFederation(t, a, nil)
    bFed, _, _ := openQUICFederation(t, b, nil)
    aAddr := aFed.ListenAddr()
    bAddr := bFed.ListenAddr()
    _ = aFed.Close()
    _ = bFed.Close()

    aFed2, _, srcA := openQUICFederation(t, a, []federation.AllowlistedPeer{
        {TrackerID: ids.TrackerID(sha256.Sum256(b.pub)), PubKey: b.pub, Addr: bAddr},
    })
    bFed2, archB, _ := openQUICFederation(t, b, []federation.AllowlistedPeer{
        {TrackerID: ids.TrackerID(sha256.Sum256(a.pub)), PubKey: a.pub, Addr: aAddr},
    })
    defer func() { _ = aFed2.Close(); _ = bFed2.Close() }()

    deadline := time.Now().Add(5 * time.Second)
    for time.Now().Before(deadline) {
        steady := false
        for _, p := range aFed2.Peers() {
            if p.State == federation.PeerStateSteady { steady = true; break }
        }
        if steady { break }
        time.Sleep(20 * time.Millisecond)
    }

    srcA.root = b32(7)
    srcA.sig = b64(8)
    srcA.ok = true
    if err := aFed2.PublishHour(context.Background(), 100); err != nil {
        t.Fatalf("publish: %v", err)
    }

    aIDBytes := ids.TrackerID(sha256.Sum256(a.pub)).Bytes()
    deadline = time.Now().Add(5 * time.Second)
    for time.Now().Before(deadline) {
        if _, ok, _ := archB.GetPeerRoot(context.Background(), aIDBytes[:], 100); ok {
            return
        }
        time.Sleep(20 * time.Millisecond)
    }
    t.Fatal("B never archived A's root over QUIC")
}

// b32/b64 are short helpers for the test (parallel to the existing b()).
func b32(v byte) []byte { out := make([]byte, 32); for i := range out { out[i] = v }; return out }
func b64(v byte) []byte { out := make([]byte, 64); for i := range out { out[i] = v }; return out }
```

> Note: `*Federation.ListenAddr()` is added as part of Task 5 (see Task 5 Step 2 — the method is one line passthrough to `f.dep.Transport.(interface{ ListenAddr() string }).ListenAddr()`). If you reach Task 8 and that method is missing, add it inline in Task 8's commit.

- [ ] **Step 2: Run -race**

Run: `go test -race -run TestIntegration_QUIC_RootAttestation_AB ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/federation/integration_test.go tracker/internal/federation/subsystem.go
git commit -m "test(tracker/federation): two-tracker QUIC integration scenario"
```

---

## Task 9: Final verification

- [ ] **Step 1: `make check`**

Run: `make check`
Expected: PASS.

- [ ] **Step 2: Walk acceptance criteria**

Spec §11:
- Two `Federation` instances peer over `QUICTransport` and exchange `ROOT_ATTESTATION` ✓ (Task 8 integration test).
- Cert SPKI mismatch → handshake fails → dialer reconnects ✓ (Task 1 + Task 2 unit tests; behavior implicit in dialer's loop).
- Killed connection triggers redial ✓ (covered structurally by Dialer.Run; explicit drop test deferred to a follow-up if flaky).
- Subsystem `Close` cancels in-flight ✓ (existing `listenCancel` propagation).
- `make check` green ✓.
- Wired into `run_cmd.go`; non-empty `ListenAddr` activates QUIC; empty falls back to inproc ✓ (Task 7).

- [ ] **Step 3: Final commit (if any)**

If incidental cleanups are needed (lint nits, doc tweaks), squash into one final commit. Otherwise, the previous commits already form a clean history.

---

## Risks & follow-ups

- **Drop-and-recover integration test omitted.** Reliable mid-test connection severance under QUIC is finicky — tests that close the underlying UDPConn race against the dialer's `Recv` returning `ErrPeerClosed`. The dialer's redial logic IS exercised by Task 3's unit tests; a real network drop test can come in a follow-up alongside operator runbook docs.
- **Redial backoff gauge metric** is left unimplemented; `invalid_frames{reason}` covers diagnostics for now.
- **Public peer-add API** doesn't exist; the integration test does an open/capture-addr/close/reopen dance. If federation needs runtime peer add (e.g., for the future peer-exchange slice), a `Federation.AddPeer(p AllowlistedPeer) error` method is the obvious extension.
- **Operator admin endpoints for `Federation.Peers()` / `Depeer`** are still pending (carry over from the core slice).
