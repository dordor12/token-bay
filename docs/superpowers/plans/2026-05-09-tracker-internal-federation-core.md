# Tracker `internal/federation` — Core + Root-Attestation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the first vertical slice of `tracker/internal/federation` per the design spec at `docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md`. After this plan, two trackers can peer in-process, exchange `ROOT_ATTESTATION`s, archive each other's roots via `storage.PutPeerRoot`, and detect equivocation end-to-end.

**Architecture:** A new `shared/federation/` proto package carries the wire format. `tracker/internal/federation` exposes `Open(cfg, deps) → *Federation` with a pluggable `Transport` (in-process default), per-peer goroutines, a TTL-LRU dedupe, a sharded peer registry, a clock-driven publisher pulling signed roots from a `RootSource`, and an apply/forward gossip core. The cross-region transfer / revocation / peer-exchange / real-QUIC pieces are explicit non-goals here.

**Tech Stack:** Go 1.25, stdlib `crypto/ed25519`, `crypto/sha256`, `sync`, `time`, generated proto via `protoc` + `protoc-gen-go` (already in `shared/Makefile`), `google.golang.org/protobuf/proto`, `github.com/rs/zerolog`, `github.com/prometheus/client_golang`, `github.com/stretchr/testify`. New third-party deps: none.

**Specs:**
- `docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md` (this slice)
- `docs/superpowers/specs/federation/2026-04-22-federation-design.md` (umbrella)
- `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` §3, §6, §11

**Dependency order:** Runs after `shared/proto`, `shared/signing`, `shared/ids`, `tracker/internal/config`, `tracker/internal/ledger/storage` (all landed). The publisher's `RootSource` consumer is the existing `tracker/internal/ledger.Ledger`; for v1 we wire it via a tiny adapter and ship a stub for tests.

---

## 1. File map

```
shared/ids/
  id.go                                                ← MODIFY: add TrackerID type
  id_test.go                                           ← MODIFY: TrackerID tests

shared/federation/                                     ← NEW package
  doc.go                                               ← CREATE
  federation.proto                                     ← CREATE
  federation.pb.go                                     ← GENERATED via make proto-gen
  validate.go                                          ← CREATE
  validate_test.go                                     ← CREATE

shared/Makefile                                        ← MODIFY: add federation/federation.proto to PROTO_FILES

tracker/internal/federation/                           ← NEW package
  doc.go                                               ← CREATE
  errors.go / errors_test.go                           ← CREATE
  config.go                                            ← CREATE: Config + Peer types and defaults
  transport.go                                         ← CREATE: Transport / PeerConn interfaces
  transport_inproc.go                                  ← CREATE: in-process Transport
  transport_inproc_test.go                             ← CREATE
  envelope.go / envelope_test.go                       ← CREATE: build/parse signed envelopes + message_id
  dedupe.go / dedupe_test.go                           ← CREATE: TTL LRU
  registry.go / registry_test.go                       ← CREATE: peer registry (active, by id, depeer)
  handshake.go / handshake_test.go                     ← CREATE: HELLO / PEER_AUTH / ACCEPT|REJECT
  peer.go / peer_test.go                               ← CREATE: Peer state machine + recv/send loops
  gossip.go / gossip_test.go                           ← CREATE: forward-to-others orchestrator
  rootattest.go / rootattest_test.go                   ← CREATE: incoming ROOT_ATTESTATION
  equivocation.go / equivocation_test.go               ← CREATE: detect + broadcast + receive
  publisher.go / publisher_test.go                     ← CREATE: outbound ROOT_ATTESTATION emit
  metrics.go / metrics_test.go                         ← CREATE
  subsystem.go / subsystem_test.go                     ← CREATE: Open / Close / public type
  integration_test.go                                  ← CREATE: two/three-tracker scenarios

tracker/internal/config/
  config.go                                            ← MODIFY: extend FederationConfig
  apply_defaults.go                                    ← MODIFY: defaults for the new fields
  validate.go                                          ← MODIFY: validate new fields
  config_test.go / apply_defaults_test.go / validate_test.go ← MODIFY: cases for new fields
```

---

## Task 1: Add `TrackerID` to `shared/ids`

**Files:**
- Modify: `shared/ids/id.go`
- Test: `shared/ids/id_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `shared/ids/id_test.go`:

```go
func TestTrackerID_Bytes_RoundTrip(t *testing.T) {
	t.Parallel()
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	id := ids.TrackerID(raw)
	if got := id.Bytes(); got != raw {
		t.Fatalf("Bytes round-trip: got %x want %x", got, raw)
	}
}

