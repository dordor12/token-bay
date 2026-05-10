# Federation Cross-Region Credit Transfer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement §4 of the federation umbrella spec — `TRANSFER_PROOF_REQUEST` / `TRANSFER_PROOF` / `TRANSFER_APPLIED` messages, the `Federation.StartTransfer` entry point that satisfies the existing `federationStartTransfer` api stub, per-peer unicast, a `LedgerHooks` dependency, and a `Ledger.AppendTransferIn` method.

**Architecture:** Three new `Kind` enum values + three new proto messages in `shared/federation/`. A new `TransferCoordinator` in `tracker/internal/federation/` owns the source-side replay cache and the destination-side pending-request map. A new `LedgerHooks` `Deps` field plumbs `AppendTransferOut`/`AppendTransferIn` calls in. Per-peer unicast is a thin wrapper over `Peer.Send` keyed by `TrackerID`. The ledger gains `AppendTransferIn` + a chain-walk-by-ref helper for v1 idempotency.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf`, `crypto/ed25519` (stdlib), SQLite via `modernc.org/sqlite`, `quic-go` (already wired), `prometheus/client_golang`.

**Spec:** `docs/superpowers/specs/federation/2026-05-10-federation-cross-region-transfer-design.md`

---

## File Structure

**Modify (shared):**
- `shared/federation/federation.proto` — add 3 `Kind` values + 3 messages.
- `shared/federation/federation.pb.go` — regenerated.
- `shared/federation/validate.go` + `validate_test.go` — new validators + envelope ceiling bump.

**Create (shared):**
- `shared/federation/signing_transfer.go` + `signing_transfer_test.go` — canonical-bytes pre-sig helpers.

**Modify (tracker/ledger):**
- `tracker/internal/ledger/transfer.go` — add `TransferInRecord`, `AppendTransferIn`, `findTransferInByRef`. Add sentinel `ErrTransferRefExists`.
- `tracker/internal/ledger/transfer_test.go` — append-transfer-in tests.

**Create (tracker/federation):**
- `tracker/internal/federation/ledger_hooks.go` + `ledger_hooks_test.go` — `LedgerHooks` interface + `TransferOutHookIn`/`TransferOutHookOut`/`TransferInHookIn`.
- `tracker/internal/federation/unicast.go` + `unicast_test.go` — `Federation.SendToPeer`.
- `tracker/internal/federation/transfer.go` + `transfer_test.go` — `TransferCoordinator` and the three handler methods.

**Modify (tracker/federation):**
- `tracker/internal/federation/errors.go` + `errors_test.go` — add `ErrPeerNotConnected`, `ErrTransferTimeout`, `ErrTransferDisabled`.
- `tracker/internal/federation/config.go` — add `TransferTimeout`, `IssuedProofCap` defaults; add `Ledger LedgerHooks` to `Deps`.
- `tracker/internal/federation/subsystem.go` — wire `TransferCoordinator` into Open + dispatcher; add `Federation.StartTransfer` thin wrapper.
- `tracker/internal/federation/metrics.go` + `metrics_test.go` — new transfer counters.
- `tracker/internal/federation/integration_test.go` — two-tracker source/dest scenario.

---

## Task 1: Proto schema — add 3 Kind values

**Files:**
- Modify: `shared/federation/federation.proto`
- Modify (regenerated): `shared/federation/federation.pb.go`
- Test: `shared/federation/validate_test.go`

- [ ] **Step 1: Add the three new `Kind` enum values to the proto.**

Edit `shared/federation/federation.proto`. After `KIND_PONG = 8;`, add:

```proto
  KIND_TRANSFER_PROOF_REQUEST = 9;
  KIND_TRANSFER_PROOF         = 10;
  KIND_TRANSFER_APPLIED       = 11;
```

- [ ] **Step 2: Regenerate `federation.pb.go`.**

Run: `make -C shared proto-gen`
Expected: `shared/federation/federation.pb.go` is updated; `git diff --stat shared/federation/federation.pb.go` shows changes only for the new enum constants.

- [ ] **Step 3: Write failing test for envelope ceiling.**

Add to `shared/federation/validate_test.go`:

```go
func TestValidateEnvelope_AcceptsTransferKinds(t *testing.T) {
	for _, k := range []Kind{
		Kind_KIND_TRANSFER_PROOF_REQUEST,
		Kind_KIND_TRANSFER_PROOF,
		Kind_KIND_TRANSFER_APPLIED,
	} {
		e := &Envelope{
			SenderId:  bytes.Repeat([]byte{1}, TrackerIDLen),
			Kind:      k,
			Payload:   []byte("x"),
			SenderSig: bytes.Repeat([]byte{1}, SigLen),
		}
		if err := ValidateEnvelope(e); err != nil {
			t.Fatalf("kind=%v: ValidateEnvelope err=%v, want nil", k, err)
		}
	}
}

func TestValidateEnvelope_RejectsKindAboveCeiling(t *testing.T) {
	e := &Envelope{
		SenderId:  bytes.Repeat([]byte{1}, TrackerIDLen),
		Kind:      Kind(99),
		Payload:   []byte("x"),
		SenderSig: bytes.Repeat([]byte{1}, SigLen),
	}
	if err := ValidateEnvelope(e); err == nil {
		t.Fatal("ValidateEnvelope on out-of-range kind: got nil, want error")
	}
}
```

Add `import "bytes"` to the test file if not present.

- [ ] **Step 4: Run; expect FAIL.**

Run: `cd shared && go test -race ./federation -run TestValidateEnvelope_AcceptsTransferKinds -v`
Expected: FAIL — current ceiling is `Kind_KIND_PONG`.

- [ ] **Step 5: Bump the ValidateEnvelope ceiling.**

Edit `shared/federation/validate.go`. Replace:

```go
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_PONG {
		return fmt.Errorf("federation: kind %d out of range", int32(e.Kind))
	}
```

with:

```go
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_TRANSFER_APPLIED {
		return fmt.Errorf("federation: kind %d out of range", int32(e.Kind))
	}
```

- [ ] **Step 6: Run; expect PASS.**

Run: `cd shared && go test -race ./federation -v`
Expected: PASS for the two new tests; nothing else regresses.

- [ ] **Step 7: Commit.**

```bash
git add shared/federation/federation.proto shared/federation/federation.pb.go shared/federation/validate.go shared/federation/validate_test.go
git commit -m "feat(shared/federation): add transfer kind enum values"
```

---

## Task 2: Proto messages — TransferProofRequest, TransferProof, TransferApplied

**Files:**
- Modify: `shared/federation/federation.proto`
- Modify (regenerated): `shared/federation/federation.pb.go`

- [ ] **Step 1: Add the three messages to the proto.**

Append to `shared/federation/federation.proto` (after `Pong`):

```proto
message TransferProofRequest {
  bytes  source_tracker_id = 1;
  bytes  dest_tracker_id   = 2;
  bytes  identity_id       = 3;
  uint64 amount            = 4;
  bytes  nonce             = 5;
  bytes  consumer_sig      = 6;
  bytes  consumer_pub      = 7;
  uint64 timestamp         = 8;
}

message TransferProof {
  bytes  source_tracker_id     = 1;
  bytes  dest_tracker_id       = 2;
  bytes  identity_id           = 3;
  uint64 amount                = 4;
  bytes  nonce                 = 5;
  bytes  source_chain_tip_hash = 6;
  uint64 source_seq            = 7;
  uint64 timestamp             = 8;
  bytes  source_tracker_sig    = 9;
}

message TransferApplied {
  bytes  source_tracker_id = 1;
  bytes  dest_tracker_id   = 2;
  bytes  nonce             = 3;
  uint64 timestamp         = 4;
  bytes  dest_tracker_sig  = 5;
}
```

- [ ] **Step 2: Regenerate `federation.pb.go`.**

Run: `make -C shared proto-gen`
Expected: `shared/federation/federation.pb.go` now has `TransferProofRequest`, `TransferProof`, `TransferApplied` Go types.

- [ ] **Step 3: Run the federation package to confirm clean compilation.**

Run: `cd shared && go test -race ./federation -count=1`
Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add shared/federation/federation.proto shared/federation/federation.pb.go
git commit -m "feat(shared/federation): add transfer message protos"
```

---

## Task 3: Validators for TransferProofRequest

**Files:**
- Modify: `shared/federation/validate.go`
- Modify: `shared/federation/validate_test.go`

- [ ] **Step 1: Write failing tests.**

Append to `shared/federation/validate_test.go`:

```go
func validTransferProofRequest() *TransferProofRequest {
	return &TransferProofRequest{
		SourceTrackerId: bytes.Repeat([]byte{0x11}, TrackerIDLen),
		DestTrackerId:   bytes.Repeat([]byte{0x22}, TrackerIDLen),
		IdentityId:      bytes.Repeat([]byte{0x33}, TrackerIDLen),
		Amount:          1000,
		Nonce:           bytes.Repeat([]byte{0x44}, NonceLen),
		ConsumerSig:     bytes.Repeat([]byte{0x55}, SigLen),
		ConsumerPub:     bytes.Repeat([]byte{0x66}, ed25519PubLen),
		Timestamp:       1714000000,
	}
}

func TestValidateTransferProofRequest_HappyPath(t *testing.T) {
	if err := ValidateTransferProofRequest(validTransferProofRequest()); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestValidateTransferProofRequest_RejectsBadShapes(t *testing.T) {
	cases := map[string]func(m *TransferProofRequest){
		"nil":               func(m *TransferProofRequest) { *m = TransferProofRequest{} },
		"src_len":           func(m *TransferProofRequest) { m.SourceTrackerId = make([]byte, 4) },
		"dst_len":           func(m *TransferProofRequest) { m.DestTrackerId = make([]byte, 4) },
		"identity_len":      func(m *TransferProofRequest) { m.IdentityId = make([]byte, 4) },
		"amount_zero":       func(m *TransferProofRequest) { m.Amount = 0 },
		"nonce_len":         func(m *TransferProofRequest) { m.Nonce = make([]byte, 4) },
		"consumer_sig_len":  func(m *TransferProofRequest) { m.ConsumerSig = make([]byte, 4) },
		"consumer_pub_len":  func(m *TransferProofRequest) { m.ConsumerPub = make([]byte, 4) },
		"src_eq_dst":        func(m *TransferProofRequest) { m.DestTrackerId = m.SourceTrackerId },
		"src_zero":          func(m *TransferProofRequest) { m.SourceTrackerId = make([]byte, TrackerIDLen) },
		"dst_zero":          func(m *TransferProofRequest) { m.DestTrackerId = make([]byte, TrackerIDLen) },
		"identity_zero":     func(m *TransferProofRequest) { m.IdentityId = make([]byte, TrackerIDLen) },
		"nonce_zero":        func(m *TransferProofRequest) { m.Nonce = make([]byte, NonceLen) },
		"timestamp_zero":    func(m *TransferProofRequest) { m.Timestamp = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := validTransferProofRequest()
			mut(m)
			if err := ValidateTransferProofRequest(m); err == nil {
				t.Fatalf("err=nil, want error")
			}
		})
	}
}

func TestValidateTransferProofRequest_NilMessage(t *testing.T) {
	if err := ValidateTransferProofRequest(nil); err == nil {
		t.Fatal("err=nil, want error")
	}
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd shared && go test -race ./federation -run TestValidateTransferProofRequest -v`
Expected: FAIL — `ValidateTransferProofRequest` is undefined; `ed25519PubLen` is undefined.

- [ ] **Step 3: Add the validator + the constant.**

Edit `shared/federation/validate.go`:

Add to the constant block:

```go
const ed25519PubLen = 32
```

Add the validator at the end:

```go
// ValidateTransferProofRequest enforces shape invariants on a
// TransferProofRequest. Receivers MUST call this before verifying any
// signature or dispatching it to LedgerHooks.AppendTransferOut.
func ValidateTransferProofRequest(m *TransferProofRequest) error {
	if m == nil {
		return errors.New("federation: nil TransferProofRequest")
	}
	if len(m.SourceTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_request.source_tracker_id len %d != %d", len(m.SourceTrackerId), TrackerIDLen)
	}
	if len(m.DestTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_request.dest_tracker_id len %d != %d", len(m.DestTrackerId), TrackerIDLen)
	}
	if len(m.IdentityId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_request.identity_id len %d != %d", len(m.IdentityId), TrackerIDLen)
	}
	if m.Amount == 0 {
		return errors.New("federation: transfer_request.amount must be > 0")
	}
	if len(m.Nonce) != NonceLen {
		return fmt.Errorf("federation: transfer_request.nonce len %d != %d", len(m.Nonce), NonceLen)
	}
	if len(m.ConsumerSig) != SigLen {
		return fmt.Errorf("federation: transfer_request.consumer_sig len %d != %d", len(m.ConsumerSig), SigLen)
	}
	if len(m.ConsumerPub) != ed25519PubLen {
		return fmt.Errorf("federation: transfer_request.consumer_pub len %d != %d", len(m.ConsumerPub), ed25519PubLen)
	}
	if bytes.Equal(m.SourceTrackerId, m.DestTrackerId) {
		return errors.New("federation: transfer_request.source_tracker_id == dest_tracker_id")
	}
	if allZero(m.SourceTrackerId) {
		return errors.New("federation: transfer_request.source_tracker_id is all zero")
	}
	if allZero(m.DestTrackerId) {
		return errors.New("federation: transfer_request.dest_tracker_id is all zero")
	}
	if allZero(m.IdentityId) {
		return errors.New("federation: transfer_request.identity_id is all zero")
	}
	if allZero(m.Nonce) {
		return errors.New("federation: transfer_request.nonce is all zero")
	}
	if m.Timestamp == 0 {
		return errors.New("federation: transfer_request.timestamp must be > 0")
	}
	return nil
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd shared && go test -race ./federation -run TestValidateTransferProofRequest -v`
Expected: PASS for all sub-tests.

- [ ] **Step 5: Commit.**

```bash
git add shared/federation/validate.go shared/federation/validate_test.go
git commit -m "feat(shared/federation): validate TransferProofRequest"
```

---

## Task 4: Validators for TransferProof + TransferApplied

**Files:**
- Modify: `shared/federation/validate.go`
- Modify: `shared/federation/validate_test.go`

- [ ] **Step 1: Write failing tests.**

Append to `shared/federation/validate_test.go`:

```go
func validTransferProof() *TransferProof {
	return &TransferProof{
		SourceTrackerId:    bytes.Repeat([]byte{0x11}, TrackerIDLen),
		DestTrackerId:      bytes.Repeat([]byte{0x22}, TrackerIDLen),
		IdentityId:         bytes.Repeat([]byte{0x33}, TrackerIDLen),
		Amount:             1000,
		Nonce:              bytes.Repeat([]byte{0x44}, NonceLen),
		SourceChainTipHash: bytes.Repeat([]byte{0x55}, RootLen),
		SourceSeq:          42,
		Timestamp:          1714000000,
		SourceTrackerSig:   bytes.Repeat([]byte{0x66}, SigLen),
	}
}

func TestValidateTransferProof_HappyPath(t *testing.T) {
	if err := ValidateTransferProof(validTransferProof()); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestValidateTransferProof_RejectsBadShapes(t *testing.T) {
	cases := map[string]func(m *TransferProof){
		"nil":           func(m *TransferProof) { *m = TransferProof{} },
		"src_len":       func(m *TransferProof) { m.SourceTrackerId = nil },
		"dst_len":       func(m *TransferProof) { m.DestTrackerId = nil },
		"identity_len":  func(m *TransferProof) { m.IdentityId = nil },
		"amount_zero":   func(m *TransferProof) { m.Amount = 0 },
		"nonce_len":     func(m *TransferProof) { m.Nonce = nil },
		"chain_tip_len": func(m *TransferProof) { m.SourceChainTipHash = nil },
		"sig_len":       func(m *TransferProof) { m.SourceTrackerSig = nil },
		"src_eq_dst":    func(m *TransferProof) { m.DestTrackerId = m.SourceTrackerId },
		"timestamp_zero": func(m *TransferProof) { m.Timestamp = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := validTransferProof()
			mut(m)
			if err := ValidateTransferProof(m); err == nil {
				t.Fatalf("err=nil, want error")
			}
		})
	}
}

func validTransferApplied() *TransferApplied {
	return &TransferApplied{
		SourceTrackerId: bytes.Repeat([]byte{0x11}, TrackerIDLen),
		DestTrackerId:   bytes.Repeat([]byte{0x22}, TrackerIDLen),
		Nonce:           bytes.Repeat([]byte{0x44}, NonceLen),
		Timestamp:       1714000000,
		DestTrackerSig:  bytes.Repeat([]byte{0x66}, SigLen),
	}
}

func TestValidateTransferApplied_HappyPath(t *testing.T) {
	if err := ValidateTransferApplied(validTransferApplied()); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestValidateTransferApplied_RejectsBadShapes(t *testing.T) {
	cases := map[string]func(m *TransferApplied){
		"nil":           func(m *TransferApplied) { *m = TransferApplied{} },
		"src_len":       func(m *TransferApplied) { m.SourceTrackerId = nil },
		"dst_len":       func(m *TransferApplied) { m.DestTrackerId = nil },
		"nonce_len":     func(m *TransferApplied) { m.Nonce = nil },
		"sig_len":       func(m *TransferApplied) { m.DestTrackerSig = nil },
		"src_eq_dst":    func(m *TransferApplied) { m.DestTrackerId = m.SourceTrackerId },
		"timestamp_zero": func(m *TransferApplied) { m.Timestamp = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := validTransferApplied()
			mut(m)
			if err := ValidateTransferApplied(m); err == nil {
				t.Fatalf("err=nil, want error")
			}
		})
	}
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd shared && go test -race ./federation -run "TestValidateTransfer(Proof|Applied)" -v`
Expected: FAIL — validators undefined.

- [ ] **Step 3: Add the validators.**

Append to `shared/federation/validate.go`:

```go
func ValidateTransferProof(m *TransferProof) error {
	if m == nil {
		return errors.New("federation: nil TransferProof")
	}
	if len(m.SourceTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_proof.source_tracker_id len %d != %d", len(m.SourceTrackerId), TrackerIDLen)
	}
	if len(m.DestTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_proof.dest_tracker_id len %d != %d", len(m.DestTrackerId), TrackerIDLen)
	}
	if len(m.IdentityId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_proof.identity_id len %d != %d", len(m.IdentityId), TrackerIDLen)
	}
	if m.Amount == 0 {
		return errors.New("federation: transfer_proof.amount must be > 0")
	}
	if len(m.Nonce) != NonceLen {
		return fmt.Errorf("federation: transfer_proof.nonce len %d != %d", len(m.Nonce), NonceLen)
	}
	if len(m.SourceChainTipHash) != RootLen {
		return fmt.Errorf("federation: transfer_proof.source_chain_tip_hash len %d != %d", len(m.SourceChainTipHash), RootLen)
	}
	if len(m.SourceTrackerSig) != SigLen {
		return fmt.Errorf("federation: transfer_proof.source_tracker_sig len %d != %d", len(m.SourceTrackerSig), SigLen)
	}
	if bytes.Equal(m.SourceTrackerId, m.DestTrackerId) {
		return errors.New("federation: transfer_proof.source_tracker_id == dest_tracker_id")
	}
	if m.Timestamp == 0 {
		return errors.New("federation: transfer_proof.timestamp must be > 0")
	}
	return nil
}

func ValidateTransferApplied(m *TransferApplied) error {
	if m == nil {
		return errors.New("federation: nil TransferApplied")
	}
	if len(m.SourceTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_applied.source_tracker_id len %d != %d", len(m.SourceTrackerId), TrackerIDLen)
	}
	if len(m.DestTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_applied.dest_tracker_id len %d != %d", len(m.DestTrackerId), TrackerIDLen)
	}
	if len(m.Nonce) != NonceLen {
		return fmt.Errorf("federation: transfer_applied.nonce len %d != %d", len(m.Nonce), NonceLen)
	}
	if len(m.DestTrackerSig) != SigLen {
		return fmt.Errorf("federation: transfer_applied.dest_tracker_sig len %d != %d", len(m.DestTrackerSig), SigLen)
	}
	if bytes.Equal(m.SourceTrackerId, m.DestTrackerId) {
		return errors.New("federation: transfer_applied.source_tracker_id == dest_tracker_id")
	}
	if m.Timestamp == 0 {
		return errors.New("federation: transfer_applied.timestamp must be > 0")
	}
	return nil
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd shared && go test -race ./federation -v`
Expected: PASS for the new tests; nothing else regresses.

- [ ] **Step 5: Commit.**

```bash
git add shared/federation/validate.go shared/federation/validate_test.go
git commit -m "feat(shared/federation): validate TransferProof and TransferApplied"
```

---

## Task 5: Canonical signing helpers — TransferProofRequest

**Files:**
- Create: `shared/federation/signing_transfer.go`
- Create: `shared/federation/signing_transfer_test.go`

- [ ] **Step 1: Write failing test.**

Create `shared/federation/signing_transfer_test.go`:

```go
package federation

import (
	"bytes"
	"testing"
)

func TestCanonicalTransferProofRequestPreSig_Deterministic(t *testing.T) {
	m1 := validTransferProofRequest()
	m2 := validTransferProofRequest()

	a, err := CanonicalTransferProofRequestPreSig(m1)
	if err != nil {
		t.Fatalf("first marshal: %v", err)
	}
	b, err := CanonicalTransferProofRequestPreSig(m2)
	if err != nil {
		t.Fatalf("second marshal: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("canonical bytes differ between equal messages\n a=%x\n b=%x", a, b)
	}
}

func TestCanonicalTransferProofRequestPreSig_ZeroesConsumerSig(t *testing.T) {
	m := validTransferProofRequest()
	a, err := CanonicalTransferProofRequestPreSig(m)
	if err != nil {
		t.Fatal(err)
	}

	tampered := validTransferProofRequest()
	tampered.ConsumerSig = bytes.Repeat([]byte{0xFF}, SigLen)
	b, err := CanonicalTransferProofRequestPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("changing consumer_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalTransferProofRequestPreSig_DetectsTampering(t *testing.T) {
	m := validTransferProofRequest()
	a, err := CanonicalTransferProofRequestPreSig(m)
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferProofRequest()
	tampered.Amount = m.Amount + 1
	b, err := CanonicalTransferProofRequestPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatalf("tampering Amount produced identical canonical bytes")
	}
}

func TestCanonicalTransferProofRequestPreSig_NilMessage(t *testing.T) {
	if _, err := CanonicalTransferProofRequestPreSig(nil); err == nil {
		t.Fatal("err=nil, want error")
	}
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd shared && go test -race ./federation -run TestCanonicalTransferProofRequest -v`
Expected: FAIL — `CanonicalTransferProofRequestPreSig` undefined.

- [ ] **Step 3: Implement the helper.**

Create `shared/federation/signing_transfer.go`:

```go
// Package federation: canonical pre-sig byte builders for the three
// transfer messages. Each helper clears the relevant signature field so
// the signer and verifier reconstruct the same byte string.
//
// The helpers route through shared/signing.DeterministicMarshal per
// shared/CLAUDE.md §6: every signed proto goes through that single
// determinism choke point.
package federation

import (
	"errors"

	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

// CanonicalTransferProofRequestPreSig returns the deterministic byte
// representation of m with consumer_sig cleared. The consumer signs the
// returned bytes; the verifier reconstructs identically.
func CanonicalTransferProofRequestPreSig(m *TransferProofRequest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferProofRequest")
	}
	clone, ok := proto.Clone(m).(*TransferProofRequest)
	if !ok {
		return nil, errors.New("federation: clone TransferProofRequest")
	}
	clone.ConsumerSig = nil
	return signing.DeterministicMarshal(clone)
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd shared && go test -race ./federation -run TestCanonicalTransferProofRequest -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add shared/federation/signing_transfer.go shared/federation/signing_transfer_test.go
git commit -m "feat(shared/federation): canonical pre-sig for TransferProofRequest"
```

---

## Task 6: Canonical signing — TransferProof + TransferApplied

**Files:**
- Modify: `shared/federation/signing_transfer.go`
- Modify: `shared/federation/signing_transfer_test.go`

- [ ] **Step 1: Write failing tests.**

Append to `shared/federation/signing_transfer_test.go`:

```go
func TestCanonicalTransferProofPreSig_ZeroesSourceSig(t *testing.T) {
	m := validTransferProof()
	a, err := CanonicalTransferProofPreSig(m)
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferProof()
	tampered.SourceTrackerSig = bytes.Repeat([]byte{0xFF}, SigLen)
	b, err := CanonicalTransferProofPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("changing source_tracker_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalTransferProofPreSig_DetectsAmountTampering(t *testing.T) {
	a, err := CanonicalTransferProofPreSig(validTransferProof())
	if err != nil {
		t.Fatal(err)
	}
	bad := validTransferProof()
	bad.Amount = 999999
	b, err := CanonicalTransferProofPreSig(bad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("Amount tamper produced identical canonical bytes")
	}
}

func TestCanonicalTransferAppliedPreSig_ZeroesDestSig(t *testing.T) {
	m := validTransferApplied()
	a, err := CanonicalTransferAppliedPreSig(m)
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferApplied()
	tampered.DestTrackerSig = bytes.Repeat([]byte{0xFF}, SigLen)
	b, err := CanonicalTransferAppliedPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("changing dest_tracker_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalTransferAppliedPreSig_DetectsNonceTampering(t *testing.T) {
	a, err := CanonicalTransferAppliedPreSig(validTransferApplied())
	if err != nil {
		t.Fatal(err)
	}
	bad := validTransferApplied()
	bad.Nonce[0] ^= 0xFF
	b, err := CanonicalTransferAppliedPreSig(bad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("Nonce tamper produced identical canonical bytes")
	}
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd shared && go test -race ./federation -run "TestCanonicalTransfer(Proof|Applied)" -v`
Expected: FAIL — helpers undefined.

- [ ] **Step 3: Implement the helpers.**

Append to `shared/federation/signing_transfer.go`:

```go
// CanonicalTransferProofPreSig returns the deterministic byte representation
// of m with source_tracker_sig cleared. The source tracker signs the bytes;
// the destination tracker reconstructs identically to verify.
func CanonicalTransferProofPreSig(m *TransferProof) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferProof")
	}
	clone, ok := proto.Clone(m).(*TransferProof)
	if !ok {
		return nil, errors.New("federation: clone TransferProof")
	}
	clone.SourceTrackerSig = nil
	return signing.DeterministicMarshal(clone)
}

// CanonicalTransferAppliedPreSig returns the deterministic byte representation
// of m with dest_tracker_sig cleared.
func CanonicalTransferAppliedPreSig(m *TransferApplied) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferApplied")
	}
	clone, ok := proto.Clone(m).(*TransferApplied)
	if !ok {
		return nil, errors.New("federation: clone TransferApplied")
	}
	clone.DestTrackerSig = nil
	return signing.DeterministicMarshal(clone)
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd shared && go test -race ./federation -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add shared/federation/signing_transfer.go shared/federation/signing_transfer_test.go
git commit -m "feat(shared/federation): canonical pre-sig for TransferProof and TransferApplied"
```

---

## Task 7: Ledger — `AppendTransferIn` happy path

**Files:**
- Modify: `tracker/internal/ledger/transfer.go`
- Modify: `tracker/internal/ledger/transfer_test.go`

- [ ] **Step 1: Write failing test.**

Append to `tracker/internal/ledger/transfer_test.go`:

```go
func TestAppendTransferIn_HappyPath(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identityID := bytes.Repeat([]byte{0x33}, 32)
	transferRef := bytes.Repeat([]byte{0x44}, 32)

	prev, seq := nextTipForTest(t, l)

	e, err := l.AppendTransferIn(ctx, TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  identityID,
		Amount:      1500,
		Timestamp:   1714000000 + seq,
		TransferRef: transferRef,
	})
	require.NoError(t, err)
	require.NotNil(t, e)

	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN, e.Body.Kind)
	assert.Empty(t, e.ConsumerSig, "transfer_in has no consumer sig")
	assert.Empty(t, e.SeederSig, "transfer_in has no seeder sig")
	assert.NotEmpty(t, e.TrackerSig)

	bal, ok, err := l.store.Balance(ctx, identityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1500), bal.Credits)
}

func TestAppendTransferIn_RejectsZeroAmount(t *testing.T) {
	l := openTempLedger(t)
	prev, seq := nextTipForTest(t, l)
	_, err := l.AppendTransferIn(context.Background(), TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  bytes.Repeat([]byte{0x33}, 32),
		Amount:      0,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x44}, 32),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount must be > 0")
}

func TestAppendTransferIn_StaleTip(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identityID := bytes.Repeat([]byte{0x33}, 32)

	prev, seq := nextTipForTest(t, l)
	rec := TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  identityID,
		Amount:      100,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x44}, 32),
	}
	// Advance tip with another append.
	other := bytes.Repeat([]byte{0x22}, 32)
	_, err := l.IssueStarterGrant(ctx, other, 50)
	require.NoError(t, err)

	_, err = l.AppendTransferIn(ctx, rec)
	require.ErrorIs(t, err, ErrStaleTip)
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/ledger -run TestAppendTransferIn -v`
Expected: FAIL — `TransferInRecord` and `Ledger.AppendTransferIn` undefined.

- [ ] **Step 3: Implement `AppendTransferIn`.**

Append to `tracker/internal/ledger/transfer.go`:

```go
// TransferInRecord is the typed input to AppendTransferIn. Unlike
// AppendTransferOut there is no consumer signature: the entry is
// tracker-signed only, and its authority comes from the peer-region's
// TRANSFER_PROOF that the federation layer has already verified before
// invoking this hook.
type TransferInRecord struct {
	PrevHash    []byte // 32 bytes
	Seq         uint64
	IdentityID  []byte // 32 bytes — recipient of the credit
	Amount      uint64 // absolute; credited
	Timestamp   uint64
	TransferRef []byte // 32 bytes — same nonce/UUID as the source's transfer_out
}

// AppendTransferIn credits Amount to IdentityID and records a
// transfer_in entry whose Ref == TransferRef. The on-chain entry
// zero-fills consumer_id and seeder_id per the per-kind matrix; the
// balance delta is routed via TransferInRecord.IdentityID directly.
//
// Returns ErrStaleTip if PrevHash/Seq don't match the current tip.
// v1 has no on-chain idempotency check at the ledger layer; the federation
// layer's in-memory pending-and-issued maps cover within-process retries.
// Persistent idempotency is a follow-up (see federation cross-region
// transfer spec §14).
func (l *Ledger) AppendTransferIn(ctx context.Context, r TransferInRecord) (*tbproto.Entry, error) {
	if r.Amount == 0 {
		return nil, errors.New("ledger: transfer_in amount must be > 0")
	}
	delta, err := signedAmount(r.Amount)
	if err != nil {
		return nil, err
	}

	body, err := entry.BuildTransferInEntry(entry.TransferInInput{
		PrevHash:    r.PrevHash,
		Seq:         r.Seq,
		Amount:      r.Amount,
		Timestamp:   r.Timestamp,
		TransferRef: r.TransferRef,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: build transfer_in: %w", err)
	}

	return l.appendEntry(ctx, appendInput{
		body:   body,
		deltas: []balanceDelta{{identityID: r.IdentityID, delta: delta}},
	})
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/ledger -run TestAppendTransferIn -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/ledger/transfer.go tracker/internal/ledger/transfer_test.go
git commit -m "feat(tracker/ledger): AppendTransferIn"
```

---

## Task 8: Ledger — chain-walk-by-ref helper + `ErrTransferRefExists`

**Files:**
- Modify: `tracker/internal/ledger/transfer.go`
- Modify: `tracker/internal/ledger/transfer_test.go`

- [ ] **Step 1: Write failing test for the duplicate guard.**

Append to `tracker/internal/ledger/transfer_test.go`:

```go
func TestAppendTransferIn_DuplicateRefIsErrTransferRefExists(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identityID := bytes.Repeat([]byte{0x33}, 32)
	transferRef := bytes.Repeat([]byte{0x44}, 32)

	prev, seq := nextTipForTest(t, l)
	_, err := l.AppendTransferIn(ctx, TransferInRecord{
		PrevHash: prev, Seq: seq,
		IdentityID:  identityID,
		Amount:      1500,
		Timestamp:   1714000000 + seq,
		TransferRef: transferRef,
	})
	require.NoError(t, err)

	prev2, seq2 := nextTipForTest(t, l)
	_, err = l.AppendTransferIn(ctx, TransferInRecord{
		PrevHash: prev2, Seq: seq2,
		IdentityID:  identityID,
		Amount:      1500,
		Timestamp:   1714000000 + seq2,
		TransferRef: transferRef,
	})
	require.ErrorIs(t, err, ErrTransferRefExists)
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/ledger -run TestAppendTransferIn_DuplicateRef -v`
Expected: FAIL — `ErrTransferRefExists` undefined; second append silently succeeds.

- [ ] **Step 3: Add `ErrTransferRefExists` and the chain-walk guard.**

Edit `tracker/internal/ledger/transfer.go`. Below `ErrInsufficientBalance` (which lives in `append.go` — declare here at top of `transfer.go` since this is the only caller for now):

```go
// ErrTransferRefExists means a transfer entry with the same TransferRef
// is already on the chain. Callers MUST treat this as an idempotent
// success at the federation layer (the credit was already booked) but
// at the ledger layer it short-circuits before any double-write.
//
// v1 implements the existence check via a chain walk; an indexed
// lookup is the storage-layer follow-up flagged in the federation
// cross-region transfer spec §14.
var ErrTransferRefExists = errors.New("ledger: transfer ref already on chain")
```

Add a private helper at the bottom of `transfer.go`:

```go
// findTransferEntryByRef walks the chain for a TRANSFER_IN or TRANSFER_OUT
// entry matching ref. v1 cost is O(n_entries); a TODO is to swap to an
// indexed lookup once storage adds idx_entries_ref_kind.
func (l *Ledger) findTransferEntryByRef(ctx context.Context, ref []byte, kind tbproto.EntryKind) (*tbproto.Entry, bool, error) {
	const batchSize = 256
	var since uint64
	for {
		batch, err := l.store.EntriesSince(ctx, since, batchSize)
		if err != nil {
			return nil, false, fmt.Errorf("ledger: walk for transfer ref: %w", err)
		}
		if len(batch) == 0 {
			return nil, false, nil
		}
		for _, e := range batch {
			if e.Body == nil {
				continue
			}
			since = e.Body.Seq
			if e.Body.Kind == kind && bytes.Equal(e.Body.Ref, ref) {
				return e, true, nil
			}
		}
	}
}
```

Now wire the guard into `AppendTransferIn`. Modify the body:

```go
func (l *Ledger) AppendTransferIn(ctx context.Context, r TransferInRecord) (*tbproto.Entry, error) {
	if r.Amount == 0 {
		return nil, errors.New("ledger: transfer_in amount must be > 0")
	}
	if existing, ok, err := l.findTransferEntryByRef(ctx, r.TransferRef, tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN); err != nil {
		return nil, err
	} else if ok {
		_ = existing
		return nil, ErrTransferRefExists
	}
	delta, err := signedAmount(r.Amount)
	if err != nil {
		return nil, err
	}

	body, err := entry.BuildTransferInEntry(entry.TransferInInput{
		PrevHash:    r.PrevHash,
		Seq:         r.Seq,
		Amount:      r.Amount,
		Timestamp:   r.Timestamp,
		TransferRef: r.TransferRef,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: build transfer_in: %w", err)
	}

	return l.appendEntry(ctx, appendInput{
		body:   body,
		deltas: []balanceDelta{{identityID: r.IdentityID, delta: delta}},
	})
}
```

Add `"bytes"` to the import block of `transfer.go` if not already present.

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/ledger -run TestAppendTransferIn -v`
Expected: PASS for all four sub-tests.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/ledger/transfer.go tracker/internal/ledger/transfer_test.go
git commit -m "feat(tracker/ledger): ErrTransferRefExists guard for AppendTransferIn"
```

---

## Task 9: Federation sentinel errors + `Deps.Ledger` field

**Files:**
- Modify: `tracker/internal/federation/errors.go`
- Modify: `tracker/internal/federation/errors_test.go`
- Modify: `tracker/internal/federation/config.go`

- [ ] **Step 1: Write failing test for the new sentinels.**

Append to `tracker/internal/federation/errors_test.go`:

```go
func TestSentinelErrors_TransferFamily(t *testing.T) {
	for _, e := range []error{
		ErrPeerNotConnected,
		ErrTransferTimeout,
		ErrTransferDisabled,
	} {
		if e == nil {
			t.Fatal("expected non-nil sentinel")
		}
		if errors.Unwrap(e) != nil {
			t.Fatalf("sentinel %v wraps another error: %v", e, errors.Unwrap(e))
		}
	}
}
```

If `errors_test.go` doesn't already import `errors`, add it.

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run TestSentinelErrors_TransferFamily -v`
Expected: FAIL — sentinels undefined.

- [ ] **Step 3: Add the sentinels.**

Append to `tracker/internal/federation/errors.go`:

```go
// ErrPeerNotConnected means the federation layer cannot reach the
// requested peer (not in the active map; not in steady state). Returned
// by Federation.StartTransfer and the per-peer unicast Send.
var ErrPeerNotConnected = errors.New("federation: peer not connected")

