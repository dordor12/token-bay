# Federation — Plugin Bootstrap Signing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement federation §7.2 plugin bootstrap signing: a new plugin-tracker RPC (`RPC_METHOD_BOOTSTRAP_PEERS`) that returns a signed `BootstrapPeerList` of the tracker's top-N known-peers (by `health_score`, slice-3 placeholder values for now). The plugin-side `Client.FetchBootstrapPeers` verifies the signature against the connected tracker's already-validated Ed25519 pubkey, checks `expires_at`, and returns a parsed `[]BootstrapPeer`. Real `health_score` (§7.3) and downstream plugin reroute logic are deferred to slice 5 and beyond.

**Architecture:** Slice 4 sits at the plugin↔tracker RPC boundary, **not** the tracker↔tracker federation channel. The wire format lives in `shared/proto/` (next to other plugin-tracker RPCs). Tracker-side: `tracker/internal/api` adds a handler that calls slice-3's `storage.ListKnownPeers(ctx, N+1, byHealthDesc=true)`, drops the issuer's self-row, signs with the tracker's existing identity Ed25519 key, and returns the marshalled list. Plugin-side: `plugin/internal/trackerclient` adds `Client.FetchBootstrapPeers(ctx)` that invokes the new RPC, verifies the sig with the connection's peer pubkey (newly exposed via `transport.Conn.PeerPublicKey()`), checks `expires_at` against `now`, and returns the list. Per-entry signing is intentionally absent — the outer envelope's signature is the only crypto binding.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf`, stdlib `crypto/ed25519`, `prometheus/client_golang`, `quic-go` (existing), `testify`.

**Spec:** `docs/superpowers/specs/federation/2026-05-10-federation-plugin-bootstrap-design.md` (commit `880dc55` on this branch).

---

## File Structure

| Path | Action | Responsibility |
|---|---|---|
| `shared/proto/rpc.proto` | MODIFY | `RPC_METHOD_BOOTSTRAP_PEERS = 10`, `BootstrapPeersRequest`, `BootstrapPeer`, `BootstrapPeerList` |
| `shared/proto/rpc.pb.go` | REGEN | `make -C shared proto-gen`; regenerated, not hand-edited |
| `shared/proto/validate.go` | MODIFY | Add `ValidateBootstrapPeerList` + caps; lift `ValidateRPCRequest` method whitelist |
| `shared/proto/validate_test.go` | MODIFY | Tests for `ValidateBootstrapPeerList` happy + bad-shape table; `RPC_METHOD_BOOTSTRAP_PEERS` accepted |
| `shared/proto/canonical.go` | CREATE | `CanonicalBootstrapPeerListPreSig`, `SignBootstrapPeerList`, `VerifyBootstrapPeerListSig`, `ErrBootstrapPeerListBadSig` |
| `shared/proto/canonical_test.go` | CREATE | Round-trip + tampered-sig + wrong-pubkey + sig-excluded-from-canonical |
| `plugin/internal/trackerclient/internal/transport/transport.go` | MODIFY | Add `PeerPublicKey() ed25519.PublicKey` to `Conn` interface |
| `plugin/internal/trackerclient/internal/transport/quic/quic.go` | MODIFY | Implement `PeerPublicKey()` from cert SPKI |
| `plugin/internal/trackerclient/internal/transport/loopback/loopback.go` | MODIFY | Implement `PeerPublicKey()`; tests pass a synthetic Ed25519 pubkey |
| `tracker/internal/api/bootstrap_peers.go` | CREATE | `BootstrapPeersService` iface, `installBootstrapPeers`, `bootstrapPeersHandler` |
| `tracker/internal/api/bootstrap_peers_test.go` | CREATE | Handler unit tests with a `fakeBootstrapService` |
| `tracker/internal/api/router.go` | MODIFY | Install `BOOTSTRAP_PEERS` handler; new `Deps.BootstrapPeers` slot |
| `tracker/internal/api/router_test.go` | MODIFY | Add OK + nil-deps cases for `BOOTSTRAP_PEERS` |
| `tracker/internal/config/config.go` | MODIFY | Add `FederationBootstrapConfig` sub-section to `FederationConfig` |
| `tracker/internal/config/apply_defaults.go` | MODIFY | Default `MaxPeers=50`, `TTLSeconds=600` |
| `tracker/internal/config/validate.go` | MODIFY | Validate ranges (MaxPeers ∈ [1,256]; TTLSeconds ∈ [60, 3600]) |
| `tracker/internal/config/testdata/full.yaml` | MODIFY | Explicit values for the new keys |
| `tracker/internal/config/validate_test.go` | MODIFY | Reject out-of-range values |
| `tracker/cmd/token-bay-tracker/federation_adapters.go` | MODIFY | Add `bootstrapPeersAdapter` |
| `tracker/cmd/token-bay-tracker/run_cmd.go` | MODIFY | Build adapter, pass into `api.Deps.BootstrapPeers` |
| `tracker/internal/api/metrics.go` *(or existing metrics file)* | MODIFY/CREATE | New counters/gauges (see Task 12) |
| `plugin/internal/trackerclient/types.go` | MODIFY | Plugin-side `BootstrapPeer` struct + sentinel errors |
| `plugin/internal/trackerclient/bootstrap_peers.go` | CREATE | `Client.FetchBootstrapPeers`, verification, parse |
| `plugin/internal/trackerclient/bootstrap_peers_test.go` | CREATE | OK / bad-sig / expired / skew / id-mismatch / invalid-payload / rpc-error |
| `plugin/internal/trackerclient/metrics.go` *(or existing)* | MODIFY/CREATE | `bootstrap_peers_fetched_total{outcome}` |

---

## Notes for the executor

- Read the spec (`docs/superpowers/specs/federation/2026-05-10-federation-plugin-bootstrap-design.md`) before Task 1; it pins the wire shape, validator caps, signing pattern, and acceptance criteria you'll be encoding.
- Read `tracker/internal/api/balance.go` first — slice 4's tracker handler mirrors that file's pattern (per-handler-narrow interface, `installXxx` constructor returning `notImpl` when deps are nil, payload-marshal-`OkResponse` flow).
- Read `plugin/internal/trackerclient/rpc.go` first — slice 4's plugin method follows `Balance` / `BrokerRequest` shape: build proto request, call `c.callUnary`, validate + parse response.
- Read `tracker/internal/federation/transfer.go:160-180` for the canonical-bytes + Ed25519-sign pattern slice 4 reuses (the canonical helper zeroes `Sig`, marshals deterministically, signs with `ed25519.Sign`).
- The plugin trackerclient transport currently exposes only `PeerIdentityID() = SHA-256(SPKI)` — slice 4 needs the raw Ed25519 pubkey for verification, so Task 6/7 widen the `Conn` interface. This is a leaf interface (only two implementations: `quic` and `loopback`); the change is contained.
- One conventional commit per red-green cycle (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `chore:`). Don't squash multiple TDD cycles into one commit.
- Working directory throughout: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/federation`. The `go.work` file links the three modules; `make check` at the repo root runs all of them.
- Use module-rooted paths in `go test`: `go test ./shared/proto/...`, `go test ./tracker/internal/api/...`, `go test ./plugin/internal/trackerclient/...` — no `cd` first.
- All three modules are on the always-`-race` list. Bootstrap-peers code is not concurrent (one RPC, one handler, one plugin call), but tests still run with `-race`.
- The `RpcMethod` whitelist in `shared/proto/validate.go:194-202` must be widened to include the new method, otherwise the trackerclient's request-side validator rejects `RPC_METHOD_BOOTSTRAP_PEERS` before it ever reaches the wire (Task 3).

---

### Task 1: Wire format additions — proto + regen

**Files:**
- Modify: `shared/proto/rpc.proto`

- [ ] **Step 1: Edit the proto.** Add `RPC_METHOD_BOOTSTRAP_PEERS = 10;` to the `RpcMethod` enum (right after `RPC_METHOD_TURN_RELAY_OPEN = 9;`). Append the new messages immediately after `TurnRelayOpenResponse` (before the `// --- Push channel messages ---` separator).

In the `RpcMethod` enum:

```proto
  RPC_METHOD_TURN_RELAY_OPEN  = 9;
  RPC_METHOD_BOOTSTRAP_PEERS  = 10;
```

Append after `TurnRelayOpenResponse`:

```proto
message BootstrapPeersRequest {}

message BootstrapPeer {
  bytes  tracker_id   = 1;  // 32 bytes — SHA-256 of peer tracker's SPKI
  string addr         = 2;  // host:port (UDP for QUIC); len ≤ 256, valid UTF-8
  string region_hint  = 3;  // human-friendly; len ≤ 64, valid UTF-8
  double health_score = 4;  // 0..1; NaN rejected by validator
  uint64 last_seen    = 5;  // unix seconds
}

message BootstrapPeerList {
  bytes  issuer_id  = 1;  // 32 — issuing tracker's IdentityID
  uint64 signed_at  = 2;  // unix seconds
  uint64 expires_at = 3;  // unix seconds
  repeated BootstrapPeer peers = 4;
  bytes  sig        = 5;  // 64 — Ed25519 over CanonicalBootstrapPeerListPreSig(list)
}
```

- [ ] **Step 2: Regenerate.** Run `make -C shared proto-gen`. Expect `shared/proto/rpc.pb.go` to update with `RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS`, `BootstrapPeersRequest`, `BootstrapPeer`, `BootstrapPeerList`.

- [ ] **Step 3: Compile-check.** Run `go build ./shared/proto/...`. Expected: success. (No callers reference the new types yet.)

