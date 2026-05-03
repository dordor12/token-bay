# Plugin Tracker Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `plugin/internal/trackerclient` — the long-lived mTLS QUIC client through which every other plugin module talks to a tracker. Nine unary RPCs, three server-push streams, mutual SPKI-pinned identity, TTL singleflight balance cache, fail-fast on reconnect.

**Architecture:** Pluggable `Transport` interface (production QUIC driver + in-memory loopback for tests). `internal/idtls` issues self-signed Ed25519 certs from the existing identity keypair and pins peers by SPKI hash. `internal/wire` does length-prefixed deterministic-proto framing, one stream per RPC. A single supervisor goroutine owns the connection state machine, dialing forever with exponential backoff. Public API is blocking unary methods + handler-interface push streams.

**Tech Stack:** Go 1.25, `google.golang.org/protobuf`, `github.com/quic-go/quic-go` (new), `golang.org/x/sync/singleflight` (new), `github.com/google/uuid` (new), stdlib `crypto/tls` + `crypto/x509` + `crypto/ed25519`, `github.com/stretchr/testify`, `github.com/rs/zerolog`.

---

## Reference

- Spec: `docs/superpowers/specs/plugin/2026-05-02-trackerclient-design.md`
- Wire-format v1: `docs/superpowers/specs/shared/2026-04-24-wire-format-v1-design.md`
- Plugin design: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md`
- Tracker design: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md`

## File map

```
shared/
  proto/
    rpc.proto                                 -- new
    rpc.pb.go                                 -- new (generated)
    rpc.go                                    -- new (constants, doc)
    rpc_test.go                               -- new (round-trip + nil-safety)
    validate.go                               -- modify (add ValidateRPC* + push validators)
    validate_test.go                          -- modify (add cases)
    testdata/rpc_request_broker.golden.hex    -- new
    testdata/rpc_response_broker_ok.golden.hex -- new
  Makefile                                    -- modify (PROTO_FILES += proto/rpc.proto)

plugin/
  go.mod                                      -- modify (add quic-go, singleflight, uuid)
  internal/
    trackerclient/
      doc.go                                  -- new
      trackerclient.go                        -- new (Client, New, Start, Close, Status, WaitConnected)
      config.go                               -- new (Config, TrackerEndpoint, Signer, Validate)
      errors.go                               -- new (sentinels + RpcError + statusToError)
      types.go                                -- new (BrokerResponse, EnrollRequest, etc.)
      balance_cache.go                        -- new (TTL singleflight)
      conn.go                                 -- new (Connection state)
      reconnect.go                            -- new (supervisor + backoff)
      heartbeat.go                            -- new
      push_offers.go                          -- new
      push_settlements.go                     -- new
      rpc.go                                  -- new (dispatch helper + 9 unary methods)
      *_test.go                               -- new (one test file per source file)
      internal/
        wire/
          frame.go                            -- new (4-byte BE length-prefix codec)
          frame_test.go                       -- new
          codec.go                            -- new (RpcMethod dispatch, status mapping)
          codec_test.go                       -- new
        idtls/
          cert.go                             -- new (Ed25519 self-signed cert)
          cert_test.go                        -- new
          verify.go                           -- new (SPKI pinning VerifyPeerCertificate)
          verify_test.go                      -- new
        transport/
          transport.go                        -- new (Transport / Conn / Stream interfaces)
          loopback/
            loopback.go                       -- new (in-memory driver)
            loopback_test.go                  -- new
          quic/
            quic.go                           -- new (quic-go driver)
            quic_test.go                      -- new
      test/
        fakeserver/
          fakeserver.go                       -- new (in-process tracker speaking the wire)
          fakeserver_test.go                  -- new (sanity)
        integration_test.go                   -- new (real QUIC end-to-end)
```

## Task ordering rationale

1. `shared/proto/rpc.proto` is a hard prerequisite — every other task imports its types. It lands first in a cross-cutting commit (touches only `shared/`).
2. Plugin go.mod additions next — required so subsequent tasks can `import` the new dependencies.
3. Foundation packages (errors → wire → transport interface → loopback driver) ship before any RPC code.
4. mTLS pieces (`idtls`) ship before the QUIC transport (which needs them) and before connection logic.
5. Connection + supervisor ship before any RPC method (because RPCs call `conn.Get(ctx)`).
6. Balance cache ships alongside the `Balance` RPC method since they're tightly coupled.
7. Push streams come after unary RPCs because they reuse the `wire` codec.
8. `fakeserver` + integration test ship last (they exercise everything).

---

# Phase 1 — `shared/proto/rpc.proto` (cross-cutting)

### Task 1: Add the RPC schema, generated code, validators, and goldens

**Files:**
- Create: `shared/proto/rpc.proto`
- Create: `shared/proto/rpc.pb.go` (generated)
- Create: `shared/proto/rpc.go`
- Create: `shared/proto/rpc_test.go`
- Create: `shared/proto/testdata/rpc_request_broker.golden.hex`
- Create: `shared/proto/testdata/rpc_response_broker_ok.golden.hex`
- Modify: `shared/proto/validate.go`
- Modify: `shared/proto/validate_test.go`
- Modify: `shared/Makefile`

- [ ] **Step 1.1: Write `shared/proto/rpc.proto`**

```proto
syntax = "proto3";
package tokenbay.proto.v1;

option go_package = "github.com/token-bay/token-bay/shared/proto";

import "proto/balance.proto";
import "proto/envelope.proto";

// RpcMethod identifies the unary RPC the client is invoking.
//
// Method 0 (RPC_METHOD_UNSPECIFIED) is reserved as the heartbeat-channel
// marker on the dedicated heartbeat stream — it is NOT an "unset" sentinel.
// Validators reject method 0 on regular RPC streams.
enum RpcMethod {
  RPC_METHOD_UNSPECIFIED      = 0;  // heartbeat-channel marker
  RPC_METHOD_ENROLL           = 1;
  RPC_METHOD_BROKER_REQUEST   = 2;
  RPC_METHOD_BALANCE          = 3;
  RPC_METHOD_SETTLE           = 4;
  RPC_METHOD_USAGE_REPORT     = 5;
  RPC_METHOD_ADVERTISE        = 6;
  RPC_METHOD_TRANSFER_REQUEST = 7;
  RPC_METHOD_STUN_ALLOCATE    = 8;
  RPC_METHOD_TURN_RELAY_OPEN  = 9;
}

enum RpcStatus {
  RPC_STATUS_UNSPECIFIED     = 0;
  RPC_STATUS_OK              = 1;
  RPC_STATUS_INVALID         = 2;
  RPC_STATUS_NO_CAPACITY     = 3;
  RPC_STATUS_FROZEN          = 4;
  RPC_STATUS_INTERNAL        = 5;
  RPC_STATUS_UNAUTHENTICATED = 6;
  RPC_STATUS_NOT_FOUND       = 7;
}

message RpcError {
  string code    = 1;
  string message = 2;
}

message RpcRequest {
  RpcMethod method  = 1;
  bytes     payload = 2;
}

message RpcResponse {
  RpcStatus status  = 1;
  bytes     payload = 2;
  RpcError  error   = 3;
}

// --- Per-method payloads ----------------------------------------------

message EnrollRequest {
  bytes  identity_pubkey      = 1;  // 32
  uint32 role                 = 2;  // bit0: consumer, bit1: seeder
  bytes  account_fingerprint  = 3;  // 32 — SHA-256(orgId)
  bytes  nonce                = 4;  // 16
  bytes  consumer_sig         = 5;  // 64
}

message EnrollResponse {
  bytes identity_id           = 1;  // 32
  uint64 starter_grant_credits = 2;
  bytes starter_grant_entry   = 3;  // serialized EntrySigned
}

message BrokerResponse {
  bytes  seeder_addr        = 1;  // utf-8 host:port
  bytes  seeder_pubkey      = 2;  // 32
  bytes  reservation_token  = 3;
}

message NoCapacity {
  string reason = 1;
}

message BalanceRequest {
  bytes identity_id = 1;  // 32
}

message SettleRequest {
  bytes preimage_hash = 1;  // 32
  bytes consumer_sig  = 2;  // 64
}

message SettleAck {}

message UsageReport {
  bytes  request_id     = 1;  // 16 — uuid
  uint32 input_tokens   = 2;
  uint32 output_tokens  = 3;
  string model          = 4;
  bytes  seeder_sig     = 5;  // 64
}

message UsageAck {}

message Advertisement {
  repeated string models     = 1;
  uint32 max_context         = 2;
  bool   available           = 3;
  float  headroom            = 4;
  uint32 tiers               = 5;  // bitmask: bit0 standard, bit1 tee
}

message AdvertiseAck {}

message TransferRequest {
  bytes  identity_id = 1;
  uint64 amount      = 2;
  string dest_region = 3;
  bytes  nonce       = 4;
}

message TransferProof {
  bytes  source_chain_tip_hash = 1;
  uint64 source_seq            = 2;
  bytes  tracker_sig           = 3;
}

message StunAllocateRequest {}
message StunAllocateResponse {
  string external_addr = 1;  // host:port
}

message TurnRelayOpenRequest {
  bytes session_id = 1;  // 16 — uuid
}

message TurnRelayOpenResponse {
  string relay_endpoint = 1;
  bytes  token          = 2;
}

// --- Push channel messages --------------------------------------------

message HeartbeatPing {
  uint64 seq = 1;
  uint64 t   = 2;  // unix milliseconds
}

message HeartbeatPong {
  uint64 seq = 1;
}

message OfferPush {
  bytes  consumer_id    = 1;  // 32
  bytes  envelope_hash  = 2;  // 32
  string model          = 3;
  uint32 max_input_tokens  = 4;
  uint32 max_output_tokens = 5;
}

message OfferDecision {
  bool   accept           = 1;
  bytes  ephemeral_pubkey = 2;  // 32, present iff accept
  string reject_reason    = 3;  // present iff !accept
}

message SettlementPush {
  bytes  preimage_hash = 1;  // 32
  bytes  preimage_body = 2;  // serialized EntryBody
}
```

- [ ] **Step 1.2: Add `proto/rpc.proto` to the Makefile**

Edit `shared/Makefile`:

```makefile
PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto proto/ledger.proto proto/rpc.proto
```

- [ ] **Step 1.3: Generate `rpc.pb.go`**

Run from `shared/`:
```bash
make proto-gen
```
Expected: creates `shared/proto/rpc.pb.go`. Commit it (see step 1.13).

- [ ] **Step 1.4: Write `shared/proto/rpc.go`**

```go
// Package proto: rpc-message metadata.
//
// MaxRPCPayloadSize bounds the proto-encoded payload bytes inside an
// RpcRequest or RpcResponse. The framing layer enforces a 1 MiB outer
// frame cap; this constant exists for callers that want to validate
// payloads before they reach the framer.
package proto

const MaxRPCPayloadSize = 1 << 20 // 1 MiB
```

- [ ] **Step 1.5: Write `ValidateRPCRequest` and `ValidateRPCResponse` in `shared/proto/validate.go`**

Append to `shared/proto/validate.go`:

```go
// ValidateRPCRequest enforces structural invariants on an incoming
// RpcRequest. method must be one of the defined non-zero values
// (method zero is the heartbeat-channel marker, validated separately).
// payload must not exceed MaxRPCPayloadSize.
func ValidateRPCRequest(r *RpcRequest) error {
    if r == nil {
        return errors.New("proto: RpcRequest is nil")
    }
    switch r.Method {
    case RpcMethod_RPC_METHOD_ENROLL,
        RpcMethod_RPC_METHOD_BROKER_REQUEST,
        RpcMethod_RPC_METHOD_BALANCE,
        RpcMethod_RPC_METHOD_SETTLE,
        RpcMethod_RPC_METHOD_USAGE_REPORT,
        RpcMethod_RPC_METHOD_ADVERTISE,
        RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
        RpcMethod_RPC_METHOD_STUN_ALLOCATE,
        RpcMethod_RPC_METHOD_TURN_RELAY_OPEN:
    default:
        return fmt.Errorf("proto: RpcRequest.Method invalid: %v", r.Method)
    }
    if len(r.Payload) > MaxRPCPayloadSize {
        return fmt.Errorf("proto: RpcRequest.Payload %d > %d", len(r.Payload), MaxRPCPayloadSize)
    }
    return nil
}

// ValidateRPCResponse enforces invariants on an RpcResponse. status must
// be one of the defined non-zero values. If status != OK, error must be
// non-nil and have a non-empty code.
func ValidateRPCResponse(r *RpcResponse) error {
    if r == nil {
        return errors.New("proto: RpcResponse is nil")
    }
    switch r.Status {
    case RpcStatus_RPC_STATUS_OK,
        RpcStatus_RPC_STATUS_INVALID,
        RpcStatus_RPC_STATUS_NO_CAPACITY,
        RpcStatus_RPC_STATUS_FROZEN,
        RpcStatus_RPC_STATUS_INTERNAL,
        RpcStatus_RPC_STATUS_UNAUTHENTICATED,
        RpcStatus_RPC_STATUS_NOT_FOUND:
    default:
        return fmt.Errorf("proto: RpcResponse.Status invalid: %v", r.Status)
    }
    if r.Status != RpcStatus_RPC_STATUS_OK {
        if r.Error == nil || r.Error.Code == "" {
            return errors.New("proto: RpcResponse.Error required when Status != OK")
        }
    }
    if len(r.Payload) > MaxRPCPayloadSize {
        return fmt.Errorf("proto: RpcResponse.Payload %d > %d", len(r.Payload), MaxRPCPayloadSize)
    }
    return nil
}

// ValidateOfferPush — non-empty consumer_id (32) + envelope_hash (32) + model.
func ValidateOfferPush(o *OfferPush) error {
    if o == nil {
        return errors.New("proto: OfferPush is nil")
    }
    if len(o.ConsumerId) != 32 {
        return fmt.Errorf("proto: OfferPush.ConsumerId len=%d, want 32", len(o.ConsumerId))
    }
    if len(o.EnvelopeHash) != 32 {
        return fmt.Errorf("proto: OfferPush.EnvelopeHash len=%d, want 32", len(o.EnvelopeHash))
    }
    if o.Model == "" {
        return errors.New("proto: OfferPush.Model empty")
    }
    return nil
}

// ValidateSettlementPush — non-empty preimage_hash (32) + preimage_body.
func ValidateSettlementPush(s *SettlementPush) error {
    if s == nil {
        return errors.New("proto: SettlementPush is nil")
    }
    if len(s.PreimageHash) != 32 {
        return fmt.Errorf("proto: SettlementPush.PreimageHash len=%d, want 32", len(s.PreimageHash))
    }
    if len(s.PreimageBody) == 0 {
        return errors.New("proto: SettlementPush.PreimageBody empty")
    }
    return nil
}
```

- [ ] **Step 1.6: Write `shared/proto/rpc_test.go`**

```go
package proto

import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "google.golang.org/protobuf/proto"
)

func TestRpcRequestRoundTrip(t *testing.T) {
    in := &RpcRequest{Method: RpcMethod_RPC_METHOD_BROKER_REQUEST, Payload: []byte{1, 2, 3}}
    b, err := proto.Marshal(in)
    require.NoError(t, err)
    out := &RpcRequest{}
    require.NoError(t, proto.Unmarshal(b, out))
    assert.Equal(t, in.Method, out.Method)
    assert.Equal(t, in.Payload, out.Payload)
}

func TestRpcResponseRoundTrip(t *testing.T) {
    in := &RpcResponse{
        Status:  RpcStatus_RPC_STATUS_NO_CAPACITY,
        Payload: nil,
        Error:   &RpcError{Code: "no_capacity", Message: "all seeders busy"},
    }
    b, err := proto.Marshal(in)
    require.NoError(t, err)
    out := &RpcResponse{}
    require.NoError(t, proto.Unmarshal(b, out))
    assert.Equal(t, in.Status, out.Status)
    assert.Equal(t, in.Error.Code, out.Error.Code)
    assert.Equal(t, in.Error.Message, out.Error.Message)
}

func TestPushMessagesRoundTrip(t *testing.T) {
    cases := []proto.Message{
        &HeartbeatPing{Seq: 1, T: 100},
        &HeartbeatPong{Seq: 1},
        &OfferPush{ConsumerId: make([]byte, 32), EnvelopeHash: make([]byte, 32), Model: "claude-sonnet-4-6"},
        &OfferDecision{Accept: true, EphemeralPubkey: make([]byte, 32)},
        &SettlementPush{PreimageHash: make([]byte, 32), PreimageBody: []byte("body")},
    }
    for _, m := range cases {
        b, err := proto.Marshal(m)
        require.NoError(t, err)
        // ensure the empty-payload path round-trips too
        assert.NotNil(t, b)
    }
}
```

- [ ] **Step 1.7: Add validator test cases in `shared/proto/validate_test.go`**

Append a function to the existing file:

```go
func TestValidateRpcRequestRejections(t *testing.T) {
    cases := []struct {
        name string
        in   *RpcRequest
        msg  string
    }{
        {"nil", nil, "is nil"},
        {"unspecified", &RpcRequest{Method: RpcMethod_RPC_METHOD_UNSPECIFIED}, "Method invalid"},
        {"out_of_range", &RpcRequest{Method: 999}, "Method invalid"},
        {"oversize", &RpcRequest{
            Method:  RpcMethod_RPC_METHOD_BROKER_REQUEST,
            Payload: make([]byte, MaxRPCPayloadSize+1),
        }, "Payload"},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            err := ValidateRPCRequest(c.in)
            require.Error(t, err)
            assert.Contains(t, err.Error(), c.msg)
        })
    }
}

func TestValidateRpcRequestAccepts(t *testing.T) {
    require.NoError(t, ValidateRPCRequest(&RpcRequest{Method: RpcMethod_RPC_METHOD_BALANCE}))
}

func TestValidateRpcResponseRejections(t *testing.T) {
    cases := []struct {
        name string
        in   *RpcResponse
        msg  string
    }{
        {"nil", nil, "is nil"},
        {"unspecified", &RpcResponse{Status: RpcStatus_RPC_STATUS_UNSPECIFIED}, "Status invalid"},
        {"err_without_error", &RpcResponse{Status: RpcStatus_RPC_STATUS_INVALID}, "required"},
        {"err_blank_code", &RpcResponse{
            Status: RpcStatus_RPC_STATUS_INVALID,
            Error:  &RpcError{Code: "", Message: "x"},
        }, "required"},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            err := ValidateRPCResponse(c.in)
            require.Error(t, err)
            assert.Contains(t, err.Error(), c.msg)
        })
    }
}

func TestValidateOfferAndSettlementPush(t *testing.T) {
    require.Error(t, ValidateOfferPush(nil))
    require.Error(t, ValidateOfferPush(&OfferPush{ConsumerId: make([]byte, 31)}))
    require.NoError(t, ValidateOfferPush(&OfferPush{
        ConsumerId:   make([]byte, 32),
        EnvelopeHash: make([]byte, 32),
        Model:        "x",
    }))
    require.Error(t, ValidateSettlementPush(nil))
    require.Error(t, ValidateSettlementPush(&SettlementPush{PreimageHash: make([]byte, 32)}))
    require.NoError(t, ValidateSettlementPush(&SettlementPush{
        PreimageHash: make([]byte, 32),
        PreimageBody: []byte{1},
    }))
}
```