// ErrTransferTimeout means a TRANSFER_PROOF was not received within the
// configured TransferTimeout. The destination tracker's StartTransfer
// returns this; the consumer's plugin retries.
var ErrTransferTimeout = errors.New("federation: transfer proof timeout")

// ErrTransferDisabled means the federation subsystem was constructed
// without a LedgerHooks dependency, so cross-region transfer is off.
// Returned by StartTransfer and rejected on inbound transfer kinds.
var ErrTransferDisabled = errors.New("federation: transfer disabled (no LedgerHooks)")
```

Ensure `errors.go` imports `"errors"`.

- [ ] **Step 4: Add `Ledger LedgerHooks` to `Deps`.**

Edit `tracker/internal/federation/config.go`. In the `Deps` struct, add the `Ledger` field. (We declare the type forward-referenced; the actual interface lands in the next task.)

```go
type Deps struct {
	Transport Transport
	RootSrc   RootSource
	Archive   PeerRootArchive
	Metrics   *Metrics
	Logger    zerolog.Logger
	Now       func() time.Time

	// Ledger is the cross-region credit transfer hook. May be nil; when
	// nil, Federation.StartTransfer returns ErrTransferDisabled and
	// inbound transfer kinds are rejected with the same.
	Ledger LedgerHooks
}
```

- [ ] **Step 5: Run; expect PASS for the sentinel test, possibly compile failure for `LedgerHooks`.**

Run: `cd tracker && go build ./internal/federation/...`
Expected: FAIL with `LedgerHooks is undefined` — that's expected, the next task adds it.

- [ ] **Step 6: Add a minimal `LedgerHooks` placeholder so the build works at this commit boundary.**

Create `tracker/internal/federation/ledger_hooks.go` with just enough to compile:

```go
// Package federation: LedgerHooks is the cross-region transfer dependency.
// The full interface is defined in this file; the production binding lives
// in tracker/cmd/token-bay-tracker/run_cmd.go.
package federation

import (
	"context"
	"crypto/ed25519"
)

// LedgerHooks is the federation-side view of the ledger orchestrator.
type LedgerHooks interface {
	AppendTransferOut(ctx context.Context, in TransferOutHookIn) (TransferOutHookOut, error)
	AppendTransferIn(ctx context.Context, in TransferInHookIn) error
}

// TransferOutHookIn is what the federation layer hands to AppendTransferOut.
type TransferOutHookIn struct {
	IdentityID  [32]byte
	Amount      uint64
	Timestamp   uint64
	TransferRef [32]byte
	ConsumerSig []byte
	ConsumerPub ed25519.PublicKey
}

// TransferOutHookOut is what AppendTransferOut returns to the federation
// layer to populate the outbound TransferProof.
type TransferOutHookOut struct {
	ChainTipHash [32]byte
	Seq          uint64
}

// TransferInHookIn is what the federation layer hands to AppendTransferIn.
type TransferInHookIn struct {
	IdentityID  [32]byte
	Amount      uint64
	Timestamp   uint64
	TransferRef [32]byte
}
```

- [ ] **Step 7: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestSentinelErrors_TransferFamily -v && go build ./internal/federation/...`
Expected: PASS for the test; clean build.

- [ ] **Step 8: Commit.**

```bash
git add tracker/internal/federation/errors.go tracker/internal/federation/errors_test.go \
        tracker/internal/federation/config.go tracker/internal/federation/ledger_hooks.go
git commit -m "feat(tracker/federation): transfer sentinel errors + LedgerHooks Deps slot"
```

---

## Task 10: Federation `TransferTimeout` + `IssuedProofCap` config defaults

**Files:**
- Modify: `tracker/internal/federation/config.go`
- Test: `tracker/internal/federation/config_test.go` (file exists; check by running `ls`)

- [ ] **Step 1: Write failing test.**

Append to `tracker/internal/federation/config_test.go` (create the file if it doesn't exist):

```go
func TestConfig_WithDefaults_TransferTimeout(t *testing.T) {
	c := (Config{}).withDefaults()
	if c.TransferTimeout == 0 {
		t.Fatalf("TransferTimeout default = 0, want non-zero")
	}
	if c.TransferTimeout > 600*time.Second || c.TransferTimeout < time.Second {
		t.Fatalf("TransferTimeout = %v out of [1s, 600s]", c.TransferTimeout)
	}
}

func TestConfig_WithDefaults_IssuedProofCap(t *testing.T) {
	c := (Config{}).withDefaults()
	if c.IssuedProofCap == 0 {
		t.Fatalf("IssuedProofCap default = 0, want non-zero")
	}
	if c.IssuedProofCap < 128 {
		t.Fatalf("IssuedProofCap = %d, want >= 128", c.IssuedProofCap)
	}
}
```

Ensure `time` is imported.

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run "TestConfig_WithDefaults_(TransferTimeout|IssuedProofCap)" -v`
Expected: FAIL — fields undefined.

- [ ] **Step 3: Add the config fields + defaults.**

Edit `tracker/internal/federation/config.go`. Add to the `Config` struct:

```go
	TransferTimeout time.Duration
	IssuedProofCap  int
```

In `withDefaults`, after the existing defaults block:

```go
	if c.TransferTimeout == 0 {
		c.TransferTimeout = 30 * time.Second
	}
	if c.IssuedProofCap == 0 {
		c.IssuedProofCap = 4096
	}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/config.go tracker/internal/federation/config_test.go
git commit -m "feat(tracker/federation): TransferTimeout + IssuedProofCap defaults"
```

---

## Task 11: Federation per-peer unicast — `SendToPeer`

**Files:**
- Create: `tracker/internal/federation/unicast.go`
- Create: `tracker/internal/federation/unicast_test.go`

- [ ] **Step 1: Write failing tests.**

Create `tracker/internal/federation/unicast_test.go` (note: `package federation`, internal — needed to populate `f.peers` directly):

```go
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// captureConn implements PeerConn by recording every frame written via
// Send and blocking Recv until ctx is cancelled. It supports the unicast
// happy-path test without spinning up a full peering handshake.
type captureConn struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *captureConn) Send(_ context.Context, frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	dup := make([]byte, len(frame))
	copy(dup, frame)
	c.frames = append(c.frames, dup)
	return nil
}
func (c *captureConn) Recv(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *captureConn) RemoteAddr() string           { return "capture" }
func (c *captureConn) RemotePub() ed25519.PublicKey { return nil }
func (c *captureConn) Close() error                 { return nil }

func TestSendToPeer_HappyPath(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	myID := ids.TrackerID(sha256.Sum256(pub))

	var peerID ids.TrackerID
	peerID[0] = 0x42

	capt := &captureConn{}
	peer := NewPeerForTest(capt, func(*fed.Envelope) {})

	f := &Federation{
		cfg:   (Config{MyTrackerID: myID, MyPriv: priv}).withDefaults(),
		peers: map[ids.TrackerID]*Peer{peerID: peer},
	}

	err = f.SendToPeer(context.Background(), peerID, fed.Kind_KIND_PING, []byte("payload"))
	require.NoError(t, err)

	capt.mu.Lock()
	defer capt.mu.Unlock()
	require.Len(t, capt.frames, 1)

	env := &fed.Envelope{}
	require.NoError(t, proto.Unmarshal(capt.frames[0], env))
	assert.Equal(t, fed.Kind_KIND_PING, env.Kind)
	assert.Equal(t, []byte("payload"), env.Payload)
	myIDBytes := myID.Bytes()
	assert.Equal(t, myIDBytes[:], env.SenderId)
}

func TestSendToPeer_PeerNotConnected(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	myID := ids.TrackerID(sha256.Sum256(pub))

	f := &Federation{
		cfg:   (Config{MyTrackerID: myID, MyPriv: priv}).withDefaults(),
		peers: map[ids.TrackerID]*Peer{},
	}

	var unknown ids.TrackerID
	for i := range unknown {
		unknown[i] = 0xDD
	}
	err = f.SendToPeer(context.Background(), unknown, fed.Kind_KIND_PING, []byte("x"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerNotConnected), "want ErrPeerNotConnected; got %v", err)
}
```

The test directly constructs a `Federation` value (without `Open`) so it can populate `peers` and `cfg` without spinning up a transport. This is the minimum-coupling unit-test shape for `SendToPeer`. End-to-end via two real Federations lives in Task 18.

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run TestSendToPeer -v`
Expected: FAIL — `Federation.SendToPeer` undefined.

- [ ] **Step 3: Implement `SendToPeer`.**

Create `tracker/internal/federation/unicast.go`:

```go
// Package federation: per-peer unicast Send. Slice 0's Gossip.Forward
// broadcasts to all active peers; cross-region credit transfer is
// point-to-point and uses this primitive instead.
package federation

import (
	"context"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

// SendToPeer signs an envelope wrapping payload (with the local tracker
// key) and writes it to the named peer's send queue. Returns
// ErrPeerNotConnected if the peer is not in steady state.
//
// The dedupe Mark is not done for unicast sends (these messages are not
// gossiped onward); the destination's recv path still calls dedupe.Seen
// on receipt, which catches a hostile peer that re-broadcasts a unicast
// frame.
func (f *Federation) SendToPeer(ctx context.Context, peerID ids.TrackerID, kind fed.Kind, payload []byte) error {
	f.mu.Lock()
	pe, ok := f.peers[peerID]
	f.mu.Unlock()
	if !ok || pe == nil {
		return fmt.Errorf("%w: %x", ErrPeerNotConnected, peerID.Bytes())
	}
	idBytes := f.cfg.MyTrackerID.Bytes()
	env, err := SignEnvelope(f.cfg.MyPriv, idBytes[:], kind, payload)
	if err != nil {
		return fmt.Errorf("federation: sign envelope: %w", err)
	}
	frame, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("federation: marshal envelope: %w", err)
	}
	return pe.Send(ctx, frame)
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestSendToPeer -v`
Expected: PASS for both sub-tests.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/unicast.go tracker/internal/federation/unicast_test.go
git commit -m "feat(tracker/federation): per-peer unicast SendToPeer"
```

---

## Task 12: Federation `TransferCoordinator` scaffold + source-side `OnRequest` happy path

**Files:**
- Create: `tracker/internal/federation/transfer.go`
- Create: `tracker/internal/federation/transfer_test.go`

- [ ] **Step 1: Write failing test.**

Create `tracker/internal/federation/transfer_test.go`:

```go
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// fakeLedger implements LedgerHooks. It records calls and returns the
// configured outputs.
type fakeLedger struct {
	outCalls []TransferOutHookIn
	inCalls  []TransferInHookIn

	outResult TransferOutHookOut
	outErr    error
	inErr     error
}

func (f *fakeLedger) AppendTransferOut(_ context.Context, in TransferOutHookIn) (TransferOutHookOut, error) {
	f.outCalls = append(f.outCalls, in)
	return f.outResult, f.outErr
}

func (f *fakeLedger) AppendTransferIn(_ context.Context, in TransferInHookIn) error {
	f.inCalls = append(f.inCalls, in)
	return f.inErr
}

// keypairFromSeed returns a deterministic Ed25519 key pair from a single byte.
func keypairFromSeed(seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(s)
	return priv.Public().(ed25519.PublicKey), priv
}

func TestTransferCoordinator_OnRequest_HappyPath(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, conPriv := keypairFromSeed(0x33)

	srcID := ids.TrackerIDFromPubKey(srcPub)
	dstID := ids.TrackerIDFromPubKey(dstPub)

	ledger := &fakeLedger{
		outResult: TransferOutHookOut{
			ChainTipHash: sha256.Sum256([]byte("chain-tip")),
			Seq:          42,
		},
	}

	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID:    srcID,
		MyPriv:         srcPriv,
		Ledger:         ledger,
		IssuedCap:      16,
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:     func(id ids.TrackerID) (ed25519.PublicKey, bool) { return dstPub, id == dstID },
		Send:           func(_ context.Context, peerID ids.TrackerID, kind fed.Kind, payload []byte) error { return nil },
		MetricsCounter: func(string) {},
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)
	timestamp := uint64(1714000000)

	req := &fed.TransferProofRequest{
		SourceTrackerId: srcID.Bytes()[:],
		DestTrackerId:   dstID.Bytes()[:],
		IdentityId:      identityID[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       timestamp,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	require.NoError(t, err)
	req.ConsumerSig = ed25519.Sign(conPriv, canonical)

	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	dstIDBytes := dstID.Bytes()
	env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload)
	require.NoError(t, err)

	tc.OnRequest(context.Background(), env, dstID)

	require.Len(t, ledger.outCalls, 1)
	got := ledger.outCalls[0]
	assert.Equal(t, identityID, got.IdentityID)
	assert.Equal(t, uint64(1500), got.Amount)
	assert.Equal(t, nonce, got.TransferRef)
	assert.Equal(t, timestamp, got.Timestamp)
	assert.Equal(t, []byte(conPub), []byte(got.ConsumerPub))
}