- [ ] **Step 4: Commit.**

```bash
git add shared/proto/rpc.proto shared/proto/rpc.pb.go
git commit -m "feat(shared/proto): RPC_METHOD_BOOTSTRAP_PEERS + BootstrapPeerList wire format"
```

---

### Task 2: ValidateBootstrapPeerList + RPC method ceiling (red)

**Files:**
- Modify: `shared/proto/validate_test.go`

- [ ] **Step 1: Append failing tests** to `shared/proto/validate_test.go`. The file is in package `proto`, so types like `BootstrapPeerList` are available unqualified.

```go
func validBootstrapPeer() *BootstrapPeer {
	return &BootstrapPeer{
		TrackerId:   bytes.Repeat([]byte{1}, 32),
		Addr:        "tracker.example.org:443",
		RegionHint:  "eu-central-1",
		HealthScore: 0.9,
		LastSeen:    1714000000,
	}
}

func validBootstrapPeerList() *BootstrapPeerList {
	bl := &BootstrapPeerList{
		IssuerId:  bytes.Repeat([]byte{2}, 32),
		SignedAt:  1714000000,
		ExpiresAt: 1714000600,
		Peers:     []*BootstrapPeer{validBootstrapPeer()},
		Sig:       bytes.Repeat([]byte{3}, ed25519.SignatureSize),
	}
	return bl
}

func TestValidateBootstrapPeerList_HappyPath(t *testing.T) {
	require.NoError(t, ValidateBootstrapPeerList(validBootstrapPeerList()))
}

func TestValidateBootstrapPeerList_EmptyPeersOK(t *testing.T) {
	bl := validBootstrapPeerList()
	bl.Peers = nil
	require.NoError(t, ValidateBootstrapPeerList(bl))
}

func TestValidateBootstrapPeerList_BadShape(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*BootstrapPeerList)
	}{
		{"nil msg", func(_ *BootstrapPeerList) {}},
		{"issuer_id wrong len", func(m *BootstrapPeerList) { m.IssuerId = bytes.Repeat([]byte{2}, 16) }},
		{"sig wrong len", func(m *BootstrapPeerList) { m.Sig = bytes.Repeat([]byte{3}, 32) }},
		{"signed_at zero", func(m *BootstrapPeerList) { m.SignedAt = 0 }},
		{"expires_at not after signed_at", func(m *BootstrapPeerList) { m.ExpiresAt = m.SignedAt }},
		{"lifetime > 1h", func(m *BootstrapPeerList) { m.ExpiresAt = m.SignedAt + 3601 }},
		{"too many entries", func(m *BootstrapPeerList) {
			m.Peers = make([]*BootstrapPeer, 257)
			for i := range m.Peers {
				m.Peers[i] = validBootstrapPeer()
			}
		}},
		{"nil entry", func(m *BootstrapPeerList) { m.Peers = []*BootstrapPeer{nil} }},
		{"entry tracker_id wrong len", func(m *BootstrapPeerList) {
			m.Peers[0].TrackerId = bytes.Repeat([]byte{1}, 16)
		}},
		{"entry addr empty", func(m *BootstrapPeerList) { m.Peers[0].Addr = "" }},
		{"entry addr too long", func(m *BootstrapPeerList) {
			m.Peers[0].Addr = string(bytes.Repeat([]byte("a"), 257))
		}},
		{"entry addr invalid utf8", func(m *BootstrapPeerList) { m.Peers[0].Addr = "\xff\xfe" }},
		{"entry region_hint too long", func(m *BootstrapPeerList) {
			m.Peers[0].RegionHint = string(bytes.Repeat([]byte("r"), 65))
		}},
		{"entry region_hint invalid utf8", func(m *BootstrapPeerList) { m.Peers[0].RegionHint = "\xff" }},
		{"entry health_score negative", func(m *BootstrapPeerList) { m.Peers[0].HealthScore = -0.1 }},
		{"entry health_score above one", func(m *BootstrapPeerList) { m.Peers[0].HealthScore = 1.1 }},
		{"entry health_score nan", func(m *BootstrapPeerList) { m.Peers[0].HealthScore = math.NaN() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *BootstrapPeerList
			if tc.name != "nil msg" {
				m = validBootstrapPeerList()
				tc.mutate(m)
			}
			require.Error(t, ValidateBootstrapPeerList(m))
		})
	}
}

func TestValidateRPCRequest_AcceptsBootstrapPeers(t *testing.T) {
	r := &RpcRequest{Method: RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS, Payload: []byte{0x00}}
	require.NoError(t, ValidateRPCRequest(r))
}
```