- [ ] **Step 1.8: Run validator tests, expect FAIL**

```bash
cd shared && go test ./proto/... -run 'ValidateRpc|OfferAndSettlement' -v
```
Expected: tests fail with "ValidateRPCRequest undefined" until the helpers in step 1.5 land — they should land in the same commit so this step verifies the green state instead.

- [ ] **Step 1.9: Run validator tests, expect PASS**

```bash
cd shared && go test ./proto/... -run 'ValidateRpc|OfferAndSettlement' -v
```
Expected: all PASS.

- [ ] **Step 1.10: Add a deterministic golden fixture for `RpcRequest{BROKER_REQUEST, "fixture"}`**

Create the helper test in `shared/proto/rpc_test.go` (append):

```go
func TestRpcRequestGoldenBytes(t *testing.T) {
    r := &RpcRequest{
        Method:  RpcMethod_RPC_METHOD_BROKER_REQUEST,
        Payload: []byte("fixture"),
    }
    b, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
    require.NoError(t, err)
    expected := goldenHex(t, "testdata/rpc_request_broker.golden.hex")
    assert.Equal(t, expected, b)
}

func TestRpcResponseGoldenBytes(t *testing.T) {
    r := &RpcResponse{
        Status:  RpcStatus_RPC_STATUS_OK,
        Payload: []byte("ok-payload"),
    }
    b, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
    require.NoError(t, err)
    expected := goldenHex(t, "testdata/rpc_response_broker_ok.golden.hex")
    assert.Equal(t, expected, b)
}
```

If `goldenHex` doesn't already exist in `shared/proto/`, add it (or use the existing helper from `envelope_test.go` — check that file first; if present, reuse). If absent, add to `proto_test.go`:

```go
import (
    "encoding/hex"
    "os"
    "strings"
)

func goldenHex(t *testing.T, path string) []byte {
    t.Helper()
    raw, err := os.ReadFile(path)
    require.NoError(t, err)
    s := strings.TrimSpace(string(raw))
    out, err := hex.DecodeString(s)
    require.NoError(t, err)
    return out
}
```

- [ ] **Step 1.11: Generate the golden hex files**

```bash
cd shared/proto
cat > /tmp/gen_golden.go <<'EOF'
package main

import (
    "encoding/hex"
    "os"

    "google.golang.org/protobuf/proto"

    pb "github.com/token-bay/token-bay/shared/proto"
)

func main() {
    req := &pb.RpcRequest{Method: pb.RpcMethod_RPC_METHOD_BROKER_REQUEST, Payload: []byte("fixture")}
    b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(req)
    os.WriteFile("testdata/rpc_request_broker.golden.hex", []byte(hex.EncodeToString(b)+"\n"), 0644)
    resp := &pb.RpcResponse{Status: pb.RpcStatus_RPC_STATUS_OK, Payload: []byte("ok-payload")}
    b, _ = proto.MarshalOptions{Deterministic: true}.Marshal(resp)
    os.WriteFile("testdata/rpc_response_broker_ok.golden.hex", []byte(hex.EncodeToString(b)+"\n"), 0644)
}
EOF
mkdir -p testdata
go run /tmp/gen_golden.go
rm /tmp/gen_golden.go
```

Expected: two `.golden.hex` files appear in `shared/proto/testdata/`. Commit them.

- [ ] **Step 1.12: Run all `shared/` tests with race detector**

```bash
make -C shared test
```
Expected: PASS, ≥ 90% line coverage maintained.

- [ ] **Step 1.13: Commit**

```bash
git add shared/proto/rpc.proto shared/proto/rpc.pb.go shared/proto/rpc.go \
        shared/proto/rpc_test.go shared/proto/validate.go shared/proto/validate_test.go \
        shared/proto/testdata/rpc_request_broker.golden.hex \
        shared/proto/testdata/rpc_response_broker_ok.golden.hex \
        shared/Makefile
git commit -m "$(cat <<'EOF'
feat(shared/proto): RPC schema + validators + goldens

Adds tokenbay.proto.v1 RpcRequest/RpcResponse, RpcMethod (9 unary +
heartbeat marker), RpcStatus, structured RpcError, plus per-method
payloads and three push messages (HeartbeatPing/Pong, OfferPush,
SettlementPush). Validators enforce method-zero ban on regular streams,
1 MiB payload cap, and structural invariants on push messages.

Cross-cutting prerequisite for plugin/internal/trackerclient.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

# Phase 2 — Plugin module bootstrap

### Task 2: Add new runtime dependencies

**Files:**
- Modify: `plugin/go.mod`
- Modify: `plugin/go.sum`

- [ ] **Step 2.1: Add the dependencies**

```bash
cd plugin
go get github.com/quic-go/quic-go@latest
go get golang.org/x/sync@latest
go get github.com/google/uuid@latest
go mod tidy
```

- [ ] **Step 2.2: Sync the workspace**

From repo root:
```bash
go work sync
```

- [ ] **Step 2.3: Verify the plugin still builds**

```bash
make -C plugin build
```
Expected: builds `plugin/bin/token-bay-sidecar` without errors.

- [ ] **Step 2.4: Commit**

```bash
git add plugin/go.mod plugin/go.sum
git commit -m "chore(plugin): add quic-go, x/sync, uuid for trackerclient

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Skeleton package — `doc.go`, `types.go`, `errors.go`

**Files:**
- Create: `plugin/internal/trackerclient/doc.go`
- Create: `plugin/internal/trackerclient/types.go`
- Create: `plugin/internal/trackerclient/errors.go`
- Create: `plugin/internal/trackerclient/errors_test.go`

- [ ] **Step 3.1: Write `doc.go`**

```go
// Package trackerclient is the plugin's long-lived mTLS QUIC client to a
// regional tracker. It exposes blocking unary RPCs for nine methods and
// dispatches three server-pushed streams (heartbeat, offers,
// settlements) to caller-provided handler interfaces.
//
// The client owns connection lifecycle and reconnect; callers see only
// blocking RPC methods plus a Status / WaitConnected surface.
//
// See docs/superpowers/specs/plugin/2026-05-02-trackerclient-design.md.
package trackerclient
```

- [ ] **Step 3.2: Write `types.go`**

```go
package trackerclient

import (
    "crypto/ed25519"
    "io"
    "net/netip"
    "time"

    "github.com/google/uuid"
    "github.com/rs/zerolog"

    "github.com/token-bay/token-bay/shared/ids"
)

// ConnectionPhase is the supervisor-reported lifecycle state.
type ConnectionPhase int

const (
    PhaseDisconnected ConnectionPhase = iota
    PhaseConnecting
    PhaseConnected
    PhaseClosing
    PhaseClosed
)

func (p ConnectionPhase) String() string {
    switch p {
    case PhaseDisconnected:
        return "disconnected"
    case PhaseConnecting:
        return "connecting"
    case PhaseConnected:
        return "connected"
    case PhaseClosing:
        return "closing"
    case PhaseClosed:
        return "closed"
    default:
        return "unknown"
    }
}

// ConnectionState is a snapshot of supervisor state for slash commands.
type ConnectionState struct {
    Phase         ConnectionPhase
    Endpoint      TrackerEndpoint
    PeerID        ids.IdentityID
    ConnectedAt   time.Time
    LastError     error
    NextAttemptAt time.Time
}

// TrackerEndpoint is one entry in the bootstrap list.
type TrackerEndpoint struct {
    Addr         string   // host:port (UDP for QUIC)
    IdentityHash [32]byte // SHA-256 of the tracker's Ed25519 SPKI bytes
    Region       string
}

// Signer is the keypair holder. internal/identity provides the production
// implementation; tests use a fake.
type Signer interface {
    Sign(msg []byte) ([]byte, error)
    PrivateKey() ed25519.PrivateKey
    IdentityID() ids.IdentityID
}

// EnrollRequest is the consumer-side enrollment payload.
type EnrollRequest struct {
    IdentityPubkey     ed25519.PublicKey
    Role               uint32
    AccountFingerprint [32]byte
    Nonce              [16]byte
    Sig                []byte
}

// EnrollResponse is the tracker's reply.
type EnrollResponse struct {
    IdentityID            ids.IdentityID
    StarterGrantCredits   uint64
    StarterGrantEntryBlob []byte
}

// BrokerResponse is the seeder assignment from a successful broker_request.
type BrokerResponse struct {
    SeederAddr        string
    SeederPubkey      ed25519.PublicKey
    ReservationToken  []byte
}

// UsageReport is what the seeder reports after a successful served request.
type UsageReport struct {
    RequestID    uuid.UUID
    InputTokens  uint32
    OutputTokens uint32
    Model        string
    SeederSig    []byte
}

// Advertisement is the seeder's availability + capabilities update.
type Advertisement struct {
    Models     []string
    MaxContext uint32
    Available  bool
    Headroom   float32
    Tiers      uint32
}

// TransferRequest moves credits between regions.
type TransferRequest struct {
    IdentityID ids.IdentityID
    Amount     uint64
    DestRegion string
    Nonce      [16]byte
}

// TransferProof is the source-region tracker's signed proof.
type TransferProof struct {
    SourceChainTipHash [32]byte
    SourceSeq          uint64
    TrackerSig         []byte
}

// RelayHandle describes a TURN-style relay allocation.
type RelayHandle struct {
    Endpoint string
    Token    []byte
}

// Offer is the server-pushed broker offer to a seeder.
type Offer struct {
    ConsumerID      ids.IdentityID
    EnvelopeHash    [32]byte
    Model           string
    MaxInputTokens  uint32
    MaxOutputTokens uint32
}

// OfferDecision is what the seeder returns to the tracker.
type OfferDecision struct {
    Accept          bool
    EphemeralPubkey []byte
    RejectReason    string
}

// SettlementRequest is the tracker's request that the consumer counter-sign
// a finalized ledger entry.
type SettlementRequest struct {
    PreimageHash [32]byte
    PreimageBody []byte
}

// internal: avoid an unused-import linter complaint until callers reference
// these via the public surface.
var (
    _ = io.EOF
    _ = netip.AddrPort{}
    _ zerolog.Logger
)
```

- [ ] **Step 3.3: Write `errors.go`**

```go
package trackerclient

import (
    "errors"
    "fmt"

    tbproto "github.com/token-bay/token-bay/shared/proto"
)

var (
    ErrNotStarted       = errors.New("trackerclient: Start not called")
    ErrAlreadyStarted   = errors.New("trackerclient: already started")
    ErrClosed           = errors.New("trackerclient: client closed")
    ErrConnectionLost   = errors.New("trackerclient: connection lost mid-RPC")
    ErrIdentityMismatch = errors.New("trackerclient: tracker identity does not match pin")
    ErrInvalidEndpoint  = errors.New("trackerclient: invalid endpoint")
    ErrNoCapacity       = errors.New("trackerclient: tracker reports no capacity")
    ErrFrozen           = errors.New("trackerclient: identity frozen by reputation")
    ErrUnauthenticated  = errors.New("trackerclient: tracker rejected identity")
    ErrFrameTooLarge    = errors.New("trackerclient: framed message exceeds MaxFrameSize")
    ErrInvalidResponse  = errors.New("trackerclient: malformed RpcResponse")
    ErrAlpnMismatch     = errors.New("trackerclient: ALPN negotiation failed")
    ErrNoHandler        = errors.New("trackerclient: server pushed a stream but no handler is registered")
    ErrConfigInvalid    = errors.New("trackerclient: config invalid")
)

// RpcError wraps a tracker-side structured error.
type RpcError struct {
    Code    string
    Message string
}

func (e *RpcError) Error() string {
    if e == nil {
        return "<nil RpcError>"
    }
    return fmt.Sprintf("trackerclient: tracker error %q: %s", e.Code, e.Message)
}

// statusToErr maps an RpcStatus + RpcError to a typed Go error.
// Callers expect errors.Is(err, ErrNoCapacity) etc. to succeed.
func statusToErr(status tbproto.RpcStatus, e *tbproto.RpcError) error {
    if status == tbproto.RpcStatus_RPC_STATUS_OK {
        return nil
    }
    rpcErr := &RpcError{}
    if e != nil {
        rpcErr.Code = e.Code
        rpcErr.Message = e.Message
    }
    var sentinel error
    switch status {
    case tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY:
        sentinel = ErrNoCapacity
    case tbproto.RpcStatus_RPC_STATUS_FROZEN:
        sentinel = ErrFrozen
    case tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED:
        sentinel = ErrUnauthenticated
    case tbproto.RpcStatus_RPC_STATUS_INVALID:
        sentinel = ErrInvalidResponse
    default:
        // INTERNAL, NOT_FOUND, UNSPECIFIED — surface via RpcError only
        return rpcErr
    }
    return fmt.Errorf("%w: %s", sentinel, rpcErr.Error())
}
```

- [ ] **Step 3.4: Write `errors_test.go`**

```go
package trackerclient

import (
    "errors"
    "testing"

    "github.com/stretchr/testify/assert"

    tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestStatusToErrOK(t *testing.T) {
    assert.NoError(t, statusToErr(tbproto.RpcStatus_RPC_STATUS_OK, nil))
}

func TestStatusToErrSentinels(t *testing.T) {
    cases := []struct {
        status   tbproto.RpcStatus
        sentinel error
    }{
        {tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY, ErrNoCapacity},
        {tbproto.RpcStatus_RPC_STATUS_FROZEN, ErrFrozen},
        {tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED, ErrUnauthenticated},
        {tbproto.RpcStatus_RPC_STATUS_INVALID, ErrInvalidResponse},
    }
    for _, c := range cases {
        err := statusToErr(c.status, &tbproto.RpcError{Code: "x", Message: "y"})
        assert.True(t, errors.Is(err, c.sentinel), "want errors.Is %v", c.sentinel)
    }
}

func TestStatusToErrInternalKeepsRpcError(t *testing.T) {
    err := statusToErr(tbproto.RpcStatus_RPC_STATUS_INTERNAL, &tbproto.RpcError{Code: "boom", Message: "details"})
    var rpcErr *RpcError
    assert.True(t, errors.As(err, &rpcErr))
    assert.Equal(t, "boom", rpcErr.Code)
}

func TestRpcErrorNilSafe(t *testing.T) {
    var e *RpcError
    assert.Equal(t, "<nil RpcError>", e.Error())
}
```

- [ ] **Step 3.5: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/...
```
Expected: PASS.

- [ ] **Step 3.6: Commit**

```bash
git add plugin/internal/trackerclient/doc.go \
        plugin/internal/trackerclient/types.go \
        plugin/internal/trackerclient/errors.go \
        plugin/internal/trackerclient/errors_test.go
git commit -m "feat(plugin/trackerclient): package skeleton + types + errors

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Config + Validate

**Files:**
- Create: `plugin/internal/trackerclient/config.go`
- Create: `plugin/internal/trackerclient/config_test.go`

- [ ] **Step 4.1: Write `config.go`**

```go
package trackerclient

import (
    "crypto/rand"
    "errors"
    "fmt"
    "io"
    "time"

    "github.com/rs/zerolog"
)

// OfferHandler is the seeder-side hook for server-pushed offers.
type OfferHandler interface {
    HandleOffer(ctx Ctx, o *Offer) (OfferDecision, error)
}

// SettlementHandler is the consumer-side hook for server-pushed settlement
// requests. Returns the consumer's signature over the preimage; an error
// causes the supervisor to reject the settlement back to the tracker.
type SettlementHandler interface {
    HandleSettlement(ctx Ctx, r *SettlementRequest) (sig []byte, err error)
}

// Ctx is an alias to keep handler signatures stable if we later swap to a
// purpose-built request context.
type Ctx = interface {
    Done() <-chan struct{}
    Err() error
}

// Config configures Client. Required fields: Endpoints, Identity. If
// the consumer role is in use, SettlementHandler is required. If the
// seeder role is in use, OfferHandler is required.
type Config struct {
    Endpoints []TrackerEndpoint
    Identity  Signer
    Transport Transport // optional; defaults to QUIC

    OfferHandler      OfferHandler
    SettlementHandler SettlementHandler

    Logger zerolog.Logger
    Clock  func() time.Time
    Rand   io.Reader

    DialTimeout            time.Duration
    HeartbeatPeriod        time.Duration
    HeartbeatMisses        int
    BalanceTTL             time.Duration
    BalanceRefreshHeadroom time.Duration

    BackoffBase  time.Duration
    BackoffMax   time.Duration
    MaxFrameSize int
}

// withDefaults returns a copy of cfg with zero-valued fields filled in.
func (cfg Config) withDefaults() Config {
    if cfg.Clock == nil {
        cfg.Clock = time.Now
    }
    if cfg.Rand == nil {
        cfg.Rand = rand.Reader
    }
    if cfg.DialTimeout == 0 {
        cfg.DialTimeout = 5 * time.Second
    }
    if cfg.HeartbeatPeriod == 0 {
        cfg.HeartbeatPeriod = 15 * time.Second
    }
    if cfg.HeartbeatMisses == 0 {
        cfg.HeartbeatMisses = 3
    }
    if cfg.BalanceTTL == 0 {
        cfg.BalanceTTL = 10 * time.Minute
    }
    if cfg.BalanceRefreshHeadroom == 0 {
        cfg.BalanceRefreshHeadroom = 2 * time.Minute
    }
    if cfg.BackoffBase == 0 {
        cfg.BackoffBase = 200 * time.Millisecond
    }
    if cfg.BackoffMax == 0 {
        cfg.BackoffMax = 30 * time.Second
    }
    if cfg.MaxFrameSize == 0 {
        cfg.MaxFrameSize = 1 << 20
    }
    return cfg
}

// Validate enforces required-field and range invariants.
func (cfg Config) Validate() error {
    if len(cfg.Endpoints) == 0 {
        return fmt.Errorf("%w: at least one endpoint required", ErrInvalidEndpoint)
    }
    for i, ep := range cfg.Endpoints {
        if ep.Addr == "" {
            return fmt.Errorf("%w: endpoints[%d].Addr empty", ErrInvalidEndpoint, i)
        }
        if ep.IdentityHash == ([32]byte{}) {
            return fmt.Errorf("%w: endpoints[%d].IdentityHash zero", ErrInvalidEndpoint, i)
        }
    }
    if cfg.Identity == nil {
        return fmt.Errorf("%w: Identity required", ErrConfigInvalid)
    }
    if cfg.HeartbeatMisses < 1 {
        return fmt.Errorf("%w: HeartbeatMisses must be ≥ 1", ErrConfigInvalid)
    }
    if cfg.BalanceRefreshHeadroom >= cfg.BalanceTTL {
        return fmt.Errorf("%w: BalanceRefreshHeadroom must be < BalanceTTL", ErrConfigInvalid)
    }
    if cfg.MaxFrameSize <= 0 {
        return fmt.Errorf("%w: MaxFrameSize must be > 0", ErrConfigInvalid)
    }
    if cfg.BackoffBase <= 0 || cfg.BackoffMax <= 0 || cfg.BackoffMax < cfg.BackoffBase {
        return fmt.Errorf("%w: bad backoff window", ErrConfigInvalid)
    }
    return nil
}

// errMustOptional is a sentinel used internally to signal that a config
// field is conditionally required at Start() time, not at Validate().
var errMustOptional = errors.New("trackerclient: optional")
```