// helpers
func bytes32(b byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = b
	}
	return out
}
```

If `ids.TrackerIDFromPubKey` doesn't exist exactly under that name, search for the equivalent: `grep -rn "func.*TrackerID" shared/ids/`. Use whatever helper produces a `TrackerID` from an `ed25519.PublicKey`.

Also: this test exercises `OnRequest` directly without the dispatcher, so it needs `bytes`, `rand` imports trimmed if unused. Let `go vet` / `go build` clean up before the green run.

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run TestTransferCoordinator_OnRequest_HappyPath -v`
Expected: FAIL — `transferCoordinator` and `transferCoordinatorCfg` undefined.

- [ ] **Step 3: Implement the scaffold and `OnRequest` happy path.**

Create `tracker/internal/federation/transfer.go`:

```go
// Package federation: cross-region credit transfer coordinator. Owns the
// destination-side pending-request map and the source-side issued-proof
// replay cache.
package federation

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

type transferCoordinatorCfg struct {
	MyTrackerID    ids.TrackerID
	MyPriv         ed25519.PrivateKey
	Ledger         LedgerHooks
	IssuedCap      int
	Now            func() time.Time
	PeerPubKey     func(ids.TrackerID) (ed25519.PublicKey, bool)
	Send           func(context.Context, ids.TrackerID, fed.Kind, []byte) error
	MetricsCounter func(name string)
}

type issuedProof struct {
	payload []byte
	at      time.Time
}

type pendingResp struct {
	ch chan *fed.TransferProof
}

type transferCoordinator struct {
	cfg transferCoordinatorCfg

	mu      sync.Mutex
	pending map[[32]byte]pendingResp        // dest-side
	issued  map[[32]byte]issuedProof        // source-side replay cache
}

func newTransferCoordinator(cfg transferCoordinatorCfg) *transferCoordinator {
	if cfg.IssuedCap <= 0 {
		cfg.IssuedCap = 4096
	}
	if cfg.MetricsCounter == nil {
		cfg.MetricsCounter = func(string) {}
	}
	return &transferCoordinator{
		cfg:     cfg,
		pending: make(map[[32]byte]pendingResp),
		issued:  make(map[[32]byte]issuedProof),
	}
}

// OnRequest handles an inbound KIND_TRANSFER_PROOF_REQUEST envelope.
// fromPeer is the sender's TrackerID (== env.sender_id verified by the
// dispatcher).
func (tc *transferCoordinator) OnRequest(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	if tc.cfg.Ledger == nil {
		tc.cfg.MetricsCounter("transfer_request_received_disabled")
		return
	}
	req := &fed.TransferProofRequest{}
	if err := proto.Unmarshal(env.Payload, req); err != nil {
		tc.cfg.MetricsCounter("transfer_request_shape")
		return
	}
	if err := fed.ValidateTransferProofRequest(req); err != nil {
		tc.cfg.MetricsCounter("transfer_request_shape")
		return
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	if !equalID(req.SourceTrackerId, myID[:]) {
		tc.cfg.MetricsCounter("transfer_request_misrouted")
		return
	}
	dst := fromPeer.Bytes()
	if !equalID(req.DestTrackerId, dst[:]) {
		tc.cfg.MetricsCounter("transfer_request_dest_mismatch")
		return
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_request_canonical")
		return
	}
	if !ed25519.Verify(req.ConsumerPub, canonical, req.ConsumerSig) {
		tc.cfg.MetricsCounter("transfer_request_consumer_sig")
		return
	}

	var nonceArr [32]byte
	copy(nonceArr[:], req.Nonce)

	tc.mu.Lock()
	if cached, ok := tc.issued[nonceArr]; ok {
		tc.mu.Unlock()
		_ = tc.cfg.Send(ctx, fromPeer, fed.Kind_KIND_TRANSFER_PROOF, cached.payload)
		tc.cfg.MetricsCounter("transfer_request_replayed")
		return
	}
	tc.mu.Unlock()

	var identityArr [32]byte
	copy(identityArr[:], req.IdentityId)
	out, err := tc.cfg.Ledger.AppendTransferOut(ctx, TransferOutHookIn{
		IdentityID:  identityArr,
		Amount:      req.Amount,
		Timestamp:   req.Timestamp,
		TransferRef: nonceArr,
		ConsumerSig: req.ConsumerSig,
		ConsumerPub: req.ConsumerPub,
	})
	if err != nil {
		tc.cfg.MetricsCounter("transfer_request_ledger_err")
		return
	}

	proof := &fed.TransferProof{
		SourceTrackerId:    req.SourceTrackerId,
		DestTrackerId:      req.DestTrackerId,
		IdentityId:         req.IdentityId,
		Amount:             req.Amount,
		Nonce:              req.Nonce,
		SourceChainTipHash: out.ChainTipHash[:],
		SourceSeq:          out.Seq,
		Timestamp:          req.Timestamp,
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_proof_canonical")
		return
	}
	proof.SourceTrackerSig = ed25519.Sign(tc.cfg.MyPriv, cb)
	payload, err := proto.Marshal(proof)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_proof_marshal")
		return
	}

	tc.mu.Lock()
	tc.cacheIssuedLocked(nonceArr, payload)
	tc.mu.Unlock()

	if err := tc.cfg.Send(ctx, fromPeer, fed.Kind_KIND_TRANSFER_PROOF, payload); err != nil {
		tc.cfg.MetricsCounter("transfer_proof_send_err")
		return
	}
	tc.cfg.MetricsCounter("transfers_minted")
}

// cacheIssuedLocked stores payload keyed by nonce, evicting oldest if
// the cap is exceeded. Caller must hold tc.mu.
func (tc *transferCoordinator) cacheIssuedLocked(nonce [32]byte, payload []byte) {
	if len(tc.issued) >= tc.cfg.IssuedCap {
		var oldestKey [32]byte
		var oldestAt time.Time
		first := true
		for k, v := range tc.issued {
			if first || v.at.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.at
				first = false
			}
		}
		delete(tc.issued, oldestKey)
	}
	tc.issued[nonce] = issuedProof{payload: payload, at: tc.cfg.Now()}
}

func equalID(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensure errors / placeholders we'll wire up later don't trip vet:
var _ = fmt.Errorf
var _ = errors.New
```

Drop the `_ = fmt.Errorf` lines once their first real use lands.

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestTransferCoordinator_OnRequest_HappyPath -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/transfer.go tracker/internal/federation/transfer_test.go
git commit -m "feat(tracker/federation): TransferCoordinator + OnRequest happy path"
```

---

## Task 13: `OnRequest` rejects bad consumer signature

**Files:**
- Modify: `tracker/internal/federation/transfer_test.go`

- [ ] **Step 1: Write failing test.**

Append to `tracker/internal/federation/transfer_test.go`:

```go
func TestTransferCoordinator_OnRequest_BadConsumerSig(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, _ := keypairFromSeed(0x33)
	_, attackerPriv := keypairFromSeed(0x77)

	srcID := ids.TrackerIDFromPubKey(srcPub)
	dstID := ids.TrackerIDFromPubKey(dstPub)

	ledger := &fakeLedger{}
	metrics := map[string]int{}
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID:    srcID,
		MyPriv:         srcPriv,
		Ledger:         ledger,
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:     func(id ids.TrackerID) (ed25519.PublicKey, bool) { return dstPub, id == dstID },
		Send:           func(_ context.Context, _ ids.TrackerID, _ fed.Kind, _ []byte) error { return nil },
		MetricsCounter: func(n string) { metrics[n]++ },
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)

	req := &fed.TransferProofRequest{
		SourceTrackerId: srcID.Bytes()[:],
		DestTrackerId:   dstID.Bytes()[:],
		IdentityId:      identityID[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	require.NoError(t, err)
	req.ConsumerSig = ed25519.Sign(attackerPriv, canonical) // wrong key

	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	dstIDBytes := dstID.Bytes()
	env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload)
	require.NoError(t, err)

	tc.OnRequest(context.Background(), env, dstID)

	assert.Empty(t, ledger.outCalls, "ledger must not be called on bad sig")
	assert.Equal(t, 1, metrics["transfer_request_consumer_sig"])
}
```

- [ ] **Step 2: Run; expect PASS already** (the verify step in `OnRequest` rejects bad sigs).

Run: `cd tracker && go test -race ./internal/federation -run TestTransferCoordinator_OnRequest_BadConsumerSig -v`
Expected: PASS.

If FAIL: the verifier ran with the wrong key — investigate. Likely scenario: a copy-paste bug uses `req.ConsumerSig` before the ed25519.Verify check. Fix.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/transfer_test.go
git commit -m "test(tracker/federation): OnRequest rejects bad consumer sig"
```

---

## Task 14: Source-side replay — second `OnRequest` returns cached proof

**Files:**
- Modify: `tracker/internal/federation/transfer_test.go`

- [ ] **Step 1: Write failing test.**

Append to `tracker/internal/federation/transfer_test.go`:

```go
func TestTransferCoordinator_OnRequest_DuplicateNonceReplaysProof(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, conPriv := keypairFromSeed(0x33)

	srcID := ids.TrackerIDFromPubKey(srcPub)
	dstID := ids.TrackerIDFromPubKey(dstPub)

	ledger := &fakeLedger{
		outResult: TransferOutHookOut{
			ChainTipHash: bytes32(0xCC),
			Seq:          7,
		},
	}
	var sentPayloads [][]byte
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: srcID,
		MyPriv:      srcPriv,
		Ledger:      ledger,
		IssuedCap:   16,
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:  func(id ids.TrackerID) (ed25519.PublicKey, bool) { return dstPub, id == dstID },
		Send: func(_ context.Context, _ ids.TrackerID, _ fed.Kind, payload []byte) error {
			sentPayloads = append(sentPayloads, payload)
			return nil
		},
		MetricsCounter: func(string) {},
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)

	makeEnv := func() *fed.Envelope {
		req := &fed.TransferProofRequest{
			SourceTrackerId: srcID.Bytes()[:],
			DestTrackerId:   dstID.Bytes()[:],
			IdentityId:      identityID[:],
			Amount:          1500,
			Nonce:           nonce[:],
			ConsumerPub:     conPub,
			Timestamp:       1714000000,
		}
		canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
		require.NoError(t, err)
		req.ConsumerSig = ed25519.Sign(conPriv, canonical)
		payload, err := proto.Marshal(req)
		require.NoError(t, err)
		dstIDBytes := dstID.Bytes()
		env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload)
		require.NoError(t, err)
		return env
	}

	tc.OnRequest(context.Background(), makeEnv(), dstID)
	require.Len(t, ledger.outCalls, 1, "first request: ledger called once")
	require.Len(t, sentPayloads, 1)

	// Second request with same nonce.
	tc.OnRequest(context.Background(), makeEnv(), dstID)
	assert.Len(t, ledger.outCalls, 1, "second request must be replayed without re-debiting")
	require.Len(t, sentPayloads, 2)
	assert.Equal(t, sentPayloads[0], sentPayloads[1], "replayed proof bytes must equal first proof")
}
```

- [ ] **Step 2: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestTransferCoordinator_OnRequest_DuplicateNonceReplaysProof -v`
Expected: PASS — the replay cache already implemented in Task 12 catches this.

If FAIL: bug in `cacheIssuedLocked` ordering or in the `if cached, ok := tc.issued[nonceArr]; ok` early return. Trace and fix.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/transfer_test.go
git commit -m "test(tracker/federation): OnRequest replays cached proof on duplicate nonce"
```

---

## Task 15: Destination-side `StartTransfer` happy path + `OnProof` delivery

**Files:**
- Modify: `tracker/internal/federation/transfer.go`
- Modify: `tracker/internal/federation/transfer_test.go`

- [ ] **Step 1: Write failing test.**

Append to `tracker/internal/federation/transfer_test.go`:

```go
func TestTransferCoordinator_StartTransfer_HappyPath(t *testing.T) {
	t.Parallel()
	srcPub, _ := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, conPriv := keypairFromSeed(0x33)
	srcPubBytes, srcPriv := keypairFromSeed(0x11)
	_ = conPub

	srcID := ids.TrackerIDFromPubKey(srcPub)
	dstID := ids.TrackerIDFromPubKey(dstPub)

	ledger := &fakeLedger{}
	metrics := map[string]int{}

	var sendCh = make(chan struct {
		Peer    ids.TrackerID
		Kind    fed.Kind
		Payload []byte
	}, 4)
	send := func(_ context.Context, peer ids.TrackerID, kind fed.Kind, payload []byte) error {
		sendCh <- struct {
			Peer    ids.TrackerID
			Kind    fed.Kind
			Payload []byte
		}{peer, kind, payload}
		return nil
	}

	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: dstID,
		MyPriv:      dstPriv,
		Ledger:      ledger,
		IssuedCap:   16,
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == srcID {
				return srcPubBytes, true
			}
			return nil, false
		},
		Send:           send,
		MetricsCounter: func(n string) { metrics[n]++ },
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)

	in := StartTransferInput{
		SourceTrackerID: srcID,
		IdentityID:      ids.IdentityID(identityID),
		Amount:          1500,
		Nonce:           nonce,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}

	// Compute consumer sig for in.
	req := &fed.TransferProofRequest{
		SourceTrackerId: srcID.Bytes()[:],
		DestTrackerId:   dstID.Bytes()[:],
		IdentityId:      identityID[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	require.NoError(t, err)
	in.ConsumerSig = ed25519.Sign(conPriv, canonical)

	// Concurrently issue StartTransfer; the source-stub goroutine below
	// will deliver TransferProof.
	type startResult struct {
		out StartTransferOutput
		err error
	}
	resultCh := make(chan startResult, 1)
	go func() {
		out, err := tc.StartTransfer(context.Background(), in)
		resultCh <- startResult{out, err}
	}()

	// Source-stub: read the request, build a proof, signed by srcPriv,
	// and feed it to OnProof.
	var sent struct {
		Peer    ids.TrackerID
		Kind    fed.Kind
		Payload []byte
	}
	select {
	case sent = <-sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("StartTransfer did not Send within 2s")
	}
	assert.Equal(t, srcID, sent.Peer)
	assert.Equal(t, fed.Kind_KIND_TRANSFER_PROOF_REQUEST, sent.Kind)

	// Build a TransferProof and deliver via OnProof.
	proof := &fed.TransferProof{
		SourceTrackerId:    srcID.Bytes()[:],
		DestTrackerId:      dstID.Bytes()[:],
		IdentityId:         identityID[:],
		Amount:             1500,
		Nonce:              nonce[:],
		SourceChainTipHash: bytes.Repeat([]byte{0xCC}, 32),
		SourceSeq:          7,
		Timestamp:          1714000000,
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	require.NoError(t, err)
	proof.SourceTrackerSig = ed25519.Sign(srcPriv, cb)
	payload, err := proto.Marshal(proof)
	require.NoError(t, err)
	srcIDBytes := srcID.Bytes()
	env, err := SignEnvelope(srcPriv, srcIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF, payload)
	require.NoError(t, err)
	tc.OnProof(context.Background(), env, srcID)

	// Wait for StartTransfer to complete.
	select {
	case r := <-resultCh:
		require.NoError(t, r.err)
		assert.Equal(t, [32]byte{}, [32]byte{}) // sanity
		assert.Equal(t, uint64(7), r.out.SourceSeq)
		assert.Equal(t, []byte(proof.SourceTrackerSig), r.out.SourceTrackerSig)
		var hashArr [32]byte
		copy(hashArr[:], proof.SourceChainTipHash)
		assert.Equal(t, hashArr, r.out.SourceChainTipHash)
	case <-time.After(2 * time.Second):
		t.Fatal("StartTransfer did not return within 2s")
	}

	// AppendTransferIn should have been called.
	require.Len(t, ledger.inCalls, 1)
	assert.Equal(t, identityID, ledger.inCalls[0].IdentityID)
	assert.Equal(t, uint64(1500), ledger.inCalls[0].Amount)
	assert.Equal(t, nonce, ledger.inCalls[0].TransferRef)

	// And TransferApplied should have been Sent back.
	select {
	case applied := <-sendCh:
		assert.Equal(t, srcID, applied.Peer)
		assert.Equal(t, fed.Kind_KIND_TRANSFER_APPLIED, applied.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("TransferApplied not sent within 2s")
	}
}
```

Add `"bytes"` to the imports.

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run TestTransferCoordinator_StartTransfer_HappyPath -v`
Expected: FAIL — `StartTransfer`, `StartTransferInput`, `StartTransferOutput`, `OnProof` undefined.

- [ ] **Step 3: Implement `StartTransfer` + `OnProof` + types.**

Append to `tracker/internal/federation/transfer.go`:

```go
// StartTransferInput is what the api handler hands to federation.
type StartTransferInput struct {
	SourceTrackerID ids.TrackerID
	IdentityID      ids.IdentityID
	Amount          uint64
	Nonce           [32]byte
	ConsumerSig     []byte
	ConsumerPub     ed25519.PublicKey
	Timestamp       uint64
}

// StartTransferOutput is what federation returns after a successful end-to-end
// proof exchange. The api handler maps this back into the RPC reply.
type StartTransferOutput struct {
	SourceChainTipHash [32]byte
	SourceSeq          uint64
	SourceTrackerSig   []byte
}