Add `"crypto/ed25519"` and `"math"` to the imports if not already present (search the file's import block; both are stdlib).

- [ ] **Step 2: Run the tests, expect failure.**

Run: `go test ./shared/proto/ -run 'TestValidateBootstrapPeerList|TestValidateRPCRequest_AcceptsBootstrapPeers' -v`
Expected: FAIL — `ValidateBootstrapPeerList` undefined; `ValidateRPCRequest` rejects `RPC_METHOD_BOOTSTRAP_PEERS` (not in the whitelist switch).

---

### Task 3: ValidateBootstrapPeerList + RPC method ceiling (green)

**Files:**
- Modify: `shared/proto/validate.go`

- [ ] **Step 1: Widen the `ValidateRPCRequest` method whitelist.** Edit `shared/proto/validate.go:194-202`:

```go
	switch r.Method {
	case RpcMethod_RPC_METHOD_ENROLL,
		RpcMethod_RPC_METHOD_BROKER_REQUEST,
		RpcMethod_RPC_METHOD_BALANCE,
		RpcMethod_RPC_METHOD_SETTLE,
		RpcMethod_RPC_METHOD_USAGE_REPORT,
		RpcMethod_RPC_METHOD_ADVERTISE,
		RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		RpcMethod_RPC_METHOD_STUN_ALLOCATE,
		RpcMethod_RPC_METHOD_TURN_RELAY_OPEN,
		RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS:
	default:
		return fmt.Errorf("proto: RpcRequest.Method invalid: %v", r.Method)
	}
```

- [ ] **Step 2: Append validator + caps** at the end of `shared/proto/validate.go`. First ensure the file imports `"crypto/ed25519"`, `"math"`, and `"unicode/utf8"` (add to the existing import block; if any are present, don't double-add).

```go
// Bootstrap-peers caps. The validator caps are conservative ceilings;
// the tracker config (FederationBootstrapConfig) imposes operational
// limits below them.
const (
	MaxBootstrapPeers              = 256
	MaxBootstrapPeerAddrLen        = 256
	MaxBootstrapPeerRegionHintLen  = 64
	MaxBootstrapPeerListLifetimeS  = 60 * 60 // 1 hour sanity ceiling
)

// ValidateBootstrapPeerList enforces shape invariants on a signed
// bootstrap snapshot. Signature verification is a separate step
// (VerifyBootstrapPeerListSig); this only checks structural validity.
func ValidateBootstrapPeerList(l *BootstrapPeerList) error {
	if l == nil {
		return errors.New("proto: nil BootstrapPeerList")
	}
	if len(l.IssuerId) != 32 {
		return fmt.Errorf("proto: BootstrapPeerList.issuer_id len %d != 32", len(l.IssuerId))
	}
	if len(l.Sig) != ed25519.SignatureSize {
		return fmt.Errorf("proto: BootstrapPeerList.sig len %d != %d", len(l.Sig), ed25519.SignatureSize)
	}
	if l.SignedAt == 0 {
		return errors.New("proto: BootstrapPeerList.signed_at zero")
	}
	if l.ExpiresAt <= l.SignedAt {
		return fmt.Errorf("proto: BootstrapPeerList.expires_at %d not > signed_at %d", l.ExpiresAt, l.SignedAt)
	}
	if l.ExpiresAt-l.SignedAt > MaxBootstrapPeerListLifetimeS {
		return fmt.Errorf("proto: BootstrapPeerList lifetime %d > %d", l.ExpiresAt-l.SignedAt, MaxBootstrapPeerListLifetimeS)
	}
	if len(l.Peers) > MaxBootstrapPeers {
		return fmt.Errorf("proto: BootstrapPeerList.peers len %d > %d", len(l.Peers), MaxBootstrapPeers)
	}
	for i, p := range l.Peers {
		if p == nil {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d] is nil", i)
		}
		if len(p.TrackerId) != 32 {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].tracker_id len %d != 32", i, len(p.TrackerId))
		}
		if len(p.Addr) == 0 {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].addr empty", i)
		}
		if len(p.Addr) > MaxBootstrapPeerAddrLen {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].addr len %d > %d", i, len(p.Addr), MaxBootstrapPeerAddrLen)
		}
		if !utf8.ValidString(p.Addr) {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].addr invalid utf-8", i)
		}
		if len(p.RegionHint) > MaxBootstrapPeerRegionHintLen {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].region_hint len %d > %d", i, len(p.RegionHint), MaxBootstrapPeerRegionHintLen)
		}
		if !utf8.ValidString(p.RegionHint) {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].region_hint invalid utf-8", i)
		}
		if math.IsNaN(p.HealthScore) || p.HealthScore < 0.0 || p.HealthScore > 1.0 {
			return fmt.Errorf("proto: BootstrapPeerList.peers[%d].health_score %f out of [0,1]", i, p.HealthScore)
		}
	}
	return nil
}
```

- [ ] **Step 3: Run, expect pass.**

Run: `go test ./shared/proto/ -run 'TestValidateBootstrapPeerList|TestValidateRPCRequest_AcceptsBootstrapPeers' -v`
Expected: PASS.

- [ ] **Step 4: Run full proto suite.**

Run: `go test ./shared/proto/ -v`
Expected: PASS — existing tests still pass.

- [ ] **Step 5: Commit.**

```bash
git add shared/proto/validate.go shared/proto/validate_test.go
git commit -m "feat(shared/proto): ValidateBootstrapPeerList + accept BOOTSTRAP_PEERS in RPC validator"
```

---

### Task 4: Canonical/Sign/Verify helpers (red)

**Files:**
- Create: `shared/proto/canonical_test.go`

- [ ] **Step 1: Write the failing tests.** Create `shared/proto/canonical_test.go`:

```go
package proto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

func TestCanonicalBootstrapPeerListPreSig_ExcludesSig(t *testing.T) {
	bl := validBootstrapPeerList()
	a, err := CanonicalBootstrapPeerListPreSig(bl)
	require.NoError(t, err)

	bl.Sig = bytes.Repeat([]byte{0xFF}, ed25519.SignatureSize)
	b, err := CanonicalBootstrapPeerListPreSig(bl)
	require.NoError(t, err)

	require.Equal(t, a, b, "canonical bytes must not depend on Sig")
}

func TestCanonicalBootstrapPeerListPreSig_Stable(t *testing.T) {
	bl1 := validBootstrapPeerList()
	bl2 := validBootstrapPeerList()
	a, err := CanonicalBootstrapPeerListPreSig(bl1)
	require.NoError(t, err)
	b, err := CanonicalBootstrapPeerListPreSig(bl2)
	require.NoError(t, err)
	require.Equal(t, a, b)
}

func TestCanonicalBootstrapPeerListPreSig_NilRejected(t *testing.T) {
	_, err := CanonicalBootstrapPeerListPreSig(nil)
	require.Error(t, err)
}

func TestSignAndVerifyBootstrapPeerList_RoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	require.Len(t, bl.Sig, ed25519.SignatureSize)
	require.NoError(t, VerifyBootstrapPeerListSig(pub, bl))
}

func TestVerifyBootstrapPeerListSig_RejectsTampered(t *testing.T) {
	pub, priv := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	bl.Peers[0].Addr = "tampered.example.org:443"
	err := VerifyBootstrapPeerListSig(pub, bl)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListBadSig))
}

func TestVerifyBootstrapPeerListSig_RejectsWrongPubkey(t *testing.T) {
	_, priv := newKey(t)
	otherPub, _ := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	err := VerifyBootstrapPeerListSig(otherPub, bl)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListBadSig))
}

func TestVerifyBootstrapPeerListSig_RejectsBadPubkey(t *testing.T) {
	_, priv := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	require.Error(t, VerifyBootstrapPeerListSig(ed25519.PublicKey{1, 2, 3}, bl))
}
```

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./shared/proto/ -run 'TestCanonical|TestSignAndVerify|TestVerifyBootstrap' -v`
Expected: FAIL — `CanonicalBootstrapPeerListPreSig`, `SignBootstrapPeerList`, `VerifyBootstrapPeerListSig`, `ErrBootstrapPeerListBadSig` undefined.

---

### Task 5: Canonical/Sign/Verify helpers (green)

**Files:**
- Create: `shared/proto/canonical.go`

- [ ] **Step 1: Implement.** Create `shared/proto/canonical.go`:

```go
// Package proto: canonical-bytes + sign/verify helpers for plugin-tracker
// signed messages. The canonical bytes for any signed message are the
// DeterministicMarshal of the message with the Sig field cleared. The
// signing call sets Sig to ed25519.Sign(priv, canonical); the verifying
// call recomputes canonical and ed25519.Verify's the stored Sig.
//
// This is the same pattern federation uses for transfer proofs and
// revocation messages (see tracker/internal/federation/transfer.go and
// shared/federation/canonical.go).
package proto

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/signing"
)

// ErrBootstrapPeerListBadSig is returned when VerifyBootstrapPeerListSig
// rejects a signature. Callers can errors.Is this sentinel to surface a
// specific "tampered or wrong-key" outcome.
var ErrBootstrapPeerListBadSig = errors.New("proto: BootstrapPeerList signature invalid")

// CanonicalBootstrapPeerListPreSig returns the bytes signed by the issuer.
// It is DeterministicMarshal of the list with Sig cleared, computed on a
// proto.Clone so the caller's value is unchanged.
func CanonicalBootstrapPeerListPreSig(list *BootstrapPeerList) ([]byte, error) {
	if list == nil {
		return nil, errors.New("proto: nil BootstrapPeerList")
	}
	clone := proto.Clone(list).(*BootstrapPeerList)
	clone.Sig = nil
	return signing.DeterministicMarshal(clone)
}

// SignBootstrapPeerList computes the canonical bytes and writes the
// resulting Ed25519 signature into list.Sig. priv must be a 64-byte
// Ed25519 private key.
func SignBootstrapPeerList(priv ed25519.PrivateKey, list *BootstrapPeerList) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("proto: SignBootstrapPeerList: priv len %d != %d", len(priv), ed25519.PrivateKeySize)
	}
	cb, err := CanonicalBootstrapPeerListPreSig(list)
	if err != nil {
		return err
	}
	list.Sig = ed25519.Sign(priv, cb)
	return nil
}

// VerifyBootstrapPeerListSig recomputes the canonical bytes and verifies
// list.Sig against pub. Returns ErrBootstrapPeerListBadSig on signature
// mismatch; other errors indicate malformed inputs.
func VerifyBootstrapPeerListSig(pub ed25519.PublicKey, list *BootstrapPeerList) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("proto: VerifyBootstrapPeerListSig: pub len %d != %d", len(pub), ed25519.PublicKeySize)
	}
	if list == nil {
		return errors.New("proto: nil BootstrapPeerList")
	}
	if len(list.Sig) != ed25519.SignatureSize {
		return fmt.Errorf("proto: BootstrapPeerList.sig len %d != %d", len(list.Sig), ed25519.SignatureSize)
	}
	cb, err := CanonicalBootstrapPeerListPreSig(list)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, cb, list.Sig) {
		return ErrBootstrapPeerListBadSig
	}
	return nil
}
```

- [ ] **Step 2: Run, expect pass.**

Run: `go test ./shared/proto/ -run 'TestCanonical|TestSignAndVerify|TestVerifyBootstrap' -v`
Expected: PASS.

- [ ] **Step 3: Run full proto suite.**

Run: `go test ./shared/proto/ -v`
Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add shared/proto/canonical.go shared/proto/canonical_test.go
git commit -m "feat(shared/proto): BootstrapPeerList canonical bytes + Ed25519 sign/verify"
```

---

### Task 6: PeerPublicKey on transport.Conn (red)

**Files:**
- Modify: `plugin/internal/trackerclient/internal/transport/transport.go`

The plugin trackerclient currently exposes `PeerIdentityID()` (= SHA-256 of SPKI) but not the raw Ed25519 pubkey. Slice 4's verifier needs the raw pubkey. The change is small: widen the `Conn` interface and update both implementations.

- [ ] **Step 1: Write a failing test.** Append to `plugin/internal/trackerclient/internal/transport/loopback/loopback_test.go` (or create it if absent — the loopback package may not have a `_test.go` yet; if so create with package `loopback`):

```go
package loopback

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestConn_PeerPublicKey(t *testing.T) {
	clientID := ids.IdentityID{1}
	serverID := ids.IdentityID{2}
	clientPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	serverPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cli, srv := PairWithKeys(clientID, serverID, clientPub, serverPub)
	require.True(t, ed25519.PublicKey(serverPub).Equal(cli.PeerPublicKey()))
	require.True(t, ed25519.PublicKey(clientPub).Equal(srv.PeerPublicKey()))
}
```

This test references a not-yet-existing `PairWithKeys` constructor that takes the peer pubkeys explicitly; the existing `Pair(clientID, serverID)` keeps working for tests that don't need pubkey verification.

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./plugin/internal/trackerclient/internal/transport/loopback/... -run TestConn_PeerPublicKey -v`
Expected: FAIL — `PairWithKeys` undefined; `(*Conn).PeerPublicKey` undefined.

---

### Task 7: PeerPublicKey on transport.Conn (green)

**Files:**
- Modify: `plugin/internal/trackerclient/internal/transport/transport.go`
- Modify: `plugin/internal/trackerclient/internal/transport/loopback/loopback.go`
- Modify: `plugin/internal/trackerclient/internal/transport/quic/quic.go`

- [ ] **Step 1: Widen the interface.** In `plugin/internal/trackerclient/internal/transport/transport.go`, add a method to `Conn`:

```go
// Conn is a connected, mTLS-authenticated transport session.
type Conn interface {
	OpenStreamSync(ctx context.Context) (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	PeerIdentityID() ids.IdentityID
	PeerPublicKey() ed25519.PublicKey
	Close() error
	Done() <-chan struct{}
}
```

- [ ] **Step 2: Implement on QUIC driver.** In `plugin/internal/trackerclient/internal/transport/quic/quic.go`, extract the Ed25519 pubkey from the validated cert at dial time and store it on `qConn`:

Inside `Driver.Dial`, after the existing block that computes `pid` from the SPKI hash, add:

```go
	peerPub, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		_ = raw.CloseWithError(0, "non-ed25519 peer pubkey")
		return nil, errors.New("quic: peer cert pubkey is not Ed25519")
	}
```

Then update the `qConn` literal:

```go
	return &qConn{raw: raw, peerID: pid, peerPub: peerPub, done: done}, nil