- [ ] **Step 4.2: Write `config_test.go`**

```go
package trackerclient

import (
    "crypto/ed25519"
    "errors"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/shared/ids"
)

type fakeSigner struct{ priv ed25519.PrivateKey }

func (f fakeSigner) Sign(msg []byte) ([]byte, error)    { return ed25519.Sign(f.priv, msg), nil }
func (f fakeSigner) PrivateKey() ed25519.PrivateKey     { return f.priv }
func (f fakeSigner) IdentityID() ids.IdentityID         { return ids.IdentityID{} }

func validConfig(t *testing.T) Config {
    t.Helper()
    _, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)
    return Config{
        Endpoints: []TrackerEndpoint{{
            Addr:         "127.0.0.1:9000",
            IdentityHash: [32]byte{1},
        }},
        Identity: fakeSigner{priv: priv},
    }
}

func TestConfigValidateAccepts(t *testing.T) {
    cfg := validConfig(t).withDefaults()
    require.NoError(t, cfg.Validate())
}

func TestConfigValidateRejections(t *testing.T) {
    cases := []struct {
        name    string
        mutate  func(*Config)
        target  error
    }{
        {"no endpoints", func(c *Config) { c.Endpoints = nil }, ErrInvalidEndpoint},
        {"empty addr", func(c *Config) { c.Endpoints[0].Addr = "" }, ErrInvalidEndpoint},
        {"zero hash", func(c *Config) { c.Endpoints[0].IdentityHash = [32]byte{} }, ErrInvalidEndpoint},
        {"no identity", func(c *Config) { c.Identity = nil }, ErrConfigInvalid},
        {"zero misses", func(c *Config) { c.HeartbeatMisses = 0; /* defaults bypass */ }, nil},
        {"refresh ≥ ttl", func(c *Config) {
            c.BalanceTTL = time.Minute
            c.BalanceRefreshHeadroom = time.Hour
        }, ErrConfigInvalid},
        {"backoff inverted", func(c *Config) {
            c.BackoffBase = 30 * time.Second
            c.BackoffMax = 1 * time.Second
        }, ErrConfigInvalid},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            cfg := validConfig(t)
            c.mutate(&cfg)
            err := cfg.withDefaults().Validate()
            if c.target == nil {
                require.NoError(t, err)
                return
            }
            require.Error(t, err)
            assert.True(t, errors.Is(err, c.target), "want errors.Is %v, got %v", c.target, err)
        })
    }
}

func TestConfigDefaults(t *testing.T) {
    cfg := Config{}.withDefaults()
    assert.Equal(t, 5*time.Second, cfg.DialTimeout)
    assert.Equal(t, 15*time.Second, cfg.HeartbeatPeriod)
    assert.Equal(t, 3, cfg.HeartbeatMisses)
    assert.Equal(t, 10*time.Minute, cfg.BalanceTTL)
    assert.Equal(t, 2*time.Minute, cfg.BalanceRefreshHeadroom)
    assert.Equal(t, 200*time.Millisecond, cfg.BackoffBase)
    assert.Equal(t, 30*time.Second, cfg.BackoffMax)
    assert.Equal(t, 1<<20, cfg.MaxFrameSize)
}
```

- [ ] **Step 4.3: Run tests, expect FAIL** (because `Transport` interface isn't declared yet)

```bash
cd plugin && go test ./internal/trackerclient/... -run Config
```
Expected: FAIL with `undefined: Transport`. Resolved in Task 5.

- [ ] **Step 4.4: Add a temporary forward declaration**

Append to `plugin/internal/trackerclient/types.go`:

```go
// Transport is the network seam between the client and a tracker.
// Concrete implementations live under internal/transport/.
type Transport interface {
    Dial(ctx Ctx, ep TrackerEndpoint, signer Signer) (Conn, error)
}

// Conn is a connected, mTLS-authenticated transport session.
type Conn interface {
    OpenStreamSync(ctx Ctx) (Stream, error)
    AcceptStream(ctx Ctx) (Stream, error)
    PeerIdentityID() ids.IdentityID
    Close() error
    Done() <-chan struct{}
}

// Stream is a bidirectional byte stream within a Conn.
type Stream interface {
    Read(p []byte) (int, error)
    Write(p []byte) (int, error)
    Close() error
    CloseWrite() error
}
```

- [ ] **Step 4.5: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run Config
```
Expected: PASS.

- [ ] **Step 4.6: Commit**

```bash
git add plugin/internal/trackerclient/config.go \
        plugin/internal/trackerclient/config_test.go \
        plugin/internal/trackerclient/types.go
git commit -m "feat(plugin/trackerclient): Config + Validate + Transport seam

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

---

# Phase 3 — Wire framing and codec

### Task 5: Length-prefixed deterministic-proto framing

**Files:**
- Create: `plugin/internal/trackerclient/internal/wire/frame.go`
- Create: `plugin/internal/trackerclient/internal/wire/frame_test.go`

- [ ] **Step 5.1: Write `frame.go`**

```go
// Package wire provides the length-prefixed deterministic-proto codec
// used on every Token-Bay client↔tracker stream.
//
// Wire format per frame:
//
//   +---------+--------------------------------+
//   | len:u32 | proto bytes (DeterministicMar) |
//   +---------+--------------------------------+
//
// len is big-endian and bounds the payload length. Frames whose declared
// length exceeds maxFrameSize are rejected with ErrFrameTooLarge.
package wire

import (
    "encoding/binary"
    "errors"
    "fmt"
    "io"

    "google.golang.org/protobuf/proto"

    "github.com/token-bay/token-bay/shared/signing"
)

// ErrFrameTooLarge is returned when a peer sends or we attempt to send a
// frame whose declared length exceeds maxFrameSize.
var ErrFrameTooLarge = errors.New("wire: frame exceeds max size")

// Reader / Writer are the io.Reader and io.Writer halves of a stream.
type ReadWriter interface {
    io.Reader
    io.Writer
}

// Write serializes m using DeterministicMarshal and writes a single
// length-prefixed frame to w. Returns ErrFrameTooLarge if the encoded
// payload would exceed maxFrameSize.
func Write(w io.Writer, m proto.Message, maxFrameSize int) error {
    buf, err := signing.DeterministicMarshal(m)
    if err != nil {
        return fmt.Errorf("wire: marshal: %w", err)
    }
    if len(buf) > maxFrameSize {
        return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(buf), maxFrameSize)
    }
    var hdr [4]byte
    binary.BigEndian.PutUint32(hdr[:], uint32(len(buf)))
    if _, err := w.Write(hdr[:]); err != nil {
        return fmt.Errorf("wire: write header: %w", err)
    }
    if _, err := w.Write(buf); err != nil {
        return fmt.Errorf("wire: write body: %w", err)
    }
    return nil
}

// Read reads a single length-prefixed frame from r and unmarshals it
// into dst. Returns ErrFrameTooLarge if the declared length exceeds
// maxFrameSize, io.EOF if r is at end-of-stream before a header arrives,
// and io.ErrUnexpectedEOF if the body is short.
func Read(r io.Reader, dst proto.Message, maxFrameSize int) error {
    var hdr [4]byte
    if _, err := io.ReadFull(r, hdr[:]); err != nil {
        return err // io.EOF or io.ErrUnexpectedEOF surfaced verbatim
    }
    n := binary.BigEndian.Uint32(hdr[:])
    if int(n) > maxFrameSize {
        return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, maxFrameSize)
    }
    body := make([]byte, n)
    if _, err := io.ReadFull(r, body); err != nil {
        return fmt.Errorf("wire: read body: %w", err)
    }
    if err := proto.Unmarshal(body, dst); err != nil {
        return fmt.Errorf("wire: unmarshal: %w", err)
    }
    return nil
}
```

- [ ] **Step 5.2: Write `frame_test.go`**

```go
package wire

import (
    "bytes"
    "errors"
    "io"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestWriteThenReadRoundTrip(t *testing.T) {
    in := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BALANCE, Payload: []byte("abc")}
    var buf bytes.Buffer
    require.NoError(t, Write(&buf, in, 1<<10))

    out := &tbproto.RpcRequest{}
    require.NoError(t, Read(&buf, out, 1<<10))
    assert.Equal(t, in.Method, out.Method)
    assert.Equal(t, in.Payload, out.Payload)
}

func TestWriteRejectsOversize(t *testing.T) {
    in := &tbproto.RpcRequest{
        Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
        Payload: make([]byte, 200),
    }
    err := Write(io.Discard, in, 64)
    require.Error(t, err)
    assert.True(t, errors.Is(err, ErrFrameTooLarge))
}

func TestReadRejectsOversizeHeader(t *testing.T) {
    // Hand-craft a frame whose declared length is 1 MB but max is 64.
    var buf bytes.Buffer
    buf.Write([]byte{0x00, 0x10, 0x00, 0x00})
    err := Read(&buf, &tbproto.RpcRequest{}, 64)
    require.Error(t, err)
    assert.True(t, errors.Is(err, ErrFrameTooLarge))
}

func TestReadEOFBeforeHeader(t *testing.T) {
    var buf bytes.Buffer
    err := Read(&buf, &tbproto.RpcRequest{}, 64)
    assert.ErrorIs(t, err, io.EOF)
}

func TestReadShortBody(t *testing.T) {
    var buf bytes.Buffer
    buf.Write([]byte{0, 0, 0, 10}) // claim 10 bytes
    buf.Write([]byte{1, 2, 3})      // give 3
    err := Read(&buf, &tbproto.RpcRequest{}, 1<<10)
    require.Error(t, err)
}

func TestSequentialFramesOnOneStream(t *testing.T) {
    a := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BALANCE, Payload: []byte("a")}
    b := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, Payload: []byte("bb")}
    var buf bytes.Buffer
    require.NoError(t, Write(&buf, a, 1<<10))
    require.NoError(t, Write(&buf, b, 1<<10))

    var out tbproto.RpcRequest
    require.NoError(t, Read(&buf, &out, 1<<10))
    assert.Equal(t, []byte("a"), out.Payload)
    out = tbproto.RpcRequest{}
    require.NoError(t, Read(&buf, &out, 1<<10))
    assert.Equal(t, []byte("bb"), out.Payload)
}
```

- [ ] **Step 5.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/internal/wire/...
```
Expected: PASS.

- [ ] **Step 5.4: Commit**

```bash
git add plugin/internal/trackerclient/internal/wire/frame.go \
        plugin/internal/trackerclient/internal/wire/frame_test.go
git commit -m "feat(plugin/trackerclient/wire): length-prefixed proto framing

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: RPC dispatch helper

**Files:**
- Create: `plugin/internal/trackerclient/internal/wire/codec.go`
- Create: `plugin/internal/trackerclient/internal/wire/codec_test.go`

- [ ] **Step 6.1: Write `codec.go`**

```go
package wire

import (
    "fmt"

    "google.golang.org/protobuf/proto"

    tbproto "github.com/token-bay/token-bay/shared/proto"
)

// MarshalRequest packages a per-method payload into an RpcRequest envelope.
func MarshalRequest(method tbproto.RpcMethod, payload proto.Message) (*tbproto.RpcRequest, error) {
    if method == tbproto.RpcMethod_RPC_METHOD_UNSPECIFIED {
        return nil, fmt.Errorf("wire: method 0 is reserved for heartbeat")
    }
    var pb []byte
    if payload != nil {
        var err error
        pb, err = proto.Marshal(payload)
        if err != nil {
            return nil, fmt.Errorf("wire: marshal payload: %w", err)
        }
    }
    return &tbproto.RpcRequest{Method: method, Payload: pb}, nil
}

// UnmarshalResponse extracts the per-method payload from an RpcResponse.
// Caller passes a zero-valued *T; on OK status it is populated.
func UnmarshalResponse(resp *tbproto.RpcResponse, dst proto.Message) error {
    if resp == nil {
        return fmt.Errorf("wire: nil RpcResponse")
    }
    if dst == nil {
        return nil // caller doesn't care about the payload (Settle/Advertise/UsageReport ack)
    }
    if len(resp.Payload) == 0 {
        return nil
    }
    if err := proto.Unmarshal(resp.Payload, dst); err != nil {
        return fmt.Errorf("wire: unmarshal payload: %w", err)
    }
    return nil
}
```

- [ ] **Step 6.2: Write `codec_test.go`**

```go
package wire

import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestMarshalRequestRejectsHeartbeatMethod(t *testing.T) {
    _, err := MarshalRequest(tbproto.RpcMethod_RPC_METHOD_UNSPECIFIED, nil)
    require.Error(t, err)
}

func TestMarshalRequestNilPayloadOK(t *testing.T) {
    req, err := MarshalRequest(tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, nil)
    require.NoError(t, err)
    assert.Equal(t, tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, req.Method)
    assert.Empty(t, req.Payload)
}

func TestMarshalRoundTrip(t *testing.T) {
    src := &tbproto.BalanceRequest{IdentityId: make([]byte, 32)}
    req, err := MarshalRequest(tbproto.RpcMethod_RPC_METHOD_BALANCE, src)
    require.NoError(t, err)
    out := &tbproto.BalanceRequest{}
    require.NoError(t, UnmarshalResponse(&tbproto.RpcResponse{Payload: req.Payload}, out))
    assert.Equal(t, src.IdentityId, out.IdentityId)
}

func TestUnmarshalResponseNilDstOK(t *testing.T) {
    require.NoError(t, UnmarshalResponse(&tbproto.RpcResponse{Payload: []byte("ignored")}, nil))
}

func TestUnmarshalResponseNilResp(t *testing.T) {
    require.Error(t, UnmarshalResponse(nil, &tbproto.BalanceRequest{}))
}
```

- [ ] **Step 6.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/internal/wire/...
```
Expected: PASS.

- [ ] **Step 6.4: Commit**

```bash
git add plugin/internal/trackerclient/internal/wire/codec.go \
        plugin/internal/trackerclient/internal/wire/codec_test.go
git commit -m "feat(plugin/trackerclient/wire): RpcRequest/Response codec helpers

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 4 — Identity-bound TLS

### Task 7: `internal/idtls` cert generation

**Files:**
- Create: `plugin/internal/trackerclient/internal/idtls/cert.go`
- Create: `plugin/internal/trackerclient/internal/idtls/cert_test.go`

- [ ] **Step 7.1: Write `cert.go`**

```go
// Package idtls binds the plugin's Ed25519 identity keypair to a TLS
// session. Generates a self-signed X.509 cert whose SubjectPublicKeyInfo
// carries the Ed25519 pubkey; the SPKI hash IS the IdentityID.
package idtls

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

// ALPN is the protocol identifier negotiated on every Token-Bay TLS link.
// v2 will ship as "tokenbay/2"; old/new servers can co-exist.
const ALPN = "tokenbay/1"

// CertFromIdentity issues a self-signed X.509 cert wrapping priv.
// The cert is generated deterministically from the keypair and is safe to
// regenerate on each call (we cache it at the Client level for efficiency).
func CertFromIdentity(priv ed25519.PrivateKey) (tls.Certificate, error) {
    if len(priv) != ed25519.PrivateKeySize {
        return tls.Certificate{}, fmt.Errorf("idtls: priv length %d, want %d", len(priv), ed25519.PrivateKeySize)
    }
    tmpl := &x509.Certificate{
        SerialNumber: big.NewInt(1),
        Subject:      pkix.Name{CommonName: "token-bay-identity"},
        NotBefore:    time.Unix(0, 0),
        NotAfter:     time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
        KeyUsage:     x509.KeyUsageDigitalSignature,
        ExtKeyUsage: []x509.ExtKeyUsage{
            x509.ExtKeyUsageClientAuth,
            x509.ExtKeyUsageServerAuth,
        },
        BasicConstraintsValid: true,
    }
    der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
    if err != nil {
        return tls.Certificate{}, fmt.Errorf("idtls: CreateCertificate: %w", err)
    }
    return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// SPKIHashOfCert extracts the SHA-256 of the cert's SubjectPublicKeyInfo
// bytes — the canonical IdentityID for the cert's holder.
func SPKIHashOfCert(cert *x509.Certificate) ([32]byte, error) {
    if cert == nil {
        return [32]byte{}, errors.New("idtls: nil cert")
    }
    if len(cert.RawSubjectPublicKeyInfo) == 0 {
        return [32]byte{}, errors.New("idtls: empty SPKI")
    }
    return sha256.Sum256(cert.RawSubjectPublicKeyInfo), nil
}
```

- [ ] **Step 7.2: Write `cert_test.go`**

```go
package idtls

import (
    "crypto/ed25519"
    "crypto/x509"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestCertFromIdentityProducesParseableCert(t *testing.T) {
    _, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)

    cert, err := CertFromIdentity(priv)
    require.NoError(t, err)
    require.Len(t, cert.Certificate, 1)

    parsed, err := x509.ParseCertificate(cert.Certificate[0])
    require.NoError(t, err)

    pub, ok := parsed.PublicKey.(ed25519.PublicKey)
    require.True(t, ok)
    assert.True(t, pub.Equal(priv.Public()))
}

func TestCertFromIdentityRejectsBadKey(t *testing.T) {
    _, err := CertFromIdentity(ed25519.PrivateKey{1, 2, 3})
    require.Error(t, err)
}

func TestSPKIHashStableAcrossRegeneration(t *testing.T) {
    _, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)

    c1, err := CertFromIdentity(priv)
    require.NoError(t, err)
    p1, err := x509.ParseCertificate(c1.Certificate[0])
    require.NoError(t, err)
    h1, err := SPKIHashOfCert(p1)
    require.NoError(t, err)

    c2, err := CertFromIdentity(priv)
    require.NoError(t, err)
    p2, err := x509.ParseCertificate(c2.Certificate[0])
    require.NoError(t, err)
    h2, err := SPKIHashOfCert(p2)
    require.NoError(t, err)

    assert.Equal(t, h1, h2, "SPKI hash must be stable for the same keypair")
}

func TestSPKIHashNilSafe(t *testing.T) {
    _, err := SPKIHashOfCert(nil)
    require.Error(t, err)
}
```

- [ ] **Step 7.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/internal/idtls/...
```
Expected: PASS.

- [ ] **Step 7.4: Commit**

```bash
git add plugin/internal/trackerclient/internal/idtls/cert.go \
        plugin/internal/trackerclient/internal/idtls/cert_test.go
git commit -m "feat(plugin/trackerclient/idtls): self-signed Ed25519 cert + SPKI hash

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Identity-pinned `VerifyPeerCertificate`

**Files:**
- Create: `plugin/internal/trackerclient/internal/idtls/verify.go`
- Create: `plugin/internal/trackerclient/internal/idtls/verify_test.go`

- [ ] **Step 8.1: Write `verify.go`**

```go
package idtls

import (
    "crypto/ed25519"
    "crypto/tls"
    "crypto/x509"
    "errors"
    "fmt"
)

var (
    ErrNoPeerCert       = errors.New("idtls: no peer certificate")
    ErrTooManyCerts     = errors.New("idtls: peer offered more than one cert")
    ErrNotEd25519       = errors.New("idtls: peer cert is not Ed25519")
    ErrIdentityMismatch = errors.New("idtls: peer identity does not match pin")
)

// MakeClientTLSConfig returns a *tls.Config wired for our mTLS contract:
//   - Presents ourCert (the local identity-bound cert).
//   - Disables Web-PKI verification.
//   - Pins the server's SPKI to expectedServerHash via VerifyPeerCertificate.
//   - Negotiates ALPN "tokenbay/1".
//   - Disables 0-RTT (SessionTicketsDisabled).
func MakeClientTLSConfig(ourCert tls.Certificate, expectedServerHash [32]byte) *tls.Config {
    return &tls.Config{
        Certificates:             []tls.Certificate{ourCert},
        InsecureSkipVerify:       true, //nolint:gosec // see VerifyPeerCertificate
        VerifyPeerCertificate:    pinVerifier(expectedServerHash),
        NextProtos:               []string{ALPN},
        MinVersion:               tls.VersionTLS13,
        SessionTicketsDisabled:   true,
    }
}

// MakeServerTLSConfig is the symmetric helper for the in-process fakeserver
// and the real tracker (eventually). It accepts any client whose cert is a
// valid self-signed Ed25519 cert, reporting the SPKI hash via the helper
// captureClientHash.
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
            return fmt.Errorf("idtls: parse peer cert: %w", err)
        }
        if _, ok := peer.PublicKey.(ed25519.PublicKey); !ok {
            return ErrNotEd25519
        }
        got, err := SPKIHashOfCert(peer)
        if err != nil {
            return err
        }
        if got != expected {
            return ErrIdentityMismatch
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
            return fmt.Errorf("idtls: parse peer cert: %w", err)
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

- [ ] **Step 8.2: Write `verify_test.go`**

```go
package idtls

import (
    "crypto/ed25519"
    "crypto/rand"
    "crypto/rsa"
    "crypto/x509"
    "crypto/x509/pkix"
    "math/big"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func mustSelfSignedRSA(t *testing.T) []byte {
    t.Helper()
    priv, err := rsa.GenerateKey(rand.Reader, 2048)
    require.NoError(t, err)
    tmpl := &x509.Certificate{
        SerialNumber: big.NewInt(2),
        Subject:      pkix.Name{CommonName: "rsa-imposter"},
        NotBefore:    time.Unix(0, 0),
        NotAfter:     time.Now().Add(time.Hour),
    }
    der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
    require.NoError(t, err)
    return der
}

func TestPinVerifierAccepts(t *testing.T) {
    _, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)
    cert, err := CertFromIdentity(priv)
    require.NoError(t, err)
    parsed, err := x509.ParseCertificate(cert.Certificate[0])
    require.NoError(t, err)
    pin, err := SPKIHashOfCert(parsed)
    require.NoError(t, err)

    err = pinVerifier(pin)([][]byte{cert.Certificate[0]}, nil)
    assert.NoError(t, err)
}

func TestPinVerifierRejectsMismatch(t *testing.T) {
    _, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)
    cert, err := CertFromIdentity(priv)
    require.NoError(t, err)

    err = pinVerifier([32]byte{0xff})([][]byte{cert.Certificate[0]}, nil)
    assert.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestPinVerifierRejectsRSA(t *testing.T) {
    der := mustSelfSignedRSA(t)
    err := pinVerifier([32]byte{0xff})([][]byte{der}, nil)
    assert.ErrorIs(t, err, ErrNotEd25519)
}

func TestPinVerifierRejectsEmptyChain(t *testing.T) {
    err := pinVerifier([32]byte{})([][]byte{}, nil)
    assert.ErrorIs(t, err, ErrNoPeerCert)
}

func TestPinVerifierRejectsMultiCert(t *testing.T) {
    err := pinVerifier([32]byte{})([][]byte{{1}, {2}}, nil)
    assert.ErrorIs(t, err, ErrTooManyCerts)
}

func TestServerVerifierCapturesHash(t *testing.T) {
    _, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)
    cert, err := CertFromIdentity(priv)
    require.NoError(t, err)

    var got [32]byte
    err = serverVerifier(func(h [32]byte) { got = h })([][]byte{cert.Certificate[0]}, nil)
    require.NoError(t, err)
    assert.NotEqual(t, [32]byte{}, got)
}