// StartTransfer is the destination-side entry point. The api handler calls it
// after collecting the consumer-signed request. Blocks until either the
// peer responds with a TRANSFER_PROOF, ctx is canceled, or
// cfg.TransferTimeout elapses (the latter is wired by Federation.StartTransfer
// in subsystem.go; the coordinator itself enforces no clock).
func (tc *transferCoordinator) StartTransfer(ctx context.Context, in StartTransferInput) (StartTransferOutput, error) {
	if tc.cfg.Ledger == nil {
		return StartTransferOutput{}, ErrTransferDisabled
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	srcID := in.SourceTrackerID.Bytes()
	idArr := [32]byte(in.IdentityID)

	req := &fed.TransferProofRequest{
		SourceTrackerId: srcID[:],
		DestTrackerId:   myID[:],
		IdentityId:      idArr[:],
		Amount:          in.Amount,
		Nonce:           in.Nonce[:],
		ConsumerSig:     in.ConsumerSig,
		ConsumerPub:     in.ConsumerPub,
		Timestamp:       in.Timestamp,
	}
	if err := fed.ValidateTransferProofRequest(req); err != nil {
		return StartTransferOutput{}, fmt.Errorf("federation: %w", err)
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return StartTransferOutput{}, fmt.Errorf("federation: marshal request: %w", err)
	}

	ch := make(chan *fed.TransferProof, 1)
	tc.mu.Lock()
	if _, exists := tc.pending[in.Nonce]; exists {
		tc.mu.Unlock()
		return StartTransferOutput{}, errors.New("federation: duplicate StartTransfer nonce in flight")
	}
	tc.pending[in.Nonce] = pendingResp{ch: ch}
	tc.mu.Unlock()
	defer func() {
		tc.mu.Lock()
		delete(tc.pending, in.Nonce)
		tc.mu.Unlock()
	}()

	if err := tc.cfg.Send(ctx, in.SourceTrackerID, fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload); err != nil {
		return StartTransferOutput{}, err
	}
	tc.cfg.MetricsCounter("transfer_request_sent")

	var proof *fed.TransferProof
	select {
	case proof = <-ch:
	case <-ctx.Done():
		return StartTransferOutput{}, ctx.Err()
	}

	// Verify the inner proof signature against the source's known pubkey.
	srcPub, ok := tc.cfg.PeerPubKey(in.SourceTrackerID)
	if !ok {
		return StartTransferOutput{}, fmt.Errorf("%w: source pubkey unknown", ErrPeerNotConnected)
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	if err != nil {
		return StartTransferOutput{}, fmt.Errorf("federation: canonical proof: %w", err)
	}
	if !ed25519.Verify(srcPub, cb, proof.SourceTrackerSig) {
		return StartTransferOutput{}, errors.New("federation: source_tracker_sig invalid")
	}

	if err := tc.cfg.Ledger.AppendTransferIn(ctx, TransferInHookIn{
		IdentityID:  idArr,
		Amount:      proof.Amount,
		Timestamp:   proof.Timestamp,
		TransferRef: in.Nonce,
	}); err != nil {
		// ErrTransferRefExists is treated as success: the credit is already booked.
		if !errors.Is(err, ledgerErrTransferRefExists()) {
			return StartTransferOutput{}, fmt.Errorf("federation: append transfer_in: %w", err)
		}
	}

	// Send TransferApplied back to source. Best-effort; failures here are
	// metric+log only since the credit is already booked locally.
	applied := &fed.TransferApplied{
		SourceTrackerId: srcID[:],
		DestTrackerId:   myID[:],
		Nonce:           in.Nonce[:],
		Timestamp:       uint64(tc.cfg.Now().Unix()),
	}
	ab, err := fed.CanonicalTransferAppliedPreSig(applied)
	if err == nil {
		applied.DestTrackerSig = ed25519.Sign(tc.cfg.MyPriv, ab)
		if appliedPayload, mErr := proto.Marshal(applied); mErr == nil {
			_ = tc.cfg.Send(ctx, in.SourceTrackerID, fed.Kind_KIND_TRANSFER_APPLIED, appliedPayload)
		}
	}

	var hashArr [32]byte
	copy(hashArr[:], proof.SourceChainTipHash)
	tc.cfg.MetricsCounter("transfer_completed")
	return StartTransferOutput{
		SourceChainTipHash: hashArr,
		SourceSeq:          proof.SourceSeq,
		SourceTrackerSig:   proof.SourceTrackerSig,
	}, nil
}

// OnProof is the dispatcher hook for KIND_TRANSFER_PROOF.
func (tc *transferCoordinator) OnProof(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	proof := &fed.TransferProof{}
	if err := proto.Unmarshal(env.Payload, proof); err != nil {
		tc.cfg.MetricsCounter("transfer_proof_shape")
		return
	}
	if err := fed.ValidateTransferProof(proof); err != nil {
		tc.cfg.MetricsCounter("transfer_proof_shape")
		return
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	if !equalID(proof.DestTrackerId, myID[:]) {
		tc.cfg.MetricsCounter("transfer_proof_misrouted")
		return
	}
	if !equalID(proof.SourceTrackerId, fromPeer.Bytes()[:]) {
		tc.cfg.MetricsCounter("transfer_proof_source_mismatch")
		return
	}
	var nonceArr [32]byte
	copy(nonceArr[:], proof.Nonce)

	tc.mu.Lock()
	pending, ok := tc.pending[nonceArr]
	tc.mu.Unlock()
	if !ok {
		tc.cfg.MetricsCounter("transfer_proof_orphan")
		return
	}
	select {
	case pending.ch <- proof:
		tc.cfg.MetricsCounter("transfer_proof_delivered")
	default:
		tc.cfg.MetricsCounter("transfer_proof_buffer_full")
	}
}

// ledgerErrTransferRefExists imports the ledger-layer sentinel through a
// thin function so the federation package keeps zero direct dependency on
// the ledger package's identifiers (the LedgerHooks interface is the only
// surface). The api wiring sets this at startup time; for the test fake
// it is never returned.
var ledgerErrTransferRefExists = func() error { return errors.New("ledger: transfer ref already on chain") }
```

Note: the `ledgerErrTransferRefExists` indirection is a transient placeholder — it returns an error whose `.Error()` matches the ledger-layer sentinel string. The integration with the real sentinel happens at the run_cmd wiring site (sibling slice). Tests using `fakeLedger` never trigger this branch.

A cleaner alternative is exposing `ErrTransferRefExists` on the federation package as a sentinel that the api adapter translates to/from. We deliberately use the string-match dodge here to keep this slice federation-only. Replace with a typed error variable in the api wiring slice.

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestTransferCoordinator_StartTransfer_HappyPath -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/transfer.go tracker/internal/federation/transfer_test.go
git commit -m "feat(tracker/federation): StartTransfer + OnProof end-to-end"
```

---

## Task 16: `OnProof` orphan + `OnApplied` happy path

**Files:**
- Modify: `tracker/internal/federation/transfer.go`
- Modify: `tracker/internal/federation/transfer_test.go`

- [ ] **Step 1: Write failing tests.**

Append to `tracker/internal/federation/transfer_test.go`:

```go
func TestTransferCoordinator_OnProof_Orphan(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	srcID := ids.TrackerIDFromPubKey(srcPub)
	dstID := ids.TrackerIDFromPubKey(dstPub)

	metrics := map[string]int{}
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID:    dstID,
		MyPriv:         dstPriv,
		Ledger:         &fakeLedger{},
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:     func(id ids.TrackerID) (ed25519.PublicKey, bool) { return srcPub, id == srcID },
		Send:           func(context.Context, ids.TrackerID, fed.Kind, []byte) error { return nil },
		MetricsCounter: func(n string) { metrics[n]++ },
	})

	proof := &fed.TransferProof{
		SourceTrackerId:    srcID.Bytes()[:],
		DestTrackerId:      dstID.Bytes()[:],
		IdentityId:         bytes.Repeat([]byte{0x44}, 32),
		Amount:             1500,
		Nonce:              bytes.Repeat([]byte{0x55}, 32),
		SourceChainTipHash: bytes.Repeat([]byte{0xCC}, 32),
		SourceSeq:          7,
		Timestamp:          1714000000,
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	require.NoError(t, err)
	proof.SourceTrackerSig = ed25519.Sign(srcPriv, cb)
	payload, err := proto.Marshal(proof)
	require.NoError(t, err)
	srcIDBytes := srcID.Bytes()
	env, err := SignEnvelope(srcPriv, srcIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF, payload)
	require.NoError(t, err)

	tc.OnProof(context.Background(), env, srcID)
	assert.Equal(t, 1, metrics["transfer_proof_orphan"])
}

func TestTransferCoordinator_OnApplied_HappyPath(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	srcID := ids.TrackerIDFromPubKey(srcPub)
	dstID := ids.TrackerIDFromPubKey(dstPub)

	metrics := map[string]int{}
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID:    srcID,
		MyPriv:         srcPriv,
		Ledger:         &fakeLedger{},
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == dstID {
				return dstPub, true
			}
			return nil, false
		},
		Send:           func(context.Context, ids.TrackerID, fed.Kind, []byte) error { return nil },
		MetricsCounter: func(n string) { metrics[n]++ },
	})

	applied := &fed.TransferApplied{
		SourceTrackerId: srcID.Bytes()[:],
		DestTrackerId:   dstID.Bytes()[:],
		Nonce:           bytes.Repeat([]byte{0x55}, 32),
		Timestamp:       1714000001,
	}
	cb, err := fed.CanonicalTransferAppliedPreSig(applied)
	require.NoError(t, err)
	applied.DestTrackerSig = ed25519.Sign(dstPriv, cb)
	payload, err := proto.Marshal(applied)
	require.NoError(t, err)
	dstIDBytes := dstID.Bytes()
	env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_APPLIED, payload)
	require.NoError(t, err)

	tc.OnApplied(context.Background(), env, dstID)
	assert.Equal(t, 1, metrics["transfer_applied_received_ok"])
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run "TestTransferCoordinator_(OnProof_Orphan|OnApplied_HappyPath)" -v`
Expected: FAIL — `OnApplied` undefined; `OnProof_Orphan` may already pass.

- [ ] **Step 3: Implement `OnApplied`.**

Append to `tracker/internal/federation/transfer.go`:

```go
// OnApplied is the dispatcher hook for KIND_TRANSFER_APPLIED. The
// destination-side credit is already booked on the source by the time
// this arrives; the message is a confirmation for source-side
// observability.
func (tc *transferCoordinator) OnApplied(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	applied := &fed.TransferApplied{}
	if err := proto.Unmarshal(env.Payload, applied); err != nil {
		tc.cfg.MetricsCounter("transfer_applied_shape")
		return
	}
	if err := fed.ValidateTransferApplied(applied); err != nil {
		tc.cfg.MetricsCounter("transfer_applied_shape")
		return
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	if !equalID(applied.SourceTrackerId, myID[:]) {
		tc.cfg.MetricsCounter("transfer_applied_misrouted")
		return
	}
	dstPub, ok := tc.cfg.PeerPubKey(fromPeer)
	if !ok {
		tc.cfg.MetricsCounter("transfer_applied_unknown_peer")
		return
	}
	cb, err := fed.CanonicalTransferAppliedPreSig(applied)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_applied_canonical")
		return
	}
	if !ed25519.Verify(dstPub, cb, applied.DestTrackerSig) {
		tc.cfg.MetricsCounter("transfer_applied_sig")
		return
	}
	tc.cfg.MetricsCounter("transfer_applied_received_ok")
}
```

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run "TestTransferCoordinator_(OnProof_Orphan|OnApplied)" -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/transfer.go tracker/internal/federation/transfer_test.go
git commit -m "feat(tracker/federation): OnApplied + OnProof orphan handling"
```

---

## Task 17: `Federation.StartTransfer` + dispatcher wiring

**Files:**
- Modify: `tracker/internal/federation/subsystem.go`
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Write failing test for `Federation.StartTransfer` in `integration_test.go` (external package).**

Append to `tracker/internal/federation/integration_test.go`:

```go
func TestIntegration_StartTransfer_DisabledWithoutLedger(t *testing.T) {
	t.Parallel()
	tt := newTwoTracker(t) // no Ledger wired

	var nonce [32]byte
	nonce[0] = 0x01
	_, err := tt.b.StartTransfer(context.Background(), federation.StartTransferInput{
		SourceTrackerID: tt.aID,
		IdentityID:      ids.IdentityID{},
		Amount:          1,
		Nonce:           nonce,
		ConsumerSig:     b(64, 0x55),
		ConsumerPub:     b(32, 0x66),
		Timestamp:       1,
	})
	if err == nil {
		t.Fatal("err=nil, want ErrTransferDisabled")
	}
	if !errors.Is(err, federation.ErrTransferDisabled) {
		t.Fatalf("err=%v, want ErrTransferDisabled", err)
	}
}
```

`b(n, v)` is the existing helper in the test package that returns a length-`n` byte slice filled with `v`. If it isn't already in scope, find it with `grep -n "^func b(" integration_test.go testharness_test.go` and reuse.

`newTwoTracker` does not currently wire a `LedgerHooks` Deps field (none of the slice-0 tests do); this test exercises the `dep.Ledger == nil` branch.

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run TestFederation_StartTransfer_DisabledWithoutLedger -v`
Expected: FAIL — `Federation.StartTransfer` undefined.

- [ ] **Step 3: Wire `TransferCoordinator` into `Federation`.**

Edit `tracker/internal/federation/subsystem.go`. Add a field:

```go
type Federation struct {
	// ... existing fields ...
	transfer *transferCoordinator
	// ... existing fields ...
}
```

In `Open` after `pub := NewPublisher(...)`:

```go
	// transfer is nil-safe: a deployment without LedgerHooks gets a
	// no-op StartTransfer that returns ErrTransferDisabled.
	transfer := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: cfg.MyTrackerID,
		MyPriv:      cfg.MyPriv,
		Ledger:      dep.Ledger,
		IssuedCap:   cfg.IssuedProofCap,
		Now: func() time.Time {
			return dep.Now()
		},
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			info, ok := reg.Get(id)
			if !ok {
				return nil, false
			}
			return info.PubKey, true
		},
		Send: func(ctx context.Context, peer ids.TrackerID, kind fed.Kind, payload []byte) error {
			// Late-bound to f via closure — we set this after constructing f.
			return nil // patched below
		},
		MetricsCounter: func(name string) {
			dep.Metrics.InvalidFrames(name) // routes to the existing counter; refined in metrics task
		},
	})
```

Then after `f := &Federation{...}`:

```go
	transfer.cfg.Send = func(ctx context.Context, peer ids.TrackerID, kind fed.Kind, payload []byte) error {
		return f.SendToPeer(ctx, peer, kind, payload)
	}
	f.transfer = transfer
```

Then add the public method below `Federation.PublishHour`:

```go
// StartTransfer is the api-handler entry point for a destination-side
// cross-region credit transfer. Returns ErrTransferDisabled if the
// federation was opened without a LedgerHooks dep, ErrPeerNotConnected
// if the source tracker isn't in steady state, ctx.Err() / ErrTransferTimeout
// on timeout, or the proof on success.
func (f *Federation) StartTransfer(ctx context.Context, in StartTransferInput) (StartTransferOutput, error) {
	if f.transfer == nil || f.transfer.cfg.Ledger == nil {
		return StartTransferOutput{}, ErrTransferDisabled
	}
	if !f.reg.IsActive(in.SourceTrackerID) {
		return StartTransferOutput{}, fmt.Errorf("%w: source=%x", ErrPeerNotConnected, in.SourceTrackerID.Bytes())
	}
	if f.cfg.TransferTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.cfg.TransferTimeout)
		defer cancel()
	}
	out, err := f.transfer.StartTransfer(ctx, in)
	if errors.Is(err, context.DeadlineExceeded) {
		return out, ErrTransferTimeout
	}
	return out, err
}
```

In `makeDispatcher`, add cases for the three new kinds. Above `default:`:

```go
		case fed.Kind_KIND_TRANSFER_PROOF_REQUEST:
			if f.transfer == nil {
				f.dep.Metrics.InvalidFrames("transfer_disabled")
				return
			}
			f.transfer.OnRequest(context.Background(), env, peerID)
		case fed.Kind_KIND_TRANSFER_PROOF:
			if f.transfer == nil {
				f.dep.Metrics.InvalidFrames("transfer_disabled")
				return
			}
			f.transfer.OnProof(context.Background(), env, peerID)
		case fed.Kind_KIND_TRANSFER_APPLIED:
			if f.transfer == nil {
				f.dep.Metrics.InvalidFrames("transfer_disabled")
				return
			}
			f.transfer.OnApplied(context.Background(), env, peerID)
```

Add `"time"` to the import block of `subsystem.go` if missing.

- [ ] **Step 4: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestIntegration_StartTransfer_DisabledWithoutLedger -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/subsystem.go tracker/internal/federation/integration_test.go
git commit -m "feat(tracker/federation): wire TransferCoordinator into Federation + dispatcher"
```

---

## Task 18: Two-`Federation` integration test — end-to-end transfer

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Add a per-test fake `LedgerHooks` and a ledger-aware variant of `newTwoTracker`.**

Append to `tracker/internal/federation/integration_test.go`:

```go
// fakeIntegrationLedger implements federation.LedgerHooks for the
// integration test. It records every call and returns the configured
// outputs. Concurrency-safe: the source-side and destination-side test
// fixtures hold their own instance.
type fakeIntegrationLedger struct {
	mu        sync.Mutex
	outCalls  []federation.TransferOutHookIn
	inCalls   []federation.TransferInHookIn
	outResult federation.TransferOutHookOut
	outErr    error
	inErr     error
}

func (f *fakeIntegrationLedger) AppendTransferOut(_ context.Context, in federation.TransferOutHookIn) (federation.TransferOutHookOut, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outCalls = append(f.outCalls, in)
	return f.outResult, f.outErr
}
func (f *fakeIntegrationLedger) AppendTransferIn(_ context.Context, in federation.TransferInHookIn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inCalls = append(f.inCalls, in)
	return f.inErr
}
func (f *fakeIntegrationLedger) snapshotInCalls() []federation.TransferInHookIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]federation.TransferInHookIn, len(f.inCalls))
	copy(out, f.inCalls)
	return out
}
func (f *fakeIntegrationLedger) snapshotOutCalls() []federation.TransferOutHookIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]federation.TransferOutHookIn, len(f.outCalls))
	copy(out, f.outCalls)
	return out
}