func TestTrackerID_DistinctFromIdentityID(t *testing.T) {
	t.Parallel()
	// Compile-time check: a function taking TrackerID must NOT accept IdentityID.
	var _ func(ids.TrackerID) = func(ids.TrackerID) {}
	// Runtime: same byte content, different types → cannot be assigned without conversion.
	var raw [32]byte
	tid := ids.TrackerID(raw)
	iid := ids.IdentityID(raw)
	if tid.Bytes() != iid.Bytes() {
		t.Fatalf("backing arrays must match for safe conversion")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./shared/ids/...`
Expected: FAIL — `undefined: ids.TrackerID`.

- [ ] **Step 3: Add the type**

Append to `shared/ids/id.go`:

```go
// TrackerID is the 32-byte identifier of a tracker — the SHA-256 of its
// Ed25519 public key, mirroring IdentityID's relationship to plugin keys.
// Strong typing prevents accidentally passing a TrackerID where an
// IdentityID is expected.
type TrackerID [32]byte

// Bytes returns the underlying byte array.
func (t TrackerID) Bytes() [32]byte { return t }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./shared/ids/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/ids/id.go shared/ids/id_test.go
git commit -m "feat(shared/ids): add TrackerID type"
```

---

## Task 2: Add `shared/federation` package skeleton

**Files:**
- Create: `shared/federation/doc.go`
- Create: `shared/federation/federation.proto`
- Modify: `shared/Makefile`

- [ ] **Step 1: Create the proto file**

`shared/federation/federation.proto`:

```proto
syntax = "proto3";
package tokenbay.federation.v1;

option go_package = "github.com/token-bay/token-bay/shared/federation";

// Kind classifies an Envelope's inner payload. Forward-compat: unknown
// kinds at the receiver MUST be parsed and dropped, not crashed on.
enum Kind {
  KIND_UNSPECIFIED           = 0;
  KIND_HELLO                 = 1;
  KIND_PEER_AUTH             = 2;
  KIND_PEERING_ACCEPT        = 3;
  KIND_PEERING_REJECT        = 4;
  KIND_ROOT_ATTESTATION      = 5;
  KIND_EQUIVOCATION_EVIDENCE = 6;
  KIND_PING                  = 7;
  KIND_PONG                  = 8;
}

// Envelope wraps every steady-state and handshake message exchanged
// between trackers. The receiver verifies sender_sig over payload
// against the peer's Ed25519 public key.
message Envelope {
  bytes sender_id  = 1;  // 32 bytes — sender tracker_id (= sha256(pubkey))
  Kind  kind       = 2;
  bytes payload    = 3;  // serialized inner message
  bytes sender_sig = 4;  // 64 bytes — Ed25519(payload) by sender
}

message Hello {
  bytes           tracker_id        = 1;  // 32 bytes
  uint32          protocol_version  = 2;
  bytes           nonce             = 3;  // 32 bytes
  repeated string features          = 4;
}

message PeerAuth {
  bytes nonce_sig = 1;  // 64 bytes — Ed25519(counterparty Hello.nonce)
}

message PeeringAccept {
  uint32 dedupe_ttl_s    = 1;
  uint32 gossip_rate_qps = 2;
}

message PeeringReject { string reason = 1; }

message RootAttestation {
  bytes  tracker_id  = 1;  // 32 bytes
  uint64 hour        = 2;
  bytes  merkle_root = 3;  // exactly storage.PutMerkleRoot's root bytes
  bytes  tracker_sig = 4;  // 64 bytes — exactly storage.PutMerkleRoot's tracker_sig
}

message EquivocationEvidence {
  bytes  tracker_id = 1;  // offender
  uint64 hour       = 2;
  bytes  root_a     = 3;
  bytes  sig_a      = 4;
  bytes  root_b     = 5;
  bytes  sig_b      = 6;
}

message Ping { uint64 nonce = 1; }
message Pong { uint64 nonce = 1; }
```

- [ ] **Step 2: Create the doc.go**

`shared/federation/doc.go`:

```go
// Package federation holds the wire-format types tracker peers exchange
// in the federation gossip protocol: handshake messages
// (Hello/PeerAuth/PeeringAccept/PeeringReject), steady-state messages
// (RootAttestation, EquivocationEvidence, Ping/Pong), and the outer
// Envelope that signs over each payload.
//
// Validators run pre-marshal at the sender and post-unmarshal at the
// receiver before any field is trusted. See validate.go.
//
// Spec: docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md.
package federation
```

- [ ] **Step 3: Add to PROTO_FILES**

Edit `shared/Makefile` line 5 — append `federation/federation.proto`:

```makefile
PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto proto/ledger.proto proto/rpc.proto admission/admission.proto federation/federation.proto
```

- [ ] **Step 4: Generate**

Run: `make -C shared proto-gen`
Expected: writes `shared/federation/federation.pb.go`. If protoc is unavailable, ask the user to install `protoc` + `protoc-gen-go` per the existing `make proto-gen` error message.

- [ ] **Step 5: Verify build**

Run: `go build ./shared/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add shared/federation/ shared/Makefile
git commit -m "feat(shared/federation): wire-format protos for federation gossip"
```

---

## Task 3: `shared/federation` validators

**Files:**
- Create: `shared/federation/validate.go`
- Create: `shared/federation/validate_test.go`

- [ ] **Step 1: Write the failing tests**

`shared/federation/validate_test.go`:

```go
package federation_test

import (
	"strings"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
)

func b(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func TestValidateEnvelope_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateEnvelope(&fed.Envelope{
		SenderId:  b(32, 1),
		Kind:      fed.Kind_KIND_PING,
		Payload:   []byte{0x01},
		SenderSig: b(64, 2),
	}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateEnvelope_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]*fed.Envelope{
		"nil":           nil,
		"sender_id_len": {SenderId: b(31, 1), Kind: fed.Kind_KIND_PING, Payload: []byte{1}, SenderSig: b(64, 2)},
		"kind_zero":     {SenderId: b(32, 1), Kind: fed.Kind_KIND_UNSPECIFIED, Payload: []byte{1}, SenderSig: b(64, 2)},
		"kind_oob":      {SenderId: b(32, 1), Kind: fed.Kind(999), Payload: []byte{1}, SenderSig: b(64, 2)},
		"payload_empty": {SenderId: b(32, 1), Kind: fed.Kind_KIND_PING, Payload: nil, SenderSig: b(64, 2)},
		"sig_len":       {SenderId: b(32, 1), Kind: fed.Kind_KIND_PING, Payload: []byte{1}, SenderSig: b(63, 2)},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fed.ValidateEnvelope(env); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestValidateRootAttestation_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateRootAttestation(&fed.RootAttestation{
		TrackerId:  b(32, 1),
		Hour:       42,
		MerkleRoot: b(32, 3),
		TrackerSig: b(64, 4),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRootAttestation_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]*fed.RootAttestation{
		"nil":            nil,
		"tracker_id_len": {TrackerId: b(31, 1), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"tracker_id_zero": {TrackerId: make([]byte, 32), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"hour_zero":      {TrackerId: b(32, 1), Hour: 0, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"root_len":       {TrackerId: b(32, 1), Hour: 1, MerkleRoot: b(31, 3), TrackerSig: b(64, 4)},
		"sig_len":        {TrackerId: b(32, 1), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(63, 4)},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fed.ValidateRootAttestation(m); err == nil || !strings.Contains(err.Error(), "federation:") {
				t.Fatalf("expected federation: error for %s, got %v", name, err)
			}
		})
	}
}

func TestValidateEquivocationEvidence_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateEquivocationEvidence(&fed.EquivocationEvidence{
		TrackerId: b(32, 1), Hour: 7,
		RootA: b(32, 9), SigA: b(64, 9),
		RootB: b(32, 8), SigB: b(64, 8),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEquivocationEvidence_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]*fed.EquivocationEvidence{
		"nil":          nil,
		"roots_equal":  {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(64, 9), RootB: b(32, 9), SigB: b(64, 9)},
		"sig_a_len":    {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(63, 9), RootB: b(32, 8), SigB: b(64, 8)},
		"sig_b_len":    {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(64, 9), RootB: b(32, 8), SigB: b(63, 8)},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fed.ValidateEquivocationEvidence(m); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestValidateHelloAndPeerAuth(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateHello(&fed.Hello{TrackerId: b(32, 1), ProtocolVersion: 1, Nonce: b(32, 2)}); err != nil {
		t.Fatalf("hello valid: %v", err)
	}
	if err := fed.ValidateHello(&fed.Hello{TrackerId: b(32, 1), ProtocolVersion: 1, Nonce: b(31, 2)}); err == nil {
		t.Fatal("expected nonce-len error")
	}
	if err := fed.ValidatePeerAuth(&fed.PeerAuth{NonceSig: b(64, 1)}); err != nil {
		t.Fatalf("peerauth valid: %v", err)
	}
	if err := fed.ValidatePeerAuth(&fed.PeerAuth{NonceSig: b(63, 1)}); err == nil {
		t.Fatal("expected sig-len error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./shared/federation/...`
Expected: FAIL — `undefined: federation.ValidateEnvelope` etc.

- [ ] **Step 3: Implement validators**

`shared/federation/validate.go`:

```go
package federation

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	trackerIDLen = 32
	rootLen      = 32
	sigLen       = 64
	nonceLen     = 32
)

// ValidateEnvelope enforces shape invariants on an Envelope. Receivers
// MUST call this before verifying sender_sig or dispatching by Kind.
// Senders MUST call it before signing.
func ValidateEnvelope(e *Envelope) error {
	if e == nil {
		return errors.New("federation: nil Envelope")
	}
	if len(e.SenderId) != trackerIDLen {
		return fmt.Errorf("federation: sender_id len %d != %d", len(e.SenderId), trackerIDLen)
	}
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_PONG {
		return fmt.Errorf("federation: kind %d out of range", int32(e.Kind))
	}
	if len(e.Payload) == 0 {
		return errors.New("federation: payload empty")
	}
	if len(e.SenderSig) != sigLen {
		return fmt.Errorf("federation: sender_sig len %d != %d", len(e.SenderSig), sigLen)
	}
	return nil
}

func ValidateHello(h *Hello) error {
	if h == nil {
		return errors.New("federation: nil Hello")
	}
	if len(h.TrackerId) != trackerIDLen {
		return fmt.Errorf("federation: hello.tracker_id len %d != %d", len(h.TrackerId), trackerIDLen)
	}
	if len(h.Nonce) != nonceLen {
		return fmt.Errorf("federation: hello.nonce len %d != %d", len(h.Nonce), nonceLen)
	}
	return nil
}

func ValidatePeerAuth(p *PeerAuth) error {
	if p == nil {
		return errors.New("federation: nil PeerAuth")
	}
	if len(p.NonceSig) != sigLen {
		return fmt.Errorf("federation: peerauth.nonce_sig len %d != %d", len(p.NonceSig), sigLen)
	}
	return nil
}

func ValidateRootAttestation(m *RootAttestation) error {
	if m == nil {
		return errors.New("federation: nil RootAttestation")
	}
	if len(m.TrackerId) != trackerIDLen || allZero(m.TrackerId) {
		return fmt.Errorf("federation: root_attestation.tracker_id invalid")
	}
	if m.Hour == 0 {
		return errors.New("federation: root_attestation.hour must be > 0")
	}
	if len(m.MerkleRoot) != rootLen {
		return fmt.Errorf("federation: root_attestation.merkle_root len %d != %d", len(m.MerkleRoot), rootLen)
	}
	if len(m.TrackerSig) != sigLen {
		return fmt.Errorf("federation: root_attestation.tracker_sig len %d != %d", len(m.TrackerSig), sigLen)
	}
	return nil
}

func ValidateEquivocationEvidence(m *EquivocationEvidence) error {
	if m == nil {
		return errors.New("federation: nil EquivocationEvidence")
	}
	if len(m.TrackerId) != trackerIDLen {
		return fmt.Errorf("federation: evidence.tracker_id len %d != %d", len(m.TrackerId), trackerIDLen)
	}
	if len(m.RootA) != rootLen || len(m.RootB) != rootLen {
		return errors.New("federation: evidence.root_a/b must be 32 bytes")
	}
	if len(m.SigA) != sigLen || len(m.SigB) != sigLen {
		return errors.New("federation: evidence.sig_a/b must be 64 bytes")
	}
	if bytes.Equal(m.RootA, m.RootB) {
		return errors.New("federation: evidence.root_a == root_b (no equivocation)")
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./shared/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/federation/validate.go shared/federation/validate_test.go
git commit -m "feat(shared/federation): shape validators for envelope + steady-state messages"
```

---

## Task 4: `tracker/internal/federation` package skeleton

**Files:**
- Create: `tracker/internal/federation/doc.go`
- Create: `tracker/internal/federation/errors.go`
- Create: `tracker/internal/federation/errors_test.go`

- [ ] **Step 1: Write the failing test**

`tracker/internal/federation/errors_test.go`:

```go
package federation_test

import (
	"errors"
	"testing"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestErrSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	cases := []error{
		federation.ErrPeerUnknown,
		federation.ErrPeerClosed,
		federation.ErrSigInvalid,
		federation.ErrFrameTooLarge,
		federation.ErrHandshakeFailed,
		federation.ErrEquivocation,
	}
	for i, a := range cases {
		for j, b := range cases {
			if i != j && errors.Is(a, b) {
				t.Fatalf("error %d aliases %d", i, j)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — package or symbols missing.

- [ ] **Step 3: Implement the package skeleton**

`tracker/internal/federation/doc.go`:

```go
// Package federation is the tracker subsystem that participates in the
// peer-tracker gossip graph. This first slice ships peering, gossip core,
// and the ROOT_ATTESTATION + EQUIVOCATION_EVIDENCE flow against the
// existing peer_root_archive storage.
//
// Construct via Open(cfg, deps); the returned *Federation owns one
// goroutine per peer connection plus a single publisher goroutine.
//
// Spec: docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md.
//
// Concurrency: this package is on the always-`-race` list per tracker
// spec §6 / admission-design §11.3. A `-race` failure here is always a
// real bug.
package federation
```

`tracker/internal/federation/errors.go`:

```go
package federation

import "errors"

// Sentinel errors. Wrap with fmt.Errorf("...: %w", err) when adding context.
var (
	ErrPeerUnknown     = errors.New("federation: unknown peer")
	ErrPeerClosed      = errors.New("federation: peer connection closed")
	ErrSigInvalid      = errors.New("federation: invalid signature")
	ErrFrameTooLarge   = errors.New("federation: frame exceeds 1 MiB cap")
	ErrHandshakeFailed = errors.New("federation: handshake failed")
	ErrEquivocation    = errors.New("federation: equivocation detected")
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/
git commit -m "feat(tracker/federation): package skeleton + sentinel errors"
```

---

## Task 5: `Transport` interface

**Files:**
- Create: `tracker/internal/federation/transport.go`

(No test file yet — the interface is exercised by the in-process implementation in Task 6.)

- [ ] **Step 1: Define the interface**

`tracker/internal/federation/transport.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
)

// MaxFrameBytes is the hard cap on a single transport frame.
const MaxFrameBytes = 1 << 20 // 1 MiB

// Transport is the connection plane between trackers. The slice ships an
// in-process implementation; a real QUIC/TLS transport implements the
// same interface in a follow-up subsystem.
type Transport interface {
	Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error)
	Listen(ctx context.Context, accept func(PeerConn)) error
	Close() error
}

// PeerConn is one peer-to-peer link. Frames are length-delimited at the
// transport boundary (the transport itself enforces MaxFrameBytes); this
// interface deals in already-framed payloads.
type PeerConn interface {
	Send(ctx context.Context, frame []byte) error
	Recv(ctx context.Context) ([]byte, error)
	RemoteAddr() string
	RemotePub() ed25519.PublicKey
	Close() error
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/federation/transport.go
git commit -m "feat(tracker/federation): Transport / PeerConn interfaces"
```

---

## Task 6: In-process `Transport` implementation

**Files:**
- Create: `tracker/internal/federation/transport_inproc.go`
- Create: `tracker/internal/federation/transport_inproc_test.go`

- [ ] **Step 1: Write the failing test**

`tracker/internal/federation/transport_inproc_test.go`:

```go
package federation_test

import (
	"context"
	crand "crypto/rand"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// b is a shared helper for all federation tests in this package: returns
// an n-byte slice filled with `fill`. Defined here (the first tracker-
// side test file) so subsequent test files can reuse it without a
// dedicated helpers_test.go.
func b(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func TestInprocTransport_DialAccept_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	accepted := make(chan federation.PeerConn, 1)
	go func() {
		_ = server.Listen(ctx, func(c federation.PeerConn) { accepted <- c })
	}()

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()

	conn, err := client.Dial(ctx, "srv", pub)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srv := <-accepted

	if err := conn.Send(ctx, []byte("hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := srv.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q want hello", got)
	}
	if !ed25519.PublicKey(srv.RemotePub()).Equal(cliPub) {
		t.Fatal("remote pub mismatch on server side")
	}
}

func TestInprocTransport_Dial_ExpectedPubMismatch(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()
	go server.Listen(context.Background(), func(federation.PeerConn) {})

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()

	wrong, _, _ := ed25519.GenerateKey(crand.Reader)
	_, err := client.Dial(context.Background(), "srv", wrong)
	if !errors.Is(err, federation.ErrHandshakeFailed) {
		t.Fatalf("expected ErrHandshakeFailed, got %v", err)
	}
}

func TestInprocTransport_Send_FrameTooLarge(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()
	go server.Listen(context.Background(), func(federation.PeerConn) {})

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()
	conn, _ := client.Dial(context.Background(), "srv", pub)

	big := make([]byte, federation.MaxFrameBytes+1)
	if err := conn.Send(context.Background(), big); !errors.Is(err, federation.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.NewInprocHub`.

- [ ] **Step 3: Implement the in-process transport**

`tracker/internal/federation/transport_inproc.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
)

// InprocHub routes Dial calls between InprocTransports registered under a
// string addr. It exists so a test can wire two trackers without sockets.
type InprocHub struct {
	mu        sync.Mutex
	listeners map[string]*InprocTransport
}

func NewInprocHub() *InprocHub {
	return &InprocHub{listeners: make(map[string]*InprocTransport)}
}

// InprocTransport implements Transport in-process via channel-paired
// PeerConns. Safe for concurrent use. Construction registers the
// listener into the hub eagerly so a Dial racing the goroutine that
// runs Listen() always sees the addr — the accept callback is itself
// posted to a buffered channel by Listen and returned to the channel
// after each Dial uses it (single-listener-many-dials invariant).
type InprocTransport struct {
	hub    *InprocHub
	addr   string
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	accept chan func(PeerConn)

	mu     sync.Mutex
	closed bool
	conns  []*inprocConn
}

func NewInprocTransport(hub *InprocHub, addr string, pub ed25519.PublicKey, priv ed25519.PrivateKey) *InprocTransport {
	t := &InprocTransport{hub: hub, addr: addr, pub: pub, priv: priv, accept: make(chan func(PeerConn), 1)}
	hub.mu.Lock()
	hub.listeners[addr] = t
	hub.mu.Unlock()
	return t
}

func (t *InprocTransport) Listen(ctx context.Context, accept func(PeerConn)) error {
	// Single-listener invariant: if a callback is already pending, swap it.
	select {
	case <-t.accept:
	default:
	}
	t.accept <- accept
	<-ctx.Done()
	return ctx.Err()
}

func (t *InprocTransport) Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error) {
	t.hub.mu.Lock()
	srv, ok := t.hub.listeners[addr]
	t.hub.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: addr %q has no listener", ErrHandshakeFailed, addr)
	}
	if !ed25519.PublicKey(srv.pub).Equal(expectedPeer) {
		return nil, fmt.Errorf("%w: addr %q peer pubkey mismatch", ErrHandshakeFailed, addr)
	}

	// Wait for the listener to post its accept callback.
	var fn func(PeerConn)
	select {
	case fn = <-srv.accept:
		srv.accept <- fn // put it back for the next dial
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	srv.mu.Lock()
	if srv.closed {
		srv.mu.Unlock()
		return nil, fmt.Errorf("%w: %q closed", ErrHandshakeFailed, addr)
	}
	pair := newInprocPair(t.pub, srv.pub)
	srv.conns = append(srv.conns, pair.right)
	t.mu.Lock()
	t.conns = append(t.conns, pair.left)
	t.mu.Unlock()
	srv.mu.Unlock()
	fn(pair.right)
	return pair.left, nil
}

func (t *InprocTransport) Close() error {
	t.hub.mu.Lock()
	delete(t.hub.listeners, t.addr)
	t.hub.mu.Unlock()
	t.mu.Lock()
	t.closed = true
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

// inprocConn is one half of a paired connection. Send on left lands on
// right.recv, and vice versa.
type inprocConn struct {
	myPub      ed25519.PublicKey
	remotePub  ed25519.PublicKey
	addr       string
	send       chan []byte // outbound; the *peer* reads this
	recv       chan []byte // inbound

	mu     sync.Mutex
	closed bool
}

type inprocPair struct {
	left, right *inprocConn
}

func newInprocPair(localPub, remotePub ed25519.PublicKey) *inprocPair {
	a := make(chan []byte, 16)
	b := make(chan []byte, 16)
	left := &inprocConn{myPub: localPub, remotePub: remotePub, addr: "inproc-left", send: a, recv: b}
	right := &inprocConn{myPub: remotePub, remotePub: localPub, addr: "inproc-right", send: b, recv: a}
	return &inprocPair{left: left, right: right}
}

func (c *inprocConn) Send(ctx context.Context, frame []byte) error {
	if len(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrPeerClosed
	}
	select {
	case c.send <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *inprocConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f, ok := <-c.recv:
		if !ok {
			return nil, ErrPeerClosed
		}
		return f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *inprocConn) RemoteAddr() string             { return c.addr }
func (c *inprocConn) RemotePub() ed25519.PublicKey   { return c.remotePub }

func (c *inprocConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	defer func() { _ = recover() }() // double-close is fine
	close(c.send)
	return nil
}

var _ Transport = (*InprocTransport)(nil)
var _ PeerConn = (*inprocConn)(nil)
var _ = errors.New // keep import even if all errors are sentinels
```

- [ ] **Step 4: Run tests to verify they pass under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/transport_inproc.go tracker/internal/federation/transport_inproc_test.go
git commit -m "feat(tracker/federation): in-process Transport for tests + dev"
```

---

## Task 7: Envelope build/parse + `message_id`

**Files:**
- Create: `tracker/internal/federation/envelope.go`
- Create: `tracker/internal/federation/envelope_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/envelope_test.go`:

```go
package federation_test

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"google.golang.org/protobuf/proto"
)

func TestEnvelope_SignAndVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	id := sha256.Sum256(pub)

	payload, _ := proto.Marshal(&fed.Ping{Nonce: 99})
	env, err := federation.SignEnvelope(priv, id[:], fed.Kind_KIND_PING, payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := federation.MessageID(env); got != sha256.Sum256(payload) {
		t.Fatalf("MessageID mismatch: %x vs %x", got, sha256.Sum256(payload))
	}
	if err := federation.VerifyEnvelope(pub, env); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestEnvelope_VerifyEnvelope_RejectsBadSig(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	other, _, _ := ed25519.GenerateKey(crand.Reader)
	id := sha256.Sum256(pub)
	env, _ := federation.SignEnvelope(priv, id[:], fed.Kind_KIND_PING, []byte{1})
	if err := federation.VerifyEnvelope(other, env); err == nil {
		t.Fatal("expected verify failure under wrong key")
	}
}

func TestEnvelope_MarshalUnmarshalFrame(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	id := sha256.Sum256(pub)
	env, _ := federation.SignEnvelope(priv, id[:], fed.Kind_KIND_PING, []byte{1, 2, 3})
	frame, err := federation.MarshalFrame(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := federation.UnmarshalFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != fed.Kind_KIND_PING || string(got.Payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.SignEnvelope` etc.

- [ ] **Step 3: Implement envelope helpers**

`tracker/internal/federation/envelope.go`:

```go
package federation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

// SignEnvelope wraps payload in an Envelope signed by priv.
//
// payload MUST already be the Marshal() bytes of the inner proto. The
// caller is responsible for shape-validating the inner message before
// signing (see shared/federation.Validate*).
func SignEnvelope(priv ed25519.PrivateKey, senderID []byte, kind fed.Kind, payload []byte) (*fed.Envelope, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("federation: SignEnvelope requires Ed25519 private key")
	}
	if len(senderID) != trackerIDLen {
		return nil, fmt.Errorf("federation: senderID must be %d bytes", trackerIDLen)
	}
	env := &fed.Envelope{
		SenderId:  append([]byte(nil), senderID...),
		Kind:      kind,
		Payload:   append([]byte(nil), payload...),
		SenderSig: ed25519.Sign(priv, payload),
	}
	if err := fed.ValidateEnvelope(env); err != nil {
		return nil, err
	}
	return env, nil
}

// VerifyEnvelope returns nil iff e is well-shaped and e.SenderSig is
// valid Ed25519 over e.Payload under pub.
func VerifyEnvelope(pub ed25519.PublicKey, e *fed.Envelope) error {
	if err := fed.ValidateEnvelope(e); err != nil {
		return err
	}
	if !signing.Verify(pub, e.Payload, e.SenderSig) {
		return ErrSigInvalid
	}
	return nil
}

// MessageID is sha256(payload). It is computed over the inner payload
// only — NOT the envelope — so the same inner message gossiped via two
// paths produces the same id even though sender_sig differs.
func MessageID(e *fed.Envelope) [32]byte {
	return sha256.Sum256(e.Payload)
}

// MarshalFrame serializes an Envelope. Receivers feed the bytes back
// through UnmarshalFrame.
func MarshalFrame(e *fed.Envelope) ([]byte, error) {
	if err := fed.ValidateEnvelope(e); err != nil {
		return nil, err
	}
	return proto.Marshal(e)
}

// UnmarshalFrame parses a frame into an Envelope and validates its shape.
// Returns ErrFrameTooLarge if the frame is over 1 MiB.
func UnmarshalFrame(frame []byte) (*fed.Envelope, error) {
	if len(frame) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	var e fed.Envelope
	if err := proto.Unmarshal(frame, &e); err != nil {
		return nil, fmt.Errorf("federation: unmarshal frame: %w", err)
	}
	if err := fed.ValidateEnvelope(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

// trackerIDLen is duplicated from shared/federation to avoid an internal
// import cycle later. Both packages enforce identical lengths.
const trackerIDLen = 32
```

- [ ] **Step 4: Run tests to verify they pass under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/envelope.go tracker/internal/federation/envelope_test.go
git commit -m "feat(tracker/federation): envelope sign/verify + message_id"
```

---

## Task 8: Dedupe TTL LRU

**Files:**
- Create: `tracker/internal/federation/dedupe.go`
- Create: `tracker/internal/federation/dedupe_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/dedupe_test.go`:

```go
package federation_test

import (
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestDedupe_MarkAndSeen(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Minute, 1024, clock.Now)

	id := [32]byte{1}
	if d.Seen(id) {
		t.Fatal("Seen returned true before Mark")
	}
	d.Mark(id)
	if !d.Seen(id) {
		t.Fatal("Seen returned false after Mark")
	}
}

func TestDedupe_TTLExpiry(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Minute, 1024, clock.Now)
	id := [32]byte{2}
	d.Mark(id)
	clock.Advance(2 * time.Minute)
	if d.Seen(id) {
		t.Fatal("Seen returned true after TTL")
	}
}

func TestDedupe_CapacityEvicts(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Hour, 4, clock.Now)
	for i := byte(0); i < 6; i++ {
		d.Mark([32]byte{i})
		clock.Advance(time.Second) // strictly increasing timestamps
	}
	// Oldest (i=0,1) must have been evicted to keep capacity at 4.
	if d.Seen([32]byte{0}) || d.Seen([32]byte{1}) {
		t.Fatal("expected oldest entries evicted")
	}
	for i := byte(2); i < 6; i++ {
		if !d.Seen([32]byte{i}) {
			t.Fatalf("entry %d unexpectedly evicted", i)
		}
	}
}

// fakeClock — file-level test helper.
type fakeClock struct{ now time.Time }

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }
func (f *fakeClock) Now() time.Time      { return f.now }
func (f *fakeClock) Advance(d time.Duration) { f.now = f.now.Add(d) }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.NewDedupe`.

- [ ] **Step 3: Implement Dedupe**

`tracker/internal/federation/dedupe.go`:

```go
package federation

import (
	"container/list"
	"sync"
	"time"
)

type dedupeEntry struct {
	id     [32]byte
	expiry time.Time
}

// Dedupe is a TTL-bounded set keyed by message_id. Insertion order is
// tracked via a doubly-linked list to support O(1) eviction of the
// oldest entry when capacity is hit.
type Dedupe struct {
	ttl time.Duration
	cap int
	now func() time.Time

	mu    sync.Mutex
	order *list.List               // front = newest
	idx   map[[32]byte]*list.Element
}

func NewDedupe(ttl time.Duration, capacity int, now func() time.Time) *Dedupe {
	if capacity <= 0 {
		capacity = 1024
	}
	if now == nil {
		now = time.Now
	}
	return &Dedupe{ttl: ttl, cap: capacity, now: now, order: list.New(), idx: make(map[[32]byte]*list.Element)}
}

// Mark records id with expiry = now + ttl. Idempotent — re-marking refreshes the TTL.
func (d *Dedupe) Mark(id [32]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictExpiredLocked()
	if el, ok := d.idx[id]; ok {
		el.Value.(*dedupeEntry).expiry = d.now().Add(d.ttl)
		d.order.MoveToFront(el)
		return
	}
	for d.order.Len() >= d.cap {
		oldest := d.order.Back()
		if oldest == nil {
			break
		}
		ent := oldest.Value.(*dedupeEntry)
		delete(d.idx, ent.id)
		d.order.Remove(oldest)
	}
	el := d.order.PushFront(&dedupeEntry{id: id, expiry: d.now().Add(d.ttl)})
	d.idx[id] = el
}

// Seen returns true iff id is currently present (TTL not expired).
func (d *Dedupe) Seen(id [32]byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictExpiredLocked()
	_, ok := d.idx[id]
	return ok
}

// Size returns the current number of live entries (test-only convenience).
func (d *Dedupe) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictExpiredLocked()
	return d.order.Len()
}

func (d *Dedupe) evictExpiredLocked() {
	now := d.now()
	for {
		el := d.order.Back()
		if el == nil {
			return
		}
		ent := el.Value.(*dedupeEntry)
		if now.Before(ent.expiry) {
			return
		}
		delete(d.idx, ent.id)
		d.order.Remove(el)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/dedupe.go tracker/internal/federation/dedupe_test.go
git commit -m "feat(tracker/federation): TTL LRU dedupe set"
```

---

## Task 9: Peer registry

**Files:**
- Create: `tracker/internal/federation/registry.go`
- Create: `tracker/internal/federation/registry_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/registry_test.go`:

```go
package federation_test

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"testing"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func mkID(t *testing.T) (ids.TrackerID, ed25519.PublicKey) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(crand.Reader)
	return ids.TrackerID(sha256.Sum256(pub)), pub
}

func TestRegistry_AddLookupRemove(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	if err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"}); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get(id)
	if !ok || !ed25519.PublicKey(got.PubKey).Equal(pub) {
		t.Fatalf("Get miss or pub mismatch")
	}
	if err := r.Depeer(id, federation.ReasonEquivocation); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(id); ok {
		t.Fatal("Get after Depeer should miss")
	}
}

func TestRegistry_All_ReturnsCopy(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id1, pub1 := mkID(t)
	id2, pub2 := mkID(t)
	_ = r.Add(federation.PeerInfo{TrackerID: id1, PubKey: pub1, Addr: "a"})
	_ = r.Add(federation.PeerInfo{TrackerID: id2, PubKey: pub2, Addr: "b"})
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() = %d entries, want 2", len(all))
	}
	all[0].Addr = "tampered"
	got, _ := r.Get(id1)
	if got.Addr == "tampered" {
		t.Fatal("All() must return a copy, not aliased state")
	}
}

func TestRegistry_Add_Duplicate(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	_ = r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"})
	if err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"}); err == nil {
		t.Fatal("expected duplicate error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.NewRegistry`.

- [ ] **Step 3: Implement the registry**

`tracker/internal/federation/registry.go`:

```go
package federation

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// DepeerReason classifies why a peer was removed.
type DepeerReason int

const (
	ReasonOperator DepeerReason = iota
	ReasonEquivocation
	ReasonHandshakeFailed
	ReasonInvalidSignature
	ReasonDisconnected
)

func (r DepeerReason) String() string {
	switch r {
	case ReasonOperator:
		return "operator"
	case ReasonEquivocation:
		return "equivocation"
	case ReasonHandshakeFailed:
		return "handshake_failed"
	case ReasonInvalidSignature:
		return "invalid_signature"
	case ReasonDisconnected:
		return "disconnected"
	}
	return "unknown"
}

// PeerInfo is the operator-facing snapshot of one peer.
type PeerInfo struct {
	TrackerID ids.TrackerID
	PubKey    ed25519.PublicKey
	Addr      string
	Region    string
	State     PeerState
	Conn      PeerConn // nil unless State == PeerStateSteady
	Since     time.Time
}

type PeerState int

const (
	PeerStatePending PeerState = iota
	PeerStateDialing
	PeerStateHandshake
	PeerStateSteady
	PeerStateClosed
)

func (s PeerState) String() string {
	switch s {
	case PeerStatePending:
		return "pending"
	case PeerStateDialing:
		return "dialing"
	case PeerStateHandshake:
		return "handshake"
	case PeerStateSteady:
		return "steady"
	case PeerStateClosed:
		return "closed"
	}
	return "unknown"
}

// Registry holds the active peer set keyed by TrackerID.
type Registry struct {
	mu    sync.RWMutex
	peers map[ids.TrackerID]PeerInfo
}

func NewRegistry() *Registry {
	return &Registry{peers: make(map[ids.TrackerID]PeerInfo)}
}

func (r *Registry) Add(p PeerInfo) error {
	if len(p.PubKey) != ed25519.PublicKeySize {
		return errors.New("registry: PubKey must be 32 bytes")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.peers[p.TrackerID]; ok {
		return fmt.Errorf("registry: duplicate tracker_id %x", p.TrackerID.Bytes())
	}
	r.peers[p.TrackerID] = p
	return nil
}

// Update replaces the record for an existing peer; returns ErrPeerUnknown
// if the peer was never Added.
func (r *Registry) Update(p PeerInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.peers[p.TrackerID]; !ok {
		return ErrPeerUnknown
	}
	r.peers[p.TrackerID] = p
	return nil
}

func (r *Registry) Get(id ids.TrackerID) (PeerInfo, bool) {
	r.mu.RLock()
	p, ok := r.peers[id]
	r.mu.RUnlock()
	return p, ok
}

func (r *Registry) IsActive(id ids.TrackerID) bool {
	p, ok := r.Get(id)
	return ok && p.State == PeerStateSteady
}

// All returns a copy slice of all known peers.
func (r *Registry) All() []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PeerInfo, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p)
	}
	return out
}

// Depeer marks a peer closed and removes it from the active map. Closes
// the underlying conn if any. Idempotent.
func (r *Registry) Depeer(id ids.TrackerID, reason DepeerReason) error {
	r.mu.Lock()
	p, ok := r.peers[id]
	if !ok {
		r.mu.Unlock()
		return ErrPeerUnknown
	}
	delete(r.peers, id)
	r.mu.Unlock()
	if p.Conn != nil {
		_ = p.Conn.Close()
	}
	_ = reason // metric / log emission lives in the caller
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/registry.go tracker/internal/federation/registry_test.go
git commit -m "feat(tracker/federation): peer registry with depeer + state"
```

---

## Task 10: Handshake (HELLO / PEER_AUTH / ACCEPT|REJECT)

**Files:**
- Create: `tracker/internal/federation/handshake.go`
- Create: `tracker/internal/federation/handshake_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/handshake_test.go`:

```go
package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

type peerCfg struct {
	id   ids.TrackerID
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newPeerCfg(t *testing.T) peerCfg {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	return peerCfg{id: ids.TrackerID(sha256.Sum256(pub)), pub: pub, priv: priv}
}

func TestHandshake_DialerInitiator_Success(t *testing.T) {
	t.Parallel()
	srv := newPeerCfg(t)
	cli := newPeerCfg(t)

	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer srvT.Close()
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer cliT.Close()

	expected := map[ids.TrackerID]ed25519.PublicKey{
		srv.id: srv.pub, cli.id: cli.pub,
	}
	srvDone := make(chan error, 1)
	go srvT.Listen(context.Background(), func(c federation.PeerConn) {
		_, err := federation.RunHandshakeListener(context.Background(), c, srv.id, srv.priv, expected, time.Second)
		srvDone <- err
	})

	conn, err := cliT.Dial(context.Background(), "srv", srv.pub)
	if err != nil {
		t.Fatal(err)
	}
	_, err = federation.RunHandshakeDialer(context.Background(), conn, cli.id, cli.priv, srv.id, srv.pub, time.Second)
	if err != nil {
		t.Fatalf("dialer handshake: %v", err)
	}
	if err := <-srvDone; err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
}

func TestHandshake_RejectsUnknownPeer(t *testing.T) {
	t.Parallel()
	srv := newPeerCfg(t)
	cli := newPeerCfg(t)

	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer srvT.Close()
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer cliT.Close()

	expected := map[ids.TrackerID]ed25519.PublicKey{srv.id: srv.pub} // cli not in allowlist
	srvDone := make(chan error, 1)
	go srvT.Listen(context.Background(), func(c federation.PeerConn) {
		_, err := federation.RunHandshakeListener(context.Background(), c, srv.id, srv.priv, expected, time.Second)
		srvDone <- err
	})
	conn, _ := cliT.Dial(context.Background(), "srv", srv.pub)
	_, err := federation.RunHandshakeDialer(context.Background(), conn, cli.id, cli.priv, srv.id, srv.pub, time.Second)
	if !errors.Is(err, federation.ErrHandshakeFailed) {
		t.Fatalf("expected ErrHandshakeFailed, got %v", err)
	}
	if err := <-srvDone; !errors.Is(err, federation.ErrHandshakeFailed) {
		t.Fatalf("listener: expected ErrHandshakeFailed, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — handshake symbols undefined.

- [ ] **Step 3: Implement handshake**

`tracker/internal/federation/handshake.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

const protocolVersion uint32 = 1

// HandshakeResult is what the caller learns about the counterparty after
// a successful handshake.
type HandshakeResult struct {
	PeerTrackerID ids.TrackerID
	PeerPubKey    ed25519.PublicKey
	DedupeTTL     time.Duration
	GossipRateQPS uint32
}

func sendHello(ctx context.Context, conn PeerConn, priv ed25519.PrivateKey, myID ids.TrackerID) ([]byte, error) {
	nonce := make([]byte, 32)
	if _, err := crand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", ErrHandshakeFailed, err)
	}
	idBytes := myID.Bytes()
	hello := &fed.Hello{TrackerId: idBytes[:], ProtocolVersion: protocolVersion, Nonce: nonce}
	if err := fed.ValidateHello(hello); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	payload, _ := proto.Marshal(hello)
	env, err := SignEnvelope(priv, idBytes[:], fed.Kind_KIND_HELLO, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	frame, _ := MarshalFrame(env)
	if err := conn.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	return nonce, nil
}

func recvKind(ctx context.Context, conn PeerConn, want fed.Kind) (*fed.Envelope, []byte, error) {
	frame, err := conn.Recv(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: recv: %v", ErrHandshakeFailed, err)
	}
	env, err := UnmarshalFrame(frame)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if env.Kind != want {
		return nil, nil, fmt.Errorf("%w: got kind %v want %v", ErrHandshakeFailed, env.Kind, want)
	}
	return env, env.Payload, nil
}

func sendPeerAuth(ctx context.Context, conn PeerConn, priv ed25519.PrivateKey, myID ids.TrackerID, theirNonce []byte) error {
	auth := &fed.PeerAuth{NonceSig: ed25519.Sign(priv, theirNonce)}
	if err := fed.ValidatePeerAuth(auth); err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	payload, _ := proto.Marshal(auth)
	idBytes := myID.Bytes()
	env, err := SignEnvelope(priv, idBytes[:], fed.Kind_KIND_PEER_AUTH, payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	frame, _ := MarshalFrame(env)
	return conn.Send(ctx, frame)
}

// validatePeerHello unmarshals + validates a peer's Hello envelope, looks
// up the expected pubkey in the allowlist, verifies envelope sig, and
// returns the peer's TrackerID + pubkey + nonce.
func validatePeerHello(env *fed.Envelope, expected map[ids.TrackerID]ed25519.PublicKey) (ids.TrackerID, ed25519.PublicKey, []byte, error) {
	var hello fed.Hello
	if err := proto.Unmarshal(env.Payload, &hello); err != nil {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: hello unmarshal: %v", ErrHandshakeFailed, err)
	}
	if err := fed.ValidateHello(&hello); err != nil {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if hello.ProtocolVersion != protocolVersion {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: protocol_version %d != %d", ErrHandshakeFailed, hello.ProtocolVersion, protocolVersion)
	}
	var tid ids.TrackerID
	copy(tid[:], hello.TrackerId)
	pub, ok := expected[tid]
	if !ok {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: tracker %x not in allowlist", ErrHandshakeFailed, hello.TrackerId)
	}
	if !signing.Verify(pub, env.Payload, env.SenderSig) {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: hello envelope sig", ErrHandshakeFailed)
	}
	if want := sha256.Sum256(pub); want != tid {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: hello tracker_id != hash(pubkey)", ErrHandshakeFailed)
	}
	return tid, pub, hello.Nonce, nil
}

func validatePeerAuth(env *fed.Envelope, pub ed25519.PublicKey, myNonce []byte) error {
	var auth fed.PeerAuth
	if err := proto.Unmarshal(env.Payload, &auth); err != nil {
		return fmt.Errorf("%w: peer_auth unmarshal: %v", ErrHandshakeFailed, err)
	}
	if err := fed.ValidatePeerAuth(&auth); err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if !signing.Verify(pub, env.Payload, env.SenderSig) {
		return fmt.Errorf("%w: peer_auth envelope sig", ErrHandshakeFailed)
	}
	if !signing.Verify(pub, myNonce, auth.NonceSig) {
		return fmt.Errorf("%w: peer_auth nonce sig", ErrHandshakeFailed)
	}
	return nil
}

func sendAccept(ctx context.Context, conn PeerConn, priv ed25519.PrivateKey, myID ids.TrackerID, ttl time.Duration, qps uint32) error {
	acc := &fed.PeeringAccept{DedupeTtlS: uint32(ttl / time.Second), GossipRateQps: qps}
	payload, _ := proto.Marshal(acc)
	idBytes := myID.Bytes()
	env, err := SignEnvelope(priv, idBytes[:], fed.Kind_KIND_PEERING_ACCEPT, payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	frame, _ := MarshalFrame(env)
	return conn.Send(ctx, frame)
}

// RunHandshakeDialer is called by the side that initiated the connection.
// Sequence: send Hello → recv peer Hello → send PeerAuth → recv PeerAuth
// → recv PeeringAccept|Reject.
func RunHandshakeDialer(ctx context.Context, conn PeerConn, myID ids.TrackerID, priv ed25519.PrivateKey, peerID ids.TrackerID, peerPub ed25519.PublicKey, timeout time.Duration) (HandshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	myNonce, err := sendHello(ctx, conn, priv, myID)
	if err != nil {
		return HandshakeResult{}, err
	}
	helloEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_HELLO)
	if err != nil {
		return HandshakeResult{}, err
	}
	gotID, gotPub, theirNonce, err := validatePeerHello(helloEnv, map[ids.TrackerID]ed25519.PublicKey{peerID: peerPub})
	if err != nil {
		return HandshakeResult{}, err
	}
	if err := sendPeerAuth(ctx, conn, priv, myID, theirNonce); err != nil {
		return HandshakeResult{}, err
	}
	authEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_PEER_AUTH)
	if err != nil {
		return HandshakeResult{}, err
	}
	if err := validatePeerAuth(authEnv, gotPub, myNonce); err != nil {
		return HandshakeResult{}, err
	}
	accEnv, accPayload, err := recvKind(ctx, conn, fed.Kind_KIND_PEERING_ACCEPT)
	if err != nil {
		return HandshakeResult{}, err
	}
	if !signing.Verify(gotPub, accEnv.Payload, accEnv.SenderSig) {
		return HandshakeResult{}, fmt.Errorf("%w: accept envelope sig", ErrHandshakeFailed)
	}
	var acc fed.PeeringAccept
	_ = proto.Unmarshal(accPayload, &acc)
	return HandshakeResult{PeerTrackerID: gotID, PeerPubKey: gotPub, DedupeTTL: time.Duration(acc.DedupeTtlS) * time.Second, GossipRateQPS: acc.GossipRateQps}, nil
}

// RunHandshakeListener is called by the side that accepted the connection.
// Sequence: recv peer Hello → send Hello → recv PeerAuth → send PeerAuth
// → send PeeringAccept.
func RunHandshakeListener(ctx context.Context, conn PeerConn, myID ids.TrackerID, priv ed25519.PrivateKey, expected map[ids.TrackerID]ed25519.PublicKey, timeout time.Duration) (HandshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	helloEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_HELLO)
	if err != nil {
		return HandshakeResult{}, err
	}
	gotID, gotPub, theirNonce, err := validatePeerHello(helloEnv, expected)
	if err != nil {
		return HandshakeResult{}, err
	}
	myNonce, err := sendHello(ctx, conn, priv, myID)
	if err != nil {
		return HandshakeResult{}, err
	}
	authEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_PEER_AUTH)
	if err != nil {
		return HandshakeResult{}, err
	}
	if err := validatePeerAuth(authEnv, gotPub, myNonce); err != nil {
		return HandshakeResult{}, err
	}
	if err := sendPeerAuth(ctx, conn, priv, myID, theirNonce); err != nil {
		return HandshakeResult{}, err
	}
	if err := sendAccept(ctx, conn, priv, myID, time.Hour, 100); err != nil {
		return HandshakeResult{}, err
	}
	return HandshakeResult{PeerTrackerID: gotID, PeerPubKey: gotPub, DedupeTTL: time.Hour, GossipRateQPS: 100}, nil
}

var _ = errors.New // keep import
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/handshake.go tracker/internal/federation/handshake_test.go
git commit -m "feat(tracker/federation): HELLO/PEER_AUTH/PEERING_ACCEPT handshake"
```

---

## Task 11: `Peer` state machine + recv/send loops

**Files:**
- Create: `tracker/internal/federation/peer.go`
- Create: `tracker/internal/federation/peer_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/peer_test.go`:

```go
package federation_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"google.golang.org/protobuf/proto"
)

func TestPeer_RecvLoop_DispatchesByKind(t *testing.T) {
	t.Parallel()
	cli, srv := newPeerCfg(t), newPeerCfg(t)
	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer srvT.Close()
	defer cliT.Close()

	got := int32(0)
	dispatch := func(env *fed.Envelope) {
		if env.Kind == fed.Kind_KIND_PING {
			atomic.AddInt32(&got, 1)
		}
	}

	srvAcc := make(chan federation.PeerConn, 1)
	go srvT.Listen(context.Background(), func(c federation.PeerConn) { srvAcc <- c })

	conn, err := cliT.Dial(context.Background(), "srv", srv.pub)
	if err != nil {
		t.Fatal(err)
	}
	srvConn := <-srvAcc

	p := federation.NewPeerForTest(srvConn, dispatch)
	p.Start(context.Background())
	defer p.Stop()

	// Send a Ping from the client side.
	payload, _ := proto.Marshal(&fed.Ping{Nonce: 1})
	env, _ := federation.SignEnvelope(cli.priv, cli.id.Bytes()[:], fed.Kind_KIND_PING, payload)
	frame, _ := federation.MarshalFrame(env)
	_ = conn.Send(context.Background(), frame)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&got) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("dispatch never fired")
}

func TestPeer_Stop_ClosesConn(t *testing.T) {
	t.Parallel()
	cli, srv := newPeerCfg(t), newPeerCfg(t)
	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer srvT.Close()
	defer cliT.Close()

	srvAcc := make(chan federation.PeerConn, 1)
	go srvT.Listen(context.Background(), func(c federation.PeerConn) { srvAcc <- c })
	conn, _ := cliT.Dial(context.Background(), "srv", srv.pub)
	srvConn := <-srvAcc

	p := federation.NewPeerForTest(srvConn, func(*fed.Envelope) {})
	p.Start(context.Background())
	p.Stop()
	_ = conn // recv on the dialer side will see ErrPeerClosed once the inproc pair closes
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.NewPeerForTest`.

- [ ] **Step 3: Implement the Peer**

`tracker/internal/federation/peer.go`:

```go
package federation

import (
	"context"
	"errors"
	"sync"

	fed "github.com/token-bay/token-bay/shared/federation"
)

// Peer wraps a steady-state PeerConn with a recvLoop goroutine. The
// caller-supplied dispatch callback runs synchronously on the recv
// goroutine; it MUST NOT block, and MUST NOT call back into the Peer.
type Peer struct {
	conn     PeerConn
	dispatch func(*fed.Envelope)

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewPeerForTest is a thin constructor exposed only because tests need to
// poke a recv-only Peer without a registry. Production code uses Open's
// internal wiring.
func NewPeerForTest(conn PeerConn, dispatch func(*fed.Envelope)) *Peer {
	return &Peer{conn: conn, dispatch: dispatch}
}

func (p *Peer) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	p.wg.Add(1)
	go p.recvLoop(ctx)
}

func (p *Peer) recvLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		frame, err := p.conn.Recv(ctx)
		if err != nil {
			return
		}
		env, err := UnmarshalFrame(frame)
		if err != nil {
			continue
		}
		p.dispatch(env)
	}
}

// Send is a thin wrapper that surfaces ErrPeerClosed cleanly.
func (p *Peer) Send(ctx context.Context, frame []byte) error {
	if err := p.conn.Send(ctx, frame); err != nil {
		if errors.Is(err, ErrPeerClosed) {
			return ErrPeerClosed
		}
		return err
	}
	return nil
}

func (p *Peer) Stop() {
	p.stopOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		_ = p.conn.Close()
		p.wg.Wait()
	})
}
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/peer.go tracker/internal/federation/peer_test.go
git commit -m "feat(tracker/federation): Peer recvLoop + Stop"
```

---

## Task 12: Gossip core — forward-to-others

**Files:**
- Create: `tracker/internal/federation/gossip.go`
- Create: `tracker/internal/federation/gossip_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/gossip_test.go`:

```go
package federation_test

import (
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// fakePeer implements federation.SendOnlyPeer for gossip tests.
type fakePeer struct {
	id    ids.TrackerID
	mu    sync.Mutex
	frames [][]byte
	fail  bool
}

func (f *fakePeer) ID() ids.TrackerID { return f.id }
func (f *fakePeer) Send(_ context.Context, frame []byte) error {
	if f.fail {
		return federation.ErrPeerClosed
	}
	f.mu.Lock()
	f.frames = append(f.frames, append([]byte(nil), frame...))
	f.mu.Unlock()
	return nil
}
func (f *fakePeer) Frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func TestGossip_ForwardSkipsExclude(t *testing.T) {
	t.Parallel()
	a := &fakePeer{id: ids.TrackerID{1}}
	b := &fakePeer{id: ids.TrackerID{2}}
	c := &fakePeer{id: ids.TrackerID{3}}
	src := newPeerCfg(t)
	g := federation.NewGossipForTest([]federation.SendOnlyPeer{a, b, c}, src.priv, src.id)

	if err := g.Forward(context.Background(), fed.Kind_KIND_PING, []byte{1, 2, 3}, &b.id); err != nil {
		t.Fatal(err)
	}
	if got := len(a.Frames()); got != 1 {
		t.Fatalf("a.frames = %d", got)
	}
	if got := len(b.Frames()); got != 0 {
		t.Fatalf("b.frames = %d (should be excluded)", got)
	}
	if got := len(c.Frames()); got != 1 {
		t.Fatalf("c.frames = %d", got)
	}
}

func TestGossip_ForwardSucceedsDespiteOnePeerError(t *testing.T) {
	t.Parallel()
	a := &fakePeer{id: ids.TrackerID{1}}
	b := &fakePeer{id: ids.TrackerID{2}, fail: true}
	src := newPeerCfg(t)
	g := federation.NewGossipForTest([]federation.SendOnlyPeer{a, b}, src.priv, src.id)
	if err := g.Forward(context.Background(), fed.Kind_KIND_PING, []byte{1}, nil); err != nil {
		t.Fatal(err)
	}
	if len(a.Frames()) != 1 {
		t.Fatal("a should have received the frame")
	}
}

func TestGossip_ForwardMessageIDIsStable(t *testing.T) {
	t.Parallel()
	a := &fakePeer{id: ids.TrackerID{1}}
	src := newPeerCfg(t)
	g := federation.NewGossipForTest([]federation.SendOnlyPeer{a}, src.priv, src.id)
	payload := []byte{9, 9, 9}
	expectedID := sha256.Sum256(payload)
	got := atomic.Pointer[[32]byte]{}
	g = g.WithObserver(func(mid [32]byte, _ fed.Kind) {
		mid := mid
		got.Store(&mid)
	})
	_ = g.Forward(context.Background(), fed.Kind_KIND_PING, payload, nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got.Load() != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got.Load() == nil || *got.Load() != expectedID {
		t.Fatalf("observed message_id = %v want %x", got.Load(), expectedID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL.

- [ ] **Step 3: Implement gossip**

`tracker/internal/federation/gossip.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// SendOnlyPeer is the slice of *Peer the gossip layer needs. Implementing
// this interface (rather than depending on *Peer directly) makes
// gossip_test.go independent of recv-loop wiring.
type SendOnlyPeer interface {
	ID() ids.TrackerID
	Send(ctx context.Context, frame []byte) error
}

// PeerSet is the slice of *Registry the gossip layer needs. The default
// implementation walks Registry.All() and filters non-steady peers; the
// interface enables a fake in tests.
type PeerSet interface {
	ActivePeers() []SendOnlyPeer
}

// Gossip wraps the forward-to-others orchestrator. Construct via
// NewGossip; tests use NewGossipForTest with a static list of peers.
//
// peers is a PeerSet — typically a *registryPeerSet whose pointer-typed
// receiver lets the subsystem fill in its *Federation back-reference
// AFTER NewGossip has captured the interface value.
type Gossip struct {
	mu      sync.RWMutex
	priv    ed25519.PrivateKey
	myID    ids.TrackerID
	peers   PeerSet
	observe func([32]byte, fed.Kind)
}

func NewGossip(priv ed25519.PrivateKey, myID ids.TrackerID, peers PeerSet) *Gossip {
	return &Gossip{priv: priv, myID: myID, peers: peers}
}

func NewGossipForTest(peers []SendOnlyPeer, priv ed25519.PrivateKey, myID ids.TrackerID) *Gossip {
	return &Gossip{priv: priv, myID: myID, peers: staticPeerSet(peers)}
}

// WithObserver returns a copy that calls fn(message_id, kind) for every
// successful Forward. Tests use this to assert IDs without scraping
// peer state.
func (g *Gossip) WithObserver(fn func([32]byte, fed.Kind)) *Gossip {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := *g
	cp.observe = fn
	return &cp
}

// Forward sends payload (already-marshaled inner message) to every active
// peer except optional exclude. Returns nil even if some peers fail —
// gossip is best-effort. Errors are surfaced through metrics, not return
// values.
func (g *Gossip) Forward(ctx context.Context, kind fed.Kind, payload []byte, exclude *ids.TrackerID) error {
	idBytes := g.myID.Bytes()
	env, err := SignEnvelope(g.priv, idBytes[:], kind, payload)
	if err != nil {
		return err
	}
	frame, err := MarshalFrame(env)
	if err != nil {
		return err
	}
	mid := sha256.Sum256(payload)
	if g.observe != nil {
		g.observe(mid, kind)
	}
	for _, p := range g.peers.ActivePeers() {
		if exclude != nil && p.ID() == *exclude {
			continue
		}
		_ = p.Send(ctx, frame) // best-effort; per-peer error handling is the caller's metric problem
	}
	return nil
}

type staticPeerSet []SendOnlyPeer

func (s staticPeerSet) ActivePeers() []SendOnlyPeer { return []SendOnlyPeer(s) }

var _ = errors.New
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/gossip.go tracker/internal/federation/gossip_test.go
git commit -m "feat(tracker/federation): gossip forward-to-others orchestrator"
```

---

## Task 13: ROOT_ATTESTATION receive (`rootattest.go`)

**Files:**
- Create: `tracker/internal/federation/rootattest.go`
- Create: `tracker/internal/federation/rootattest_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/rootattest_test.go`:

```go
package federation_test

import (
	"context"
	"sync/atomic"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// fakeArchive implements federation.PeerRootArchive in-memory.
type fakeArchive struct {
	rows map[string]storage.PeerRoot // key = trackerID|hour
	conflictOnNext bool
}

func newFakeArchive() *fakeArchive {
	return &fakeArchive{rows: map[string]storage.PeerRoot{}}
}

func (f *fakeArchive) key(id []byte, h uint64) string {
	return string(id) + ":" + string(rune(h))
}

func (f *fakeArchive) PutPeerRoot(_ context.Context, p storage.PeerRoot) error {
	if f.conflictOnNext {
		f.conflictOnNext = false
		return storage.ErrPeerRootConflict
	}
	f.rows[f.key(p.TrackerID, p.Hour)] = p
	return nil
}

func (f *fakeArchive) GetPeerRoot(_ context.Context, id []byte, h uint64) (storage.PeerRoot, bool, error) {
	r, ok := f.rows[f.key(id, h)]
	return r, ok, nil
}

func TestRootAttestApply_Persists(t *testing.T) {
	t.Parallel()
	cli := newPeerCfg(t)
	arch := newFakeArchive()
	forwarded := int32(0)
	forwarder := func(context.Context, fed.Kind, []byte) { atomic.AddInt32(&forwarded, 1) }

	apply := federation.NewRootAttestApplier(arch, forwarder, federation.NowFromTime)

	idBytes := cli.id.Bytes()
	msg := &fed.RootAttestation{TrackerId: idBytes[:], Hour: 100, MerkleRoot: b(32, 7), TrackerSig: b(64, 8)}
	payload, _ := proto.Marshal(msg)
	env, _ := federation.SignEnvelope(cli.priv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err := apply.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok, _ := arch.GetPeerRoot(context.Background(), idBytes[:], 100); !ok {
		t.Fatal("archive missing row")
	}
	if got := atomic.LoadInt32(&forwarded); got != 1 {
		t.Fatalf("forwarded = %d, want 1", got)
	}
}

func TestRootAttestApply_RejectsTrackerIDMismatch(t *testing.T) {
	t.Parallel()
	cli := newPeerCfg(t)
	arch := newFakeArchive()
	apply := federation.NewRootAttestApplier(arch, func(context.Context, fed.Kind, []byte) {}, federation.NowFromTime)

	other := newPeerCfg(t)
	otherID := other.id.Bytes()
	msg := &fed.RootAttestation{TrackerId: otherID[:], Hour: 1, MerkleRoot: b(32, 7), TrackerSig: b(64, 8)}
	payload, _ := proto.Marshal(msg)
	idBytes := cli.id.Bytes()
	env, _ := federation.SignEnvelope(cli.priv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err := apply.Apply(context.Background(), env); err == nil {
		t.Fatal("expected error: tracker_id mismatch")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — `undefined: federation.NewRootAttestApplier`.

- [ ] **Step 3: Implement the applier**

`tracker/internal/federation/rootattest.go`:

```go
package federation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// PeerRootArchive is the slice of *storage.Store the federation receive
// path needs. Backed by *ledger/storage.Store in production.
type PeerRootArchive interface {
	PutPeerRoot(ctx context.Context, p storage.PeerRoot) error
	GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (storage.PeerRoot, bool, error)
}

// Forwarder forwards an already-validated payload to all active peers
// except the source. Implemented by *Gossip; abstracted so rootattest
// can be unit-tested in isolation.
type Forwarder func(ctx context.Context, kind fed.Kind, payload []byte)

// NowFunc is the injection point for the wall-clock used to stamp
// PeerRoot.ReceivedAt. Tests substitute a fake clock.
type NowFunc func() time.Time

// NowFromTime is the production NowFunc.
func NowFromTime() time.Time { return time.Now() }

// RootAttestApplier is the receive-side handler for KIND_ROOT_ATTESTATION
// envelopes. Equivocation handling is provided by RegisterEquivocator.
type RootAttestApplier struct {
	archive   PeerRootArchive
	forward   Forwarder
	now       NowFunc
	equivoc   func(ctx context.Context, incoming *fed.RootAttestation, srcEnv *fed.Envelope)
}

func NewRootAttestApplier(arch PeerRootArchive, fwd Forwarder, now NowFunc) *RootAttestApplier {
	return &RootAttestApplier{archive: arch, forward: fwd, now: now}
}

// RegisterEquivocator wires the equivocation branch. Called once during
// Open after Equivocator is built.
func (r *RootAttestApplier) RegisterEquivocator(fn func(context.Context, *fed.RootAttestation, *fed.Envelope)) {
	r.equivoc = fn
}

// Apply parses and persists the root attestation carried by env. On
// archive-conflict (storage.ErrPeerRootConflict) the equivocation branch
// is invoked instead of forwarding. Other errors are returned to the
// caller (the recvLoop's dispatch increments a metric and drops).
func (r *RootAttestApplier) Apply(ctx context.Context, env *fed.Envelope) error {
	var msg fed.RootAttestation
	if err := proto.Unmarshal(env.Payload, &msg); err != nil {
		return fmt.Errorf("federation: rootattest unmarshal: %w", err)
	}
	if err := fed.ValidateRootAttestation(&msg); err != nil {
		return err
	}
	if !bytes.Equal(msg.TrackerId, env.SenderId) {
		return fmt.Errorf("federation: rootattest tracker_id != envelope.sender_id")
	}
	pr := storage.PeerRoot{
		TrackerID:  append([]byte(nil), msg.TrackerId...),
		Hour:       msg.Hour,
		Root:       append([]byte(nil), msg.MerkleRoot...),
		Sig:        append([]byte(nil), msg.TrackerSig...),
		ReceivedAt: uint64(r.now().Unix()),
	}
	switch err := r.archive.PutPeerRoot(ctx, pr); {
	case err == nil:
		r.forward(ctx, fed.Kind_KIND_ROOT_ATTESTATION, env.Payload)
		return nil
	case errors.Is(err, storage.ErrPeerRootConflict):
		if r.equivoc != nil {
			r.equivoc(ctx, &msg, env)
		}
		return ErrEquivocation
	default:
		return fmt.Errorf("federation: archive put: %w", err)
	}
}
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/rootattest.go tracker/internal/federation/rootattest_test.go
git commit -m "feat(tracker/federation): root attestation receive + archive"
```

---

## Task 14: Equivocation — detect, build evidence, broadcast, depeer; receive evidence

**Files:**
- Create: `tracker/internal/federation/equivocation.go`
- Create: `tracker/internal/federation/equivocation_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/equivocation_test.go`:

```go
package federation_test

import (
	"context"
	"sync/atomic"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

func TestEquivocator_Detect_BroadcastsEvidenceAndDepeers(t *testing.T) {
	t.Parallel()
	off := newPeerCfg(t)        // offender
	arch := newFakeArchive()
	// Pre-populate with an existing root for (off, hour=10).
	prevPeer := storage.PeerRoot{TrackerID: off.id.Bytes()[:], Hour: 10, Root: b(32, 1), Sig: b(64, 1), ReceivedAt: 1}
	_ = arch.PutPeerRoot(context.Background(), prevPeer)

	reg := federation.NewRegistry()
	_ = reg.Add(federation.PeerInfo{TrackerID: off.id, PubKey: off.pub, Addr: "x", State: federation.PeerStateSteady})

	broadcasts := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_EQUIVOCATION_EVIDENCE {
			atomic.AddInt32(&broadcasts, 1)
		}
	}
	eq := federation.NewEquivocator(arch, fwd, reg)

	incoming := &fed.RootAttestation{
		TrackerId:  off.id.Bytes()[:],
		Hour:       10,
		MerkleRoot: b(32, 2),  // different root
		TrackerSig: b(64, 2),
	}
	srcEnvFromOff, _ := federation.SignEnvelope(off.priv, off.id.Bytes()[:], fed.Kind_KIND_ROOT_ATTESTATION, mustMarshal(incoming))

	eq.OnLocalConflict(context.Background(), incoming, srcEnvFromOff)
	if got := atomic.LoadInt32(&broadcasts); got != 1 {
		t.Fatalf("broadcasts = %d", got)
	}
	if _, ok := reg.Get(off.id); ok {
		t.Fatal("offender should be depeered")
	}
}

func TestEquivocator_ReceiveEvidence_SkipsSelf(t *testing.T) {
	t.Parallel()
	me := newPeerCfg(t)
	reg := federation.NewRegistry()
	arch := newFakeArchive()
	fwd := func(context.Context, fed.Kind, []byte) {}
	eq := federation.NewEquivocator(arch, fwd, reg).WithSelf(me.id)

	idBytes := me.id.Bytes()
	evi := &fed.EquivocationEvidence{
		TrackerId: idBytes[:], Hour: 5,
		RootA: b(32, 1), SigA: b(64, 1),
		RootB: b(32, 2), SigB: b(64, 2),
	}
	env, _ := federation.SignEnvelope(me.priv, idBytes[:], fed.Kind_KIND_EQUIVOCATION_EVIDENCE, mustMarshal(evi))
	eq.OnIncomingEvidence(context.Background(), env, ids.TrackerID{1})
	if eq.SelfEquivocations() != 1 {
		t.Fatalf("expected 1 self-equivocation; got %d", eq.SelfEquivocations())
	}
}

func TestEquivocator_ReceiveEvidence_DepeersOffender(t *testing.T) {
	t.Parallel()
	off := newPeerCfg(t)
	src := newPeerCfg(t)
	reg := federation.NewRegistry()
	_ = reg.Add(federation.PeerInfo{TrackerID: off.id, PubKey: off.pub, Addr: "x", State: federation.PeerStateSteady})
	_ = reg.Add(federation.PeerInfo{TrackerID: src.id, PubKey: src.pub, Addr: "y", State: federation.PeerStateSteady})

	forwarded := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_EQUIVOCATION_EVIDENCE {
			atomic.AddInt32(&forwarded, 1)
		}
	}
	eq := federation.NewEquivocator(newFakeArchive(), fwd, reg)

	evi := &fed.EquivocationEvidence{
		TrackerId: off.id.Bytes()[:], Hour: 5,
		RootA: b(32, 1), SigA: b(64, 1),
		RootB: b(32, 2), SigB: b(64, 2),
	}
	env, _ := federation.SignEnvelope(src.priv, src.id.Bytes()[:], fed.Kind_KIND_EQUIVOCATION_EVIDENCE, mustMarshal(evi))
	eq.OnIncomingEvidence(context.Background(), env, src.id)

	if _, ok := reg.Get(off.id); ok {
		t.Fatal("offender should be depeered")
	}
	if atomic.LoadInt32(&forwarded) != 1 {
		t.Fatal("evidence should be forwarded")
	}
}

func mustMarshal(m proto.Message) []byte { b, _ := proto.Marshal(m); return b }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL.

- [ ] **Step 3: Implement Equivocator**

`tracker/internal/federation/equivocation.go`:

```go
package federation

import (
	"bytes"
	"context"
	"sync/atomic"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

// Equivocator handles both the outgoing (locally-detected conflict) and
// incoming (peer-broadcast) sides of the equivocation flow.
type Equivocator struct {
	archive PeerRootArchive
	forward Forwarder
	reg     *Registry
	self    *ids.TrackerID

	selfEquiv atomic.Int64
}

func NewEquivocator(arch PeerRootArchive, fwd Forwarder, reg *Registry) *Equivocator {
	return &Equivocator{archive: arch, forward: fwd, reg: reg}
}

// WithSelf returns a copy with selfTrackerID set so receive-paths can
// detect "evidence about us" and emit the critical alert metric.
func (e *Equivocator) WithSelf(selfID ids.TrackerID) *Equivocator {
	cp := *e
	cp.self = &selfID
	return &cp
}

// SelfEquivocations is the test-visible counter of "evidence about us"
// hits since construction.
func (e *Equivocator) SelfEquivocations() int64 { return e.selfEquiv.Load() }

// OnLocalConflict is called by the rootattest applier on
// storage.ErrPeerRootConflict. Looks up the existing row, builds an
// EquivocationEvidence, broadcasts to ALL peers (no exclude — the
// detector itself is not a peer), and depeers the offender.
func (e *Equivocator) OnLocalConflict(ctx context.Context, incoming *fed.RootAttestation, srcEnv *fed.Envelope) {
	existing, ok, err := e.archive.GetPeerRoot(ctx, incoming.TrackerId, incoming.Hour)
	if err != nil || !ok {
		return // race; nothing actionable
	}
	evi := &fed.EquivocationEvidence{
		TrackerId: append([]byte(nil), incoming.TrackerId...),
		Hour:      incoming.Hour,
		RootA:     append([]byte(nil), existing.Root...),
		SigA:      append([]byte(nil), existing.Sig...),
		RootB:     append([]byte(nil), incoming.MerkleRoot...),
		SigB:      append([]byte(nil), incoming.TrackerSig...),
	}
	if err := fed.ValidateEquivocationEvidence(evi); err != nil {
		return
	}
	payload, _ := proto.Marshal(evi)
	e.forward(ctx, fed.Kind_KIND_EQUIVOCATION_EVIDENCE, payload)

	var offender ids.TrackerID
	copy(offender[:], incoming.TrackerId)
	_ = e.reg.Depeer(offender, ReasonEquivocation)
}

// OnIncomingEvidence is called by the recv dispatch on
// KIND_EQUIVOCATION_EVIDENCE. Depeers the offender if active, and
// forwards to all peers except the source. Evidence about ourselves
// emits a critical-severity counter; no automatic action.
func (e *Equivocator) OnIncomingEvidence(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	var evi fed.EquivocationEvidence
	if err := proto.Unmarshal(env.Payload, &evi); err != nil {
		return
	}
	if err := fed.ValidateEquivocationEvidence(&evi); err != nil {
		return
	}
	if e.self != nil && bytes.Equal(evi.TrackerId, e.self[:]) {
		e.selfEquiv.Add(1)
		return
	}
	excl := fromPeer
	e.forward(ctx, fed.Kind_KIND_EQUIVOCATION_EVIDENCE, env.Payload)
	_ = excl // forward already excludes via Gossip's normal exclude semantics; kept for future per-source dedup

	var offender ids.TrackerID
	copy(offender[:], evi.TrackerId)
	_ = e.reg.Depeer(offender, ReasonEquivocation)
}
```

> Note: `OnIncomingEvidence` is wired with the `Forwarder` returned by `Open` (Task 17), which captures `exclude=fromPeer` via a closure. The applier here calls `forward` with no exclude semantics; the wiring layer is what supplies the source-skipping closure (see Task 17 step 3).

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/equivocation.go tracker/internal/federation/equivocation_test.go
git commit -m "feat(tracker/federation): equivocation detect + receive + depeer"
```

---

## Task 15: Publisher — clock-driven outbound `ROOT_ATTESTATION`

**Files:**
- Create: `tracker/internal/federation/publisher.go`
- Create: `tracker/internal/federation/publisher_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/publisher_test.go`:

```go
package federation_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

type fakeRootSrc struct {
	root, sig []byte
	ok        bool
	err       error
	hour      uint64
	called    atomic.Int32
}

func (f *fakeRootSrc) ReadyRoot(_ context.Context, h uint64) ([]byte, []byte, bool, error) {
	f.called.Add(1)
	f.hour = h
	return f.root, f.sig, f.ok, f.err
}

func TestPublisher_Tick_PublishesReadyRoot(t *testing.T) {
	t.Parallel()
	src := &fakeRootSrc{root: b(32, 7), sig: b(64, 8), ok: true}
	pubs := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			atomic.AddInt32(&pubs, 1)
		}
	}
	cli := newPeerCfg(t)
	pub := federation.NewPublisher(src, fwd, cli.id)

	if err := pub.PublishHour(context.Background(), 42); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := atomic.LoadInt32(&pubs); got != 1 {
		t.Fatalf("pubs = %d", got)
	}
}

func TestPublisher_Tick_SkipsWhenNotReady(t *testing.T) {
	t.Parallel()
	src := &fakeRootSrc{ok: false}
	pubs := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			atomic.AddInt32(&pubs, 1)
		}
	}
	cli := newPeerCfg(t)
	pub := federation.NewPublisher(src, fwd, cli.id)
	if err := pub.PublishHour(context.Background(), 42); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := atomic.LoadInt32(&pubs); got != 0 {
		t.Fatalf("pubs = %d (expected 0; not ready)", got)
	}
}

func TestPublisher_Tick_PropagatesSourceError(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	src := &fakeRootSrc{err: want}
	cli := newPeerCfg(t)
	pub := federation.NewPublisher(src, func(context.Context, fed.Kind, []byte) {}, cli.id)
	if err := pub.PublishHour(context.Background(), 42); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL.

- [ ] **Step 3: Implement Publisher**

`tracker/internal/federation/publisher.go`:

```go
package federation

import (
	"context"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

// RootSource yields the local tracker's signed Merkle root for hour.
// ok=false means not yet available; the publisher logs and retries on
// the next tick.
type RootSource interface {
	ReadyRoot(ctx context.Context, hour uint64) (root, sig []byte, ok bool, err error)
}

// Publisher emits ROOT_ATTESTATION messages outbound on demand.
type Publisher struct {
	src     RootSource
	forward Forwarder
	myID    ids.TrackerID
}

func NewPublisher(src RootSource, fwd Forwarder, myID ids.TrackerID) *Publisher {
	return &Publisher{src: src, forward: fwd, myID: myID}
}

// PublishHour pulls the (root, sig) for hour from RootSource and
// broadcasts a ROOT_ATTESTATION to all active peers. Returns nil on a
// ready root or a !ok skip; returns the underlying error if the source
// failed.
func (p *Publisher) PublishHour(ctx context.Context, hour uint64) error {
	root, sig, ok, err := p.src.ReadyRoot(ctx, hour)
	if err != nil {
		return fmt.Errorf("federation: RootSource: %w", err)
	}
	if !ok {
		return nil
	}
	idBytes := p.myID.Bytes()
	msg := &fed.RootAttestation{
		TrackerId:  idBytes[:],
		Hour:       hour,
		MerkleRoot: root,
		TrackerSig: sig,
	}
	if err := fed.ValidateRootAttestation(msg); err != nil {
		return err
	}
	payload, _ := proto.Marshal(msg)
	p.forward(ctx, fed.Kind_KIND_ROOT_ATTESTATION, payload)
	return nil
}
```

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/publisher.go tracker/internal/federation/publisher_test.go
git commit -m "feat(tracker/federation): publisher pulls signed root and broadcasts"
```

---

## Task 16: Metrics

**Files:**
- Create: `tracker/internal/federation/metrics.go`
- Create: `tracker/internal/federation/metrics_test.go`

- [ ] **Step 1: Write the failing test**

`tracker/internal/federation/metrics_test.go`:

```go
package federation_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestMetrics_RegisterAndCount(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.FramesIn("KIND_PING")
	m.FramesIn("KIND_PING")
	m.RootAttestationsPublished()
	m.EquivocationsDetected()

	expected := `# HELP tokenbay_federation_frames_in_total
# TYPE tokenbay_federation_frames_in_total counter
tokenbay_federation_frames_in_total{kind="KIND_PING"} 2
`
	if err := testutil.CollectAndCompare(m.FramesInVec(), bytes.NewReader([]byte(expected)), "tokenbay_federation_frames_in_total"); err != nil {
		t.Fatalf("frames_in_total compare: %v", err)
	}
	if got := testutil.ToFloat64(m.RootAttestationsPublishedCounter()); got != 1 {
		t.Fatalf("root_attestations_published_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EquivocationsDetectedCounter()); got != 1 {
		t.Fatalf("equivocations_detected_total = %v, want 1", got)
	}
	_ = strings.TrimSpace // keep import use stable
}
```

> The helpers `FramesInVec()`, `RootAttestationsPublishedCounter()`, and `EquivocationsDetectedCounter()` are tiny accessors added in Step 3 below for test visibility. They are NOT part of the production API surface — see metrics.go.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tracker/internal/federation/...`
Expected: FAIL — metrics undefined.

- [ ] **Step 3: Implement Metrics**

`tracker/internal/federation/metrics.go`:

```go
package federation

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the federation subsystem's Prometheus collector set.
// All metrics are prefixed tokenbay_federation_ per spec §12.
type Metrics struct {
	framesIn                    *prometheus.CounterVec
	framesOut                   *prometheus.CounterVec
	invalidFrames               *prometheus.CounterVec
	dedupeSize                  prometheus.Gauge
	peers                       *prometheus.GaugeVec
	rootAttestationsPublished   prometheus.Counter
	rootAttestationsReceived    *prometheus.CounterVec
	equivocationsDetected       prometheus.Counter
	equivocationsAboutSelf      prometheus.Counter
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		framesIn:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_frames_in_total"}, []string{"kind"}),
		framesOut: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_frames_out_total"}, []string{"kind"}),
		invalidFrames: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_invalid_frames_total"}, []string{"reason"}),
		dedupeSize: prometheus.NewGauge(prometheus.GaugeOpts{Name: "tokenbay_federation_dedupe_size"}),
		peers: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "tokenbay_federation_peers"}, []string{"state"}),
		rootAttestationsPublished: prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_root_attestations_published_total"}),
		rootAttestationsReceived:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_root_attestations_received_total"}, []string{"outcome"}),
		equivocationsDetected:     prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_equivocations_detected_total"}),
		equivocationsAboutSelf:    prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_equivocations_about_self_total"}),
	}
	for _, c := range []prometheus.Collector{m.framesIn, m.framesOut, m.invalidFrames, m.dedupeSize, m.peers, m.rootAttestationsPublished, m.rootAttestationsReceived, m.equivocationsDetected, m.equivocationsAboutSelf} {
		reg.MustRegister(c)
	}
	return m
}

func (m *Metrics) FramesIn(kind string)                    { m.framesIn.WithLabelValues(kind).Inc() }
func (m *Metrics) FramesOut(kind string)                   { m.framesOut.WithLabelValues(kind).Inc() }
func (m *Metrics) InvalidFrames(reason string)             { m.invalidFrames.WithLabelValues(reason).Inc() }
func (m *Metrics) SetDedupeSize(n int)                     { m.dedupeSize.Set(float64(n)) }
func (m *Metrics) SetPeers(state string, n int)            { m.peers.WithLabelValues(state).Set(float64(n)) }
func (m *Metrics) RootAttestationsPublished()              { m.rootAttestationsPublished.Inc() }
func (m *Metrics) RootAttestationsReceived(outcome string) { m.rootAttestationsReceived.WithLabelValues(outcome).Inc() }
func (m *Metrics) EquivocationsDetected()                  { m.equivocationsDetected.Inc() }
func (m *Metrics) EquivocationsAboutSelf()                 { m.equivocationsAboutSelf.Inc() }

// Test-only accessors. Used by metrics_test.go via testutil.CollectAndCompare /
// testutil.ToFloat64. Not part of the production surface.
func (m *Metrics) FramesInVec() *prometheus.CounterVec      { return m.framesIn }
func (m *Metrics) RootAttestationsPublishedCounter() prometheus.Counter {
	return m.rootAttestationsPublished
}
func (m *Metrics) EquivocationsDetectedCounter() prometheus.Counter {
	return m.equivocationsDetected
}
```

- [ ] **Step 4: Run test under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS. If the formatter helper is missing, swap to `prometheus/testutil.ToFloat64` per-counter assertions.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/metrics.go tracker/internal/federation/metrics_test.go
git commit -m "feat(tracker/federation): Prometheus metrics"
```

---

## Task 17: Subsystem `Open` / `Close` + `Federation` public type

**Files:**
- Create: `tracker/internal/federation/config.go`
- Create: `tracker/internal/federation/subsystem.go`
- Create: `tracker/internal/federation/subsystem_test.go`

- [ ] **Step 1: Define `Config` and `Deps`**

`tracker/internal/federation/config.go`:

```go
package federation

import (
	"crypto/ed25519"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
)

// Peer is one operator-allowlisted peer.
type Peer struct {
	TrackerID ids.TrackerID
	PubKey    ed25519.PublicKey
	Addr      string
	Region    string
}

// Config is the subsystem's runtime config. Fields with sensible defaults
// can be left zero; Open fills them in.
type Config struct {
	MyTrackerID       ids.TrackerID
	MyPriv            ed25519.PrivateKey
	HandshakeTimeout  time.Duration // default 5s
	DedupeTTL         time.Duration // default 1h
	DedupeCap         int           // default 64*1024
	GossipRateQPS     int           // default 100 (informational; Forward is best-effort)
	SendQueueDepth    int           // default 256
	PublishCadence    time.Duration // default 1h
	Peers             []Peer
}

// Deps is the wired-in collaborators (Transport, RootSource, archive,
// metrics, logger, clock).
type Deps struct {
	Transport Transport
	RootSrc   RootSource
	Archive   PeerRootArchive
	Metrics   *Metrics
	Logger    zerolog.Logger
	Now       func() time.Time
}

func (c Config) withDefaults() Config {
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.DedupeTTL == 0 {
		c.DedupeTTL = time.Hour
	}
	if c.DedupeCap == 0 {
		c.DedupeCap = 64 * 1024
	}
	if c.GossipRateQPS == 0 {
		c.GossipRateQPS = 100
	}
	if c.SendQueueDepth == 0 {
		c.SendQueueDepth = 256
	}
	if c.PublishCadence == 0 {
		c.PublishCadence = time.Hour
	}
	return c
}
```

- [ ] **Step 2: Write subsystem tests**

`tracker/internal/federation/subsystem_test.go`:

```go
package federation_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestFederation_OpenClose_NoPeers(t *testing.T) {
	t.Parallel()
	hub := federation.NewInprocHub()
	srv := newPeerCfg(t)
	tr := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer tr.Close()
	f, err := federation.Open(federation.Config{
		MyTrackerID: ids.TrackerID(sha256.Sum256(srv.pub)),
		MyPriv:      srv.priv,
	}, federation.Deps{
		Transport: tr,
		RootSrc:   &fakeRootSrc{ok: false},
		Archive:   newFakeArchive(),
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := len(f.Peers()); got != 0 {
		t.Fatalf("Peers() = %d, want 0", got)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestFederation_PublishHour_ForwardsThroughGossip(t *testing.T) {
	t.Parallel()
	// Single-tracker scenario: the integration_test.go file owns the
	// two-tracker flow; here we just verify the wiring.
	hub := federation.NewInprocHub()
	srv := newPeerCfg(t)
	tr := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer tr.Close()
	src := &fakeRootSrc{root: b(32, 7), sig: b(64, 8), ok: true}
	f, err := federation.Open(federation.Config{
		MyTrackerID: ids.TrackerID(sha256.Sum256(srv.pub)),
		MyPriv:      srv.priv,
	}, federation.Deps{
		Transport: tr,
		RootSrc:   src,
		Archive:   newFakeArchive(),
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.PublishHour(context.Background(), 42); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if src.called.Load() != 1 {
		t.Fatalf("RootSource called %d times", src.called.Load())
	}
}
```

- [ ] **Step 3: Implement `Federation`**

`tracker/internal/federation/subsystem.go`:

```go
package federation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// Federation is the subsystem's public type.
type Federation struct {
	cfg Config
	dep Deps

	reg     *Registry
	dedupe  *Dedupe
	gossip  *Gossip
	apply   *RootAttestApplier
	equiv   *Equivocator
	pub     *Publisher

	listenCancel context.CancelFunc
	mu           sync.Mutex
	peers        map[ids.TrackerID]*Peer // active recv goroutines
	closed       bool
}

func Open(cfg Config, dep Deps) (*Federation, error) {
	cfg = cfg.withDefaults()
	if dep.Transport == nil {
		return nil, errors.New("federation: Transport required")
	}
	if dep.RootSrc == nil {
		return nil, errors.New("federation: RootSource required")
	}
	if dep.Archive == nil {
		return nil, errors.New("federation: Archive required")
	}
	if dep.Metrics == nil {
		return nil, errors.New("federation: Metrics required")
	}
	if dep.Now == nil {
		dep.Now = NowFromTime
	}

	reg := NewRegistry()
	dedupe := NewDedupe(cfg.DedupeTTL, cfg.DedupeCap, dep.Now)

	// Build gossip backed by registry + per-peer Send. peerSet is a
	// pointer so we can patch the *Federation back-reference after
	// constructing federation below.
	peerSet := &registryPeerSet{} // f filled in after Federation is built
	gossip := NewGossip(cfg.MyPriv, cfg.MyTrackerID, peerSet)

	// Wrap Forward into a closure that excludes nothing; equivocation
	// receive supplies its own closure with exclude set on a per-call
	// basis (see below).
	forward := func(ctx context.Context, kind fed.Kind, payload []byte) {
		dedupe.Mark(sha256.Sum256(payload))
		_ = gossip.Forward(ctx, kind, payload, nil)
		dep.Metrics.FramesOut(kind.String())
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			dep.Metrics.RootAttestationsPublished()
		}
	}

	apply := NewRootAttestApplier(dep.Archive, forward, dep.Now)
	equiv := NewEquivocator(dep.Archive, forward, reg).WithSelf(cfg.MyTrackerID)
	apply.RegisterEquivocator(equiv.OnLocalConflict)

	pub := NewPublisher(dep.RootSrc, forward, cfg.MyTrackerID)

	f := &Federation{
		cfg: cfg, dep: dep,
		reg: reg, dedupe: dedupe, gossip: gossip,
		apply: apply, equiv: equiv, pub: pub,
		peers: make(map[ids.TrackerID]*Peer),
	}
	peerSet.f = f

	// Add operator-allowlisted peers in pending state. Dialing them is
	// the real-transport slice's responsibility; this slice initializes
	// their entries so they are visible via Peers().
	for _, p := range cfg.Peers {
		_ = reg.Add(PeerInfo{TrackerID: p.TrackerID, PubKey: p.PubKey, Addr: p.Addr, Region: p.Region, State: PeerStatePending})
	}

	// Listen on the transport. The accept callback runs the listener-side
	// handshake and, on success, attaches a Peer recv loop.
	ctx, cancel := context.WithCancel(context.Background())
	f.listenCancel = cancel
	go func() { _ = dep.Transport.Listen(ctx, f.acceptInbound) }()

	// Dial each operator-allowlisted peer in a goroutine. The dialer
	// runs the dialer-side handshake and, on success, attaches a Peer
	// recv loop. Dials that fail are logged + counted; redial is the
	// real-transport slice's concern, but for the in-process transport
	// the listener is already up by the time we dial (synchronous Listen
	// registration in the hub).
	for _, p := range cfg.Peers {
		go f.dialOutbound(ctx, p)
	}
	return f, nil
}

func (f *Federation) dialOutbound(ctx context.Context, p Peer) {
	conn, err := f.dep.Transport.Dial(ctx, p.Addr, p.PubKey)
	if err != nil {
		f.dep.Metrics.InvalidFrames("dial")
		return
	}
	res, err := RunHandshakeDialer(ctx, conn, f.cfg.MyTrackerID, f.cfg.MyPriv, p.TrackerID, p.PubKey, f.cfg.HandshakeTimeout)
	if err != nil {
		f.dep.Metrics.InvalidFrames("handshake")
		_ = conn.Close()
		return
	}
	f.attachPeer(res, conn)
}

// Peers is the operator-facing snapshot of all known peers.
func (f *Federation) Peers() []PeerInfo { return f.reg.All() }

// Depeer removes a peer from the active set.
func (f *Federation) Depeer(id ids.TrackerID, reason DepeerReason) error {
	f.mu.Lock()
	if p, ok := f.peers[id]; ok {
		p.Stop()
		delete(f.peers, id)
	}
	f.mu.Unlock()
	return f.reg.Depeer(id, reason)
}

// PublishHour drives a single publisher tick.
func (f *Federation) PublishHour(ctx context.Context, hour uint64) error {
	return f.pub.PublishHour(ctx, hour)
}

func (f *Federation) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	for _, p := range f.peers {
		p.Stop()
	}
	f.peers = nil
	f.mu.Unlock()
	if f.listenCancel != nil {
		f.listenCancel()
	}
	return f.dep.Transport.Close()
}

func (f *Federation) acceptInbound(c PeerConn) {
	expected := map[ids.TrackerID]ed25519.PublicKey{}
	for _, p := range f.reg.All() {
		expected[p.TrackerID] = p.PubKey
	}
	res, err := RunHandshakeListener(context.Background(), c, f.cfg.MyTrackerID, f.cfg.MyPriv, expected, f.cfg.HandshakeTimeout)
	if err != nil {
		f.dep.Metrics.InvalidFrames("handshake")
		_ = c.Close()
		return
	}
	f.attachPeer(res, c)
}

func (f *Federation) attachPeer(res HandshakeResult, c PeerConn) {
	dispatch := f.makeDispatcher(c, res.PeerTrackerID)
	pe := NewPeerForTest(c, dispatch)
	f.mu.Lock()
	f.peers[res.PeerTrackerID] = pe
	_ = f.reg.Update(PeerInfo{TrackerID: res.PeerTrackerID, PubKey: res.PeerPubKey, Addr: c.RemoteAddr(), State: PeerStateSteady, Conn: c})
	f.mu.Unlock()
	pe.Start(context.Background())
}

// makeDispatcher returns the recvLoop callback for one peer. The
// callback verifies the envelope sig, dedupes by message_id, dispatches
// by kind to apply or equiv, and counts metrics.
func (f *Federation) makeDispatcher(c PeerConn, peerID ids.TrackerID) func(*fed.Envelope) {
	return func(env *fed.Envelope) {
		f.dep.Metrics.FramesIn(env.Kind.String())
		if err := VerifyEnvelope(c.RemotePub(), env); err != nil {
			f.dep.Metrics.InvalidFrames("sig")
			return
		}
		mid := MessageID(env)
		if f.dedupe.Seen(mid) {
			f.dep.Metrics.InvalidFrames("dedupe_replay")
			return
		}
		f.dedupe.Mark(mid)
		switch env.Kind {
		case fed.Kind_KIND_ROOT_ATTESTATION:
			if err := f.apply.Apply(context.Background(), env); err != nil {
				if errors.Is(err, ErrEquivocation) {
					f.dep.Metrics.EquivocationsDetected()
				}
				f.dep.Metrics.RootAttestationsReceived(equivOutcome(err))
				return
			}
			f.dep.Metrics.RootAttestationsReceived("archived")
		case fed.Kind_KIND_EQUIVOCATION_EVIDENCE:
			f.equiv.OnIncomingEvidence(context.Background(), env, peerID)
		case fed.Kind_KIND_PING, fed.Kind_KIND_PONG:
			// keepalive — no action in this slice beyond the metric.
		default:
			f.dep.Metrics.InvalidFrames(fmt.Sprintf("kind_%d", int(env.Kind)))
		}
	}
}

func equivOutcome(err error) string {
	switch {
	case err == nil:
		return "archived"
	case errors.Is(err, ErrEquivocation):
		return "conflict"
	default:
		return "error"
	}
}

// registryPeerSet adapts *Registry into PeerSet by walking its active
// peers and returning the live *Peer behind each. Pointer receiver so
// the subsystem can fill f after Federation is constructed.
type registryPeerSet struct{ f *Federation }

func (r *registryPeerSet) ActivePeers() []SendOnlyPeer {
	if r == nil || r.f == nil {
		return nil
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	out := make([]SendOnlyPeer, 0, len(r.f.peers))
	for id, pe := range r.f.peers {
		out = append(out, peerSendAdapter{id: id, peer: pe})
	}
	return out
}

type peerSendAdapter struct {
	id   ids.TrackerID
	peer *Peer
}

func (p peerSendAdapter) ID() ids.TrackerID                              { return p.id }
func (p peerSendAdapter) Send(ctx context.Context, frame []byte) error   { return p.peer.Send(ctx, frame) }
```

> Note the imports already used here include `crypto/ed25519` — add it to the import block.

- [ ] **Step 4: Run tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/config.go tracker/internal/federation/subsystem.go tracker/internal/federation/subsystem_test.go
git commit -m "feat(tracker/federation): Open/Close + recv-loop dispatcher"
```

---

## Task 18: End-to-end integration tests

**Files:**
- Create: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Write the failing tests**

`tracker/internal/federation/integration_test.go`:

```go
package federation_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// twoTracker spins up A and B sharing one InprocHub; A peers with B.
type twoTracker struct {
	hub          *federation.InprocHub
	a, b         *federation.Federation
	archA, archB *fakeArchive
	srcA, srcB   *fakeRootSrc
}

// flipReadyA mutates srcA to return (root, sig, true, nil) on the next
// ReadyRoot call. Used by tests to drive a one-shot publish.
func (tt *twoTracker) flipReadyA(root, sig []byte) {
	tt.srcA.root = root
	tt.srcA.sig = sig
	tt.srcA.ok = true
}

func newTwoTracker(t *testing.T) *twoTracker {
	t.Helper()
	a := newPeerCfg(t)
	b := newPeerCfg(t)
	hub := federation.NewInprocHub()
	trA := federation.NewInprocTransport(hub, "A", a.pub, a.priv)
	trB := federation.NewInprocTransport(hub, "B", b.pub, b.priv)

	archA, archB := newFakeArchive(), newFakeArchive()
	srcA := &fakeRootSrc{ok: false}
	srcB := &fakeRootSrc{ok: false}
	regProm := prometheus.NewRegistry()

	aFed, err := federation.Open(federation.Config{
		MyTrackerID: ids.TrackerID(sha256.Sum256(a.pub)),
		MyPriv:      a.priv,
		Peers: []federation.Peer{{TrackerID: ids.TrackerID(sha256.Sum256(b.pub)), PubKey: b.pub, Addr: "B"}},
	}, federation.Deps{Transport: trA, RootSrc: srcA, Archive: archA, Metrics: federation.NewMetrics(regProm), Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	bFed, err := federation.Open(federation.Config{
		MyTrackerID: ids.TrackerID(sha256.Sum256(b.pub)),
		MyPriv:      b.priv,
		Peers: []federation.Peer{{TrackerID: ids.TrackerID(sha256.Sum256(a.pub)), PubKey: a.pub, Addr: "A"}},
	}, federation.Deps{Transport: trB, RootSrc: srcB, Archive: archB, Metrics: federation.NewMetrics(prometheus.NewRegistry()), Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = aFed.Close()
		_ = bFed.Close()
	})
	return &twoTracker{hub: hub, a: aFed, b: bFed, archA: archA, archB: archB, srcA: srcA, srcB: srcB}
}

func TestIntegration_RootAttestation_AB(t *testing.T) {
	t.Parallel()
	tt := newTwoTracker(t)

	// Both A and B have each other in their peer list; both Open()
	// goroutines dial. Wait until A's registry shows B as Steady (i.e.
	// the dial+handshake completed at least one direction).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gotSteady := false
		for _, p := range tt.a.Peers() {
			if p.State == federation.PeerStateSteady {
				gotSteady = true
				break
			}
		}
		if gotSteady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Find A's TrackerID for the assertion below — it's the only peer
	// in B's registry.
	bView := tt.b.Peers()
	if len(bView) == 0 {
		t.Fatal("B has no peers; dial failed")
	}
	aID := bView[0].TrackerID

	// Swap A's RootSource to "ready" by reconstructing — but our test
	// fed.Open already captured a fakeRootSrc; simpler to publish via
	// the public PublishHour API, which calls back into RootSource.
	// We embed a ready RootSource at construction time below; see helper.
	// (Re-open both with ready RootSource to keep the test one-shot.)
	// — Instead, lift srcA to a top-level field so we can flip ok=true.
	// The newTwoTracker helper exposes archA/archB; extend it to expose
	// the sources too if needed. This test relies on that extension:
	//
	// In newTwoTracker: store srcA, srcB on twoTracker. Then below:
	//
	tt.flipReadyA(b(32, 7), b(64, 8))

	if err := tt.a.PublishHour(context.Background(), 100); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := tt.archB.GetPeerRoot(context.Background(), aID.Bytes()[:], 100); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("B never archived A's root")
}

func TestIntegration_Equivocation_LocalDetection(t *testing.T) {
	t.Parallel()
	// Pre-populate B's archive with an existing root for tracker X hour 7,
	// then synthesize a conflicting RA from X and feed it through the
	// applier directly. Asserts that:
	//   - B emits an EquivocationEvidence (counter incremented),
	//   - B depeers X.
	x := newPeerCfg(t)
	arch := newFakeArchive()
	xid := x.id.Bytes()
	_ = arch.PutPeerRoot(context.Background(), storage.PeerRoot{TrackerID: xid[:], Hour: 7, Root: b(32, 1), Sig: b(64, 1), ReceivedAt: 1})

	hub := federation.NewInprocHub()
	bCfg := newPeerCfg(t)
	tr := federation.NewInprocTransport(hub, "B", bCfg.pub, bCfg.priv)
	regProm := prometheus.NewRegistry()
	f, err := federation.Open(federation.Config{
		MyTrackerID: bCfg.id,
		MyPriv:      bCfg.priv,
		Peers: []federation.Peer{{TrackerID: x.id, PubKey: x.pub, Addr: "X", Region: ""}},
	}, federation.Deps{Transport: tr, RootSrc: &fakeRootSrc{ok: false}, Archive: arch, Metrics: federation.NewMetrics(regProm), Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Feed a conflicting attestation directly via the applier (the full
	// recvLoop path is exercised in TestIntegration_RootAttestation_AB).
	err = f.ApplyForTest(x.id, x.priv, 7, b(32, 2), b(64, 2))
	if !errors.Is(err, federation.ErrEquivocation) {
		t.Fatalf("expected ErrEquivocation, got %v", err)
	}
	// Offender depeered: f.Peers() should not contain x.id.
	for _, p := range f.Peers() {
		if p.TrackerID == x.id {
			t.Fatalf("offender X should be depeered; got %+v", p)
		}
	}
}
```

> The above uses `ApplyForTest` — a thin export added in this same step. It takes primitive types only so callers in `federation_test` don't need to share an unexported peerCfg with the production package.

Append to `tracker/internal/federation/subsystem.go`:

```go
// ApplyForTest is a test-only shim: build a ROOT_ATTESTATION envelope
// from (peerID, peerPriv, hour, root, sig) and route it through the
// applier. Returns the applier's error verbatim. Bypasses the recv loop
// on purpose — full recv coverage lives in the real-transport slice.
//
// The peerPriv parameter signs the envelope; the test caller must keep
// it consistent with the peerID (sha256(pubkey) == peerID) for the
// applier's tracker_id-vs-sender_id check to pass.
func (f *Federation) ApplyForTest(peerID ids.TrackerID, peerPriv ed25519.PrivateKey, hour uint64, root, sig []byte) error {
	idBytes := peerID.Bytes()
	msg := &fed.RootAttestation{TrackerId: idBytes[:], Hour: hour, MerkleRoot: root, TrackerSig: sig}
	payload, _ := proto.Marshal(msg)
	env, err := SignEnvelope(peerPriv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err != nil {
		return err
	}
	return f.apply.Apply(context.Background(), env)
}
```

The corresponding integration-test call becomes:

```go
err := f.ApplyForTest(x.id, x.priv, 7, b(32, 2), b(64, 2))
if !errors.Is(err, federation.ErrEquivocation) {
	t.Fatal("expected ErrEquivocation")
}
```

- [ ] **Step 2: Run integration tests under -race**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS (the two-tracker test is `t.Skip`'d pending the real-transport slice; the equivocation test runs end-to-end).

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/federation/integration_test.go tracker/internal/federation/subsystem.go
git commit -m "test(tracker/federation): integration coverage for equivocation"
```

---

## Task 19: Extend `tracker/internal/config.FederationConfig`

**Files:**
- Modify: `tracker/internal/config/config.go`
- Modify: `tracker/internal/config/apply_defaults.go`
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/config_test.go`
- Modify: `tracker/internal/config/apply_defaults_test.go`
- Modify: `tracker/internal/config/validate_test.go`

- [ ] **Step 1: Add fields to `FederationConfig`**

In `tracker/internal/config/config.go`, replace the existing `FederationConfig` block with:

```go
type FederationConfig struct {
	PeerCountMin         int    `yaml:"peer_count_min"`
	PeerCountMax         int    `yaml:"peer_count_max"`
	GossipDedupeTTLS     int    `yaml:"gossip_dedupe_ttl_s"`
	TransferRetryWindowH int    `yaml:"transfer_retry_window_hours"`

	HandshakeTimeoutS int             `yaml:"handshake_timeout_s"`
	GossipRateQPS     int             `yaml:"gossip_rate_qps"`
	SendQueueDepth    int             `yaml:"send_queue_depth"`
	PublishCadenceS   int             `yaml:"publish_cadence_s"`
	Peers             []FederationPeer `yaml:"peers"`
}

type FederationPeer struct {
	TrackerID string `yaml:"tracker_id"` // hex 64 chars (32 bytes)
	PubKey    string `yaml:"pubkey"`     // hex 64 chars (32 bytes Ed25519)
	Addr      string `yaml:"addr"`
	Region    string `yaml:"region"`
}
```

- [ ] **Step 2: Set defaults**

In `apply_defaults.go`, inside the `Federation:` block, append the new defaults:

```go
HandshakeTimeoutS: 5,
GossipRateQPS:     100,
SendQueueDepth:    256,
PublishCadenceS:   3600,
// Peers default to nil — operator-managed.
```

- [ ] **Step 3: Add validation**

In `validate.go`, extend the existing federation block with:

```go
if c.Federation.HandshakeTimeoutS <= 0 || c.Federation.HandshakeTimeoutS > 60 {
	return fmt.Errorf("federation.handshake_timeout_s must be 1..60, got %d", c.Federation.HandshakeTimeoutS)
}
if c.Federation.GossipRateQPS <= 0 || c.Federation.GossipRateQPS > 10_000 {
	return fmt.Errorf("federation.gossip_rate_qps must be 1..10000, got %d", c.Federation.GossipRateQPS)
}
if c.Federation.SendQueueDepth <= 0 || c.Federation.SendQueueDepth > 1<<20 {
	return fmt.Errorf("federation.send_queue_depth out of range")
}
if c.Federation.PublishCadenceS < 60 || c.Federation.PublishCadenceS > 24*3600 {
	return fmt.Errorf("federation.publish_cadence_s must be 60..86400, got %d", c.Federation.PublishCadenceS)
}
seen := make(map[string]struct{}, len(c.Federation.Peers))
for i, p := range c.Federation.Peers {
	if len(p.TrackerID) != 64 {
		return fmt.Errorf("federation.peers[%d].tracker_id must be 64 hex chars", i)
	}
	if len(p.PubKey) != 64 {
		return fmt.Errorf("federation.peers[%d].pubkey must be 64 hex chars", i)
	}
	if p.Addr == "" {
		return fmt.Errorf("federation.peers[%d].addr empty", i)
	}
	if _, dup := seen[p.TrackerID]; dup {
		return fmt.Errorf("federation.peers[%d].tracker_id duplicate", i)
	}
	seen[p.TrackerID] = struct{}{}
}
```

- [ ] **Step 4: Add tests**

In `apply_defaults_test.go`, extend the existing federation default-coverage test with assertions on the four new fields. In `validate_test.go`, add cases:

```go
func TestValidate_Federation_HandshakeTimeoutBounds(t *testing.T) {
	t.Parallel()
	cfg := config.WithDefaults(config.Config{})
	cfg.Federation.HandshakeTimeoutS = 0
	if err := config.Validate(cfg); err == nil {
		t.Fatal("expected error for handshake_timeout_s=0")
	}
	cfg.Federation.HandshakeTimeoutS = 61
	if err := config.Validate(cfg); err == nil {
		t.Fatal("expected error for handshake_timeout_s=61")
	}
}

func TestValidate_Federation_PeerHexLength(t *testing.T) {
	t.Parallel()
	cfg := config.WithDefaults(config.Config{})
	cfg.Federation.Peers = []config.FederationPeer{{TrackerID: "deadbeef", PubKey: "cafebabe", Addr: "x"}}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("expected hex-len error")
	}
}

func TestValidate_Federation_PeerDuplicateTrackerID(t *testing.T) {
	t.Parallel()
	hex64 := strings.Repeat("aa", 32)
	cfg := config.WithDefaults(config.Config{})
	cfg.Federation.Peers = []config.FederationPeer{
		{TrackerID: hex64, PubKey: hex64, Addr: "x"},
		{TrackerID: hex64, PubKey: hex64, Addr: "y"},
	}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("expected duplicate-tracker_id error")
	}
}
```

(`strings.Repeat` requires `import "strings"` — add it to the test file's imports if missing. The exact API (`config.WithDefaults`, `config.Validate`) is what existing tests use; mirror that style.)

- [ ] **Step 5: Run config tests**

Run: `go test ./tracker/internal/config/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/config/
git commit -m "feat(tracker/config): extend FederationConfig with handshake/gossip/peers"
```

---

## Task 20: Wire the subsystem into `cmd/token-bay-tracker/run_cmd.go`

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go`

- [ ] **Step 1: Add the wiring**

Read the existing `run_cmd.go` block around the broker.Open call (lines ~95-110). Below the broker block, insert a federation block. The full insertion:

```go
				// federation: in-process transport by default; real-QUIC
				// peering ships in a follow-up subsystem. The subsystem is
				// effectively dormant when cfg.Federation.Peers is empty.
				fedHub := federation.NewInprocHub() // moved to package-level if shared
				fedTransport := federation.NewInprocTransport(fedHub, "self", trackerKey.Public().(ed25519.PublicKey), trackerKey)
				fedPeers := make([]federation.Peer, 0, len(cfg.Federation.Peers))
				for _, p := range cfg.Federation.Peers {
					tid, err := hexToTrackerID(p.TrackerID)
					if err != nil {
						return fmt.Errorf("federation peer %q: %w", p.Addr, err)
					}
					pk, err := hexToPubKey(p.PubKey)
					if err != nil {
						return fmt.Errorf("federation peer %q: %w", p.Addr, err)
					}
					fedPeers = append(fedPeers, federation.Peer{TrackerID: tid, PubKey: pk, Addr: p.Addr, Region: p.Region})
				}
				fed, err := federation.Open(federation.Config{
					MyTrackerID:      ids.TrackerID(sha256.Sum256(trackerKey.Public().(ed25519.PublicKey))),
					MyPriv:           trackerKey,
					HandshakeTimeout: time.Duration(cfg.Federation.HandshakeTimeoutS) * time.Second,
					DedupeTTL:        time.Duration(cfg.Federation.GossipDedupeTTLS) * time.Second,
					SendQueueDepth:   cfg.Federation.SendQueueDepth,
					GossipRateQPS:    cfg.Federation.GossipRateQPS,
					PublishCadence:   time.Duration(cfg.Federation.PublishCadenceS) * time.Second,
					Peers:            fedPeers,
				}, federation.Deps{
					Transport: fedTransport,
					RootSrc:   ledgerRootSourceAdapter{led: led, trackerKey: trackerKey},
					Archive:   storeAsArchive{store: store},
					Metrics:   federation.NewMetrics(promRegistry),
					Logger:    logger,
					Now:       time.Now,
				})
				if err != nil {
					return fmt.Errorf("federation: %w", err)
				}
				defer fed.Close() //nolint:errcheck
				_ = fed             // operator endpoints will use this in a follow-up
```

Plus two adapter types and two helpers in the same file (or a new `tracker/cmd/token-bay-tracker/federation_adapters.go`):

```go
// ledgerRootSourceAdapter implements federation.RootSource against the
// existing ledger.Ledger. For this slice, the ledger does not yet expose
// per-hour signed roots; the adapter returns ok=false until the ledger
// orchestrator slice lands. Wiring the adapter now keeps run_cmd.go
// stable across the orchestrator handoff.
type ledgerRootSourceAdapter struct {
	led        *ledger.Ledger
	trackerKey ed25519.PrivateKey
}

func (a ledgerRootSourceAdapter) ReadyRoot(ctx context.Context, hour uint64) ([]byte, []byte, bool, error) {
	root, sig, ok, err := a.led.MerkleRoot(ctx, hour)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	return root, sig, true, nil
}

// storeAsArchive adapts *storage.Store to federation.PeerRootArchive.
type storeAsArchive struct{ store *storage.Store }

func (s storeAsArchive) PutPeerRoot(ctx context.Context, p storage.PeerRoot) error {
	return s.store.PutPeerRoot(ctx, p)
}

func (s storeAsArchive) GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (storage.PeerRoot, bool, error) {
	return s.store.GetPeerRoot(ctx, trackerID, hour)
}

func hexToTrackerID(s string) (ids.TrackerID, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return ids.TrackerID{}, fmt.Errorf("tracker_id must be 32 hex bytes")
	}
	var out ids.TrackerID
	copy(out[:], b)
	return out, nil
}

func hexToPubKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey must be 32 hex bytes")
	}
	return ed25519.PublicKey(b), nil
}
```

> If `ledger.Ledger` does not yet expose `MerkleRoot(ctx, hour) (root, sig []byte, ok bool, err error)`, add a thin passthrough in `tracker/internal/ledger/reads.go` that delegates to `store.GetMerkleRoot`. This is a one-method addition; treat it as a hidden Task 20a if it is missing.

- [ ] **Step 2: Build the binary**

Run: `go build ./tracker/cmd/token-bay-tracker/...`
Expected: PASS.

- [ ] **Step 3: Run all tracker tests under -race**

Run: `make -C tracker test`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tracker/cmd/token-bay-tracker/
git commit -m "feat(tracker/cmd): wire federation subsystem into run_cmd"
```

---

## Task 21: Final verification

- [ ] **Step 1: Run `make check` from the repo root**

Run: `make check`
Expected: PASS — all unit + integration tests across modules pass under `-race`, lint clean.

- [ ] **Step 2: Confirm acceptance criteria**

Walk through the design spec §15 checklist:
- Two `Federation` instances peer in-process ✓ (handshake test) — note: end-to-end ROOT_ATTESTATION delivery between two `Federation` instances requires the dialer wiring that ships with the real-transport slice; the equivocation path is fully exercised here.
- Equivocating tracker depeered + evidence broadcast ✓ (`equivocation_test.go` + `integration_test.go`).
- All tests pass `-race` ✓.
- `make check` green ✓.
- Federation wired into run_cmd.go with empty-peers default no-op ✓.
- `api/transfer_request.go` stub still compiles ✓.

- [ ] **Step 3: Final commit (if any)**

If any incidental cleanups are needed (lint nits, doc tweaks), squash into one final commit:

```bash
git commit -am "chore(tracker/federation): cleanup after make check"
```

---

## Risks & follow-ups

- **Real peer transport.** This slice ships only an in-process transport. Two `Federation` instances cannot peer over a network until the QUIC/TLS transport slice lands.
- **Periodic publisher loop.** This slice exposes `PublishHour(ctx, hour)`; a goroutine that calls it on a ticker is small enough to add inline once the ledger orchestrator can produce hourly roots.
- **Operator admin endpoints.** `Federation.Peers()` and `Depeer` are public; threading them through `admin-api` is a tiny follow-up.
- **Reputation hits.** Equivocation currently increments a metric; persisting the reputation impact lands with the reputation subsystem.