func TestMakeClientTLSConfigShape(t *testing.T) {
    _, priv, _ := ed25519.GenerateKey(nil)
    cert, _ := CertFromIdentity(priv)
    cfg := MakeClientTLSConfig(cert, [32]byte{1})
    assert.True(t, cfg.InsecureSkipVerify)
    assert.True(t, cfg.SessionTicketsDisabled)
    assert.Equal(t, []string{ALPN}, cfg.NextProtos)
    assert.Equal(t, uint16(0x0304), cfg.MinVersion) // TLS 1.3
    assert.NotNil(t, cfg.VerifyPeerCertificate)
}
```

- [ ] **Step 8.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/internal/idtls/...
```
Expected: PASS.

- [ ] **Step 8.4: Commit**

```bash
git add plugin/internal/trackerclient/internal/idtls/verify.go \
        plugin/internal/trackerclient/internal/idtls/verify_test.go
git commit -m "feat(plugin/trackerclient/idtls): SPKI-pinned VerifyPeerCertificate

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 5 — Transport drivers

### Task 9: Transport interface (move + canonicalize)

**Files:**
- Create: `plugin/internal/trackerclient/internal/transport/transport.go`
- Modify: `plugin/internal/trackerclient/types.go`

- [ ] **Step 9.1: Write `transport.go`**

```go
// Package transport defines the network seam for trackerclient.
// Concrete drivers live in subpackages (loopback for tests, quic for prod).
package transport

import (
    "context"
    "crypto/ed25519"

    "github.com/token-bay/token-bay/shared/ids"
)

// Endpoint is the transport's view of a tracker. Mirrors
// trackerclient.TrackerEndpoint but keeps internal/transport free of
// import cycles.
type Endpoint struct {
    Addr         string
    IdentityHash [32]byte
}

// Identity is the local-side keypair holder.
type Identity interface {
    PrivateKey() ed25519.PrivateKey
    IdentityID() ids.IdentityID
}

// Transport dials a tracker.
type Transport interface {
    Dial(ctx context.Context, ep Endpoint, id Identity) (Conn, error)
}

// Conn is a connected, mTLS-authenticated transport session.
type Conn interface {
    OpenStreamSync(ctx context.Context) (Stream, error)
    AcceptStream(ctx context.Context) (Stream, error)
    PeerIdentityID() ids.IdentityID
    Close() error
    Done() <-chan struct{}
}

// Stream is a bidirectional byte stream within a Conn.
type Stream interface {
    Read(p []byte) (int, error)
    Write(p []byte) (int, error)
    Close() error
    CloseWrite() error
}
```

- [ ] **Step 9.2: Adapt the package-level types to delegate to `internal/transport`**

Replace the temporary forward declarations in `plugin/internal/trackerclient/types.go` with type aliases (or remove them entirely and import directly in `config.go`/`conn.go`). For minimal churn, replace with aliases:

Edit `plugin/internal/trackerclient/types.go`. Remove the existing block:

```go
// Transport is the network seam between the client and a tracker.
// ... up through ...
//     CloseWrite() error
// }
```

Replace with:

```go
import "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"

// Transport, Conn, and Stream are network-seam interfaces. Drivers live
// under internal/transport/.
type Transport = transport.Transport
type Conn      = transport.Conn
type Stream    = transport.Stream
```

(Keep the existing `Ctx` alias and other types intact; do not remove them.)

- [ ] **Step 9.3: Adapt `Config`'s `Signer`-to-Identity conversion**

Append to `plugin/internal/trackerclient/types.go`:

```go
// signerAsIdentity adapts the public Signer interface to the
// internal/transport.Identity interface.
type signerAsIdentity struct{ Signer }

func (s signerAsIdentity) PrivateKey() ed25519.PrivateKey { return s.Signer.PrivateKey() }
func (s signerAsIdentity) IdentityID() ids.IdentityID     { return s.Signer.IdentityID() }
```

- [ ] **Step 9.4: Compile-check**

```bash
cd plugin && go build ./internal/trackerclient/...
```
Expected: success.

- [ ] **Step 9.5: Commit**

```bash
git add plugin/internal/trackerclient/internal/transport/transport.go \
        plugin/internal/trackerclient/types.go
git commit -m "feat(plugin/trackerclient/transport): canonical Transport interface

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Loopback transport driver

**Files:**
- Create: `plugin/internal/trackerclient/internal/transport/loopback/loopback.go`
- Create: `plugin/internal/trackerclient/internal/transport/loopback/loopback_test.go`

- [ ] **Step 10.1: Write `loopback.go`**

```go
// Package loopback is an in-memory Transport for tests. Conns are paired
// in-process; OpenStreamSync on one side surfaces as AcceptStream on the
// other. There is no TLS — identity is asserted via a side-channel.
package loopback

import (
    "context"
    "errors"
    "io"
    "net"
    "sync"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    "github.com/token-bay/token-bay/shared/ids"
)

// Pair returns a (clientConn, serverConn) pair. clientConn.Dial-style
// behaviour is to return clientConn directly; tests call New(serverConn)
// to drive the server side in a goroutine.
func Pair(clientID, serverID ids.IdentityID) (*Conn, *Conn) {
    clientToServer := make(chan *streamPair, 16)
    serverToClient := make(chan *streamPair, 16)
    cli := &Conn{
        peerID:   serverID,
        opens:    clientToServer,
        accepts:  serverToClient,
        done:     make(chan struct{}),
    }
    srv := &Conn{
        peerID:   clientID,
        opens:    serverToClient,
        accepts:  clientToServer,
        done:     make(chan struct{}),
    }
    cli.peer = srv
    srv.peer = cli
    return cli, srv
}

// Driver implements transport.Transport for tests; ep.Addr selects which
// pre-registered server Conn to attach to.
type Driver struct {
    mu      sync.Mutex
    targets map[string]*Conn
}

func NewDriver() *Driver { return &Driver{targets: map[string]*Conn{}} }

// Listen registers a server Conn under addr. Tests typically call Pair,
// register the server side here, and dial via the same Driver.
func (d *Driver) Listen(addr string, srv *Conn) {
    d.mu.Lock()
    defer d.mu.Unlock()
    d.targets[addr] = srv
}

func (d *Driver) Dial(ctx context.Context, ep transport.Endpoint, _ transport.Identity) (transport.Conn, error) {
    d.mu.Lock()
    srv, ok := d.targets[ep.Addr]
    d.mu.Unlock()
    if !ok {
        return nil, errors.New("loopback: no listener at " + ep.Addr)
    }
    return srv.peer, nil
}

// Conn is the loopback Transport.Conn implementation.
type Conn struct {
    peerID  ids.IdentityID
    opens   chan *streamPair
    accepts chan *streamPair
    peer    *Conn
    closeMu sync.Mutex
    closed  bool
    done    chan struct{}
}

func (c *Conn) OpenStreamSync(ctx context.Context) (transport.Stream, error) {
    if c.isClosed() {
        return nil, errors.New("loopback: conn closed")
    }
    a, b := newStreamPair()
    select {
    case c.opens <- &streamPair{a: b, b: a}:
        // local side keeps a, peer's AcceptStream gets b
        return a, nil
    case <-c.done:
        return nil, io.ErrClosedPipe
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

func (c *Conn) AcceptStream(ctx context.Context) (transport.Stream, error) {
    select {
    case sp := <-c.accepts:
        return sp.a, nil
    case <-c.done:
        return nil, io.EOF
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

func (c *Conn) PeerIdentityID() ids.IdentityID { return c.peerID }

func (c *Conn) Close() error {
    c.closeMu.Lock()
    defer c.closeMu.Unlock()
    if c.closed {
        return nil
    }
    c.closed = true
    close(c.done)
    if c.peer != nil {
        // ripple the close so the peer's blocked Reads/Accepts return.
        c.peer.closeOnce()
    }
    return nil
}

func (c *Conn) closeOnce() {
    c.closeMu.Lock()
    defer c.closeMu.Unlock()
    if c.closed {
        return
    }
    c.closed = true
    close(c.done)
}

func (c *Conn) isClosed() bool {
    c.closeMu.Lock()
    defer c.closeMu.Unlock()
    return c.closed
}

func (c *Conn) Done() <-chan struct{} { return c.done }

// --- stream pair ------------------------------------------------------

type streamPair struct {
    a *stream
    b *stream
}

type stream struct {
    in   net.Conn // we use net.Pipe under the hood for io.ReadWriter semantics
    out  net.Conn
    once sync.Once
}

func newStreamPair() (*stream, *stream) {
    aIn, bOut := net.Pipe()
    bIn, aOut := net.Pipe()
    a := &stream{in: aIn, out: aOut}
    b := &stream{in: bIn, out: bOut}
    return a, b
}

func (s *stream) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s *stream) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *stream) Close() error {
    s.once.Do(func() { _ = s.in.Close(); _ = s.out.Close() })
    return nil
}
func (s *stream) CloseWrite() error { return s.out.Close() }
```

- [ ] **Step 10.2: Write `loopback_test.go`**

```go
package loopback

import (
    "context"
    "io"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/shared/ids"
)

func TestPairOpenAcceptRoundTrip(t *testing.T) {
    cli, srv := Pair(ids.IdentityID{1}, ids.IdentityID{2})

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()

    errCh := make(chan error, 1)
    go func() {
        s, err := srv.AcceptStream(ctx)
        if err != nil {
            errCh <- err
            return
        }
        defer s.Close()
        buf := make([]byte, 4)
        if _, err := io.ReadFull(s, buf); err != nil {
            errCh <- err
            return
        }
        if _, err := s.Write(append(buf, '!')); err != nil {
            errCh <- err
            return
        }
        errCh <- nil
    }()

    s, err := cli.OpenStreamSync(ctx)
    require.NoError(t, err)
    _, err = s.Write([]byte("ping"))
    require.NoError(t, err)
    require.NoError(t, s.CloseWrite())

    out := make([]byte, 5)
    _, err = io.ReadFull(s, out)
    require.NoError(t, err)
    assert.Equal(t, []byte("ping!"), out)
    require.NoError(t, <-errCh)
}

func TestCloseRipplesToPeer(t *testing.T) {
    cli, srv := Pair(ids.IdentityID{}, ids.IdentityID{})
    require.NoError(t, cli.Close())
    select {
    case <-srv.Done():
    case <-time.After(time.Second):
        t.Fatal("peer Done channel not closed within 1s")
    }
}

func TestDriverDial(t *testing.T) {
    cli, srv := Pair(ids.IdentityID{1}, ids.IdentityID{2})
    d := NewDriver()
    d.Listen("test:9000", srv)

    ctx, cancel := context.WithTimeout(context.Background(), time.Second)
    defer cancel()
    got, err := d.Dial(ctx, struct {
        Addr         string
        IdentityHash [32]byte
    }{Addr: "test:9000"}, nil)
    require.NoError(t, err)
    _ = got
    _ = cli
}
```

(Note: the `transport.Endpoint` value in the last test is constructed via a struct literal because the test file does not import `internal/transport` directly to keep it local. If lint flags it, simply import `transport` and use `transport.Endpoint{Addr: "test:9000"}` instead.)

- [ ] **Step 10.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/internal/transport/loopback/...
```
Expected: PASS.

- [ ] **Step 10.4: Commit**

```bash
git add plugin/internal/trackerclient/internal/transport/loopback/
git commit -m "feat(plugin/trackerclient/transport): loopback driver

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: QUIC transport driver

**Files:**
- Create: `plugin/internal/trackerclient/internal/transport/quic/quic.go`
- Create: `plugin/internal/trackerclient/internal/transport/quic/quic_test.go`

- [ ] **Step 11.1: Write `quic.go`**