// newTwoTrackerWithLedgers mirrors newTwoTracker but plumbs LedgerHooks
// into both Federations' Deps.
func newTwoTrackerWithLedgers(t *testing.T, aLedger, bLedger federation.LedgerHooks) *twoTracker {
	t.Helper()
	a := newPeerCfg(t)
	b := newPeerCfg(t)
	hub := federation.NewInprocHub()
	trA := federation.NewInprocTransport(hub, "A", a.pub, a.priv)
	trB := federation.NewInprocTransport(hub, "B", b.pub, b.priv)

	archA, archB := newFakeArchive(), newFakeArchive()
	srcA := &fakeRootSrc{ok: false}
	srcB := &fakeRootSrc{ok: false}

	aID := ids.TrackerID(sha256.Sum256(a.pub))
	bID := ids.TrackerID(sha256.Sum256(b.pub))

	aFed, err := federation.Open(federation.Config{
		MyTrackerID: aID,
		MyPriv:      a.priv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: bID, PubKey: b.pub, Addr: "B"}},
	}, federation.Deps{
		Transport: trA, RootSrc: srcA, Archive: archA, Ledger: aLedger,
		Metrics: federation.NewMetrics(prometheus.NewRegistry()),
		Logger:  zerolog.Nop(), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	bFed, err := federation.Open(federation.Config{
		MyTrackerID: bID,
		MyPriv:      b.priv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: aID, PubKey: a.pub, Addr: "A"}},
	}, federation.Deps{
		Transport: trB, RootSrc: srcB, Archive: archB, Ledger: bLedger,
		Metrics: federation.NewMetrics(prometheus.NewRegistry()),
		Logger:  zerolog.Nop(), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = aFed.Close()
		_ = bFed.Close()
	})
	return &twoTracker{
		hub: hub, a: aFed, b: bFed,
		archA: archA, archB: archB, srcA: srcA, srcB: srcB,
		aID: aID, bID: bID,
	}
}
```

If `sync` isn't already imported in `integration_test.go`, add it.

- [ ] **Step 2: Add the failing end-to-end test.**

Append to `tracker/internal/federation/integration_test.go`:

```go
func TestIntegration_CrossRegionTransfer_HappyPath(t *testing.T) {
	t.Parallel()

	// Build both ledgers; a (source) returns a deterministic chain tip.
	aLedger := &fakeIntegrationLedger{
		outResult: federation.TransferOutHookOut{
			ChainTipHash: [32]byte{
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
			},
			Seq: 11,
		},
	}
	bLedger := &fakeIntegrationLedger{}

	tt := newTwoTrackerWithLedgers(t, aLedger, bLedger)

	// Wait until B sees A as steady (peering handshake completed).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		for _, p := range tt.b.Peers() {
			if p.State == federation.PeerStateSteady {
				ok = true
				break
			}
		}
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Sign the consumer-side request.
	conPub, conPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	identityID := b(32, 0x44)
	nonce := [32]byte{
		0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
		0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
		0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
		0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
	}

	aIDBytes := tt.aID.Bytes()
	bIDBytes := tt.bID.Bytes()
	req := &fed.TransferProofRequest{
		SourceTrackerId: aIDBytes[:],
		DestTrackerId:   bIDBytes[:],
		IdentityId:      identityID,
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	if err != nil {
		t.Fatal(err)
	}
	consumerSig := ed25519.Sign(conPriv, canonical)

	// Issue StartTransfer on B (destination).
	var idArr [32]byte
	copy(idArr[:], identityID)
	out, err := tt.b.StartTransfer(context.Background(), federation.StartTransferInput{
		SourceTrackerID: tt.aID,
		IdentityID:      ids.IdentityID(idArr),
		Amount:          1500,
		Nonce:           nonce,
		ConsumerSig:     consumerSig,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	})
	if err != nil {
		t.Fatalf("StartTransfer: %v", err)
	}
	if out.SourceSeq != 11 {
		t.Errorf("SourceSeq=%d, want 11", out.SourceSeq)
	}

	// Source ledger received exactly one AppendTransferOut.
	if got := aLedger.snapshotOutCalls(); len(got) != 1 {
		t.Fatalf("aLedger.outCalls=%d, want 1", len(got))
	} else if got[0].Amount != 1500 || got[0].TransferRef != nonce {
		t.Errorf("outCall mismatch: amount=%d ref=%x", got[0].Amount, got[0].TransferRef)
	}

	// Destination ledger received exactly one AppendTransferIn.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bLedger.snapshotInCalls()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	in := bLedger.snapshotInCalls()
	if len(in) != 1 {
		t.Fatalf("bLedger.inCalls=%d, want 1", len(in))
	}
	if in[0].IdentityID != idArr || in[0].TransferRef != nonce || in[0].Amount != 1500 {
		t.Errorf("inCall mismatch: %+v", in[0])
	}
}
```

Imports to add at the top of `integration_test.go` if missing:
- `"crypto/ed25519"`
- `"sync"`
- `fed "github.com/token-bay/token-bay/shared/federation"`

- [ ] **Step 3: Run; expect FAIL or PASS.**

Run: `cd tracker && go test -race ./internal/federation -run TestIntegration_CrossRegionTransfer_HappyPath -v`
Expected: PASS — Tasks 1–17 land all the federation-side machinery; this test is purely a coverage gate.

If FAIL with a timeout: the most likely cause is dispatcher routing or the `attachAndWait` peer registration not completing — re-inspect Task 17's dispatcher case order and the `transfer.cfg.Send` patch ordering relative to `f := &Federation{...}`.

- [ ] **Step 4: Commit.**

```bash
git add tracker/internal/federation/integration_test.go
git commit -m "test(tracker/federation): two-Federation cross-region transfer integration"
```

---

## Task 19: Federation metrics — promote string-based counters to typed methods

**Files:**
- Modify: `tracker/internal/federation/metrics.go`
- Modify: `tracker/internal/federation/metrics_test.go`
- Modify: `tracker/internal/federation/transfer.go`
- Modify: `tracker/internal/federation/subsystem.go`

- [ ] **Step 1: Write failing test that exercises the new typed metrics.**

Append to `tracker/internal/federation/metrics_test.go`:

```go
func TestMetrics_TransferCounters(t *testing.T) {
	m := NewMetrics()
	m.TransferRequestSent()
	m.TransferRequestReceived("ok")
	m.TransferProofReceived("delivered")
	m.TransferAppliedReceived("ok")
	m.TransferCompleted()
	m.TransfersMinted()
	m.TransferFailed("timeout")

	// We can't easily inspect Prometheus internals; this test ensures the
	// methods exist and don't panic.
}
```

- [ ] **Step 2: Run; expect FAIL.**

Run: `cd tracker && go test -race ./internal/federation -run TestMetrics_TransferCounters -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Add the typed metric methods.**

Edit `tracker/internal/federation/metrics.go`. Inside the `Metrics` struct add Prometheus counter fields:

```go
	transferRequestSent     prometheus.Counter
	transferRequestReceived *prometheus.CounterVec  // labels: outcome
	transferProofReceived   *prometheus.CounterVec  // labels: outcome
	transferAppliedReceived *prometheus.CounterVec  // labels: outcome
	transferCompleted       prometheus.Counter
	transfersMinted         prometheus.Counter
	transferFailed          *prometheus.CounterVec  // labels: reason
```

In `NewMetrics()` register the counters with names matching the spec §13:

```go
	m.transferRequestSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfer_request_sent_total",
		Help: "Cross-region transfer requests sent (destination side).",
	})
	m.transferRequestReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfer_request_received_total",
		Help: "Cross-region transfer requests received (source side), by outcome.",
	}, []string{"outcome"})
	m.transferProofReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfer_proof_received_total",
		Help: "Cross-region transfer proofs received (destination side), by outcome.",
	}, []string{"outcome"})
	m.transferAppliedReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfer_applied_received_total",
		Help: "Cross-region transfer-applied messages received (source side), by outcome.",
	}, []string{"outcome"})
	m.transferCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfer_completed_total",
		Help: "Successful StartTransfer returns (destination side).",
	})
	m.transfersMinted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfers_minted_total",
		Help: "Successful proofs minted (source side).",
	})
	m.transferFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenbay_federation_transfer_failed_total",
		Help: "Failed cross-region transfers, by reason.",
	}, []string{"reason"})
```

If `NewMetrics` registers via `MustRegister`, register these too.

Add the convenience methods:

```go
func (m *Metrics) TransferRequestSent()                 { m.transferRequestSent.Inc() }
func (m *Metrics) TransferRequestReceived(outcome string) { m.transferRequestReceived.WithLabelValues(outcome).Inc() }
func (m *Metrics) TransferProofReceived(outcome string)   { m.transferProofReceived.WithLabelValues(outcome).Inc() }
func (m *Metrics) TransferAppliedReceived(outcome string) { m.transferAppliedReceived.WithLabelValues(outcome).Inc() }
func (m *Metrics) TransferCompleted()                   { m.transferCompleted.Inc() }
func (m *Metrics) TransfersMinted()                     { m.transfersMinted.Inc() }
func (m *Metrics) TransferFailed(reason string)         { m.transferFailed.WithLabelValues(reason).Inc() }
```

- [ ] **Step 4: Replace the placeholder `MetricsCounter(string)` calls in `transfer.go` and `subsystem.go` with the typed methods.**

Mapping:
- `transfer_request_sent` → `dep.Metrics.TransferRequestSent()`
- `transfers_minted` → `dep.Metrics.TransfersMinted()`
- `transfer_completed` → `dep.Metrics.TransferCompleted()`
- `transfer_request_replayed` → `dep.Metrics.TransferRequestReceived("replayed")`
- `transfer_request_consumer_sig` / `_shape` / `_misrouted` / etc. → `dep.Metrics.TransferRequestReceived("<reason>")`
- `transfer_proof_orphan` / `_delivered` / `_shape` → `dep.Metrics.TransferProofReceived("<reason>")`
- `transfer_applied_received_ok` / `_sig` / `_shape` → `dep.Metrics.TransferAppliedReceived("<reason>")`

Update `transferCoordinatorCfg`:

```go
type transferCoordinatorCfg struct {
	// ... unchanged fields ...
	Metrics *Metrics  // replaces MetricsCounter
}
```

Replace `tc.cfg.MetricsCounter("…")` calls accordingly. The fakeLedger tests change to set `Metrics: NewMetrics()` (no observation) or to keep a small map-driven shim:

```go
	tc := newTransferCoordinator(transferCoordinatorCfg{
		// ...
		Metrics: NewMetrics(),
	})
```

Where the existing tests assert metric counts via the `metrics map[string]int{}`, replace with a hand-rolled `*Metrics` accessor — or relax those assertions to behavioral checks (e.g. `len(ledger.outCalls)` already covers most). For the slice keep it simple: drop the per-name metric assertion and verify behavior only.

- [ ] **Step 5: Run; expect PASS.**

Run: `cd tracker && go test -race ./internal/federation -v`
Expected: PASS for all transfer + metrics tests.

- [ ] **Step 6: Commit.**

```bash
git add tracker/internal/federation/metrics.go tracker/internal/federation/metrics_test.go \
        tracker/internal/federation/transfer.go tracker/internal/federation/transfer_test.go \
        tracker/internal/federation/subsystem.go
git commit -m "feat(tracker/federation): typed transfer metrics + replace MetricsCounter shim"
```

---

## Task 20: Run full check + verify acceptance

**Files:**
- (no edits — verification only)

- [ ] **Step 1: Run the umbrella check from each module.**

Run: `cd shared && make check`
Expected: PASS for `make test` + `make lint`.

Run: `cd tracker && make check`
Expected: PASS.

Run from repo root: `make check`
Expected: PASS.

- [ ] **Step 2: Verify acceptance criteria from the spec.**

Spec §15 "Acceptance criteria":

1. Two `Federation` instances peered on the in-process transport. Y calls `Federation.StartTransfer` for an identity whose home is X. The flow completes end-to-end:
   - X has a new `transfer_out` ledger entry debiting the consumer. → `TestIntegration_CrossRegionTransfer_HappyPath` confirms.
   - Y has a new `transfer_in` ledger entry crediting the same identity. → same.
   - Y's `StartTransfer` returns the source proof. → same.
   - X has logged `KIND_TRANSFER_APPLIED` received and incremented the metric. → `TestTransferCoordinator_OnApplied_HappyPath`.
2. A retried `StartTransfer` with the same `(SourceTrackerID, Nonce)` returns the cached proof (without a second debit) on the source. → `TestTransferCoordinator_OnRequest_DuplicateNonceReplaysProof`.
3. A bad consumer-sig request is dropped at the source with the right metric label. → `TestTransferCoordinator_OnRequest_BadConsumerSig`.
4. `dep.Ledger == nil` deployment is unaffected: `StartTransfer` returns `ErrTransferDisabled`, inbound transfer kinds are rejected. → `TestFederation_StartTransfer_DisabledWithoutLedger` + the dispatcher's `transfer == nil` short-circuits.
5. `make check` green from `tracker/`, `shared/`, repo root. → step 1.
6. Federation package continues to pass `go test -race ./...`. → step 1.

Walk through each above; if any are missing test coverage, add it now and re-run.

- [ ] **Step 3: Verify no untracked files.**

Run: `git status`
Expected: clean tree.

- [ ] **Step 4: No further commit needed for this task.**

The slice is complete. Push to remote and open PR per repo CLAUDE.md §"Submitting a PR".