```

Update the `qConn` struct + accessor:

```go
type qConn struct {
	raw     *quicgo.Conn
	peerID  ids.IdentityID
	peerPub ed25519.PublicKey
	done    chan struct{}
}

func (c *qConn) PeerIdentityID() ids.IdentityID { return c.peerID }
func (c *qConn) PeerPublicKey() ed25519.PublicKey { return c.peerPub }
```

The `crypto/ed25519` import is already present.

- [ ] **Step 3: Implement on loopback driver.** In `plugin/internal/trackerclient/internal/transport/loopback/loopback.go`:

a. Add `crypto/ed25519` to imports.

b. Extend `Conn`:

```go
type Conn struct {
	peerID  ids.IdentityID
	peerPub ed25519.PublicKey
	// ... existing fields ...
}
```

c. Add `PairWithKeys` and update `Pair` to delegate (keep existing zero-pubkey behavior for tests that don't care — many existing trackerclient tests construct loopback pairs):

```go
// Pair returns a (clientConn, serverConn) pair without peer pubkeys.
// Used by tests that don't verify Ed25519 signatures over the loopback.
func Pair(clientID, serverID ids.IdentityID) (*Conn, *Conn) {
	return PairWithKeys(clientID, serverID, nil, nil)
}

// PairWithKeys is like Pair but each side observes the other's Ed25519
// pubkey via PeerPublicKey(). Used by tests that exercise signed-message
// verification (slice 4 bootstrap peers).
func PairWithKeys(clientID, serverID ids.IdentityID, clientPub, serverPub ed25519.PublicKey) (*Conn, *Conn) {
	clientToServer := make(chan *streamPair, 16)
	serverToClient := make(chan *streamPair, 16)
	cli := &Conn{
		peerID:  serverID,
		peerPub: serverPub,
		opens:   clientToServer,
		accepts: serverToClient,
		done:    make(chan struct{}),
	}
	srv := &Conn{
		peerID:  clientID,
		peerPub: clientPub,
		opens:   serverToClient,
		accepts: clientToServer,
		done:    make(chan struct{}),
	}
	cli.peer = srv
	srv.peer = cli
	return cli, srv
}
```

d. Add the accessor:

```go
func (c *Conn) PeerPublicKey() ed25519.PublicKey { return c.peerPub }
```

- [ ] **Step 4: Run the new test, expect pass.**

Run: `go test ./plugin/internal/trackerclient/internal/transport/loopback/... -run TestConn_PeerPublicKey -v`
Expected: PASS.

- [ ] **Step 5: Run full plugin transport suite.**

Run: `go test ./plugin/internal/trackerclient/internal/transport/... -v`
Expected: PASS — existing tests still compile and pass (they used `Pair`, which now delegates).

- [ ] **Step 6: Commit.**

```bash
git add plugin/internal/trackerclient/internal/transport/transport.go \
        plugin/internal/trackerclient/internal/transport/quic/quic.go \
        plugin/internal/trackerclient/internal/transport/loopback/loopback.go \
        plugin/internal/trackerclient/internal/transport/loopback/loopback_test.go
git commit -m "feat(plugin/trackerclient/transport): expose PeerPublicKey() Ed25519 from Conn"
```

---

### Task 8: BootstrapPeersService + handler (red)

**Files:**
- Create: `tracker/internal/api/bootstrap_peers_test.go`

- [ ] **Step 1: Write failing tests.** Create `tracker/internal/api/bootstrap_peers_test.go`:

```go
package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

type fakeBootstrapService struct {
	rows     []storage.KnownPeer
	listErr  error
	signErr  error
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	issuerID ids.IdentityID
	maxPeers int
	ttl      time.Duration
}

func newFakeBootstrapService(t *testing.T) *fakeBootstrapService {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var id ids.IdentityID
	copy(id[:], pub) // synthetic — real adapter would hash SPKI
	return &fakeBootstrapService{
		pub: pub, priv: priv, issuerID: id,
		maxPeers: 50, ttl: 10 * time.Minute,
	}
}

func (f *fakeBootstrapService) ListKnownPeers(_ context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.rows
	if !byHealthDesc {
		t := f
		_ = t
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeBootstrapService) IssuerID() ids.IdentityID { return f.issuerID }

func (f *fakeBootstrapService) Sign(canonical []byte) ([]byte, error) {
	if f.signErr != nil {
		return nil, f.signErr
	}
	return ed25519.Sign(f.priv, canonical), nil
}

func (f *fakeBootstrapService) MaxPeers() int      { return f.maxPeers }
func (f *fakeBootstrapService) TTL() time.Duration { return f.ttl }

func newRouterWithBootstrap(t *testing.T, svc BootstrapPeersService) *Router {
	t.Helper()
	r, err := NewRouter(Deps{
		Logger: zerolog.Nop(),
		Now:    func() time.Time { return time.Unix(1714000000, 0) },
		BootstrapPeers: svc,
	})
	require.NoError(t, err)
	return r
}

func TestBootstrapPeers_OK(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.rows = []storage.KnownPeer{
		{TrackerID: bytes.Repeat([]byte{0x10}, 32), Addr: "a.example:443", RegionHint: "r1", HealthScore: 0.9, LastSeen: time.Unix(1713999000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x11}, 32), Addr: "b.example:443", RegionHint: "r2", HealthScore: 0.7, LastSeen: time.Unix(1713998000, 0), Source: "gossip"},
	}
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS, Payload: nil})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)

	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.NoError(t, tbproto.ValidateBootstrapPeerList(&list))
	require.Equal(t, svc.issuerID[:], list.IssuerId)
	require.Equal(t, uint64(1714000000), list.SignedAt)
	require.Equal(t, uint64(1714000600), list.ExpiresAt) // signed_at + 10m
	require.Len(t, list.Peers, 2)
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(svc.pub, &list))
}

func TestBootstrapPeers_FiltersSelf(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.rows = []storage.KnownPeer{
		{TrackerID: svc.issuerID[:], Addr: "self.example:443", RegionHint: "self", HealthScore: 1.0, LastSeen: time.Unix(1713999000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x11}, 32), Addr: "b.example:443", RegionHint: "r2", HealthScore: 0.7, LastSeen: time.Unix(1713998000, 0), Source: "gossip"},
	}
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.Len(t, list.Peers, 1)
	require.Equal(t, "b.example:443", list.Peers[0].Addr)
}

func TestBootstrapPeers_TruncatesToMax(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.maxPeers = 3
	svc.rows = make([]storage.KnownPeer, 10)
	for i := range svc.rows {
		svc.rows[i] = storage.KnownPeer{
			TrackerID: bytes.Repeat([]byte{byte(0x20 + i)}, 32),
			Addr:      "x.example:443", RegionHint: "r",
			HealthScore: float64(10-i) / 10.0,
			LastSeen:    time.Unix(int64(1713999000+i), 0),
			Source:      "gossip",
		}
	}
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.Len(t, list.Peers, 3)
}

func TestBootstrapPeers_Empty(t *testing.T) {
	svc := newFakeBootstrapService(t)
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.Empty(t, list.Peers)
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(svc.pub, &list))
}

func TestBootstrapPeers_StorageError(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.listErr = errors.New("disk on fire")
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_INTERNAL, resp.Status)
	require.NotNil(t, resp.Error)
	require.Equal(t, "BOOTSTRAP_LIST_STORAGE", resp.Error.Code)
}

func TestBootstrapPeers_NilDeps(t *testing.T) {
	r, err := NewRouter(Deps{Logger: zerolog.Nop(), Now: func() time.Time { return time.Unix(1714000000, 0) }})
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_INTERNAL, resp.Status)
	require.NotNil(t, resp.Error)
	require.Equal(t, "NOT_IMPLEMENTED", resp.Error.Code)
}
```

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/api/ -run TestBootstrapPeers -v`
Expected: FAIL — `BootstrapPeersService` undefined; `Deps.BootstrapPeers` undefined; the router doesn't install a `BOOTSTRAP_PEERS` handler.

---

### Task 9: BootstrapPeersService + handler (green)

**Files:**
- Create: `tracker/internal/api/bootstrap_peers.go`
- Modify: `tracker/internal/api/router.go`

- [ ] **Step 1: Implement the handler.** Create `tracker/internal/api/bootstrap_peers.go`:

```go
package api

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// BootstrapPeersService is the slice of dependencies the BOOTSTRAP_PEERS
// handler needs. The composition root in cmd/token-bay-tracker wires a
// small adapter that delegates ListKnownPeers to *storage.Store and
// IssuerID/Sign to closures over the tracker's identity Ed25519 key.
//
// MaxPeers is the per-call clamp; the handler over-fetches by 1 so the
// self-row drop still leaves MaxPeers entries. TTL is the snapshot
// lifetime.
type BootstrapPeersService interface {
	ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
	IssuerID() ids.IdentityID
	Sign(canonical []byte) ([]byte, error)
	MaxPeers() int
	TTL() time.Duration
}

func (r *Router) installBootstrapPeers() handlerFunc {
	if r.deps.BootstrapPeers == nil {
		return notImpl("bootstrap_peers")
	}
	svc := r.deps.BootstrapPeers
	return func(ctx context.Context, rc *RequestCtx, _ []byte) (*tbproto.RpcResponse, error) {
		// Request payload is BootstrapPeersRequest{} (empty in v1) — we
		// do not unmarshal it; future params would be parsed here.

		max := svc.MaxPeers()
		if max <= 0 {
			max = 50 // defensive fallback, matches default
		}
		rows, err := svc.ListKnownPeers(ctx, max+1, true)
		if err != nil {
			return &tbproto.RpcResponse{
				Status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
				Error:  &tbproto.RpcError{Code: "BOOTSTRAP_LIST_STORAGE", Message: err.Error()},
			}, nil
		}

		issuer := svc.IssuerID()
		out := make([]*tbproto.BootstrapPeer, 0, len(rows))
		for _, row := range rows {
			if bytes.Equal(row.TrackerID, issuer[:]) {
				continue
			}
			out = append(out, &tbproto.BootstrapPeer{
				TrackerId:   row.TrackerID,
				Addr:        row.Addr,
				RegionHint:  row.RegionHint,
				HealthScore: row.HealthScore,
				LastSeen:    uint64(row.LastSeen.Unix()), //nolint:gosec // unix seconds fit u64
			})
			if len(out) >= max {
				break
			}
		}

		now := rc.Now
		if now.IsZero() {
			now = time.Now()
		}
		ttl := svc.TTL()
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}

		list := &tbproto.BootstrapPeerList{
			IssuerId:  issuer[:],
			SignedAt:  uint64(now.Unix()), //nolint:gosec
			ExpiresAt: uint64(now.Add(ttl).Unix()), //nolint:gosec
			Peers:     out,
		}
		cb, err := tbproto.CanonicalBootstrapPeerListPreSig(list)
		if err != nil {
			return nil, fmt.Errorf("bootstrap_peers: canonical: %w", err)
		}
		sig, err := svc.Sign(cb)
		if err != nil {
			return &tbproto.RpcResponse{
				Status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
				Error:  &tbproto.RpcError{Code: "BOOTSTRAP_LIST_SIGN", Message: err.Error()},
			}, nil
		}
		list.Sig = sig
		payload, err := proto.Marshal(list)
		if err != nil {
			return nil, fmt.Errorf("bootstrap_peers: marshal: %w", err)
		}
		return OkResponse(payload), nil
	}
}
```

- [ ] **Step 2: Wire the handler into the router.** In `tracker/internal/api/router.go`:

a. Add to the `Deps` struct (after `Reputation ReputationRecorder`):

```go
	BootstrapPeers BootstrapPeersService
```

b. Add to `NewRouter`'s table-build (after the `TURN_RELAY_OPEN` registration):

```go
	r.handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = r.installBootstrapPeers()
```

- [ ] **Step 3: Run the new tests, expect pass.**

Run: `go test ./tracker/internal/api/ -run TestBootstrapPeers -v`
Expected: PASS for all seven cases (`OK`, `FiltersSelf`, `TruncatesToMax`, `Empty`, `StorageError`, `NilDeps`, plus any sub-tests).

- [ ] **Step 4: Run full api suite.**

Run: `go test ./tracker/internal/api/ -v`
Expected: PASS — existing tests still compile and pass.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/api/bootstrap_peers.go \
        tracker/internal/api/bootstrap_peers_test.go \
        tracker/internal/api/router.go
git commit -m "feat(tracker/api): bootstrap_peers handler + Deps.BootstrapPeers slot"
```

---

### Task 10: Federation bootstrap config sub-section

**Files:**
- Modify: `tracker/internal/config/config.go`
- Modify: `tracker/internal/config/apply_defaults.go`
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/testdata/full.yaml`
- Modify: `tracker/internal/config/validate_test.go`

- [ ] **Step 1: Add the sub-section type.** In `tracker/internal/config/config.go`, append the new type after `FederationConfig`:

```go
// FederationBootstrapConfig governs the plugin-facing signed
// bootstrap-list endpoint (RPC_METHOD_BOOTSTRAP_PEERS).
type FederationBootstrapConfig struct {
	MaxPeers   int `yaml:"max_peers"`   // [1, 256]; default 50
	TTLSeconds int `yaml:"ttl_seconds"` // [60, 3600]; default 600 (10 minutes)
}
```

Then add a field to `FederationConfig`:

```go
	// Bootstrap is the plugin-facing signed peer list (federation §7.2).
	Bootstrap FederationBootstrapConfig `yaml:"bootstrap"`
```

- [ ] **Step 2: Defaults.** In `tracker/internal/config/apply_defaults.go`, find the federation defaulting block and append:

```go
	if c.Federation.Bootstrap.MaxPeers == 0 {
		c.Federation.Bootstrap.MaxPeers = 50
	}
	if c.Federation.Bootstrap.TTLSeconds == 0 {
		c.Federation.Bootstrap.TTLSeconds = 600
	}
```

- [ ] **Step 3: Validate.** In `tracker/internal/config/validate.go`, find the federation-validation block (search for `checkFederation` or `Federation`) and append range checks. If the file uses a `checkFederation` helper, add to its body; otherwise append to whichever switch dispatches federation validation:

```go
	if c.Federation.Bootstrap.MaxPeers < 1 || c.Federation.Bootstrap.MaxPeers > 256 {
		add(FieldError{
			Field:   "federation.bootstrap.max_peers",
			Got:     c.Federation.Bootstrap.MaxPeers,
			Want:    "in [1, 256]",
		})
	}
	if c.Federation.Bootstrap.TTLSeconds < 60 || c.Federation.Bootstrap.TTLSeconds > 3600 {
		add(FieldError{
			Field:   "federation.bootstrap.ttl_seconds",
			Got:     c.Federation.Bootstrap.TTLSeconds,
			Want:    "in [60, 3600]",
		})
	}
```