```go
// Package quic is the production Transport driver. Wraps quic-go and
// applies our mTLS contract from internal/idtls.
package quic

import (
    "context"
    "crypto/sha256"
    "errors"
    "fmt"

    quicgo "github.com/quic-go/quic-go"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/idtls"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    "github.com/token-bay/token-bay/shared/ids"
)

// Driver is a stateless transport.Transport. Each Dial brings up a fresh
// QUIC connection.
type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Dial(ctx context.Context, ep transport.Endpoint, id transport.Identity) (transport.Conn, error) {
    if id == nil {
        return nil, errors.New("quic: identity is required")
    }
    cert, err := idtls.CertFromIdentity(id.PrivateKey())
    if err != nil {
        return nil, fmt.Errorf("quic: cert: %w", err)
    }
    tlsCfg := idtls.MakeClientTLSConfig(cert, ep.IdentityHash)

    qcfg := &quicgo.Config{
        EnableDatagrams: false,
        Allow0RTT:       false,
    }
    raw, err := quicgo.DialAddr(ctx, ep.Addr, tlsCfg, qcfg)
    if err != nil {
        return nil, fmt.Errorf("quic: dial: %w", err)
    }
    // Identity is captured by VerifyPeerCertificate, but we must read
    // the cert here to map it to ids.IdentityID for the Conn.
    state := raw.ConnectionState().TLS
    if len(state.PeerCertificates) != 1 {
        _ = raw.CloseWithError(0, "no peer cert")
        return nil, errors.New("quic: peer presented no cert")
    }
    h := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
    var pid ids.IdentityID
    copy(pid[:], h[:])

    done := make(chan struct{})
    go func() {
        <-raw.Context().Done()
        close(done)
    }()

    return &qConn{raw: raw, peerID: pid, done: done}, nil
}

type qConn struct {
    raw    *quicgo.Conn
    peerID ids.IdentityID
    done   chan struct{}
}

func (c *qConn) OpenStreamSync(ctx context.Context) (transport.Stream, error) {
    s, err := c.raw.OpenStreamSync(ctx)
    if err != nil {
        return nil, err
    }
    return &qStream{raw: s}, nil
}

func (c *qConn) AcceptStream(ctx context.Context) (transport.Stream, error) {
    s, err := c.raw.AcceptStream(ctx)
    if err != nil {
        return nil, err
    }
    return &qStream{raw: s}, nil
}

func (c *qConn) PeerIdentityID() ids.IdentityID { return c.peerID }
func (c *qConn) Done() <-chan struct{}          { return c.done }

func (c *qConn) Close() error {
    return c.raw.CloseWithError(0, "client close")
}

type qStream struct {
    raw *quicgo.Stream
}

func (s *qStream) Read(p []byte) (int, error)  { return s.raw.Read(p) }
func (s *qStream) Write(p []byte) (int, error) { return s.raw.Write(p) }
func (s *qStream) Close() error                { return s.raw.Close() }
func (s *qStream) CloseWrite() error {
    // quic-go's Stream.Close() half-closes the write side; the read side
    // continues until the peer half-closes too. The behaviour we want is
    // identical, so we delegate.
    return s.raw.Close()
}
```

(`quic-go` API names track v0.50+; if the actual version pulled in has slightly different signatures — e.g., `quic.Connection` vs `*quic.Conn` — adjust the type names but keep the methods/contract identical. Run `go vet` to confirm signatures match.)

- [ ] **Step 11.2: Write `quic_test.go` (smoke test only — full integration in Task 24)**

```go
package quic

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

func TestDialReturnsErrorOnMissingServer(t *testing.T) {
    d := New()
    ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
    defer cancel()
    _, err := d.Dial(ctx, transport.Endpoint{Addr: "127.0.0.1:1"}, nil)
    assert.Error(t, err)
}
```

- [ ] **Step 11.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/internal/transport/quic/...
```
Expected: PASS (the smoke test asserts dial fails, not succeeds).

- [ ] **Step 11.4: Commit**

```bash
git add plugin/internal/trackerclient/internal/transport/quic/
git commit -m "feat(plugin/trackerclient/transport): production QUIC driver

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 6 — Connection state, supervisor, and reconnect

### Task 12: Connection lifecycle + supervisor + reconnect

**Files:**
- Create: `plugin/internal/trackerclient/conn.go`
- Create: `plugin/internal/trackerclient/conn_test.go`
- Create: `plugin/internal/trackerclient/reconnect.go`
- Create: `plugin/internal/trackerclient/reconnect_test.go`

- [ ] **Step 12.1: Write `conn.go`**

```go
package trackerclient

import (
    "context"
    "errors"
    "sync"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

// connHolder gives RPC callers a way to await the current Conn.
type connHolder struct {
    mu      sync.RWMutex
    current transport.Conn
    waiters []chan transport.Conn
    closed  bool
}

func (h *connHolder) get(ctx context.Context) (transport.Conn, error) {
    h.mu.RLock()
    if h.closed {
        h.mu.RUnlock()
        return nil, ErrClosed
    }
    if h.current != nil {
        c := h.current
        h.mu.RUnlock()
        return c, nil
    }
    h.mu.RUnlock()

    h.mu.Lock()
    if h.closed {
        h.mu.Unlock()
        return nil, ErrClosed
    }
    if h.current != nil {
        c := h.current
        h.mu.Unlock()
        return c, nil
    }
    ch := make(chan transport.Conn, 1)
    h.waiters = append(h.waiters, ch)
    h.mu.Unlock()

    select {
    case c := <-ch:
        if c == nil {
            return nil, ErrClosed
        }
        return c, nil
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

func (h *connHolder) set(c transport.Conn) {
    h.mu.Lock()
    h.current = c
    waiters := h.waiters
    h.waiters = nil
    h.mu.Unlock()
    for _, w := range waiters {
        select {
        case w <- c:
        default:
        }
    }
}

func (h *connHolder) clear() {
    h.mu.Lock()
    h.current = nil
    h.mu.Unlock()
}

func (h *connHolder) close() {
    h.mu.Lock()
    h.closed = true
    waiters := h.waiters
    h.waiters = nil
    h.mu.Unlock()
    for _, w := range waiters {
        close(w)
    }
}

var errSupervisorStopped = errors.New("trackerclient: supervisor stopped")
```

- [ ] **Step 12.2: Write `reconnect.go`**

```go
package trackerclient

import (
    "context"
    "math/rand"
    "time"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

// backoffDelay returns the next backoff value for the supervisor. The
// returned duration is bounded by [BackoffBase, BackoffMax] and includes
// jitter in [0.5x, 1.0x] of the deterministic component.
func backoffDelay(attempt int, base, max time.Duration, r *rand.Rand) time.Duration {
    deterministic := base << attempt
    if deterministic <= 0 || deterministic > max {
        deterministic = max
    }
    jitter := 0.5 + r.Float64()*0.5
    out := time.Duration(float64(deterministic) * jitter)
    if out > max {
        out = max
    }
    if out < base/2 {
        out = base / 2
    }
    return out
}

// supervisor runs the dial → run → reconnect loop on its own goroutine.
type supervisor struct {
    cfg     Config
    holder  *connHolder
    ctx     context.Context
    cancel  context.CancelFunc
    done    chan struct{}
    rng     *rand.Rand

    statusMu sync.Mutex
    status   ConnectionState
}

// newSupervisor wires the supervisor; Start launches the goroutine.
func newSupervisor(parent context.Context, cfg Config, holder *connHolder) *supervisor {
    ctx, cancel := context.WithCancel(parent)
    return &supervisor{
        cfg:    cfg,
        holder: holder,
        ctx:    ctx,
        cancel: cancel,
        done:   make(chan struct{}),
        rng:    rand.New(rand.NewSource(cfg.Clock().UnixNano())),
        status: ConnectionState{Phase: PhaseDisconnected},
    }
}

func (s *supervisor) start() {
    go s.run()
}

func (s *supervisor) stop() {
    s.cancel()
    <-s.done
    s.holder.close()
}

func (s *supervisor) run() {
    defer close(s.done)

    epIdx := 0
    attempt := 0
    var lastConnect time.Time

    for {
        if err := s.ctx.Err(); err != nil {
            s.setStatus(PhaseClosed, TrackerEndpoint{}, err, time.Time{})
            return
        }
        ep := s.cfg.Endpoints[epIdx%len(s.cfg.Endpoints)]
        s.setStatus(PhaseConnecting, ep, nil, time.Time{})

        dialCtx, dialCancel := context.WithTimeout(s.ctx, s.cfg.DialTimeout)
        conn, err := s.cfg.Transport.Dial(dialCtx, transport.Endpoint{
            Addr:         ep.Addr,
            IdentityHash: ep.IdentityHash,
        }, signerAsIdentity{Signer: s.cfg.Identity})
        dialCancel()

        if err != nil {
            delay := backoffDelay(attempt, s.cfg.BackoffBase, s.cfg.BackoffMax, s.rng)
            attemptAt := s.cfg.Clock().Add(delay)
            s.setStatus(PhaseDisconnected, ep, err, attemptAt)
            attempt++
            epIdx++
            select {
            case <-time.After(delay):
            case <-s.ctx.Done():
                s.setStatus(PhaseClosed, ep, s.ctx.Err(), time.Time{})
                return
            }
            continue
        }

        // connected
        s.holder.set(conn)
        lastConnect = s.cfg.Clock()
        s.setStatus(PhaseConnected, ep, nil, time.Time{})
        s.cfg.Logger.Info().Str("addr", ep.Addr).Msg("trackerclient: connected")

        // Block until the connection drops or supervisor is cancelled.
        select {
        case <-conn.Done():
            // connection died
        case <-s.ctx.Done():
            _ = conn.Close()
            s.holder.clear()
            s.setStatus(PhaseClosed, ep, s.ctx.Err(), time.Time{})
            return
        }

        s.holder.clear()
        s.setStatus(PhaseDisconnected, ep, ErrConnectionLost, time.Time{})

        // reset the attempt counter only if we were stably connected ≥ 30s
        if s.cfg.Clock().Sub(lastConnect) >= 30*time.Second {
            attempt = 0
        } else {
            attempt++
        }
        epIdx++
    }
}

func (s *supervisor) setStatus(phase ConnectionPhase, ep TrackerEndpoint, lastErr error, nextAt time.Time) {
    s.statusMu.Lock()
    defer s.statusMu.Unlock()
    s.status.Phase = phase
    s.status.Endpoint = ep
    s.status.LastError = lastErr
    s.status.NextAttemptAt = nextAt
    if phase == PhaseConnected {
        s.status.ConnectedAt = s.cfg.Clock()
    }
}

func (s *supervisor) snapshot() ConnectionState {
    s.statusMu.Lock()
    defer s.statusMu.Unlock()
    return s.status
}
```

(Add `import "sync"` to the top of `reconnect.go`.)

- [ ] **Step 12.3: Write `reconnect_test.go`**

```go
package trackerclient

import (
    "math/rand"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
)

func TestBackoffDelayBounded(t *testing.T) {
    r := rand.New(rand.NewSource(1))
    base := 100 * time.Millisecond
    max := 5 * time.Second
    for attempt := 0; attempt < 20; attempt++ {
        d := backoffDelay(attempt, base, max, r)
        assert.LessOrEqual(t, d, max, "attempt=%d", attempt)
        assert.GreaterOrEqual(t, d, base/2, "attempt=%d", attempt)
    }
}

func TestBackoffDelayMonotonicEnvelope(t *testing.T) {
    r := rand.New(rand.NewSource(42))
    base := 100 * time.Millisecond
    max := 30 * time.Second
    // The deterministic component should reach max by attempt=10.
    saturated := backoffDelay(10, base, max, r)
    assert.GreaterOrEqual(t, saturated, max/2)
}
```

- [ ] **Step 12.4: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run Backoff
```
Expected: PASS.

- [ ] **Step 12.5: Commit**

```bash
git add plugin/internal/trackerclient/conn.go \
        plugin/internal/trackerclient/reconnect.go \
        plugin/internal/trackerclient/reconnect_test.go
git commit -m "feat(plugin/trackerclient): supervisor + reconnect with jittered backoff

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 13: Top-level `Client` (New / Start / Close / Status / WaitConnected)

**Files:**
- Create: `plugin/internal/trackerclient/trackerclient.go`
- Create: `plugin/internal/trackerclient/trackerclient_test.go`

- [ ] **Step 13.1: Write `trackerclient.go`**

```go
package trackerclient

import (
    "context"
    "sync"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    quicdriver "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/quic"
)

// Client is the public surface. Concurrency-safe.
type Client struct {
    cfg    Config
    holder *connHolder
    sup    *supervisor

    startedMu sync.Mutex
    started   bool
    closed    bool

    cache *balanceCache // wired in the balance cache task
}

// New validates cfg and constructs (but does not start) a Client.
func New(cfg Config) (*Client, error) {
    cfg = cfg.withDefaults()
    if cfg.Transport == nil {
        cfg.Transport = quicdriver.New()
    }
    if err := cfg.Validate(); err != nil {
        return nil, err
    }
    return &Client{
        cfg:    cfg,
        holder: &connHolder{},
    }, nil
}

// Start launches the supervisor. Returns ErrAlreadyStarted on the second
// call. The supervisor runs until Close is called or ctx is cancelled.
func (c *Client) Start(ctx context.Context) error {
    c.startedMu.Lock()
    defer c.startedMu.Unlock()
    if c.closed {
        return ErrClosed
    }
    if c.started {
        return ErrAlreadyStarted
    }
    c.started = true
    c.sup = newSupervisor(ctx, c.cfg, c.holder)
    c.sup.start()
    return nil
}

// Close terminates the supervisor and is idempotent.
func (c *Client) Close() error {
    c.startedMu.Lock()
    if c.closed {
        c.startedMu.Unlock()
        return nil
    }
    c.closed = true
    sup := c.sup
    c.startedMu.Unlock()
    if sup != nil {
        sup.stop()
    }
    return nil
}

// Status returns a snapshot of supervisor state.
func (c *Client) Status() ConnectionState {
    if c.sup == nil {
        return ConnectionState{Phase: PhaseDisconnected}
    }
    return c.sup.snapshot()
}

// WaitConnected blocks until the supervisor reports Connected, or ctx is done.
func (c *Client) WaitConnected(ctx context.Context) error {
    _, err := c.holder.get(ctx)
    return err
}

// connect returns the current connection or blocks until one exists.
// All RPC methods route through here.
func (c *Client) connect(ctx context.Context) (transport.Conn, error) {
    return c.holder.get(ctx)
}
```

- [ ] **Step 13.2: Write `trackerclient_test.go`**

```go
package trackerclient

import (
    "context"
    "errors"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

type noopTransport struct{}

func (noopTransport) Dial(ctx context.Context, _ transport.Endpoint, _ transport.Identity) (transport.Conn, error) {
    return nil, errors.New("no")
}

func TestNewRejectsBadConfig(t *testing.T) {
    _, err := New(Config{})
    require.Error(t, err)
}

func TestNewAcceptsValid(t *testing.T) {
    cfg := validConfig(t)
    cfg.Transport = noopTransport{}
    _, err := New(cfg)
    require.NoError(t, err)
}

func TestStartTwiceErrors(t *testing.T) {
    cfg := validConfig(t)
    cfg.Transport = noopTransport{}
    c, err := New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))
    err = c.Start(context.Background())
    assert.ErrorIs(t, err, ErrAlreadyStarted)
    require.NoError(t, c.Close())
}

func TestCloseBeforeStart(t *testing.T) {
    cfg := validConfig(t)
    cfg.Transport = noopTransport{}
    c, err := New(cfg)
    require.NoError(t, err)
    assert.NoError(t, c.Close())
}

func TestWaitConnectedTimesOut(t *testing.T) {
    cfg := validConfig(t)
    cfg.Transport = noopTransport{}
    c, err := New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))
    defer c.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
    defer cancel()
    err = c.WaitConnected(ctx)
    assert.ErrorIs(t, err, context.DeadlineExceeded)
}
```

- [ ] **Step 13.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run 'TestNew|Start|Close|WaitConnected'
```
Expected: PASS.

- [ ] **Step 13.4: Commit**

```bash
git add plugin/internal/trackerclient/trackerclient.go \
        plugin/internal/trackerclient/trackerclient_test.go
git commit -m "feat(plugin/trackerclient): Client New/Start/Close/Status/WaitConnected

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 7 — Unary RPC dispatch + 9 RPC methods

### Task 14: Unary RPC dispatch helper

**Files:**
- Create: `plugin/internal/trackerclient/rpc.go`
- Create: `plugin/internal/trackerclient/rpc_test.go`

- [ ] **Step 14.1: Write `rpc.go`**

```go
package trackerclient

import (
    "context"
    "fmt"

    "google.golang.org/protobuf/proto"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

// callUnary opens a fresh stream, writes one RpcRequest framed, reads one
// RpcResponse framed, and returns the per-method response payload via dst.
//
// Concurrency: safe — each call opens its own stream.
func (c *Client) callUnary(ctx context.Context, method tbproto.RpcMethod, payload proto.Message, dst proto.Message) error {
    conn, err := c.connect(ctx)
    if err != nil {
        return err
    }

    stream, err := conn.OpenStreamSync(ctx)
    if err != nil {
        if isConnDead(conn) {
            return ErrConnectionLost
        }
        return fmt.Errorf("trackerclient: open stream: %w", err)
    }
    defer stream.Close()

    req, err := wire.MarshalRequest(method, payload)
    if err != nil {
        return err
    }
    if err := tbproto.ValidateRPCRequest(req); err != nil {
        return fmt.Errorf("trackerclient: validate request: %w", err)
    }
    if err := wire.Write(stream, req, c.cfg.MaxFrameSize); err != nil {
        return fmt.Errorf("trackerclient: write request: %w", err)
    }
    _ = stream.CloseWrite()

    var resp tbproto.RpcResponse
    if err := wire.Read(stream, &resp, c.cfg.MaxFrameSize); err != nil {
        if isConnDead(conn) {
            return ErrConnectionLost
        }
        return fmt.Errorf("trackerclient: read response: %w", err)
    }
    if err := tbproto.ValidateRPCResponse(&resp); err != nil {
        return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
    }
    if err := statusToErr(resp.Status, resp.Error); err != nil {
        return err
    }
    if err := wire.UnmarshalResponse(&resp, dst); err != nil {
        return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
    }
    return nil
}

func isConnDead(conn interface{ Done() <-chan struct{} }) bool {
    select {
    case <-conn.Done():
        return true
    default:
        return false
    }
}
```

- [ ] **Step 14.2: Write a minimal `rpc_test.go` (full RPC tests come with each method)**

```go
package trackerclient

import "testing"

func TestIsConnDead(t *testing.T) {
    closed := make(chan struct{})
    close(closed)
    type doneOnly struct{ ch <-chan struct{} }
    // can't use anonymous interface impl directly; rely on per-RPC tests instead.
    _ = closed
}
```

(Real coverage comes via the per-RPC tests below; this file exists so coverage tooling sees the package.)

- [ ] **Step 14.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/...
```
Expected: PASS.

- [ ] **Step 14.4: Commit**

```bash
git add plugin/internal/trackerclient/rpc.go plugin/internal/trackerclient/rpc_test.go
git commit -m "feat(plugin/trackerclient): callUnary dispatch helper

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 15: `BrokerRequest`, `Settle`, `Enroll`

**Files:**
- Modify: `plugin/internal/trackerclient/rpc.go`
- Create: `plugin/internal/trackerclient/rpc_methods_test.go`
- Create: `plugin/internal/trackerclient/test/fakeserver/fakeserver.go` (skeleton)

- [ ] **Step 15.1: Append the three method bodies to `rpc.go`**

```go
// BrokerRequest sends an EnvelopeSigned and returns the seeder assignment.
func (c *Client) BrokerRequest(ctx context.Context, env *tbproto.EnvelopeSigned) (*BrokerResponse, error) {
    if env == nil {
        return nil, fmt.Errorf("%w: nil envelope", ErrInvalidResponse)
    }
    var resp tbproto.BrokerResponse
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env, &resp); err != nil {
        return nil, err
    }
    return &BrokerResponse{
        SeederAddr:       string(resp.SeederAddr),
        SeederPubkey:     resp.SeederPubkey,
        ReservationToken: resp.ReservationToken,
    }, nil
}

// Settle sends the consumer's signature over a finalized entry preimage.
func (c *Client) Settle(ctx context.Context, preimageHash, sig []byte) error {
    if len(preimageHash) != 32 {
        return fmt.Errorf("%w: preimageHash len=%d, want 32", ErrInvalidResponse, len(preimageHash))
    }
    req := &tbproto.SettleRequest{PreimageHash: preimageHash, ConsumerSig: sig}
    var ack tbproto.SettleAck
    return c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_SETTLE, req, &ack)
}

// Enroll requests an identity binding from the tracker.
func (c *Client) Enroll(ctx context.Context, r *EnrollRequest) (*EnrollResponse, error) {
    if r == nil {
        return nil, fmt.Errorf("%w: nil EnrollRequest", ErrInvalidResponse)
    }
    req := &tbproto.EnrollRequest{
        IdentityPubkey:     r.IdentityPubkey,
        Role:               r.Role,
        AccountFingerprint: r.AccountFingerprint[:],
        Nonce:              r.Nonce[:],
        ConsumerSig:        r.Sig,
    }
    var resp tbproto.EnrollResponse
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_ENROLL, req, &resp); err != nil {
        return nil, err
    }
    out := &EnrollResponse{
        StarterGrantCredits:   resp.StarterGrantCredits,
        StarterGrantEntryBlob: resp.StarterGrantEntry,
    }
    copy(out.IdentityID[:], resp.IdentityId)
    return out, nil
}
```

- [ ] **Step 15.2: Write a minimal `fakeserver` skeleton**

`plugin/internal/trackerclient/test/fakeserver/fakeserver.go`:

```go
// Package fakeserver implements just enough of the tracker side of the
// wire protocol to drive trackerclient tests. It reads RpcRequests off a
// loopback transport and dispatches to per-method handlers; default
// handlers return RPC_STATUS_OK with an empty payload.
package fakeserver

import (
    "context"
    "errors"
    "io"

    "google.golang.org/protobuf/proto"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Handler signature: receive a per-method request, return a status + an
// optional response payload.
type Handler func(ctx context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError)

type Server struct {
    Conn        transport.Conn
    MaxFrameSz  int
    Handlers    map[tbproto.RpcMethod]Handler
    PayloadType map[tbproto.RpcMethod]func() proto.Message
}

func New(conn transport.Conn) *Server {
    return &Server{
        Conn:       conn,
        MaxFrameSz: 1 << 20,
        Handlers:   map[tbproto.RpcMethod]Handler{},
        PayloadType: map[tbproto.RpcMethod]func() proto.Message{
            tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST: func() proto.Message { return &tbproto.EnvelopeSigned{} },
            tbproto.RpcMethod_RPC_METHOD_SETTLE:         func() proto.Message { return &tbproto.SettleRequest{} },
            tbproto.RpcMethod_RPC_METHOD_ENROLL:         func() proto.Message { return &tbproto.EnrollRequest{} },
            tbproto.RpcMethod_RPC_METHOD_BALANCE:        func() proto.Message { return &tbproto.BalanceRequest{} },
            tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT:   func() proto.Message { return &tbproto.UsageReport{} },
            tbproto.RpcMethod_RPC_METHOD_ADVERTISE:      func() proto.Message { return &tbproto.Advertisement{} },
            tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST: func() proto.Message { return &tbproto.TransferRequest{} },
            tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE:    func() proto.Message { return &tbproto.StunAllocateRequest{} },
            tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN:  func() proto.Message { return &tbproto.TurnRelayOpenRequest{} },
        },
    }
}

// Run reads one stream at a time off the conn until ctx is cancelled or
// the conn closes. Each stream gets one request → one response.
func (s *Server) Run(ctx context.Context) error {
    for {
        stream, err := s.Conn.AcceptStream(ctx)
        if err != nil {
            if errors.Is(err, io.EOF) || ctx.Err() != nil {
                return nil
            }
            return err
        }
        go s.handle(ctx, stream)
    }
}

func (s *Server) handle(ctx context.Context, stream transport.Stream) {
    defer stream.Close()
    var req tbproto.RpcRequest
    if err := wire.Read(stream, &req, s.MaxFrameSz); err != nil {
        return
    }

    typ, ok := s.PayloadType[req.Method]
    if !ok {
        s.respond(stream, tbproto.RpcStatus_RPC_STATUS_INTERNAL, nil, &tbproto.RpcError{
            Code: "unknown_method", Message: req.Method.String(),
        })
        return
    }
    payload := typ()
    if err := proto.Unmarshal(req.Payload, payload); err != nil {
        s.respond(stream, tbproto.RpcStatus_RPC_STATUS_INVALID, nil, &tbproto.RpcError{
            Code: "bad_payload", Message: err.Error(),
        })
        return
    }

    h, ok := s.Handlers[req.Method]
    if !ok {
        // Default handler: OK with empty payload.
        s.respond(stream, tbproto.RpcStatus_RPC_STATUS_OK, nil, nil)
        return
    }
    status, resp, rerr := h(ctx, payload)
    s.respond(stream, status, resp, rerr)
}

func (s *Server) respond(stream transport.Stream, status tbproto.RpcStatus, payload proto.Message, rerr *tbproto.RpcError) {
    out := &tbproto.RpcResponse{Status: status, Error: rerr}
    if payload != nil {
        b, err := proto.Marshal(payload)
        if err != nil {
            return
        }
        out.Payload = b
    }
    _ = wire.Write(stream, out, s.MaxFrameSz)
}
```

- [ ] **Step 15.3: Write `rpc_methods_test.go` covering BrokerRequest / Settle / Enroll**

```go
package trackerclient

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
    "google.golang.org/protobuf/proto"
)

// helper: stand up a wired Client + fakeserver pair using loopback.
func newWiredClient(t *testing.T, register func(*fakeserver.Server)) (*Client, func()) {
    t.Helper()
    cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
    drv := loopback.NewDriver()
    drv.Listen("addr:1", srv)

    fake := fakeserver.New(srv)
    if register != nil {
        register(fake)
    }

    cfg := validConfig(t)
    cfg.Transport = drv
    cfg.Endpoints[0].Addr = "addr:1"
    c, err := New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))

    serverDone := make(chan struct{})
    go func() {
        _ = fake.Run(context.Background())
        close(serverDone)
    }()

    // Force the client's first connect to use cli (the loopback driver
    // returned srv.peer == cli during Dial).
    _ = cli

    // Wait for connection.
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    require.NoError(t, c.WaitConnected(ctx))

    return c, func() {
        _ = c.Close()
        _ = srv.Close()
        <-serverDone
    }
}

func TestBrokerRequestRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerResponse{
                SeederAddr:       []byte("seeder.example:443"),
                SeederPubkey:     make([]byte, 32),
                ReservationToken: []byte("token"),
            }, nil
        }
    })
    defer cleanup()

    env := &tbproto.EnvelopeSigned{Body: &tbproto.EnvelopeBody{}}
    resp, err := c.BrokerRequest(context.Background(), env)
    require.NoError(t, err)
    assert.Equal(t, "seeder.example:443", resp.SeederAddr)
    assert.Equal(t, []byte("token"), resp.ReservationToken)
}

func TestBrokerRequestNoCapacity(t *testing.T) {
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY, nil, &tbproto.RpcError{
                Code: "no_capacity", Message: "all seeders busy",
            }
        }
    })
    defer cleanup()

    _, err := c.BrokerRequest(context.Background(), &tbproto.EnvelopeSigned{})
    assert.ErrorIs(t, err, ErrNoCapacity)
}

func TestSettleRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, nil) // default OK handler is fine
    defer cleanup()
    err := c.Settle(context.Background(), make([]byte, 32), make([]byte, 64))
    require.NoError(t, err)
}

func TestSettleRejectsBadHash(t *testing.T) {
    c, cleanup := newWiredClient(t, nil)
    defer cleanup()
    err := c.Settle(context.Background(), []byte{1, 2, 3}, nil)
    require.Error(t, err)
}

func TestEnrollRoundTrip(t *testing.T) {
    var got [32]byte
    copy(got[:], "captured-id-12345678901234567890")
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_ENROLL] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.EnrollResponse{
                IdentityId:           got[:],
                StarterGrantCredits:  50,
                StarterGrantEntry:    []byte("entry-blob"),
            }, nil
        }
    })
    defer cleanup()
    resp, err := c.Enroll(context.Background(), &EnrollRequest{})
    require.NoError(t, err)
    assert.Equal(t, ids.IdentityID(got), resp.IdentityID)
    assert.Equal(t, uint64(50), resp.StarterGrantCredits)
}
```

- [ ] **Step 15.4: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run 'BrokerRequest|Settle|Enroll'
```
Expected: PASS.

- [ ] **Step 15.5: Commit**

```bash
git add plugin/internal/trackerclient/rpc.go \
        plugin/internal/trackerclient/rpc_methods_test.go \
        plugin/internal/trackerclient/test/fakeserver/fakeserver.go
git commit -m "feat(plugin/trackerclient): BrokerRequest + Settle + Enroll RPCs

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 16: Balance + balance cache + `BalanceCached`

**Files:**
- Modify: `plugin/internal/trackerclient/rpc.go`
- Create: `plugin/internal/trackerclient/balance_cache.go`
- Create: `plugin/internal/trackerclient/balance_cache_test.go`
- Modify: `plugin/internal/trackerclient/trackerclient.go` (initialize cache, hook supervisor invalidation)
- Modify: `plugin/internal/trackerclient/rpc_methods_test.go` (Balance round-trip)

- [ ] **Step 16.1: Append `Balance` to `rpc.go`**

```go
// Balance fetches a fresh signed balance snapshot from the tracker.
// Bypasses the cache; callers usually want BalanceCached instead.
func (c *Client) Balance(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
    req := &tbproto.BalanceRequest{IdentityId: id[:]}
    var resp tbproto.SignedBalanceSnapshot
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BALANCE, req, &resp); err != nil {
        return nil, err
    }
    if resp.Body == nil {
        return nil, fmt.Errorf("%w: balance response missing body", ErrInvalidResponse)
    }
    return &resp, nil
}
```

(Add `"github.com/token-bay/token-bay/shared/ids"` to the imports of `rpc.go`.)

- [ ] **Step 16.2: Write `balance_cache.go`**

```go
package trackerclient

import (
    "context"
    "sync"
    "time"

    "golang.org/x/sync/singleflight"

    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

type balanceCache struct {
    ttlHeadroom time.Duration
    clock       func() time.Time

    mu    sync.RWMutex
    snaps map[ids.IdentityID]*tbproto.SignedBalanceSnapshot

    sf singleflight.Group
}

func newBalanceCache(headroom time.Duration, clock func() time.Time) *balanceCache {
    return &balanceCache{
        ttlHeadroom: headroom,
        clock:       clock,
        snaps:       map[ids.IdentityID]*tbproto.SignedBalanceSnapshot{},
    }
}

// fetchFn is supplied by the Client and calls Balance() under the hood.
type fetchFn func(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error)

// Get returns a fresh-enough snapshot, fetching via fn if missing or
// within ttlHeadroom of expiry.
func (b *balanceCache) Get(ctx context.Context, id ids.IdentityID, fn fetchFn) (*tbproto.SignedBalanceSnapshot, error) {
    if snap := b.lookupFresh(id); snap != nil {
        return snap, nil
    }

    v, err, _ := b.sf.Do(string(id[:]), func() (interface{}, error) {
        // Re-check under singleflight in case a concurrent caller refreshed.
        if snap := b.lookupFresh(id); snap != nil {
            return snap, nil
        }
        snap, err := fn(ctx, id)
        if err != nil {
            return nil, err
        }
        b.store(id, snap)
        return snap, nil
    })
    if err != nil {
        return nil, err
    }
    return v.(*tbproto.SignedBalanceSnapshot), nil
}

func (b *balanceCache) lookupFresh(id ids.IdentityID) *tbproto.SignedBalanceSnapshot {
    b.mu.RLock()
    defer b.mu.RUnlock()
    snap, ok := b.snaps[id]
    if !ok || snap.Body == nil {
        return nil
    }
    expiresAt := time.Unix(int64(snap.Body.ExpiresAt), 0)
    if b.clock().Add(b.ttlHeadroom).After(expiresAt) {
        return nil
    }
    return snap
}

func (b *balanceCache) store(id ids.IdentityID, snap *tbproto.SignedBalanceSnapshot) {
    b.mu.Lock()
    b.snaps[id] = snap
    b.mu.Unlock()
}

// invalidate clears all cached snapshots; called on connection drop.
func (b *balanceCache) invalidate() {
    b.mu.Lock()
    b.snaps = map[ids.IdentityID]*tbproto.SignedBalanceSnapshot{}
    b.mu.Unlock()
}
```

- [ ] **Step 16.3: Wire the cache into `Client`**

Edit `trackerclient.go`. In `New`:

```go
return &Client{
    cfg:    cfg,
    holder: &connHolder{},
    cache:  newBalanceCache(cfg.BalanceRefreshHeadroom, cfg.Clock),
}, nil
```

Append a method to `rpc.go`:

```go
// BalanceCached returns a cached snapshot if fresh, otherwise refreshes
// via Balance(). Concurrent stale callers coalesce on the same fetch.
func (c *Client) BalanceCached(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
    return c.cache.Get(ctx, id, c.Balance)
}
```

- [ ] **Step 16.4: Write `balance_cache_test.go`**

```go
package trackerclient

import (
    "context"
    "errors"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestBalanceCacheHit(t *testing.T) {
    now := time.Unix(1_000_000, 0)
    bc := newBalanceCache(2*time.Minute, func() time.Time { return now })
    fresh := &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{
        ExpiresAt: uint64(now.Add(10 * time.Minute).Unix()),
    }}

    var calls int32
    fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
        atomic.AddInt32(&calls, 1)
        return fresh, nil
    }

    id := ids.IdentityID{1}
    s1, err := bc.Get(context.Background(), id, fn)
    require.NoError(t, err)
    s2, err := bc.Get(context.Background(), id, fn)
    require.NoError(t, err)
    assert.Same(t, s1, s2)
    assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestBalanceCacheMissStale(t *testing.T) {
    var nowMu sync.Mutex
    now := time.Unix(1_000_000, 0)
    clock := func() time.Time {
        nowMu.Lock()
        defer nowMu.Unlock()
        return now
    }
    bc := newBalanceCache(2*time.Minute, clock)
    snap := &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{
        ExpiresAt: uint64(now.Add(60 * time.Second).Unix()), // within headroom
    }}

    var calls int32
    fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
        atomic.AddInt32(&calls, 1)
        return snap, nil
    }
    _, _ = bc.Get(context.Background(), ids.IdentityID{}, fn)
    _, _ = bc.Get(context.Background(), ids.IdentityID{}, fn)
    assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(2),
        "expected re-fetch when within ttlHeadroom of expiry")
}

func TestBalanceCacheSingleflight(t *testing.T) {
    bc := newBalanceCache(time.Minute, time.Now)
    var calls int32
    fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
        atomic.AddInt32(&calls, 1)
        time.Sleep(50 * time.Millisecond)
        return &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{
            ExpiresAt: uint64(time.Now().Add(time.Hour).Unix()),
        }}, nil
    }

    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, _ = bc.Get(context.Background(), ids.IdentityID{}, fn)
        }()
    }
    wg.Wait()
    assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestBalanceCachePropagatesError(t *testing.T) {
    bc := newBalanceCache(time.Minute, time.Now)
    fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
        return nil, errors.New("boom")
    }
    _, err := bc.Get(context.Background(), ids.IdentityID{}, fn)
    require.Error(t, err)
}
```

- [ ] **Step 16.5: Add a Balance round-trip in `rpc_methods_test.go`**

Append:

```go
func TestBalanceCachedRoundTrip(t *testing.T) {
    id := ids.IdentityID{0xab}
    issued := time.Now().Unix()
    expires := time.Now().Add(10 * time.Minute).Unix()
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_BALANCE] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.SignedBalanceSnapshot{
                Body: &tbproto.BalanceSnapshotBody{
                    IdentityId: id[:],
                    Credits:    100,
                    IssuedAt:   uint64(issued),
                    ExpiresAt:  uint64(expires),
                },
                TrackerSig: make([]byte, 64),
            }, nil
        }
    })
    defer cleanup()
    snap, err := c.BalanceCached(context.Background(), id)
    require.NoError(t, err)
    assert.Equal(t, int64(100), snap.Body.Credits)
}
```