(Substitute the file's existing `add` / `errs.append(...)` style if it differs — match the surrounding code, do not introduce a new pattern.)

- [ ] **Step 4: Update full.yaml fixture.** In `tracker/internal/config/testdata/full.yaml`, find the `federation:` block and add a `bootstrap:` sub-block with explicit values:

```yaml
  bootstrap:
    max_peers: 50
    ttl_seconds: 600
```

- [ ] **Step 5: Add validation tests.** In `tracker/internal/config/validate_test.go`, append:

```go
func TestValidate_FederationBootstrap_OutOfRange(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		field   string
	}{
		{"max_peers zero", func(c *Config) { c.Federation.Bootstrap.MaxPeers = 0 }, "federation.bootstrap.max_peers"},
		{"max_peers too high", func(c *Config) { c.Federation.Bootstrap.MaxPeers = 257 }, "federation.bootstrap.max_peers"},
		{"ttl_seconds too low", func(c *Config) { c.Federation.Bootstrap.TTLSeconds = 30 }, "federation.bootstrap.ttl_seconds"},
		{"ttl_seconds too high", func(c *Config) { c.Federation.Bootstrap.TTLSeconds = 3601 }, "federation.bootstrap.ttl_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := Validate(c)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.field)
		})
	}
}
```

(`validConfig()` is the existing test helper — search the file to confirm; if absent, fall back to `Load` on `testdata/full.yaml` and mutate.)

- [ ] **Step 6: Run tests, expect pass.**

Run: `go test ./tracker/internal/config/ -v`
Expected: PASS — full.yaml round-trips, defaults fill, validation rejects out-of-range.

- [ ] **Step 7: Commit.**

```bash
git add tracker/internal/config/config.go \
        tracker/internal/config/apply_defaults.go \
        tracker/internal/config/validate.go \
        tracker/internal/config/validate_test.go \
        tracker/internal/config/testdata/full.yaml
git commit -m "feat(tracker/config): federation.bootstrap (max_peers, ttl_seconds)"
```

---

### Task 11: Wire BootstrapPeersService adapter in run_cmd

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/federation_adapters.go`
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go`

- [ ] **Step 1: Add the adapter type.** In `tracker/cmd/token-bay-tracker/federation_adapters.go`, append:

```go
// bootstrapPeersAdapter implements api.BootstrapPeersService against the
// SQLite store + the tracker's identity Ed25519 key. Construction is
// trivial: the composition root passes the *storage.Store, the tracker
// pubkey hash (= IssuerID), the priv key, and the cfg-derived MaxPeers /
// TTL. The adapter holds value types only and is safe to share.
type bootstrapPeersAdapter struct {
	store    *storage.Store
	issuer   ids.IdentityID
	priv     ed25519.PrivateKey
	maxPeers int
	ttl      time.Duration
}

func (a bootstrapPeersAdapter) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error) {
	return a.store.ListKnownPeers(ctx, limit, byHealthDesc)
}

func (a bootstrapPeersAdapter) IssuerID() ids.IdentityID { return a.issuer }

func (a bootstrapPeersAdapter) Sign(canonical []byte) ([]byte, error) {
	return ed25519.Sign(a.priv, canonical), nil
}

func (a bootstrapPeersAdapter) MaxPeers() int      { return a.maxPeers }
func (a bootstrapPeersAdapter) TTL() time.Duration { return a.ttl }
```

Add `"time"` to the imports if not already present.

Append to the `var (...)` interface-assertion block at the end:

```go
	_ api.BootstrapPeersService = bootstrapPeersAdapter{}
```

Add `"github.com/token-bay/token-bay/tracker/internal/api"` to the imports.

- [ ] **Step 2: Wire it in run_cmd.** In `tracker/cmd/token-bay-tracker/run_cmd.go`, find the `api.NewRouter(api.Deps{...})` call and add a new field to the literal:

```go
				router, err := api.NewRouter(api.Deps{
					Logger:     logger,
					Now:        time.Now,
					Ledger:     led,
					Registry:   reg,
					StunTurn:   stunTurnAdapter{alloc: alloc, reflect: reflectFn},
					Broker:     brokerSubs.Broker,
					Settlement: brokerSubs.Settlement,
					Admission:  admissionAdapter{adm},
					Reputation: rep,
					BootstrapPeers: bootstrapPeersAdapter{
						store:    store,
						issuer:   ids.IdentityID(sha256.Sum256(trackerPub)),
						priv:     trackerKey,
						maxPeers: cfg.Federation.Bootstrap.MaxPeers,
						ttl:      time.Duration(cfg.Federation.Bootstrap.TTLSeconds) * time.Second,
					},
				})
```

(`trackerPub`, `trackerKey`, `cfg`, `store` are all already in scope — search the surrounding code to confirm.)

- [ ] **Step 3: Build and run.**

Run: `go build ./tracker/...`
Expected: success.

Run: `go test ./tracker/...`
Expected: PASS — no regressions; the api/router now ships a real `BootstrapPeers` handler when the binary runs, but no test changed its expectations.

- [ ] **Step 4: Commit.**

```bash
git add tracker/cmd/token-bay-tracker/federation_adapters.go \
        tracker/cmd/token-bay-tracker/run_cmd.go
git commit -m "feat(tracker/cmd): wire bootstrapPeersAdapter into api.Deps"
```

---

### Task 12: Tracker bootstrap-peers metrics (red)

**Files:**
- Modify: `tracker/internal/api/bootstrap_peers_test.go`

This task only adds counter assertions to existing tests; the metric registration happens in green.

- [ ] **Step 1: Add a metrics-observability stanza.** Either: (a) extend the existing `fakeBootstrapService` to count served / errors, or (b) check `Deps.Metrics` if the api package already has a metrics field. Check the api package for an existing metrics injection pattern:

Run: `grep -n "Metrics\|prometheus" tracker/internal/api/*.go | head -20`

If no metrics injection exists in `api.Deps`, add one in this task:

a. In `tracker/internal/api/router.go`, add to `Deps`:

```go
	BootstrapMetrics BootstrapPeersMetrics // optional; nil → no observability
```

b. In `tracker/internal/api/bootstrap_peers.go`, add the interface (small, stateless):

```go
// BootstrapPeersMetrics is the optional observability hook for the
// bootstrap_peers handler. Nil-safe: handler skips metrics if absent.
type BootstrapPeersMetrics interface {
	IncBootstrapServed()
	IncBootstrapErrors(code string)
	SetBootstrapListSize(n int)
}
```

c. Append a failing test to `bootstrap_peers_test.go`:

```go
type fakeBootstrapMetrics struct {
	served int
	errors map[string]int
	size   int
}

func newFakeBootstrapMetrics() *fakeBootstrapMetrics {
	return &fakeBootstrapMetrics{errors: map[string]int{}}
}
func (m *fakeBootstrapMetrics) IncBootstrapServed()           { m.served++ }
func (m *fakeBootstrapMetrics) IncBootstrapErrors(code string) { m.errors[code]++ }
func (m *fakeBootstrapMetrics) SetBootstrapListSize(n int)    { m.size = n }

func TestBootstrapPeers_MetricsOnSuccess(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.rows = []storage.KnownPeer{
		{TrackerID: bytes.Repeat([]byte{0x10}, 32), Addr: "a:1", RegionHint: "r", HealthScore: 0.5, LastSeen: time.Unix(1, 0), Source: "gossip"},
	}
	met := newFakeBootstrapMetrics()
	r, err := NewRouter(Deps{
		Logger:           zerolog.Nop(),
		Now:              func() time.Time { return time.Unix(1714000000, 0) },
		BootstrapPeers:   svc,
		BootstrapMetrics: met,
	})
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	require.Equal(t, 1, met.served)
	require.Equal(t, 1, met.size)
	require.Empty(t, met.errors)
}

func TestBootstrapPeers_MetricsOnStorageError(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.listErr = errors.New("disk on fire")
	met := newFakeBootstrapMetrics()
	r, err := NewRouter(Deps{
		Logger:           zerolog.Nop(),
		Now:              func() time.Time { return time.Unix(1714000000, 0) },
		BootstrapPeers:   svc,
		BootstrapMetrics: met,
	})
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_INTERNAL, resp.Status)
	require.Equal(t, 0, met.served)
	require.Equal(t, 1, met.errors["BOOTSTRAP_LIST_STORAGE"])
}
```

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/api/ -run TestBootstrapPeers_Metrics -v`
Expected: FAIL — handler doesn't call any metrics methods.

---

### Task 13: Tracker bootstrap-peers metrics (green)

**Files:**
- Modify: `tracker/internal/api/bootstrap_peers.go`

- [ ] **Step 1: Wire metrics into the handler.** In `installBootstrapPeers`, after `svc := r.deps.BootstrapPeers`, capture metrics:

```go
	met := r.deps.BootstrapMetrics
```

In the storage-error branch, before returning:

```go
		if met != nil {
			met.IncBootstrapErrors("BOOTSTRAP_LIST_STORAGE")
		}
```

In the sign-error branch, before returning:

```go
		if met != nil {
			met.IncBootstrapErrors("BOOTSTRAP_LIST_SIGN")
		}
```

Just before `return OkResponse(payload), nil`:

```go
	if met != nil {
		met.IncBootstrapServed()
		met.SetBootstrapListSize(len(out))
	}
```

- [ ] **Step 2: Run, expect pass.**

Run: `go test ./tracker/internal/api/ -run TestBootstrapPeers -v`
Expected: PASS for all bootstrap-peers tests including the two metrics ones.

- [ ] **Step 3: Wire a real Prometheus implementation.** A real `BootstrapPeersMetrics` is wired in run_cmd. If the tracker has a central metrics package (search `tracker/internal/metrics/`), add to it; otherwise add a small adapter in run_cmd. Either approach:

Option A — central (preferred if metrics package exists):

```go
// tracker/internal/metrics/bootstrap_peers.go (or wherever counters live)
type BootstrapPeersMetrics struct {
	served *prometheus.CounterVec  // labels: none — single counter
	served0 prometheus.Counter
	errs   *prometheus.CounterVec // labels: code
	size   prometheus.Gauge
}

func NewBootstrapPeersMetrics(reg prometheus.Registerer) *BootstrapPeersMetrics {
	m := &BootstrapPeersMetrics{
		served0: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tokenbay_tracker_bootstrap_peers_served_total",
		}),
		errs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tokenbay_tracker_bootstrap_peers_errors_total",
		}, []string{"code"}),
		size: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tokenbay_tracker_bootstrap_peers_list_size",
		}),
	}
	reg.MustRegister(m.served0, m.errs, m.size)
	return m
}

func (m *BootstrapPeersMetrics) IncBootstrapServed()              { m.served0.Inc() }
func (m *BootstrapPeersMetrics) IncBootstrapErrors(code string)   { m.errs.WithLabelValues(code).Inc() }
func (m *BootstrapPeersMetrics) SetBootstrapListSize(n int)       { m.size.Set(float64(n)) }
```

Option B — inline in run_cmd: build the same struct anonymously and pass to api.Deps. Whichever pattern matches existing tracker metrics wiring; do not invent a new one.

In run_cmd.go, pass it to `api.Deps`:

```go
					BootstrapMetrics: tbmetrics.NewBootstrapPeersMetrics(prometheus.DefaultRegisterer),
```

(or the equivalent for Option B).

- [ ] **Step 4: Run all tracker tests.**

Run: `go test ./tracker/...`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/api/bootstrap_peers.go \
        tracker/internal/api/bootstrap_peers_test.go \
        tracker/internal/api/router.go \
        tracker/cmd/token-bay-tracker/run_cmd.go
# Plus tracker/internal/metrics/bootstrap_peers.go if Option A was used.
git commit -m "feat(tracker/api): bootstrap_peers Prometheus counters + gauge"
```

---

### Task 14: Plugin BootstrapPeer type + sentinel errors (red)

**Files:**
- Create: `plugin/internal/trackerclient/bootstrap_peers_test.go`

- [ ] **Step 1: Write failing tests.** Create `plugin/internal/trackerclient/bootstrap_peers_test.go`:

```go
package trackerclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// signedBootstrapList builds a list signed by priv with the given
// signed_at / expires_at, suitable for a fake tracker to return.
func signedBootstrapList(t *testing.T, priv ed25519.PrivateKey, issuerID ids.IdentityID, signedAt, expiresAt time.Time, peers []*tbproto.BootstrapPeer) []byte {
	t.Helper()
	list := &tbproto.BootstrapPeerList{
		IssuerId:  issuerID[:],
		SignedAt:  uint64(signedAt.Unix()),
		ExpiresAt: uint64(expiresAt.Unix()),
		Peers:     peers,
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	out, err := proto.Marshal(list)
	require.NoError(t, err)
	return out
}

// runFakeServer accepts one stream, reads the request, returns respBytes
// in an OK RpcResponse. Closes the stream and conn when done.
func runFakeServer(t *testing.T, srv *loopback.Conn, respPayload []byte) {
	t.Helper()
	go func() {
		stream, err := srv.AcceptStream(context.Background())
		if err != nil {
			return
		}
		defer stream.Close() //nolint:errcheck
		var req tbproto.RpcRequest
		if err := wire.Read(stream, &req, 1<<20); err != nil {
			return
		}
		resp := &tbproto.RpcResponse{Status: tbproto.RpcStatus_RPC_STATUS_OK, Payload: respPayload}
		_ = wire.Write(stream, resp, 1<<20)
		_ = stream.CloseWrite()
	}()
}

func newClientWithLoopback(t *testing.T, peerPub ed25519.PublicKey, peerIdentity ids.IdentityID) (*Client, *loopback.Conn) {
	t.Helper()
	cli, srv := loopback.PairWithKeys(ids.IdentityID{1}, peerIdentity, nil, peerPub)
	cfg := Config{
		Endpoints:    []TrackerEndpoint{{Addr: "loopback:1", IdentityHash: peerIdentity, Region: "r"}},
		MaxFrameSize: 1 << 20,
		// minimal — fill remaining required fields from your existing tests
	}
	c, err := New(cfg.withDefaults())
	require.NoError(t, err)
	// inject the loopback conn directly into the holder (matches existing
	// trackerclient test helpers — see trackerclient_test.go for the
	// pattern; copy-adapt the helper that sets c.holder.set(...))
	c.holder.set(cli)
	return c, srv
}

func TestFetchBootstrapPeers_OK(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var issuerID ids.IdentityID
	copy(issuerID[:], sha256.New().Sum(pub)[:32]) // matches loopback.PeerIdentityID stub
	// In production, IdentityHash is sha256(SPKI). For this loopback test,
	// we set the conn.peerID directly to issuerID via PairWithKeys above.
	now := time.Now()
	peerKey := bytes.Repeat([]byte{0x10}, 32)
	respBytes := signedBootstrapList(t, priv, issuerID, now, now.Add(10*time.Minute), []*tbproto.BootstrapPeer{
		{TrackerId: peerKey, Addr: "a.example:443", RegionHint: "r1", HealthScore: 0.9, LastSeen: uint64(now.Add(-time.Hour).Unix())},
	})

	c, srv := newClientWithLoopback(t, pub, issuerID)
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, respBytes)

	got, err := c.FetchBootstrapPeers(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "a.example:443", got[0].Addr)
}

func TestFetchBootstrapPeers_BadSig(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var issuerID ids.IdentityID
	copy(issuerID[:], pub) // synthetic for test
	now := time.Now()

	list := &tbproto.BootstrapPeerList{
		IssuerId:  issuerID[:],
		SignedAt:  uint64(now.Unix()),
		ExpiresAt: uint64(now.Add(10 * time.Minute).Unix()),
		Peers: []*tbproto.BootstrapPeer{{
			TrackerId: bytes.Repeat([]byte{0x10}, 32), Addr: "a:1", RegionHint: "r", HealthScore: 0.5,
		}},
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	list.Peers[0].Addr = "tampered:1" // mutate AFTER signing
	respBytes, err := proto.Marshal(list)
	require.NoError(t, err)

	c, srv := newClientWithLoopback(t, pub, issuerID)
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, respBytes)

	_, err = c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListBadSig))
	_ = netip.AddrPort{} // keep import live in case future tests use it
}

func TestFetchBootstrapPeers_Expired(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var issuerID ids.IdentityID
	copy(issuerID[:], pub)
	expired := time.Now().Add(-2 * time.Minute) // beyond skew tolerance
	respBytes := signedBootstrapList(t, priv, issuerID, expired.Add(-10*time.Minute), expired, nil)

	c, srv := newClientWithLoopback(t, pub, issuerID)
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, respBytes)

	_, err = c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListExpired))
}

func TestFetchBootstrapPeers_SkewTolerance(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var issuerID ids.IdentityID
	copy(issuerID[:], pub)
	// expired 30 s ago — within 60 s tolerance
	expires := time.Now().Add(-30 * time.Second)
	respBytes := signedBootstrapList(t, priv, issuerID, expires.Add(-10*time.Minute), expires, nil)

	c, srv := newClientWithLoopback(t, pub, issuerID)
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, respBytes)

	_, err = c.FetchBootstrapPeers(context.Background())
	require.NoError(t, err)
}

func TestFetchBootstrapPeers_IdentityMismatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherIssuer := ids.IdentityID{0x99}
	now := time.Now()
	respBytes := signedBootstrapList(t, priv, otherIssuer, now, now.Add(10*time.Minute), nil)

	var connectedID ids.IdentityID
	copy(connectedID[:], pub) // conn says it connected to "pub"
	c, srv := newClientWithLoopback(t, pub, connectedID)
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, respBytes)

	_, err = c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapIssuerMismatch))
}

func TestFetchBootstrapPeers_InvalidPayload(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var issuerID ids.IdentityID
	copy(issuerID[:], pub)
	c, srv := newClientWithLoopback(t, pub, issuerID)
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, []byte{0xff, 0xff, 0xff})

	_, err = c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidResponse))
	_ = transport.Endpoint{}
}
```