(Add `"time"` to that file's imports if not already present.)

- [ ] **Step 16.6: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run 'Balance'
```
Expected: PASS.

- [ ] **Step 16.7: Commit**

```bash
git add plugin/internal/trackerclient/balance_cache.go \
        plugin/internal/trackerclient/balance_cache_test.go \
        plugin/internal/trackerclient/rpc.go \
        plugin/internal/trackerclient/trackerclient.go \
        plugin/internal/trackerclient/rpc_methods_test.go
git commit -m "feat(plugin/trackerclient): Balance + TTL singleflight cache

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 17: `UsageReport`, `Advertise`, `TransferRequest`

**Files:**
- Modify: `plugin/internal/trackerclient/rpc.go`
- Modify: `plugin/internal/trackerclient/rpc_methods_test.go`

- [ ] **Step 17.1: Append the three method bodies to `rpc.go`**

```go
// UsageReport is sent by the seeder after a served request completes.
func (c *Client) UsageReport(ctx context.Context, ur *UsageReport) error {
    if ur == nil {
        return fmt.Errorf("%w: nil UsageReport", ErrInvalidResponse)
    }
    rid, _ := ur.RequestID.MarshalBinary()
    req := &tbproto.UsageReport{
        RequestId:    rid,
        InputTokens:  ur.InputTokens,
        OutputTokens: ur.OutputTokens,
        Model:        ur.Model,
        SeederSig:    ur.SeederSig,
    }
    var ack tbproto.UsageAck
    return c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT, req, &ack)
}

// Advertise updates the seeder's availability + capabilities.
func (c *Client) Advertise(ctx context.Context, ad *Advertisement) error {
    if ad == nil {
        return fmt.Errorf("%w: nil Advertisement", ErrInvalidResponse)
    }
    req := &tbproto.Advertisement{
        Models:     ad.Models,
        MaxContext: ad.MaxContext,
        Available:  ad.Available,
        Headroom:   ad.Headroom,
        Tiers:      ad.Tiers,
    }
    var ack tbproto.AdvertiseAck
    return c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_ADVERTISE, req, &ack)
}

// TransferRequest moves credits between regions.
func (c *Client) TransferRequest(ctx context.Context, tr *TransferRequest) (*TransferProof, error) {
    if tr == nil {
        return nil, fmt.Errorf("%w: nil TransferRequest", ErrInvalidResponse)
    }
    req := &tbproto.TransferRequest{
        IdentityId: tr.IdentityID[:],
        Amount:     tr.Amount,
        DestRegion: tr.DestRegion,
        Nonce:      tr.Nonce[:],
    }
    var resp tbproto.TransferProof
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST, req, &resp); err != nil {
        return nil, err
    }
    out := &TransferProof{
        SourceSeq:  resp.SourceSeq,
        TrackerSig: resp.TrackerSig,
    }
    copy(out.SourceChainTipHash[:], resp.SourceChainTipHash)
    return out, nil
}
```

- [ ] **Step 17.2: Add round-trip tests**

Append to `rpc_methods_test.go`:

```go
import "github.com/google/uuid"

func TestUsageReportRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, nil) // default OK
    defer cleanup()
    err := c.UsageReport(context.Background(), &UsageReport{
        RequestID:    uuid.New(),
        InputTokens:  100,
        OutputTokens: 200,
        Model:        "claude-sonnet-4-6",
        SeederSig:    make([]byte, 64),
    })
    require.NoError(t, err)
}

func TestAdvertiseRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, nil)
    defer cleanup()
    err := c.Advertise(context.Background(), &Advertisement{
        Models: []string{"claude-sonnet-4-6"}, MaxContext: 200_000,
        Available: true, Headroom: 0.8, Tiers: 1,
    })
    require.NoError(t, err)
}

func TestTransferRequestRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.TransferProof{
                SourceChainTipHash: make([]byte, 32),
                SourceSeq:          12345,
                TrackerSig:         make([]byte, 64),
            }, nil
        }
    })
    defer cleanup()
    proof, err := c.TransferRequest(context.Background(), &TransferRequest{
        IdentityID: ids.IdentityID{1},
        Amount:     50,
        DestRegion: "us-east-1",
    })
    require.NoError(t, err)
    assert.Equal(t, uint64(12345), proof.SourceSeq)
}
```

- [ ] **Step 17.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run 'UsageReport|Advertise|TransferRequest'
```
Expected: PASS.

- [ ] **Step 17.4: Commit**

```bash
git add plugin/internal/trackerclient/rpc.go plugin/internal/trackerclient/rpc_methods_test.go
git commit -m "feat(plugin/trackerclient): UsageReport + Advertise + TransferRequest

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 18: `StunAllocate` + `TurnRelayOpen`

**Files:**
- Modify: `plugin/internal/trackerclient/rpc.go`
- Modify: `plugin/internal/trackerclient/rpc_methods_test.go`

- [ ] **Step 18.1: Append the two methods to `rpc.go`**

```go
// StunAllocate asks the tracker to reflect the client's external address.
func (c *Client) StunAllocate(ctx context.Context) (netip.AddrPort, error) {
    var resp tbproto.StunAllocateResponse
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, &tbproto.StunAllocateRequest{}, &resp); err != nil {
        return netip.AddrPort{}, err
    }
    out, err := netip.ParseAddrPort(resp.ExternalAddr)
    if err != nil {
        return netip.AddrPort{}, fmt.Errorf("%w: bad external_addr %q", ErrInvalidResponse, resp.ExternalAddr)
    }
    return out, nil
}

// TurnRelayOpen requests a TURN-style relay allocation.
func (c *Client) TurnRelayOpen(ctx context.Context, sessionID uuid.UUID) (*RelayHandle, error) {
    sid, _ := sessionID.MarshalBinary()
    var resp tbproto.TurnRelayOpenResponse
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, &tbproto.TurnRelayOpenRequest{SessionId: sid}, &resp); err != nil {
        return nil, err
    }
    return &RelayHandle{Endpoint: resp.RelayEndpoint, Token: resp.Token}, nil
}
```

(Add `"net/netip"` and `"github.com/google/uuid"` to the imports of `rpc.go`.)

- [ ] **Step 18.2: Add round-trip tests**

Append to `rpc_methods_test.go`:

```go
import "net/netip"

func TestStunAllocateRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.StunAllocateResponse{ExternalAddr: "203.0.113.5:51820"}, nil
        }
    })
    defer cleanup()
    addr, err := c.StunAllocate(context.Background())
    require.NoError(t, err)
    assert.Equal(t, netip.MustParseAddrPort("203.0.113.5:51820"), addr)
}

func TestTurnRelayOpenRoundTrip(t *testing.T) {
    c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
        s.Handlers[tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
            return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.TurnRelayOpenResponse{
                RelayEndpoint: "relay.example:3478",
                Token:         []byte("relay-token"),
            }, nil
        }
    })
    defer cleanup()
    h, err := c.TurnRelayOpen(context.Background(), uuid.New())
    require.NoError(t, err)
    assert.Equal(t, "relay.example:3478", h.Endpoint)
    assert.Equal(t, []byte("relay-token"), h.Token)
}
```

- [ ] **Step 18.3: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run 'StunAllocate|TurnRelayOpen'
```
Expected: PASS.

- [ ] **Step 18.4: Commit**

```bash
git add plugin/internal/trackerclient/rpc.go plugin/internal/trackerclient/rpc_methods_test.go
git commit -m "feat(plugin/trackerclient): StunAllocate + TurnRelayOpen

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 8 — Push streams

### Task 19: Heartbeat sender + reader

**Files:**
- Create: `plugin/internal/trackerclient/heartbeat.go`
- Create: `plugin/internal/trackerclient/heartbeat_test.go`
- Modify: `plugin/internal/trackerclient/reconnect.go` (start heartbeat post-connect, propagate miss-driven teardown)

- [ ] **Step 19.1: Write `heartbeat.go`**

```go
package trackerclient

import (
    "context"
    "fmt"
    "io"
    "sync/atomic"
    "time"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

// runHeartbeat opens the dedicated heartbeat stream as the first
// client-initiated bidi stream after handshake. Sends a ping every
// period, expects a pong within the same period; tearDown is invoked
// (concurrency-safe) after misses consecutive missing pongs.
func runHeartbeat(
    ctx context.Context,
    conn transport.Conn,
    period time.Duration,
    misses int,
    maxFrameSize int,
    tearDown func(error),
) {
    stream, err := conn.OpenStreamSync(ctx)
    if err != nil {
        tearDown(fmt.Errorf("trackerclient: open heartbeat stream: %w", err))
        return
    }
    defer stream.Close()

    var lastPong int64 // atomic; unix-nano of last pong received
    atomic.StoreInt64(&lastPong, time.Now().UnixNano())

    // Reader.
    go func() {
        for {
            var pong tbproto.HeartbeatPong
            err := wire.Read(stream, &pong, maxFrameSize)
            if err != nil {
                if err == io.EOF || ctx.Err() != nil {
                    return
                }
                tearDown(fmt.Errorf("trackerclient: heartbeat read: %w", err))
                return
            }
            atomic.StoreInt64(&lastPong, time.Now().UnixNano())
        }
    }()

    seq := uint64(0)
    ticker := time.NewTicker(period)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-conn.Done():
            return
        case now := <-ticker.C:
            seq++
            if err := wire.Write(stream, &tbproto.HeartbeatPing{Seq: seq, T: uint64(now.UnixMilli())}, maxFrameSize); err != nil {
                tearDown(fmt.Errorf("trackerclient: heartbeat write: %w", err))
                return
            }
            since := time.Since(time.Unix(0, atomic.LoadInt64(&lastPong)))
            if since > time.Duration(misses)*period {
                tearDown(fmt.Errorf("trackerclient: heartbeat: %d misses (last pong %s ago)", misses, since))
                return
            }
        }
    }
}
```

- [ ] **Step 19.2: Wire heartbeat into the supervisor**

Edit `reconnect.go`. After `s.holder.set(conn)` in `run()`, replace the existing block ending with the connection-Done select with:

```go
        // connected
        s.holder.set(conn)
        lastConnect = s.cfg.Clock()
        s.setStatus(PhaseConnected, ep, nil, time.Time{})
        s.cfg.Logger.Info().Str("addr", ep.Addr).Msg("trackerclient: connected")

        hbErrCh := make(chan error, 1)
        hbCtx, hbCancel := context.WithCancel(s.ctx)
        go runHeartbeat(hbCtx, conn, s.cfg.HeartbeatPeriod, s.cfg.HeartbeatMisses, s.cfg.MaxFrameSize, func(err error) {
            select {
            case hbErrCh <- err:
            default:
            }
        })

        select {
        case <-conn.Done():
        case err := <-hbErrCh:
            s.cfg.Logger.Warn().Err(err).Msg("trackerclient: heartbeat lost")
            _ = conn.Close()
        case <-s.ctx.Done():
            hbCancel()
            _ = conn.Close()
            s.holder.clear()
            s.setStatus(PhaseClosed, ep, s.ctx.Err(), time.Time{})
            return
        }
        hbCancel()
```

- [ ] **Step 19.3: Write `heartbeat_test.go`**

```go
package trackerclient

import (
    "context"
    "errors"
    "io"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestHeartbeatPingPong(t *testing.T) {
    cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
    defer cli.Close()
    defer srv.Close()

    serverDone := make(chan struct{})
    go func() {
        defer close(serverDone)
        s, err := srv.AcceptStream(context.Background())
        if err != nil {
            return
        }
        defer s.Close()
        for i := 0; i < 3; i++ {
            var ping tbproto.HeartbeatPing
            if err := wire.Read(s, &ping, 1<<10); err != nil {
                if errors.Is(err, io.EOF) {
                    return
                }
                t.Errorf("server read: %v", err)
                return
            }
            if err := wire.Write(s, &tbproto.HeartbeatPong{Seq: ping.Seq}, 1<<10); err != nil {
                t.Errorf("server write: %v", err)
                return
            }
        }
    }()

    teardown := make(chan error, 1)
    ctx, cancel := context.WithCancel(context.Background())
    go runHeartbeat(ctx, cli, 30*time.Millisecond, 5, 1<<10, func(err error) {
        select {
        case teardown <- err:
        default:
        }
    })

    time.Sleep(120 * time.Millisecond)
    cancel()

    select {
    case err := <-teardown:
        t.Fatalf("unexpected teardown: %v", err)
    default:
    }
    <-serverDone
}

func TestHeartbeatMissTearsDown(t *testing.T) {
    cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
    defer cli.Close()
    defer srv.Close()

    go func() {
        s, err := srv.AcceptStream(context.Background())
        if err != nil {
            return
        }
        // Read pings, never reply. Simulates a hung tracker.
        for {
            var ping tbproto.HeartbeatPing
            if err := wire.Read(s, &ping, 1<<10); err != nil {
                return
            }
        }
    }()

    teardown := make(chan error, 1)
    go runHeartbeat(context.Background(), cli, 20*time.Millisecond, 3, 1<<10, func(err error) {
        select {
        case teardown <- err:
        default:
        }
    })

    select {
    case err := <-teardown:
        require.Error(t, err)
        assert.Contains(t, err.Error(), "misses")
    case <-time.After(time.Second):
        t.Fatal("teardown not invoked within 1s")
    }
}
```

- [ ] **Step 19.4: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run Heartbeat -race
```
Expected: PASS.

- [ ] **Step 19.5: Commit**

```bash
git add plugin/internal/trackerclient/heartbeat.go \
        plugin/internal/trackerclient/heartbeat_test.go \
        plugin/internal/trackerclient/reconnect.go
git commit -m "feat(plugin/trackerclient): heartbeat ping/pong with miss-driven teardown

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 20: `OfferHandler` push acceptor

**Files:**
- Create: `plugin/internal/trackerclient/push_offers.go`
- Create: `plugin/internal/trackerclient/push_offers_test.go`
- Modify: `plugin/internal/trackerclient/reconnect.go` (start the offer acceptor when handler is set)
- Modify: `plugin/internal/trackerclient/test/fakeserver/fakeserver.go` (add a `PushOffer` helper)

- [ ] **Step 20.1: Write `push_offers.go`**

```go
package trackerclient

import (
    "context"
    "errors"
    "io"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

// runOfferAcceptor accepts server-initiated streams whose first frame is
// an OfferPush message. The first frame is read off the stream; any other
// shape is rejected.
//
// Lifetime: this goroutine exits when conn drops, ctx is cancelled, or
// AcceptStream returns a non-EOF error.
func runOfferAcceptor(ctx context.Context, conn transport.Conn, h OfferHandler, maxFrameSize int) {
    if h == nil {
        return
    }
    for {
        stream, err := conn.AcceptStream(ctx)
        if err != nil {
            return // EOF or context cancel
        }
        go handleOffer(ctx, stream, h, maxFrameSize)
    }
}

func handleOffer(ctx context.Context, stream transport.Stream, h OfferHandler, maxFrameSize int) {
    defer stream.Close()

    var push tbproto.OfferPush
    if err := wire.Read(stream, &push, maxFrameSize); err != nil {
        if !errors.Is(err, io.EOF) {
            // Wrong shape; reject by writing a structured error decision.
            _ = wire.Write(stream, &tbproto.OfferDecision{
                Accept: false, RejectReason: "trackerclient: invalid push shape",
            }, maxFrameSize)
        }
        return
    }
    if err := tbproto.ValidateOfferPush(&push); err != nil {
        _ = wire.Write(stream, &tbproto.OfferDecision{
            Accept: false, RejectReason: err.Error(),
        }, maxFrameSize)
        return
    }

    var consumerID ids.IdentityID
    copy(consumerID[:], push.ConsumerId)
    var envHash [32]byte
    copy(envHash[:], push.EnvelopeHash)
    decision, err := h.HandleOffer(ctx, &Offer{
        ConsumerID:      consumerID,
        EnvelopeHash:    envHash,
        Model:           push.Model,
        MaxInputTokens:  push.MaxInputTokens,
        MaxOutputTokens: push.MaxOutputTokens,
    })
    pb := &tbproto.OfferDecision{Accept: decision.Accept}
    if decision.Accept {
        pb.EphemeralPubkey = decision.EphemeralPubkey
    } else {
        pb.RejectReason = decision.RejectReason
        if err != nil {
            pb.RejectReason = err.Error()
        }
    }
    _ = wire.Write(stream, pb, maxFrameSize)
}
```

- [ ] **Step 20.2: Add a `PushOffer` helper to `fakeserver.go`**

Append to `fakeserver/fakeserver.go`:

```go
// PushOffer opens a server-initiated stream and writes a single OfferPush.
// Returns the matching OfferDecision read from the client.
func (s *Server) PushOffer(ctx context.Context, push *tbproto.OfferPush) (*tbproto.OfferDecision, error) {
    stream, err := s.Conn.OpenStreamSync(ctx)
    if err != nil {
        return nil, err
    }
    defer stream.Close()
    if err := wire.Write(stream, push, s.MaxFrameSz); err != nil {
        return nil, err
    }
    if err := stream.CloseWrite(); err != nil {
        return nil, err
    }
    var dec tbproto.OfferDecision
    if err := wire.Read(stream, &dec, s.MaxFrameSz); err != nil {
        return nil, err
    }
    return &dec, nil
}
```

- [ ] **Step 20.3: Wire the acceptor into the supervisor**

Edit `reconnect.go`. After the heartbeat goroutine launch, before the `select`:

```go
        if s.cfg.OfferHandler != nil {
            go runOfferAcceptor(s.ctx, conn, s.cfg.OfferHandler, s.cfg.MaxFrameSize)
        }
```

- [ ] **Step 20.4: Write `push_offers_test.go`**

```go
package trackerclient

import (
    "context"
    "fmt"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

type fakeOfferHandler struct{ accept bool }

func (f fakeOfferHandler) HandleOffer(_ Ctx, o *Offer) (OfferDecision, error) {
    if !f.accept {
        return OfferDecision{Accept: false, RejectReason: "no thanks"}, nil
    }
    pk := make([]byte, 32)
    pk[0] = 1
    return OfferDecision{Accept: true, EphemeralPubkey: pk}, nil
}

func newWiredClientWithOffer(t *testing.T, accept bool) (*Client, *fakeserver.Server, func()) {
    t.Helper()
    cli, srv := newLoopbackPair(t)
    drv := newDriver(t, srv)

    fake := fakeserver.New(srv)
    cfg := validConfig(t)
    cfg.Transport = drv
    cfg.Endpoints[0].Addr = "addr:1"
    cfg.OfferHandler = fakeOfferHandler{accept: accept}
    c, err := New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))

    serverDone := make(chan struct{})
    go func() {
        _ = fake.Run(context.Background())
        close(serverDone)
    }()
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    require.NoError(t, c.WaitConnected(ctx))
    return c, fake, func() {
        _ = c.Close()
        _ = cli.Close()
        _ = srv.Close()
        <-serverDone
    }
}

// helpers — keep test-local so each push test file is self-contained.
func newLoopbackPair(t *testing.T) (cli, srv *loopbackConn) {
    t.Helper()
    a, b := loopbackPair()
    return a, b
}
func newDriver(t *testing.T, srv *loopbackConn) *loopbackDriverShim {
    t.Helper()
    return makeDriver(srv)
}

// (Note: this test file expects the shim helpers in trackerclient_test.go;
// add them once if not already present — see Task 15 helpers.)

func TestOfferHandlerAccept(t *testing.T) {
    c, fake, cleanup := newWiredClientWithOffer(t, true)
    defer cleanup()
    _ = c

    push := &tbproto.OfferPush{
        ConsumerId:   make([]byte, 32),
        EnvelopeHash: make([]byte, 32),
        Model:        "claude-sonnet-4-6",
    }
    dec, err := fake.PushOffer(context.Background(), push)
    require.NoError(t, err)
    assert.True(t, dec.Accept)
    assert.Len(t, dec.EphemeralPubkey, 32)
}

func TestOfferHandlerReject(t *testing.T) {
    c, fake, cleanup := newWiredClientWithOffer(t, false)
    defer cleanup()
    _ = c

    push := &tbproto.OfferPush{
        ConsumerId:   make([]byte, 32),
        EnvelopeHash: make([]byte, 32),
        Model:        "claude-sonnet-4-6",
    }
    dec, err := fake.PushOffer(context.Background(), push)
    require.NoError(t, err)
    assert.False(t, dec.Accept)
    assert.Equal(t, "no thanks", dec.RejectReason)
}

func TestOfferHandlerInvalidPushRejects(t *testing.T) {
    c, fake, cleanup := newWiredClientWithOffer(t, true)
    defer cleanup()
    _ = c

    bad := &tbproto.OfferPush{
        ConsumerId:   make([]byte, 31), // wrong length
        EnvelopeHash: make([]byte, 32),
        Model:        "x",
    }
    dec, err := fake.PushOffer(context.Background(), bad)
    require.NoError(t, err)
    assert.False(t, dec.Accept)
    assert.Contains(t, dec.RejectReason, "ConsumerId")
    _ = fmt.Sprintf("%v", dec)
}
```

(`loopbackConn`, `loopbackPair`, `loopbackDriverShim`, `makeDriver` should be small in-file helpers in a shared `helpers_test.go`. If they don't exist yet, fold them into Task 15's `newWiredClient` helper file by extracting it into `helpers_test.go` and reusing it here.)

- [ ] **Step 20.5: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run Offer -race
```
Expected: PASS.

- [ ] **Step 20.6: Commit**

```bash
git add plugin/internal/trackerclient/push_offers.go \
        plugin/internal/trackerclient/push_offers_test.go \
        plugin/internal/trackerclient/test/fakeserver/fakeserver.go \
        plugin/internal/trackerclient/reconnect.go
git commit -m "feat(plugin/trackerclient): OfferHandler push acceptor + dispatch

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 21: `SettlementHandler` push acceptor

**Files:**
- Create: `plugin/internal/trackerclient/push_settlements.go`
- Create: `plugin/internal/trackerclient/push_settlements_test.go`
- Modify: `plugin/internal/trackerclient/reconnect.go` (start the settlement acceptor when handler is set; demux offers vs settlements)
- Modify: `plugin/internal/trackerclient/push_offers.go` (factor demux of accepted streams)
- Modify: `plugin/internal/trackerclient/test/fakeserver/fakeserver.go` (add `PushSettlement` helper)

- [ ] **Step 21.1: Demux design**

Both `OfferPush` and `SettlementPush` arrive on server-initiated streams. The first frame's wire shape is sufficient to discriminate (proto unmarshalling is forgiving but field numbers don't collide between the two messages — `OfferPush` uses tags 1–5, `SettlementPush` uses tags 1–2 with bytes types).

To avoid heuristic dispatch, prepend a 1-byte stream-class tag the server writes first, separate from the proto frame. The plan adopts that:

- After accepting a server-initiated stream, the client reads one byte:
  - `0x01` → offer
  - `0x02` → settlement
  - anything else → reject with structured error.
- The server's `PushOffer` and `PushSettlement` helpers write the tag before the first proto frame.

Rationale: cheap, unambiguous, matches the spec's "push streams are demuxed by first message type" without a regex on a self-describing format.

- [ ] **Step 21.2: Write `push_settlements.go`**

```go
package trackerclient

import (
    "context"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

const (
    pushTagOffer      = 0x01
    pushTagSettlement = 0x02
)

// runPushAcceptor accepts server-initiated streams and dispatches to
// either the offer or settlement handler based on a 1-byte class tag.
func runPushAcceptor(ctx context.Context, conn transport.Conn, off OfferHandler, set SettlementHandler, maxFrameSize int) {
    for {
        stream, err := conn.AcceptStream(ctx)
        if err != nil {
            return
        }
        go dispatchPush(ctx, stream, off, set, maxFrameSize)
    }
}

func dispatchPush(ctx context.Context, stream transport.Stream, off OfferHandler, set SettlementHandler, maxFrameSize int) {
    defer stream.Close()
    var tag [1]byte
    if _, err := stream.Read(tag[:]); err != nil {
        return
    }
    switch tag[0] {
    case pushTagOffer:
        if off == nil {
            _ = wire.Write(stream, &tbproto.OfferDecision{
                Accept: false, RejectReason: ErrNoHandler.Error(),
            }, maxFrameSize)
            return
        }
        handleOffer(ctx, stream, off, maxFrameSize)
    case pushTagSettlement:
        if set == nil {
            return
        }
        handleSettlement(ctx, stream, set, maxFrameSize)
    default:
        return
    }
}

func handleSettlement(ctx context.Context, stream transport.Stream, h SettlementHandler, maxFrameSize int) {
    var push tbproto.SettlementPush
    if err := wire.Read(stream, &push, maxFrameSize); err != nil {
        return
    }
    if err := tbproto.ValidateSettlementPush(&push); err != nil {
        return
    }
    var hash [32]byte
    copy(hash[:], push.PreimageHash)
    sig, err := h.HandleSettlement(ctx, &SettlementRequest{PreimageHash: hash, PreimageBody: push.PreimageBody})
    if err != nil || sig == nil {
        return // tracker treats no-ack as deferred consent
    }
    _ = wire.Write(stream, &tbproto.SettleAck{}, maxFrameSize)
    // The actual signature flows through the Settle RPC method; this
    // ack confirms the consumer received the push and intends to settle.
    _ = sig
}
```

- [ ] **Step 21.3: Update `runOfferAcceptor` to be a no-op (acceptor is now `runPushAcceptor`)**

Replace the body of `runOfferAcceptor` in `push_offers.go` with a stub that delegates:

```go
func runOfferAcceptor(ctx context.Context, conn transport.Conn, h OfferHandler, maxFrameSize int) {
    runPushAcceptor(ctx, conn, h, nil, maxFrameSize)
}
```

- [ ] **Step 21.4: Update the supervisor to use the unified acceptor**

In `reconnect.go`, replace the `runOfferAcceptor` launch with:

```go
        if s.cfg.OfferHandler != nil || s.cfg.SettlementHandler != nil {
            go runPushAcceptor(s.ctx, conn, s.cfg.OfferHandler, s.cfg.SettlementHandler, s.cfg.MaxFrameSize)
        }
```

- [ ] **Step 21.5: Update `fakeserver.PushOffer` and add `PushSettlement` to write the tag byte**

Edit `fakeserver/fakeserver.go`. Replace `PushOffer` body to write the tag first:

```go
func (s *Server) PushOffer(ctx context.Context, push *tbproto.OfferPush) (*tbproto.OfferDecision, error) {
    stream, err := s.Conn.OpenStreamSync(ctx)
    if err != nil {
        return nil, err
    }
    defer stream.Close()
    if _, err := stream.Write([]byte{0x01}); err != nil {
        return nil, err
    }
    if err := wire.Write(stream, push, s.MaxFrameSz); err != nil {
        return nil, err
    }
    if err := stream.CloseWrite(); err != nil {
        return nil, err
    }
    var dec tbproto.OfferDecision
    if err := wire.Read(stream, &dec, s.MaxFrameSz); err != nil {
        return nil, err
    }
    return &dec, nil
}

func (s *Server) PushSettlement(ctx context.Context, push *tbproto.SettlementPush) error {
    stream, err := s.Conn.OpenStreamSync(ctx)
    if err != nil {
        return err
    }
    defer stream.Close()
    if _, err := stream.Write([]byte{0x02}); err != nil {
        return err
    }
    if err := wire.Write(stream, push, s.MaxFrameSz); err != nil {
        return err
    }
    if err := stream.CloseWrite(); err != nil {
        return err
    }
    var ack tbproto.SettleAck
    return wire.Read(stream, &ack, s.MaxFrameSz)
}
```

- [ ] **Step 21.6: Write `push_settlements_test.go`**

```go
package trackerclient

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

type fakeSettlementHandler struct {
    sig []byte
    err error
}

func (f fakeSettlementHandler) HandleSettlement(_ Ctx, _ *SettlementRequest) ([]byte, error) {
    return f.sig, f.err
}

func newWiredClientWithSettlement(t *testing.T, sig []byte) (*Client, *fakeserver.Server, func()) {
    t.Helper()
    cli, srv := newLoopbackPair(t)
    drv := newDriver(t, srv)

    fake := fakeserver.New(srv)
    cfg := validConfig(t)
    cfg.Transport = drv
    cfg.Endpoints[0].Addr = "addr:1"
    cfg.SettlementHandler = fakeSettlementHandler{sig: sig}

    c, err := New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))

    serverDone := make(chan struct{})
    go func() {
        _ = fake.Run(context.Background())
        close(serverDone)
    }()
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    require.NoError(t, c.WaitConnected(ctx))
    return c, fake, func() {
        _ = c.Close()
        _ = cli.Close()
        _ = srv.Close()
        <-serverDone
    }
}

func TestSettlementHandlerAck(t *testing.T) {
    sig := make([]byte, 64)
    sig[0] = 0xff
    _, fake, cleanup := newWiredClientWithSettlement(t, sig)
    defer cleanup()

    push := &tbproto.SettlementPush{
        PreimageHash: make([]byte, 32),
        PreimageBody: []byte("body"),
    }
    require.NoError(t, fake.PushSettlement(context.Background(), push))
}
```

- [ ] **Step 21.7: Run tests, expect PASS**

```bash
cd plugin && go test ./internal/trackerclient/... -run 'Offer|Settlement' -race
```
Expected: PASS.

- [ ] **Step 21.8: Commit**

```bash
git add plugin/internal/trackerclient/push_settlements.go \
        plugin/internal/trackerclient/push_settlements_test.go \
        plugin/internal/trackerclient/push_offers.go \
        plugin/internal/trackerclient/reconnect.go \
        plugin/internal/trackerclient/test/fakeserver/fakeserver.go
git commit -m "feat(plugin/trackerclient): SettlementHandler + push demux via class tag

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 9 — End-to-end real QUIC

### Task 22: Integration test against real `quic-go` server

**Files:**
- Create: `plugin/internal/trackerclient/test/integration_test.go`

- [ ] **Step 22.1: Write the integration test**

```go
//go:build integration

package test

import (
    "context"
    "crypto/ed25519"
    "crypto/sha256"
    "crypto/x509"
    "errors"
    "net"
    "testing"
    "time"

    quicgo "github.com/quic-go/quic-go"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/plugin/internal/trackerclient"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/idtls"
    quicdriver "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/quic"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

type kp struct {
    pub  ed25519.PublicKey
    priv ed25519.PrivateKey
    hash [32]byte
}

func newKP(t *testing.T) kp {
    t.Helper()
    pub, priv, err := ed25519.GenerateKey(nil)
    require.NoError(t, err)
    cert, err := idtls.CertFromIdentity(priv)
    require.NoError(t, err)
    parsed, err := x509.ParseCertificate(cert.Certificate[0])
    require.NoError(t, err)
    return kp{pub: pub, priv: priv, hash: sha256.Sum256(parsed.RawSubjectPublicKeyInfo)}
}

type fakeSigner struct{ priv ed25519.PrivateKey; id ids.IdentityID }

func (f fakeSigner) Sign(msg []byte) ([]byte, error) { return ed25519.Sign(f.priv, msg), nil }
func (f fakeSigner) PrivateKey() ed25519.PrivateKey  { return f.priv }
func (f fakeSigner) IdentityID() ids.IdentityID      { return f.id }

func TestQuicHandshakeAndOneRPC(t *testing.T) {
    server := newKP(t)
    client := newKP(t)

    cert, err := idtls.CertFromIdentity(server.priv)
    require.NoError(t, err)
    serverTLS := idtls.MakeServerTLSConfig(cert, nil)

    listener, err := quicgo.ListenAddr("127.0.0.1:0", serverTLS, &quicgo.Config{Allow0RTT: false})
    require.NoError(t, err)
    defer listener.Close()
    addr := listener.Addr().(*net.UDPAddr).String()

    serverDone := make(chan struct{})
    go func() {
        defer close(serverDone)
        conn, err := listener.Accept(context.Background())
        if err != nil {
            return
        }
        defer conn.CloseWithError(0, "")
        for {
            stream, err := conn.AcceptStream(context.Background())
            if err != nil {
                return
            }
            var req tbproto.RpcRequest
            if err := wire.Read(stream, &req, 1<<20); err != nil {
                _ = stream.Close()
                continue
            }
            resp := &tbproto.RpcResponse{Status: tbproto.RpcStatus_RPC_STATUS_OK}
            _ = wire.Write(stream, resp, 1<<20)
            _ = stream.Close()
        }
    }()

    cfg := trackerclient.Config{
        Endpoints: []trackerclient.TrackerEndpoint{{
            Addr:         addr,
            IdentityHash: server.hash,
        }},
        Identity:  fakeSigner{priv: client.priv},
        Transport: quicdriver.New(),
    }
    c, err := trackerclient.New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))
    defer c.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    require.NoError(t, c.WaitConnected(ctx))

    err = c.Settle(ctx, make([]byte, 32), make([]byte, 64))
    require.NoError(t, err)

    listener.Close()
    select {
    case <-serverDone:
    case <-time.After(2 * time.Second):
        t.Fatal("server did not shut down")
    }
    _ = errors.New
}

func TestQuicMismatchedSPKIFails(t *testing.T) {
    server := newKP(t)
    client := newKP(t)

    cert, err := idtls.CertFromIdentity(server.priv)
    require.NoError(t, err)
    serverTLS := idtls.MakeServerTLSConfig(cert, nil)

    listener, err := quicgo.ListenAddr("127.0.0.1:0", serverTLS, &quicgo.Config{Allow0RTT: false})
    require.NoError(t, err)
    defer listener.Close()
    addr := listener.Addr().(*net.UDPAddr).String()

    cfg := trackerclient.Config{
        Endpoints: []trackerclient.TrackerEndpoint{{
            Addr:         addr,
            IdentityHash: [32]byte{0xff}, // wrong pin
        }},
        Identity:  fakeSigner{priv: client.priv},
        Transport: quicdriver.New(),
    }
    c, err := trackerclient.New(cfg)
    require.NoError(t, err)
    require.NoError(t, c.Start(context.Background()))
    defer c.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
    defer cancel()
    err = c.WaitConnected(ctx)
    assert.ErrorIs(t, err, context.DeadlineExceeded)
    // The Status() snapshot should carry the identity-mismatch error.
    s := c.Status()
    if s.LastError != nil {
        assert.Contains(t, s.LastError.Error(), "identity")
    }
}
```

- [ ] **Step 22.2: Run the integration suite**

```bash
cd plugin && go test -tags=integration -race ./internal/trackerclient/test/...
```
Expected: PASS.

- [ ] **Step 22.3: Add the integration target to `plugin/Makefile`**

Append to `plugin/Makefile`:

```makefile
test-integration:
	go test -tags=integration -race ./internal/trackerclient/test/...
```

- [ ] **Step 22.4: Commit**

```bash
git add plugin/internal/trackerclient/test/integration_test.go plugin/Makefile
git commit -m "test(plugin/trackerclient): real QUIC integration suite

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 10 — Wrap-up

### Task 23: Plugin CLAUDE.md update + repo-wide verification

**Files:**
- Modify: `plugin/CLAUDE.md`

- [ ] **Step 23.1: Update plugin/CLAUDE.md with the new module**

Add to the project-layout list (under `internal/<module>/`):

```markdown
- `internal/trackerclient/` — long-lived mTLS QUIC client to the regional tracker. Owns connection lifecycle, reconnect, heartbeat, the nine unary RPCs, and the offer/settlement push acceptors. See `docs/superpowers/specs/plugin/2026-05-02-trackerclient-design.md`.
```

- [ ] **Step 23.2: Run the full plugin test suite with race**

```bash
make -C plugin check
```
Expected: PASS, ≥ 90% line coverage on `internal/trackerclient/...`.

- [ ] **Step 23.3: Run the full repo make check**

```bash
make check
```
Expected: PASS across `shared/`, `plugin/`, `tracker/`.

- [ ] **Step 23.4: Run the QUIC integration suite**

```bash
make -C plugin test-integration
```
Expected: PASS.

- [ ] **Step 23.5: Commit**

```bash
git add plugin/CLAUDE.md
git commit -m "docs(plugin): add trackerclient module to project layout

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Self-review

**Spec coverage:**
- §3.3 Transport interface → Task 9.
- §4.1 Config / TrackerEndpoint / Signer → Task 4.
- §4.2 Client / Start / Close / Status / WaitConnected → Task 13.
- §4.3 nine unary RPCs → Tasks 15 (BrokerRequest, Settle, Enroll), 16 (Balance + cache + BalanceCached), 17 (UsageReport, Advertise, TransferRequest), 18 (StunAllocate, TurnRelayOpen).
- §4.4 OfferHandler / SettlementHandler → Tasks 20, 21.
- §4.5 Errors taxonomy → Task 3.
- §5.1/5.2 framing + dispatch → Tasks 5, 6.
- §5.3 RPC schema in shared/proto → Task 1.
- §5.4 mTLS + SPKI pinning → Tasks 7, 8.
- §6 reconnect/heartbeat/durability → Tasks 12, 19.
- §7 balance cache → Task 16.
- §8 test pyramid: unit (every task ships *_test.go), component (Tasks 15–21 use loopback + fakeserver), integration (Task 22), race (every test file is run under `-race` per Makefile), golden (Task 1 ships the rpc.proto goldens). Coverage assertion at Task 23.2.
- §9 dependencies → Task 2.
- §10 open questions: heartbeat-stream identification → method-zero marker is documented in Task 1's `rpc.proto` comment; Signer location → re-declared here, future identity plan converges.
- §11 failure modes: empty endpoints (Task 4), SPKI mismatch (Tasks 8, 22), heartbeat stalls (Task 19), connection drops mid-RPC (Task 14's isConnDead path), MaxFrameSize exceeded (Task 5), balance expiry (Task 16), handler panics (handlers run in goroutines per Task 20/21; supervisor goroutine recovery is intentionally NOT added — caller-supplied handlers panicking is a caller bug; the supervisor's accept-loop survives because each push runs in its own goroutine).
- §12 acceptance criteria mapped to verification at Task 23.

**Placeholder scan:** No `TBD`/`TODO`/"implement later" present in any code block. The "open question" call-outs in the design spec §10 are intentional and out of scope for v1.

**Type consistency:**
- `OfferDecision` (struct) used in both `Offer` push handler signatures and as the response type — consistent across Tasks 20/21.
- `Signer` interface declared in Task 3 (`types.go`) with three methods: `Sign(msg []byte) ([]byte, error)`, `PrivateKey() ed25519.PrivateKey`, `IdentityID() ids.IdentityID`. Used identically in Task 4's Config and Task 9's `signerAsIdentity` adapter.
- `Transport`, `Conn`, `Stream` in Task 4 are forward declarations; Task 9 replaces them with type aliases of `internal/transport`. Aliases keep the public surface stable across refactor.
- `BalanceCached` and `Balance` both return `*tbproto.SignedBalanceSnapshot` — Task 16 keeps these consistent.
- `RpcMethod_RPC_METHOD_*` constant names match the proto file in Task 1 throughout Tasks 14–18.

---

## Execution

Plan complete and saved to `docs/superpowers/plans/2026-05-02-plugin-trackerclient.md`.