Note: the loopback wiring in `newClientWithLoopback` references a hypothetical `c.holder.set(cli)` test seam — search `plugin/internal/trackerclient/trackerclient_test.go` for the existing helper that injects a connection (it likely has a different name like `setConn` or unexported). Replace `c.holder.set(cli)` with the actual call. If the existing tests use a different shape (e.g., a fake transport at `Config.Transport` plus a `Start`), adopt that instead of a holder injection — match the file's existing pattern.

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./plugin/internal/trackerclient/ -run TestFetchBootstrapPeers -v`
Expected: FAIL — `FetchBootstrapPeers`, `ErrBootstrapPeerListBadSig`, `ErrBootstrapPeerListExpired`, `ErrBootstrapIssuerMismatch` undefined.

---

### Task 15: Plugin FetchBootstrapPeers (green)

**Files:**
- Modify: `plugin/internal/trackerclient/types.go`
- Modify: `plugin/internal/trackerclient/errors.go`
- Create: `plugin/internal/trackerclient/bootstrap_peers.go`

- [ ] **Step 1: Add the plugin-side type + errors.** In `plugin/internal/trackerclient/types.go`, append:

```go
// BootstrapPeer is a plugin-friendly view of a single tracker that came
// out of a verified BootstrapPeerList from FetchBootstrapPeers.
type BootstrapPeer struct {
	TrackerID   ids.IdentityID
	Addr        string
	RegionHint  string
	HealthScore float64
	LastSeen    time.Time
}
```

(`time` and `ids` are likely already imported; add if not.)

In `plugin/internal/trackerclient/errors.go`, append:

```go
import (
	// ... existing imports ...
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// ErrBootstrapPeerListBadSig is the plugin-side alias for the shared
// signature-mismatch error. Re-exported for caller-side errors.Is checks
// without forcing the caller to import shared/proto.
var ErrBootstrapPeerListBadSig = tbproto.ErrBootstrapPeerListBadSig

// ErrBootstrapPeerListExpired is returned when the snapshot's expires_at
// is in the past beyond the 60-second skew tolerance.
var ErrBootstrapPeerListExpired = errors.New("trackerclient: bootstrap peer list expired")

// ErrBootstrapIssuerMismatch is returned when the snapshot's issuer_id
// does not match the connected tracker's identity.
var ErrBootstrapIssuerMismatch = errors.New("trackerclient: bootstrap issuer does not match connected tracker")
```

- [ ] **Step 2: Implement the method.** Create `plugin/internal/trackerclient/bootstrap_peers.go`:

```go
package trackerclient

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// bootstrapPeerListSkewToleranceS is the plugin-side clock-skew window
// applied when checking expires_at. A snapshot whose expires_at is up
// to 60 s in the past is still accepted.
const bootstrapPeerListSkewToleranceS = 60

// FetchBootstrapPeers calls RPC_METHOD_BOOTSTRAP_PEERS on the connected
// tracker, verifies the signature against the connection's peer
// pubkey, checks expires_at, and returns the parsed list. It does not
// persist the result — caller decides what to do with it.
func (c *Client) FetchBootstrapPeers(ctx context.Context) ([]BootstrapPeer, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	pub := conn.PeerPublicKey()
	connID := conn.PeerIdentityID()

	var resp tbproto.BootstrapPeerList
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS,
		&tbproto.BootstrapPeersRequest{}, &resp); err != nil {
		c.observeBootstrapOutcome("rpc_error")
		return nil, err
	}
	if err := tbproto.ValidateBootstrapPeerList(&resp); err != nil {
		c.observeBootstrapOutcome("invalid")
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if !bytes.Equal(resp.IssuerId, connID[:]) {
		c.observeBootstrapOutcome("issuer_mismatch")
		return nil, ErrBootstrapIssuerMismatch
	}
	if err := tbproto.VerifyBootstrapPeerListSig(pub, &resp); err != nil {
		c.observeBootstrapOutcome("sig_invalid")
		return nil, err
	}
	now := time.Now().Unix()
	if uint64(now) > resp.ExpiresAt+bootstrapPeerListSkewToleranceS { //nolint:gosec
		c.observeBootstrapOutcome("expired")
		return nil, ErrBootstrapPeerListExpired
	}

	out := make([]BootstrapPeer, 0, len(resp.Peers))
	for _, p := range resp.Peers {
		var tid ids.IdentityID
		copy(tid[:], p.TrackerId)
		out = append(out, BootstrapPeer{
			TrackerID:   tid,
			Addr:        p.Addr,
			RegionHint:  p.RegionHint,
			HealthScore: p.HealthScore,
			LastSeen:    time.Unix(int64(p.LastSeen), 0), //nolint:gosec
		})
	}
	if len(out) == 0 {
		c.observeBootstrapOutcome("empty")
	} else {
		c.observeBootstrapOutcome("ok")
	}
	return out, nil
}

// observeBootstrapOutcome is the no-op default when no metrics sink is
// configured. Wired up in Task 16.
func (c *Client) observeBootstrapOutcome(outcome string) {
	if c.cfg.Metrics != nil {
		c.cfg.Metrics.IncBootstrapPeersFetched(outcome)
	}
}
```

(If `Config.Metrics` doesn't exist yet, this snippet introduces it as part of slice 4 — see Task 16. Until then, the helper compiles by guarding on `c.cfg.Metrics != nil`; the field default is nil, so existing tests are unaffected.)

- [ ] **Step 3: Run new tests, expect pass.**

Run: `go test ./plugin/internal/trackerclient/ -run TestFetchBootstrapPeers -v`
Expected: PASS for OK / BadSig / Expired / SkewTolerance / IdentityMismatch / InvalidPayload.

- [ ] **Step 4: Run full trackerclient suite.**

Run: `go test ./plugin/internal/trackerclient/...`
Expected: PASS — existing tests still pass.

- [ ] **Step 5: Commit.**

```bash
git add plugin/internal/trackerclient/bootstrap_peers.go \
        plugin/internal/trackerclient/bootstrap_peers_test.go \
        plugin/internal/trackerclient/types.go \
        plugin/internal/trackerclient/errors.go
git commit -m "feat(plugin/trackerclient): FetchBootstrapPeers verify+parse signed list"
```

---

### Task 16: Plugin bootstrap-peers metrics

**Files:**
- Modify: `plugin/internal/trackerclient/config.go` (or wherever `Config` lives)
- Modify: `plugin/internal/trackerclient/bootstrap_peers_test.go`

- [ ] **Step 1: Add the metrics interface.** In `plugin/internal/trackerclient/config.go`, find the `Config` struct and append a new field. If a `Metrics` field already exists with its own interface, extend that interface; otherwise:

```go
// BootstrapMetrics is the optional observability hook for bootstrap-list
// fetches. Nil-safe — Client skips metrics if absent.
type BootstrapMetrics interface {
	IncBootstrapPeersFetched(outcome string) // outcome ∈ {ok, sig_invalid, expired, invalid, empty, issuer_mismatch, rpc_error}
}
```

Add to `Config`:

```go
	// Metrics is an optional observability hook. Currently only used by
	// FetchBootstrapPeers; future RPCs may attach their own counters.
	Metrics BootstrapMetrics
```

(If a richer metrics interface already exists in trackerclient, fold `IncBootstrapPeersFetched` into it instead — match the file's pattern.)

- [ ] **Step 2: Add a metrics-asserting test.** Append to `bootstrap_peers_test.go`:

```go
type fakeBootstrapMetrics struct{ counts map[string]int }

func newFakeBootstrapMetrics() *fakeBootstrapMetrics { return &fakeBootstrapMetrics{counts: map[string]int{}} }
func (m *fakeBootstrapMetrics) IncBootstrapPeersFetched(outcome string) { m.counts[outcome]++ }

func TestFetchBootstrapPeers_MetricsOnSuccess(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var issuerID ids.IdentityID
	copy(issuerID[:], pub)
	now := time.Now()
	respBytes := signedBootstrapList(t, priv, issuerID, now, now.Add(10*time.Minute), nil)

	met := newFakeBootstrapMetrics()
	c, srv := newClientWithLoopback(t, pub, issuerID)
	c.cfg.Metrics = met
	defer c.Close() //nolint:errcheck
	runFakeServer(t, srv, respBytes)

	_, err = c.FetchBootstrapPeers(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, met.counts["empty"]) // empty peers -> "empty"
}
```

- [ ] **Step 3: Run, expect pass.**

Run: `go test ./plugin/internal/trackerclient/ -run TestFetchBootstrapPeers -v`
Expected: PASS — including the new metrics assertion.

- [ ] **Step 4: Wire a real Prometheus implementation in plugin/cmd/.** Search for the existing plugin metrics wiring (`grep -rn "prometheus.NewCounter\|MustRegister" plugin/cmd plugin/internal/sidecar | head`); add `tokenbay_plugin_bootstrap_peers_fetched_total{outcome}` next to it. If no central plugin metrics file exists, defer registration to `plugin/internal/sidecar` and pass the impl into `trackerclient.Config.Metrics` from the sidecar wiring.

- [ ] **Step 5: Commit.**

```bash
git add plugin/internal/trackerclient/config.go \
        plugin/internal/trackerclient/bootstrap_peers_test.go \
        plugin/internal/trackerclient/bootstrap_peers.go
# Plus the sidecar wiring file from Step 4.
git commit -m "feat(plugin/trackerclient): bootstrap_peers_fetched_total{outcome} counter"
```

---

### Task 17: Tracker integration test (real storage + real signer)

**Files:**
- Create: `tracker/internal/api/bootstrap_peers_integration_test.go` *(or extend the existing api integration test file if there is one — search `tracker/internal/api/*integration*` first)*

- [ ] **Step 1: Write the integration test.** This exercises the api handler against a real `*storage.Store` seeded via `UpsertKnownPeer`, with a real Ed25519 keypair driving both the signer and the verifier. It catches integration bugs at the api/storage/signing boundary that the `fakeBootstrapService` can't.

```go
package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

type realSigner struct {
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	issuer   ids.IdentityID
	store    *storage.Store
	maxPeers int
	ttl      time.Duration
}

func (a realSigner) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error) {
	return a.store.ListKnownPeers(ctx, limit, byHealthDesc)
}
func (a realSigner) IssuerID() ids.IdentityID { return a.issuer }
func (a realSigner) Sign(canonical []byte) ([]byte, error) { return ed25519.Sign(a.priv, canonical), nil }
func (a realSigner) MaxPeers() int             { return a.maxPeers }
func (a realSigner) TTL() time.Duration        { return a.ttl }

func TestBootstrapPeers_Integration_RealStorage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tracker.db")
	store, err := storage.Open(context.Background(), dbPath)
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuer := ids.IdentityID(sha256.Sum256(pub))

	// Seed: 5 allowlist + 3 gossip rows. health_score values pick the
	// desired sort order.
	rows := []storage.KnownPeer{
		{TrackerID: bytes.Repeat([]byte{0x10}, 32), Addr: "a:443", RegionHint: "r1", HealthScore: 1.0, LastSeen: time.Unix(1714000000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x11}, 32), Addr: "b:443", RegionHint: "r2", HealthScore: 0.9, LastSeen: time.Unix(1714000000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x12}, 32), Addr: "c:443", RegionHint: "r3", HealthScore: 0.5, LastSeen: time.Unix(1714000000, 0), Source: "gossip"},
		{TrackerID: bytes.Repeat([]byte{0x13}, 32), Addr: "d:443", RegionHint: "r4", HealthScore: 0.3, LastSeen: time.Unix(1714000000, 0), Source: "gossip"},
	}
	for _, r := range rows {
		require.NoError(t, store.UpsertKnownPeer(context.Background(), r))
	}

	svc := realSigner{
		pub: pub, priv: priv, issuer: issuer, store: store,
		maxPeers: 50, ttl: 10 * time.Minute,
	}
	r, err := NewRouter(Deps{
		Logger:         zerolog.Nop(),
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		BootstrapPeers: svc,
	})
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)

	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.NoError(t, tbproto.ValidateBootstrapPeerList(&list))
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(pub, &list))

	// All 4 rows are non-self (issuer wasn't seeded), so all 4 returned.
	require.Len(t, list.Peers, 4)
	// Sorted by health_score DESC, ties by tracker_id ASC.
	require.Equal(t, "a:443", list.Peers[0].Addr)
	require.Equal(t, "b:443", list.Peers[1].Addr)
}
```

- [ ] **Step 2: Run, expect pass.**

Run: `go test ./tracker/internal/api/ -run TestBootstrapPeers_Integration -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/api/bootstrap_peers_integration_test.go
git commit -m "test(tracker/api): bootstrap_peers integration with real storage + signer"
```

---

### Task 18: Final make check + race-clean verification

**Files:** none

- [ ] **Step 1: Run full repo-wide tests with race detector.**

Run: `make test`
Expected: PASS across `shared/`, `plugin/`, `tracker/` modules. No race-detector hits.

- [ ] **Step 2: Run lint.**

Run: `make lint`
Expected: clean — no new findings.

- [ ] **Step 3: Build all binaries.**

Run: `make build`
Expected: all binaries compile, including `tracker/bin/token-bay-tracker` (which now ships the `BOOTSTRAP_PEERS` handler) and `plugin/bin/token-bay-sidecar` (which now ships `Client.FetchBootstrapPeers`).

- [ ] **Step 4: Spot-check coverage.**

Run: `go test -cover ./tracker/internal/api/ ./shared/proto/ ./plugin/internal/trackerclient/`
Expected: coverage ≥ slice-3 baselines. Federation coverage stays ≥ 76.1%.

- [ ] **Step 5: Final acceptance verification against the spec.** Walk through `docs/superpowers/specs/federation/2026-05-10-federation-plugin-bootstrap-design.md` §15 acceptance criteria:
- Plugin can fetch + verify (TestFetchBootstrapPeers_OK)
- Empty `known_peers` returns OK with empty list (TestBootstrapPeers_Empty)
- Tampered response rejected (TestFetchBootstrapPeers_BadSig)
- Stale snapshot rejected with skew tolerance (TestFetchBootstrapPeers_Expired + TestFetchBootstrapPeers_SkewTolerance)
- Race-clean across modules (Step 1)
- Lint clean (Step 2)
- Handler wired in run_cmd (Task 11)
- Slice-3 acceptance still passes (Step 1 includes peerexchange tests)

- [ ] **Step 6: Push + open PR.** *(Out of scope for plan-level steps; the executor handles this via the finishing-a-development-branch skill at the end of slice execution.)*

---

## End of plan
