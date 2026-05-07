# Tracker Broker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `tracker/internal/broker` per the design spec — sibling subsystems `*Broker` (selection + offer roundtrip) and `*Settlement` (usage_report → ledger entry) sharing in-memory `Inflight` and `Reservations`. Integrate the admission gate at the api layer and finalize the `BrokerRequestResponse` oneof on the wire.

**Architecture:** One Go package (`tracker/internal/broker`) housing both subsystems. Selection and settlement share an in-memory state machine; goroutine ownership is split between the two (queue-drain in `*Broker`, reservation-TTL reaper in `*Settlement`). The api/-layer `installBrokerRequest` becomes the gate: validate envelope → admission.Decide → translate to wire response, calling `broker.Submit` only on `OutcomeAdmit`.

**Tech Stack:** Go 1.25, `google.golang.org/protobuf`, stdlib `crypto/ed25519`, `crypto/sha256`, `github.com/google/uuid`, `github.com/rs/zerolog`, `github.com/prometheus/client_golang`, `github.com/stretchr/testify`.

---

## Reference

- Spec: `docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md`
- Tracker root spec: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` (§5.1 selection, §5.2 settlement, §5.3 timeouts, §6 concurrency, §11.2 admission amendment)
- Admission spec: `docs/superpowers/specs/admission/2026-04-25-admission-design.md` (§5.1 Decide, §5.4 queue draining, §11.2 wire amendment)
- Wire-format v1: `docs/superpowers/specs/shared/2026-04-24-wire-format-v1-design.md`
- Sibling plan (api+server, merged): `docs/superpowers/plans/2026-05-06-tracker-internal-api-server.md`

## File map

```
shared/proto/
  rpc.proto                                                  -- modify (rename BrokerResponse→SeederAssignment; add Queued, Rejected, BrokerRequestResponse oneof, 3 enums)
  rpc.pb.go                                                  -- regenerated
  validate.go                                                -- modify (add ValidateBrokerRequestResponse + helpers)
  validate_test.go                                           -- modify (add cases)
  rpc_test.go                                                -- modify (round-trip the new messages)

tracker/internal/admission/
  queue.go                                                   -- modify (add PopReadyForBroker)
  admission.go                                               -- modify (add PressureGauge accessor)
  queue_test.go                                              -- modify (test PopReadyForBroker semantics)

tracker/internal/config/
  config.go                                                  -- modify (PricingConfig; new BrokerConfig.QueueDrainIntervalMs, InflightTerminalTTLS; new SettlementConfig.StaleTipRetries)
  apply_defaults.go                                          -- modify (defaults for new fields/section)
  validate.go                                                -- modify (validation rules for new section/fields)
  apply_defaults_test.go                                     -- modify
  validate_test.go                                           -- modify
  testdata/full.yaml                                         -- modify (add new section + knobs)

tracker/internal/broker/
  doc.go                                                     -- new (package overview + spec link)
  errors.go                                                  -- new (sentinel errors)
  errors_test.go                                             -- new
  types.go                                                   -- new (Result, Outcome, Assignment, NoCapacityDetails, QueuedDetails, RejectedDetails)
  types_test.go                                              -- new
  deps.go                                                    -- new (RegistryService, LedgerService, AdmissionService, ReputationService, PushService, Deps)
  deps_test.go                                               -- new (compile-time iface checks)
  pricing.go                                                 -- new (PriceTable, ModelPrices, MaxCost, ActualCost, DefaultPriceTable)
  pricing_test.go                                            -- new
  reservation.go                                             -- new (Reservations, Reserve, Release, Reserved, Sweep)
  reservation_test.go                                        -- new
  inflight.go                                                -- new (Request, State, Inflight, Insert, Get, Transition, MarkSeeder, IndexByHash, LookupByHash, Sweep)
  inflight_test.go                                           -- new
  selector.go                                                -- new (Score, Pick; filter helpers)
  selector_test.go                                           -- new
  offer_loop.go                                              -- new (runOne offer roundtrip)
  offer_loop_test.go                                         -- new (fake PushService)
  broker.go                                                  -- new (*Broker: Open, Submit, RegisterQueued, Close)
  broker_test.go                                             -- new
  queue_drain.go                                             -- new (drain goroutine; pendingQueued map)
  queue_drain_test.go                                        -- new
  settlement.go                                              -- new (*Settlement: Open, HandleUsageReport, HandleSettle, Close)
  settlement_test.go                                         -- new
  reaper.go                                                  -- new (reservation TTL goroutine)
  reaper_test.go                                             -- new
  subsystems.go                                              -- new (Subsystems composite Open)
  subsystems_test.go                                         -- new
  metrics.go                                                 -- new (Prometheus counters/histograms/gauges)
  metrics_test.go                                            -- new
  admin.go                                                   -- new (HTTP handler factory for /broker/*)
  admin_test.go                                              -- new
  race_test.go                                               -- new (concurrent Submit + Settle)

tracker/internal/api/
  router.go                                                  -- modify (Settlement field on Deps; SettlementService union)
  broker_request.go                                          -- modify (real handler: admission gate → broker.Submit → wire oneof)
  broker_request_test.go                                     -- modify (replace Scope-2 stub assertions)
  usage_report.go                                            -- modify (real handler: settlement.HandleUsageReport)
  usage_report_test.go                                       -- modify
  settle.go                                                  -- modify (real handler: settlement.HandleSettle)
  settle_test.go                                             -- modify
  iface_check_test.go                                        -- modify (assert *Broker, *Settlement satisfy unions)

tracker/internal/admin/                                       -- NOTE: directory exists per repo layout; route /broker/* via existing admin
  router.go                                                  -- modify (mount broker.AdminHandler under /broker/)

plugin/internal/trackerclient/
  types.go                                                   -- modify (BrokerOutcome, BrokerResult sum type)
  rpc.go                                                     -- modify (BrokerRequest returns *BrokerResult)
  rpc_methods_test.go                                        -- modify (assert sum-typed result)
  test/fakeserver/fakeserver.go                              -- modify (handler returns BrokerRequestResponse)

tracker/cmd/token-bay-tracker/
  run_cmd.go                                                 -- modify (build broker.Subsystems, thread into api.Deps; remove "Broker / Admission / Federation left nil" comment)
  run_cmd_test.go                                            -- modify (composition assertion)

tracker/test/integration/
  broker_e2e_test.go                                         -- new (Submit → offer accept → usage_report → settle → ledger; admission queue path)
```

---

## Phase 1 — Wire amendment (`shared/proto`)

The plugin trackerclient already lands a `BrokerRequest` against the *current* `BrokerResponse` shape. Phase 1 renames + extends, regens pb.go, updates validators, and updates the trackerclient + fakeserver in Phase 9 (deferred so the broker package builds against the renamed types first).

### Task 1: Rename `BrokerResponse` → `SeederAssignment` in `rpc.proto`

**Files:**
- Modify: `shared/proto/rpc.proto`
- Modify: `shared/proto/rpc.pb.go` (regenerated)
- Modify: `shared/proto/validate.go` (rename ValidateBrokerResponse if present; otherwise no-op)

- [ ] **Step 1: Edit `rpc.proto`** — rename the message:

```proto
message SeederAssignment {
  bytes  seeder_addr        = 1;  // utf-8 host:port
  bytes  seeder_pubkey      = 2;  // 32
  bytes  reservation_token  = 3;  // 16 bytes — equals request_id
}
```

- [ ] **Step 2: Regenerate `rpc.pb.go`**

Run: `make -C shared proto` (or `protoc --go_out=. shared/proto/rpc.proto` per repo's existing generator config).

Expected: `shared/proto/rpc.pb.go` updated; symbol `BrokerResponse` removed; `SeederAssignment` present.

- [ ] **Step 3: Update grep-callers**

Run: `grep -rn 'BrokerResponse' shared/ tracker/ plugin/`

Expected initial output: usages in `plugin/internal/trackerclient/{types.go, rpc.go, rpc_methods_test.go, test/fakeserver/fakeserver.go}`. Leave those — Phase 9 fixes them. The broker package isn't yet authored. `tracker/internal/api/broker_request.go` — the present `_ = br` line must compile because `brokerSubmitter` returns `*BrokerResponse`. Update that interface signature in Task 17 to use `*broker.Result`; do NOT touch `BrokerResponse` references in plugin yet.

- [ ] **Step 4: Compile shared/**

Run: `cd shared && go build ./...`

Expected: success.

- [ ] **Step 5: Commit**

```bash
git add shared/proto/rpc.proto shared/proto/rpc.pb.go
git commit -m "refactor(shared/proto)!: rename BrokerResponse to SeederAssignment"
```

The `!` flags the breaking shared/proto rename per repo CLAUDE.md rule §5.

### Task 2: Add `Queued`, `Rejected`, `BrokerRequestResponse` oneof, and three enums

**Files:**
- Modify: `shared/proto/rpc.proto`
- Modify: `shared/proto/rpc.pb.go` (regenerated)

- [ ] **Step 1: Append to `rpc.proto`** (after `SeederAssignment`):

```proto
message Queued {
  bytes        request_id    = 1;  // 16
  PositionBand position_band = 2;
  EtaBand      eta_band      = 3;
}

message Rejected {
  RejectReason reason        = 1;
  uint32       retry_after_s = 2;
}

message BrokerRequestResponse {
  oneof outcome {
    SeederAssignment seeder_assignment = 1;
    NoCapacity       no_capacity       = 2;
    Queued           queued            = 3;
    Rejected         rejected          = 4;
  }
}

enum PositionBand {
  POSITION_BAND_UNSPECIFIED = 0;
  POSITION_BAND_1_TO_10     = 1;
  POSITION_BAND_11_TO_50    = 2;
  POSITION_BAND_51_TO_200   = 3;
  POSITION_BAND_200_PLUS    = 4;
}

enum EtaBand {
  ETA_BAND_UNSPECIFIED = 0;
  ETA_BAND_LT_30S      = 1;
  ETA_BAND_30S_TO_2M   = 2;
  ETA_BAND_2M_TO_5M    = 3;
  ETA_BAND_5M_PLUS     = 4;
}

enum RejectReason {
  REJECT_REASON_UNSPECIFIED       = 0;
  REJECT_REASON_REGION_OVERLOADED = 1;
  REJECT_REASON_QUEUE_TIMEOUT     = 2;
}
```

- [ ] **Step 2: Regenerate `rpc.pb.go`**

Run: `make -C shared proto`. Expected: new types appear in pb.go.

- [ ] **Step 3: Compile shared/**

Run: `cd shared && go build ./...`. Expected: success.

- [ ] **Step 4: Commit**

```bash
git add shared/proto/rpc.proto shared/proto/rpc.pb.go
git commit -m "feat(shared/proto): add BrokerRequestResponse oneof per admission §11.2"
```

### Task 3: Validation helpers for new messages

**Files:**
- Modify: `shared/proto/validate.go`
- Modify: `shared/proto/validate_test.go`

- [ ] **Step 1: Write failing tests** in `validate_test.go`:

```go
func TestValidateQueued(t *testing.T) {
    require.Error(t, ValidateQueued(nil))
    require.Error(t, ValidateQueued(&Queued{RequestId: make([]byte, 15)}))
    require.Error(t, ValidateQueued(&Queued{RequestId: make([]byte, 16)})) // unspecified bands
    require.NoError(t, ValidateQueued(&Queued{
        RequestId:    make([]byte, 16),
        PositionBand: PositionBand_POSITION_BAND_1_TO_10,
        EtaBand:      EtaBand_ETA_BAND_LT_30S,
    }))
}

func TestValidateRejected(t *testing.T) {
    require.Error(t, ValidateRejected(nil))
    require.Error(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_UNSPECIFIED, RetryAfterS: 60}))
    require.Error(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_REGION_OVERLOADED, RetryAfterS: 59}))
    require.Error(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_REGION_OVERLOADED, RetryAfterS: 601}))
    require.NoError(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_QUEUE_TIMEOUT, RetryAfterS: 120}))
}

func TestValidateBrokerRequestResponse(t *testing.T) {
    require.Error(t, ValidateBrokerRequestResponse(nil))
    // Empty oneof:
    require.Error(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{}))
    // Each variant individually:
    require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
        Outcome: &BrokerRequestResponse_SeederAssignment{SeederAssignment: &SeederAssignment{
            SeederAddr:       []byte("127.0.0.1:0"),
            SeederPubkey:     make([]byte, 32),
            ReservationToken: make([]byte, 16),
        }},
    }))
    require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
        Outcome: &BrokerRequestResponse_NoCapacity{NoCapacity: &NoCapacity{Reason: "ok"}},
    }))
    require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
        Outcome: &BrokerRequestResponse_Queued{Queued: &Queued{
            RequestId: make([]byte, 16),
            PositionBand: PositionBand_POSITION_BAND_1_TO_10,
            EtaBand: EtaBand_ETA_BAND_LT_30S,
        }},
    }))
    require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
        Outcome: &BrokerRequestResponse_Rejected{Rejected: &Rejected{
            Reason: RejectReason_REJECT_REASON_REGION_OVERLOADED, RetryAfterS: 60,
        }},
    }))
}

func TestValidateSeederAssignment(t *testing.T) {
    require.Error(t, ValidateSeederAssignment(nil))
    require.Error(t, ValidateSeederAssignment(&SeederAssignment{}))
    require.Error(t, ValidateSeederAssignment(&SeederAssignment{
        SeederAddr: []byte(""), SeederPubkey: make([]byte, 32), ReservationToken: make([]byte, 16),
    }))
    require.Error(t, ValidateSeederAssignment(&SeederAssignment{
        SeederAddr: []byte("127.0.0.1:0"), SeederPubkey: make([]byte, 31), ReservationToken: make([]byte, 16),
    }))
    require.Error(t, ValidateSeederAssignment(&SeederAssignment{
        SeederAddr: []byte("127.0.0.1:0"), SeederPubkey: make([]byte, 32), ReservationToken: make([]byte, 15),
    }))
    require.NoError(t, ValidateSeederAssignment(&SeederAssignment{
        SeederAddr: []byte("127.0.0.1:0"), SeederPubkey: make([]byte, 32), ReservationToken: make([]byte, 16),
    }))
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `cd shared && go test ./proto/... -run 'TestValidate(Queued|Rejected|BrokerRequestResponse|SeederAssignment)' -v`
Expected: FAIL with "undefined: ValidateQueued" etc.

- [ ] **Step 3: Implement validators** in `validate.go`:

```go
// ValidateSeederAssignment — non-empty addr, pubkey 32, reservation_token 16.
func ValidateSeederAssignment(a *SeederAssignment) error {
    if a == nil {
        return errors.New("proto: SeederAssignment is nil")
    }
    if len(a.SeederAddr) == 0 {
        return errors.New("proto: SeederAssignment.SeederAddr empty")
    }
    if len(a.SeederPubkey) != 32 {
        return fmt.Errorf("proto: SeederAssignment.SeederPubkey len=%d, want 32", len(a.SeederPubkey))
    }
    if len(a.ReservationToken) != 16 {
        return fmt.Errorf("proto: SeederAssignment.ReservationToken len=%d, want 16", len(a.ReservationToken))
    }
    return nil
}

func ValidateQueued(q *Queued) error {
    if q == nil {
        return errors.New("proto: Queued is nil")
    }
    if len(q.RequestId) != 16 {
        return fmt.Errorf("proto: Queued.RequestId len=%d, want 16", len(q.RequestId))
    }
    if q.PositionBand == PositionBand_POSITION_BAND_UNSPECIFIED {
        return errors.New("proto: Queued.PositionBand UNSPECIFIED")
    }
    if q.EtaBand == EtaBand_ETA_BAND_UNSPECIFIED {
        return errors.New("proto: Queued.EtaBand UNSPECIFIED")
    }
    return nil
}

func ValidateRejected(r *Rejected) error {
    if r == nil {
        return errors.New("proto: Rejected is nil")
    }
    if r.Reason == RejectReason_REJECT_REASON_UNSPECIFIED {
        return errors.New("proto: Rejected.Reason UNSPECIFIED")
    }
    if r.RetryAfterS < 60 || r.RetryAfterS > 600 {
        return fmt.Errorf("proto: Rejected.RetryAfterS %d outside [60, 600]", r.RetryAfterS)
    }
    return nil
}

func ValidateBrokerRequestResponse(r *BrokerRequestResponse) error {
    if r == nil {
        return errors.New("proto: BrokerRequestResponse is nil")
    }
    switch o := r.Outcome.(type) {
    case *BrokerRequestResponse_SeederAssignment:
        return ValidateSeederAssignment(o.SeederAssignment)
    case *BrokerRequestResponse_NoCapacity:
        if o.NoCapacity == nil {
            return errors.New("proto: BrokerRequestResponse.NoCapacity is nil")
        }
        return nil
    case *BrokerRequestResponse_Queued:
        return ValidateQueued(o.Queued)
    case *BrokerRequestResponse_Rejected:
        return ValidateRejected(o.Rejected)
    case nil:
        return errors.New("proto: BrokerRequestResponse.Outcome unset")
    default:
        return fmt.Errorf("proto: BrokerRequestResponse unknown Outcome %T", o)
    }
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd shared && go test ./proto/... -v`
Expected: all PASS, race-clean.

- [ ] **Step 5: Round-trip + golden test**

Add to `rpc_test.go`:

```go
func TestBrokerRequestResponse_RoundTrip(t *testing.T) {
    cases := []*BrokerRequestResponse{
        {Outcome: &BrokerRequestResponse_SeederAssignment{SeederAssignment: &SeederAssignment{
            SeederAddr: []byte("127.0.0.1:1234"),
            SeederPubkey: bytesAll(32, 0xAA), ReservationToken: bytesAll(16, 0x55),
        }}},
        {Outcome: &BrokerRequestResponse_NoCapacity{NoCapacity: &NoCapacity{Reason: "no_eligible_seeder"}}},
        {Outcome: &BrokerRequestResponse_Queued{Queued: &Queued{
            RequestId: bytesAll(16, 0x01),
            PositionBand: PositionBand_POSITION_BAND_11_TO_50,
            EtaBand: EtaBand_ETA_BAND_30S_TO_2M,
        }}},
        {Outcome: &BrokerRequestResponse_Rejected{Rejected: &Rejected{
            Reason: RejectReason_REJECT_REASON_QUEUE_TIMEOUT, RetryAfterS: 300,
        }}},
    }
    for _, c := range cases {
        t.Run("", func(t *testing.T) {
            b, err := proto.Marshal(c)
            require.NoError(t, err)
            var got BrokerRequestResponse
            require.NoError(t, proto.Unmarshal(b, &got))
            require.True(t, proto.Equal(c, &got))
            require.NoError(t, ValidateBrokerRequestResponse(c))
        })
    }
}

func bytesAll(n int, v byte) []byte {
    b := make([]byte, n)
    for i := range b { b[i] = v }
    return b
}
```

Run: `cd shared && go test ./proto/... -v`. Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add shared/proto/validate.go shared/proto/validate_test.go shared/proto/rpc_test.go
git commit -m "test(shared/proto): validate + round-trip BrokerRequestResponse variants"
```

---

## Phase 2 — Config additions

### Task 4: Add `PricingConfig` section + new broker/settlement knobs

**Files:**
- Modify: `tracker/internal/config/config.go`
- Modify: `tracker/internal/config/apply_defaults.go`
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/testdata/full.yaml`
- Modify: `tracker/internal/config/apply_defaults_test.go`
- Modify: `tracker/internal/config/validate_test.go`

- [ ] **Step 1: Write failing tests**

In `validate_test.go`, add:

```go
func TestValidate_Pricing_RejectsEmpty(t *testing.T) {
    c := DefaultConfig()
    c.Pricing.Models = map[string]ModelPriceConfig{}
    err := Validate(c)
    require.ErrorContains(t, err, "pricing.models")
}

func TestValidate_Pricing_RejectsZeroRates(t *testing.T) {
    c := DefaultConfig()
    c.Pricing.Models["claude-opus-4-7"] = ModelPriceConfig{InCreditsPerToken: 0, OutCreditsPerToken: 75}
    err := Validate(c)
    require.ErrorContains(t, err, "in_credits_per_token")
}

func TestValidate_Broker_QueueDrainIntervalRange(t *testing.T) {
    c := DefaultConfig()
    c.Broker.QueueDrainIntervalMs = 50
    require.ErrorContains(t, Validate(c), "queue_drain_interval_ms")
    c.Broker.QueueDrainIntervalMs = 60001
    require.ErrorContains(t, Validate(c), "queue_drain_interval_ms")
}

func TestValidate_Settlement_StaleTipRetriesRange(t *testing.T) {
    c := DefaultConfig()
    c.Settlement.StaleTipRetries = -1
    require.ErrorContains(t, Validate(c), "stale_tip_retries")
    c.Settlement.StaleTipRetries = 11
    require.ErrorContains(t, Validate(c), "stale_tip_retries")
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/config/... -run 'TestValidate_(Pricing|Broker_QueueDrain|Settlement_StaleTip)'`
Expected: FAIL.

- [ ] **Step 3: Add types in `config.go`**

```go
type PricingConfig struct {
    Models map[string]ModelPriceConfig `yaml:"models"`
}

type ModelPriceConfig struct {
    InCreditsPerToken  uint64 `yaml:"in_credits_per_token"`
    OutCreditsPerToken uint64 `yaml:"out_credits_per_token"`
}
```

Add `Pricing PricingConfig` to top-level `Config` struct.

Add to `BrokerConfig`:

```go
QueueDrainIntervalMs   int `yaml:"queue_drain_interval_ms"`
InflightTerminalTTLS   int `yaml:"inflight_terminal_ttl_s"`
```

Add to `SettlementConfig`:

```go
StaleTipRetries int `yaml:"stale_tip_retries"`
```

Update `DefaultConfig`:

```go
Pricing: PricingConfig{
    Models: map[string]ModelPriceConfig{
        "claude-opus-4-7":            {InCreditsPerToken: 15, OutCreditsPerToken: 75},
        "claude-sonnet-4-6":          {InCreditsPerToken: 3,  OutCreditsPerToken: 15},
        "claude-haiku-4-5-20251001":  {InCreditsPerToken: 1,  OutCreditsPerToken: 5},
    },
},
Broker: BrokerConfig{
    // existing fields …
    QueueDrainIntervalMs: 1000,
    InflightTerminalTTLS: 600,
},
Settlement: SettlementConfig{
    // existing fields …
    StaleTipRetries: 3,
},
```

- [ ] **Step 4: Update `apply_defaults.go`**

```go
if c.Broker.QueueDrainIntervalMs == 0 {
    c.Broker.QueueDrainIntervalMs = d.Broker.QueueDrainIntervalMs
}
if c.Broker.InflightTerminalTTLS == 0 {
    c.Broker.InflightTerminalTTLS = d.Broker.InflightTerminalTTLS
}
if c.Settlement.StaleTipRetries == 0 {
    c.Settlement.StaleTipRetries = d.Settlement.StaleTipRetries
}
if len(c.Pricing.Models) == 0 {
    c.Pricing.Models = d.Pricing.Models
}
```

- [ ] **Step 5: Update `validate.go`**

```go
func (c *Config) checkPricing(errs *ValidationError) {
    if len(c.Pricing.Models) == 0 {
        errs.append(FieldError{Path: "pricing.models", Msg: "must contain at least one model"})
        return
    }
    for name, m := range c.Pricing.Models {
        if name == "" {
            errs.append(FieldError{Path: "pricing.models", Msg: "model name empty"})
        }
        if m.InCreditsPerToken == 0 {
            errs.append(FieldError{Path: "pricing.models." + name + ".in_credits_per_token", Msg: "must be > 0"})
        }
        if m.OutCreditsPerToken == 0 {
            errs.append(FieldError{Path: "pricing.models." + name + ".out_credits_per_token", Msg: "must be > 0"})
        }
    }
}

// In checkBroker (existing):
if c.Broker.QueueDrainIntervalMs < 100 || c.Broker.QueueDrainIntervalMs > 60000 {
    errs.append(FieldError{Path: "broker.queue_drain_interval_ms", Msg: "must be in [100, 60000]"})
}
if c.Broker.InflightTerminalTTLS < 60 {
    errs.append(FieldError{Path: "broker.inflight_terminal_ttl_s", Msg: "must be >= 60"})
}

// In checkSettlement:
if c.Settlement.StaleTipRetries < 0 || c.Settlement.StaleTipRetries > 10 {
    errs.append(FieldError{Path: "settlement.stale_tip_retries", Msg: "must be in [0, 10]"})
}
```

Wire `c.checkPricing(errs)` into the top-level `Validate` dispatcher.

- [ ] **Step 6: Update `testdata/full.yaml`**

Add explicit values for every new field:

```yaml
pricing:
  models:
    claude-opus-4-7:
      in_credits_per_token: 15
      out_credits_per_token: 75
    claude-sonnet-4-6:
      in_credits_per_token: 3
      out_credits_per_token: 15
    claude-haiku-4-5-20251001:
      in_credits_per_token: 1
      out_credits_per_token: 5

broker:
  # … existing …
  queue_drain_interval_ms: 1000
  inflight_terminal_ttl_s: 600

settlement:
  # … existing …
  stale_tip_retries: 3
```

- [ ] **Step 7: Run all config tests**

Run: `go test ./tracker/internal/config/... -v`
Expected: all PASS, including the round-trip on `testdata/full.yaml`.

- [ ] **Step 8: Commit**

```bash
git add tracker/internal/config/
git commit -m "feat(tracker/config): add pricing block + broker/settlement knobs for broker"
```

---

## Phase 3 — Admission accessors

### Task 5: Add `PopReadyForBroker` and `PressureGauge` to `*admission.Subsystem`

**Files:**
- Modify: `tracker/internal/admission/queue.go` (add Pop method)
- Modify: `tracker/internal/admission/admission.go` (add PressureGauge)
- Modify: `tracker/internal/admission/queue_test.go`

- [ ] **Step 1: Write failing tests** in `queue_test.go`:

```go
func TestPopReadyForBroker_EmptyQueue(t *testing.T) {
    s := openAdmissionForTest(t)
    defer s.Close()
    _, ok := s.PopReadyForBroker(time.Now(), 0.0)
    require.False(t, ok)
}

func TestPopReadyForBroker_BelowMinPriority(t *testing.T) {
    s := openAdmissionForTest(t)
    defer s.Close()
    s.queueMu.Lock()
    s.queue.Push(QueueEntry{
        RequestID:   [16]byte{1},
        ConsumerID:  ids.IdentityID{0xAA},
        CreditScore: 0.3,
        EnqueuedAt:  time.Now(),
    })
    s.queueMu.Unlock()
    _, ok := s.PopReadyForBroker(time.Now(), 0.5) // bar above 0.3
    require.False(t, ok)
}

func TestPopReadyForBroker_AbovePriority_Pops(t *testing.T) {
    s := openAdmissionForTest(t)
    defer s.Close()
    s.queueMu.Lock()
    s.queue.Push(QueueEntry{RequestID: [16]byte{1}, CreditScore: 0.8, EnqueuedAt: time.Now()})
    s.queueMu.Unlock()
    e, ok := s.PopReadyForBroker(time.Now(), 0.5)
    require.True(t, ok)
    require.Equal(t, [16]byte{1}, e.RequestID)
}

func TestPressureGauge_BootTime_ZeroComputedAt(t *testing.T) {
    s := openAdmissionForTest(t)
    defer s.Close()
    require.Zero(t, s.PressureGauge())
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/admission/... -run 'TestPopReadyForBroker|TestPressureGauge' -v`
Expected: FAIL.

- [ ] **Step 3: Implement** in `queue.go`:

```go
// PopReadyForBroker peeks the queue head. If empty or its EffectivePriority(now)
// is < minServePriority, returns (_, false). Otherwise pops and returns the entry.
func (s *Subsystem) PopReadyForBroker(now time.Time, minServePriority float64) (QueueEntry, bool) {
    s.queueMu.Lock()
    defer s.queueMu.Unlock()
    s.queue.AdvanceTime(now)
    if s.queue.Len() == 0 {
        return QueueEntry{}, false
    }
    head := s.queue.Peek()
    if head.EffectivePriority(now, s.cfg.AgingAlphaPerMinute) < minServePriority {
        return QueueEntry{}, false
    }
    return s.queue.Pop(), true
}
```

In `admission.go`:

```go
// PressureGauge returns the most recent supply-aggregator pressure (demand_rate /
// supply_estimate). Returns 0.0 when the aggregator hasn't published yet
// (boot-time), which the broker treats as "no pressure".
func (s *Subsystem) PressureGauge() float64 {
    snap := s.Supply()
    if snap.ComputedAt.IsZero() {
        return 0.0
    }
    return snap.Pressure
}
```

If `queueHeap.Peek()` doesn't exist yet, add it (returns the head without removing). If `queueHeap.Pop()` already removes, fine.

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/admission/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/
git commit -m "feat(tracker/admission): add PopReadyForBroker + PressureGauge for broker"
```

---

## Phase 4 — Broker package primitives

### Task 6: Package skeleton

**Files:**
- Create: `tracker/internal/broker/doc.go`
- Create: `tracker/internal/broker/errors.go`
- Create: `tracker/internal/broker/errors_test.go`

- [ ] **Step 1: Create `doc.go`**

```go
// Package broker is the tracker subsystem that, after admission has admitted
// a broker_request, picks a capable seeder via the offer round-trip and later
// finalizes the request as a USAGE ledger entry.
//
// Broker is two sibling subsystems sharing in-memory state:
//   - *Broker     — selection: validates pricing, reserves credits, scores
//                   candidates from the registry, runs the offer push loop.
//                   Exposes Submit(ctx, env) → *Result.
//   - *Settlement — finalization: receives usage_report from the seeder,
//                   pushes settle_request to the consumer, awaits the
//                   counter-sig (or times out → ConsumerSigMissing entry),
//                   appends via internal/ledger.
//
// The two share *Inflight + *Reservations. Construct via Open which returns
// a *Subsystems composite; cmd/run_cmd threads each into api.Deps.
//
// Concurrency: registry shards + per-mutex on Inflight/Reservations + CAS
// state transitions. Race-clean tests are mandatory (this package is on
// the always-`-race` list per tracker spec §6).
//
// Spec: docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md.
package broker
```

- [ ] **Step 2: Create `errors.go`**

```go
package broker

import "errors"

// Sentinel errors returned by exported broker methods.
var (
    ErrInsufficientCredits   = errors.New("broker: insufficient credits")
    ErrDuplicateReservation  = errors.New("broker: duplicate reservation")
    ErrUnknownReservation    = errors.New("broker: unknown reservation")
    ErrUnknownModel          = errors.New("broker: unknown model")
    ErrIllegalTransition     = errors.New("broker: illegal state transition")
    ErrUnknownRequest        = errors.New("broker: unknown request")
    ErrSeederMismatch        = errors.New("broker: usage_report seeder mismatch")
    ErrModelMismatch         = errors.New("broker: usage_report model mismatch")
    ErrCostOverspend         = errors.New("broker: usage_report cost overspend")
    ErrSeederSigInvalid      = errors.New("broker: usage_report seeder signature invalid")
    ErrDuplicateSettle       = errors.New("broker: duplicate settle for preimage")
    ErrUnknownPreimage       = errors.New("broker: unknown preimage hash")
)
```

- [ ] **Step 3: Trivial errors_test**

```go
package broker

import (
    "errors"
    "testing"
)

func TestErrorsAreSentinel(t *testing.T) {
    sentinels := []error{
        ErrInsufficientCredits, ErrDuplicateReservation, ErrUnknownReservation,
        ErrUnknownModel, ErrIllegalTransition, ErrUnknownRequest,
        ErrSeederMismatch, ErrModelMismatch, ErrCostOverspend,
        ErrSeederSigInvalid, ErrDuplicateSettle, ErrUnknownPreimage,
    }
    for _, e := range sentinels {
        if errors.Unwrap(e) != nil {
            t.Errorf("%v should be a leaf sentinel", e)
        }
        if e.Error() == "" {
            t.Errorf("%v has empty message", e)
        }
    }
}
```

- [ ] **Step 4: Compile + test**

Run: `go test ./tracker/internal/broker/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/
git commit -m "chore(tracker/broker): package skeleton + sentinel errors"
```

### Task 7: `PriceTable`

**Files:**
- Create: `tracker/internal/broker/pricing.go`
- Create: `tracker/internal/broker/pricing_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestDefaultPriceTable_KnownModels(t *testing.T) {
    p := DefaultPriceTable()
    body := &tbproto.EnvelopeBody{
        Model: "claude-sonnet-4-6", MaxInputTokens: 1000, MaxOutputTokens: 2000,
    }
    cost, err := p.MaxCost(body)
    require.NoError(t, err)
    require.Equal(t, uint64(1000*3+2000*15), cost)
}

func TestPriceTable_UnknownModel(t *testing.T) {
    p := DefaultPriceTable()
    _, err := p.MaxCost(&tbproto.EnvelopeBody{Model: "gpt-9"})
    require.ErrorIs(t, err, ErrUnknownModel)
    _, err = p.ActualCost("gpt-9", 1, 1)
    require.ErrorIs(t, err, ErrUnknownModel)
}

func TestPriceTable_FromConfig(t *testing.T) {
    p := NewPriceTableFromConfig(config.PricingConfig{
        Models: map[string]config.ModelPriceConfig{
            "x": {InCreditsPerToken: 2, OutCreditsPerToken: 3},
        },
    })
    cost, err := p.ActualCost("x", 4, 5)
    require.NoError(t, err)
    require.Equal(t, uint64(4*2+5*3), cost)
}

func TestPriceTable_NilEnvelope(t *testing.T) {
    p := DefaultPriceTable()
    _, err := p.MaxCost(nil)
    require.Error(t, err)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestPriceTable -v`
Expected: FAIL.

- [ ] **Step 3: Implement `pricing.go`**

```go
package broker

import (
    "errors"

    tbproto "github.com/token-bay/token-bay/shared/proto"
    "github.com/token-bay/token-bay/tracker/internal/config"
)

type ModelPrices struct {
    InCreditsPerToken  uint64
    OutCreditsPerToken uint64
}

type PriceTable struct {
    byModel map[string]ModelPrices
}

func DefaultPriceTable() *PriceTable {
    return &PriceTable{byModel: map[string]ModelPrices{
        "claude-opus-4-7":            {InCreditsPerToken: 15, OutCreditsPerToken: 75},
        "claude-sonnet-4-6":          {InCreditsPerToken: 3,  OutCreditsPerToken: 15},
        "claude-haiku-4-5-20251001":  {InCreditsPerToken: 1,  OutCreditsPerToken: 5},
    }}
}

func NewPriceTableFromConfig(c config.PricingConfig) *PriceTable {
    out := &PriceTable{byModel: make(map[string]ModelPrices, len(c.Models))}
    for name, m := range c.Models {
        out.byModel[name] = ModelPrices{
            InCreditsPerToken:  m.InCreditsPerToken,
            OutCreditsPerToken: m.OutCreditsPerToken,
        }
    }
    return out
}

func (p *PriceTable) MaxCost(env *tbproto.EnvelopeBody) (uint64, error) {
    if env == nil {
        return 0, errors.New("broker: MaxCost called with nil envelope")
    }
    m, ok := p.byModel[env.Model]
    if !ok {
        return 0, ErrUnknownModel
    }
    return m.InCreditsPerToken*env.MaxInputTokens + m.OutCreditsPerToken*env.MaxOutputTokens, nil
}

func (p *PriceTable) ActualCost(model string, in, out uint32) (uint64, error) {
    m, ok := p.byModel[model]
    if !ok {
        return 0, ErrUnknownModel
    }
    return m.InCreditsPerToken*uint64(in) + m.OutCreditsPerToken*uint64(out), nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./tracker/internal/broker/... -run TestPriceTable -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/pricing.go tracker/internal/broker/pricing_test.go
git commit -m "feat(tracker/broker): pricing table for max+actual cost computation"
```

### Task 8: `Reservations`

**Files:**
- Create: `tracker/internal/broker/reservation.go`
- Create: `tracker/internal/broker/reservation_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestReserveAndRelease_HappyPath(t *testing.T) {
    r := NewReservations()
    consumer := ids.IdentityID{1}
    reqID := [16]byte{0xAA}
    err := r.Reserve(reqID, consumer, 100, 200, time.Now().Add(time.Minute))
    require.NoError(t, err)
    require.Equal(t, uint64(100), r.Reserved(consumer))
    c, amt, ok := r.Release(reqID)
    require.True(t, ok)
    require.Equal(t, consumer, c)
    require.Equal(t, uint64(100), amt)
    require.Equal(t, uint64(0), r.Reserved(consumer))
}

func TestReserve_OverCommit(t *testing.T) {
    r := NewReservations()
    consumer := ids.IdentityID{1}
    require.NoError(t, r.Reserve([16]byte{1}, consumer, 100, 150, time.Now().Add(time.Minute)))
    err := r.Reserve([16]byte{2}, consumer, 100, 150, time.Now().Add(time.Minute))
    require.ErrorIs(t, err, ErrInsufficientCredits)
    require.Equal(t, uint64(100), r.Reserved(consumer))
}

func TestReserve_DuplicateReqID(t *testing.T) {
    r := NewReservations()
    require.NoError(t, r.Reserve([16]byte{1}, ids.IdentityID{1}, 10, 100, time.Now().Add(time.Minute)))
    err := r.Reserve([16]byte{1}, ids.IdentityID{1}, 5, 100, time.Now().Add(time.Minute))
    require.ErrorIs(t, err, ErrDuplicateReservation)
}

func TestRelease_Idempotent(t *testing.T) {
    r := NewReservations()
    require.NoError(t, r.Reserve([16]byte{1}, ids.IdentityID{1}, 10, 100, time.Now().Add(time.Minute)))
    _, _, ok := r.Release([16]byte{1})
    require.True(t, ok)
    _, _, ok = r.Release([16]byte{1})
    require.False(t, ok)
}

func TestSweep_TTL(t *testing.T) {
    r := NewReservations()
    now := time.Now()
    require.NoError(t, r.Reserve([16]byte{1}, ids.IdentityID{1}, 10, 100, now.Add(-time.Second)))
    require.NoError(t, r.Reserve([16]byte{2}, ids.IdentityID{2}, 5,  100, now.Add(time.Hour)))
    expired := r.Sweep(now)
    require.Len(t, expired, 1)
    require.Equal(t, [16]byte{1}, expired[0].ReqID)
}

func TestReservations_Concurrent_RaceClean(t *testing.T) {
    r := NewReservations()
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            id := [16]byte{byte(i)}
            _ = r.Reserve(id, ids.IdentityID{byte(i)}, 1, 10, time.Now().Add(time.Minute))
            _, _, _ = r.Release(id)
        }(i)
    }
    wg.Wait()
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestReserve -v`
Expected: FAIL.

- [ ] **Step 3: Implement `reservation.go`**

```go
package broker

import (
    "sync"
    "time"

    "github.com/token-bay/token-bay/shared/ids"
)

type Reservation struct {
    ReqID      [16]byte
    ConsumerID ids.IdentityID
    Amount     uint64
    ExpiresAt  time.Time
}

type Reservations struct {
    mu    sync.Mutex
    byID  map[ids.IdentityID]uint64
    byReq map[[16]byte]Reservation
}

func NewReservations() *Reservations {
    return &Reservations{
        byID:  make(map[ids.IdentityID]uint64),
        byReq: make(map[[16]byte]Reservation),
    }
}

func (r *Reservations) Reserve(reqID [16]byte, consumer ids.IdentityID, amount, snapshotCredits uint64, expiresAt time.Time) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    if _, exists := r.byReq[reqID]; exists {
        return ErrDuplicateReservation
    }
    if r.byID[consumer]+amount > snapshotCredits {
        return ErrInsufficientCredits
    }
    r.byID[consumer] += amount
    r.byReq[reqID] = Reservation{ReqID: reqID, ConsumerID: consumer, Amount: amount, ExpiresAt: expiresAt}
    return nil
}

func (r *Reservations) Release(reqID [16]byte) (ids.IdentityID, uint64, bool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    slot, ok := r.byReq[reqID]
    if !ok {
        return ids.IdentityID{}, 0, false
    }
    delete(r.byReq, reqID)
    r.byID[slot.ConsumerID] -= slot.Amount
    if r.byID[slot.ConsumerID] == 0 {
        delete(r.byID, slot.ConsumerID)
    }
    return slot.ConsumerID, slot.Amount, true
}

func (r *Reservations) Reserved(consumer ids.IdentityID) uint64 {
    r.mu.Lock()
    defer r.mu.Unlock()
    return r.byID[consumer]
}

func (r *Reservations) Sweep(now time.Time) []Reservation {
    r.mu.Lock()
    defer r.mu.Unlock()
    var expired []Reservation
    for id, slot := range r.byReq {
        if !slot.ExpiresAt.After(now) {
            expired = append(expired, slot)
            delete(r.byReq, id)
            r.byID[slot.ConsumerID] -= slot.Amount
            if r.byID[slot.ConsumerID] == 0 {
                delete(r.byID, slot.ConsumerID)
            }
        }
    }
    return expired
}
```

- [ ] **Step 4: Run, expect PASS (race-clean)**

Run: `go test -race ./tracker/internal/broker/... -run TestReserve -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/reservation.go tracker/internal/broker/reservation_test.go
git commit -m "feat(tracker/broker): in-memory credit reservation ledger"
```

### Task 9: `Inflight` state machine

**Files:**
- Create: `tracker/internal/broker/inflight.go`
- Create: `tracker/internal/broker/inflight_test.go`

- [ ] **Step 1: Write failing tests** for state CAS, MarkSeeder, IndexByHash, Sweep:

```go
func TestInflight_InsertGet(t *testing.T) {
    f := NewInflight()
    req := &Request{RequestID: [16]byte{1}, State: StateSelecting}
    f.Insert(req)
    got, ok := f.Get([16]byte{1})
    require.True(t, ok)
    require.Same(t, req, got)
}

func TestInflight_TransitionCAS(t *testing.T) {
    f := NewInflight()
    f.Insert(&Request{RequestID: [16]byte{1}, State: StateSelecting})
    require.NoError(t, f.Transition([16]byte{1}, StateSelecting, StateAssigned))
    require.ErrorIs(t, f.Transition([16]byte{1}, StateSelecting, StateAssigned), ErrIllegalTransition)
    got, _ := f.Get([16]byte{1})
    require.Equal(t, StateAssigned, got.State)
}

func TestInflight_MarkSeeder(t *testing.T) {
    f := NewInflight()
    f.Insert(&Request{RequestID: [16]byte{1}, State: StateSelecting})
    pub := ed25519.PublicKey(make([]byte, 32))
    require.NoError(t, f.MarkSeeder([16]byte{1}, ids.IdentityID{0xAA}, pub))
    got, _ := f.Get([16]byte{1})
    require.Equal(t, ids.IdentityID{0xAA}, got.AssignedSeeder)
    require.Equal(t, pub, got.SeederPubkey)
}

func TestInflight_IndexLookupByHash(t *testing.T) {
    f := NewInflight()
    req := &Request{RequestID: [16]byte{1}, State: StateServing}
    f.Insert(req)
    f.IndexByHash([16]byte{1}, [32]byte{0xAB})
    got, ok := f.LookupByHash([32]byte{0xAB})
    require.True(t, ok)
    require.Same(t, req, got)
}

func TestInflight_Sweep_RemovesTerminal(t *testing.T) {
    f := NewInflight()
    now := time.Now()
    completed := &Request{RequestID: [16]byte{1}, State: StateCompleted, TerminatedAt: now.Add(-11 * time.Minute)}
    fresh := &Request{RequestID: [16]byte{2}, State: StateCompleted, TerminatedAt: now}
    serving := &Request{RequestID: [16]byte{3}, State: StateServing, TerminatedAt: time.Time{}}
    f.Insert(completed); f.Insert(fresh); f.Insert(serving)
    swept := f.Sweep(now, 10*time.Minute)
    require.Len(t, swept, 1)
    require.Equal(t, [16]byte{1}, swept[0].RequestID)
    _, ok := f.Get([16]byte{2})
    require.True(t, ok)
    _, ok = f.Get([16]byte{3})
    require.True(t, ok)
}

func TestInflight_RaceClean_Transition(t *testing.T) {
    f := NewInflight()
    f.Insert(&Request{RequestID: [16]byte{1}, State: StateServing})
    var wins int32
    var wg sync.WaitGroup
    for i := 0; i < 50; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            if err := f.Transition([16]byte{1}, StateServing, StateCompleted); err == nil {
                atomic.AddInt32(&wins, 1)
            }
        }()
    }
    wg.Wait()
    require.Equal(t, int32(1), wins)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestInflight -v`
Expected: FAIL.

- [ ] **Step 3: Implement `inflight.go`**

```go
package broker

import (
    "crypto/ed25519"
    "sync"
    "time"

    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

type State uint8

const (
    StateUnspecified State = iota
    StateSelecting
    StateAssigned
    StateServing
    StateCompleted
    StateFailed
)

type Request struct {
    RequestID       [16]byte
    ConsumerID      ids.IdentityID
    EnvelopeBody    *tbproto.EnvelopeBody
    EnvelopeHash    [32]byte
    MaxCostReserved uint64
    AssignedSeeder  ids.IdentityID
    SeederPubkey    ed25519.PublicKey
    OfferAttempts   []ids.IdentityID
    StartedAt       time.Time
    State           State

    PreimageHash [32]byte
    SettleSig    chan []byte
    TerminatedAt time.Time
}

type Inflight struct {
    mu     sync.RWMutex
    byID   map[[16]byte]*Request
    byHash map[[32]byte]*Request
}

func NewInflight() *Inflight {
    return &Inflight{
        byID:   make(map[[16]byte]*Request),
        byHash: make(map[[32]byte]*Request),
    }
}

func (f *Inflight) Insert(r *Request) {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.byID[r.RequestID] = r
}

func (f *Inflight) Get(id [16]byte) (*Request, bool) {
    f.mu.RLock()
    defer f.mu.RUnlock()
    r, ok := f.byID[id]
    return r, ok
}

func (f *Inflight) Transition(id [16]byte, from, to State) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    r, ok := f.byID[id]
    if !ok {
        return ErrUnknownRequest
    }
    if r.State != from {
        return ErrIllegalTransition
    }
    r.State = to
    if to == StateCompleted || to == StateFailed {
        r.TerminatedAt = time.Now()
    }
    return nil
}

func (f *Inflight) MarkSeeder(id [16]byte, seeder ids.IdentityID, pub ed25519.PublicKey) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    r, ok := f.byID[id]
    if !ok {
        return ErrUnknownRequest
    }
    r.AssignedSeeder = seeder
    r.SeederPubkey = pub
    return nil
}

func (f *Inflight) IndexByHash(reqID [16]byte, hash [32]byte) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    r, ok := f.byID[reqID]
    if !ok {
        return ErrUnknownRequest
    }
    r.PreimageHash = hash
    f.byHash[hash] = r
    return nil
}

func (f *Inflight) LookupByHash(hash [32]byte) (*Request, bool) {
    f.mu.RLock()
    defer f.mu.RUnlock()
    r, ok := f.byHash[hash]
    return r, ok
}

func (f *Inflight) Sweep(now time.Time, terminalTTL time.Duration) []*Request {
    f.mu.Lock()
    defer f.mu.Unlock()
    var swept []*Request
    cutoff := now.Add(-terminalTTL)
    for id, r := range f.byID {
        if (r.State == StateCompleted || r.State == StateFailed) && !r.TerminatedAt.After(cutoff) {
            swept = append(swept, r)
            delete(f.byID, id)
            if r.PreimageHash != ([32]byte{}) {
                delete(f.byHash, r.PreimageHash)
            }
        }
    }
    return swept
}
```

- [ ] **Step 4: Run, expect PASS (race-clean)**

Run: `go test -race ./tracker/internal/broker/... -run TestInflight -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/inflight.go tracker/internal/broker/inflight_test.go
git commit -m "feat(tracker/broker): in-flight request state machine with CAS transitions"
```

### Task 10: `Result` types + `deps.go`

**Files:**
- Create: `tracker/internal/broker/types.go`
- Create: `tracker/internal/broker/types_test.go`
- Create: `tracker/internal/broker/deps.go`

- [ ] **Step 1: Write `types.go`**

```go
package broker

import "github.com/token-bay/token-bay/shared/ids"

type Outcome uint8

const (
    OutcomeUnspecified Outcome = iota
    OutcomeAdmit
    OutcomeQueued
    OutcomeRejected
    OutcomeNoCapacity
)

type Assignment struct {
    SeederAddr       string
    SeederPubkey     []byte
    ReservationToken []byte // 16 bytes
    RequestID        [16]byte
    AssignedSeeder   ids.IdentityID
}

type NoCapacityDetails struct {
    Reason string
}

type QueuedDetails struct {
    RequestID    [16]byte
    PositionBand uint8
    EtaBand      uint8
}

type RejectedDetails struct {
    Reason      uint8
    RetryAfterS uint32
}

type Result struct {
    Outcome  Outcome
    Admit    *Assignment
    NoCap    *NoCapacityDetails
    Queued   *QueuedDetails
    Rejected *RejectedDetails
}
```

The `PositionBand` / `EtaBand` / `RejectReason` enums lift from the `tbproto` package; we mirror them as `uint8` here so the broker doesn't import proto in types.go. Translation happens in `api/broker_request.go`.

- [ ] **Step 2: Write `deps.go`**

```go
package broker

import (
    "context"
    "crypto/ed25519"
    "time"

    "github.com/rs/zerolog"

    sharedadmission "github.com/token-bay/token-bay/shared/admission"
    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
    "github.com/token-bay/token-bay/tracker/internal/admission"
    "github.com/token-bay/token-bay/tracker/internal/ledger"
    "github.com/token-bay/token-bay/tracker/internal/registry"
)

type RegistryService interface {
    Match(registry.Filter) []registry.SeederRecord
    Get(ids.IdentityID) (registry.SeederRecord, bool)
    IncLoad(ids.IdentityID) (int, error)
    DecLoad(ids.IdentityID) (int, error)
}

type LedgerService interface {
    Tip(ctx context.Context) (uint64, []byte, bool, error)
    AppendUsage(ctx context.Context, r ledger.UsageRecord) (*tbproto.Entry, error)
}

type AdmissionService interface {
    PopReadyForBroker(now time.Time, minServePriority float64) (admission.QueueEntry, bool)
    PressureGauge() float64
    // Decide is invoked by api/, not broker, but kept on the union for symmetry
    // so api can supply *admission.Subsystem once and have all broker collaborators wired.
    Decide(consumerID ids.IdentityID, att *sharedadmission.SignedCreditAttestation, now time.Time) admission.Result
}

type ReputationService interface {
    Score(ids.IdentityID) (float64, bool)
    IsFrozen(ids.IdentityID) bool
    RecordOfferOutcome(seeder ids.IdentityID, outcome string)
}

type PushService interface {
    PushOfferTo(ids.IdentityID, *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool)
    PushSettlementTo(ids.IdentityID, *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool)
}

type Deps struct {
    Logger     zerolog.Logger
    Now        func() time.Time
    Registry   RegistryService
    Ledger     LedgerService
    Admission  AdmissionService
    Reputation ReputationService
    Pusher     PushService
    Pricing    *PriceTable
    TrackerKey ed25519.PrivateKey
}

// fallbackReputation is the v1 default when no reputation subsystem is wired.
type fallbackReputation struct{}

func (fallbackReputation) Score(ids.IdentityID) (float64, bool)        { return 0.5, false }
func (fallbackReputation) IsFrozen(ids.IdentityID) bool                { return false }
func (fallbackReputation) RecordOfferOutcome(ids.IdentityID, string)   {}
```

- [ ] **Step 3: Compile-time iface check** in `deps_test.go`:

```go
package broker_test

import (
    "github.com/token-bay/token-bay/tracker/internal/admission"
    "github.com/token-bay/token-bay/tracker/internal/broker"
    "github.com/token-bay/token-bay/tracker/internal/ledger"
    "github.com/token-bay/token-bay/tracker/internal/registry"
)

var (
    _ broker.RegistryService  = (*registry.Registry)(nil)
    _ broker.LedgerService    = (*ledger.Ledger)(nil)
    _ broker.AdmissionService = (*admission.Subsystem)(nil)
)
```

- [ ] **Step 4: Run, expect compile success**

Run: `go test ./tracker/internal/broker/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/types.go tracker/internal/broker/deps.go tracker/internal/broker/deps_test.go tracker/internal/broker/types_test.go
git commit -m "feat(tracker/broker): result types + dep interfaces"
```

### Task 11: `Selector`

**Files:**
- Create: `tracker/internal/broker/selector.go`
- Create: `tracker/internal/broker/selector_test.go`

- [ ] **Step 1: Write failing tests** covering: filter (model/tier/headroom/load), reputation freeze exclusion, score formula, tie-break by id, empty input, alreadyTried exclusion.

```go
func TestPick_FiltersByModel(t *testing.T) {
    snap := []registry.SeederRecord{
        {IdentityID: ids.IdentityID{1}, Available: true, HeadroomEstimate: 0.5,
         Capabilities: registry.Capabilities{
             Models: []string{"claude-opus-4-7"},
             Tiers:  []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD},
         }},
        {IdentityID: ids.IdentityID{2}, Available: true, HeadroomEstimate: 0.5,
         Capabilities: registry.Capabilities{
             Models: []string{"claude-sonnet-4-6"},
             Tiers:  []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD},
         }},
    }
    cands := Pick(snap, &tbproto.EnvelopeBody{
        Model: "claude-opus-4-7",
        Tier:  tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
    }, defaultWeights, fallbackReputation{}, defaultBrokerCfg, nil)
    require.Len(t, cands, 1)
    require.Equal(t, ids.IdentityID{1}, cands[0].Record.IdentityID)
}

func TestPick_ScoreOrder(t *testing.T) {
    snap := []registry.SeederRecord{
        {IdentityID: ids.IdentityID{1}, Available: true, HeadroomEstimate: 0.9,
         Capabilities: registry.Capabilities{Models: []string{"x"}, Tiers: []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}}},
        {IdentityID: ids.IdentityID{2}, Available: true, HeadroomEstimate: 0.1,
         Capabilities: registry.Capabilities{Models: []string{"x"}, Tiers: []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}}},
    }
    env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
    cands := Pick(snap, env, defaultWeights, fallbackReputation{}, defaultBrokerCfg, nil)
    require.Len(t, cands, 2)
    require.Equal(t, ids.IdentityID{1}, cands[0].Record.IdentityID) // higher headroom wins
}

func TestPick_ExcludesAlreadyTried(t *testing.T) {
    /* … */
}

func TestPick_FrozenSeederExcluded(t *testing.T) {
    /* … using a stub reputation with one frozen id */
}

func TestPick_LoadCeiling(t *testing.T) {
    /* … MaxLoad excludes seeders at/above */
}

func TestPick_TieBreakLex(t *testing.T) {
    /* equal scores → IdentityID byte order */
}
```

`defaultWeights` and `defaultBrokerCfg` are test helpers in `helpers_test.go` (create as needed).

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestPick -v`. Expected: FAIL.

- [ ] **Step 3: Implement `selector.go`**

```go
package broker

import (
    "bytes"
    "sort"

    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
    "github.com/token-bay/token-bay/tracker/internal/config"
    "github.com/token-bay/token-bay/tracker/internal/registry"
)

type Candidate struct {
    Record registry.SeederRecord
    Score  float64
}

func Score(rec registry.SeederRecord, w config.BrokerScoreWeights, repScore float64, rttMs float64) float64 {
    return w.Reputation*repScore +
        w.Headroom*rec.HeadroomEstimate -
        w.RTT*rttMs -
        w.Load*float64(rec.Load)
}

func Pick(snapshot []registry.SeederRecord, env *tbproto.EnvelopeBody,
    w config.BrokerScoreWeights, rep ReputationService, cfg config.BrokerConfig,
    alreadyTried []ids.IdentityID) []Candidate {

    triedSet := make(map[ids.IdentityID]struct{}, len(alreadyTried))
    for _, id := range alreadyTried {
        triedSet[id] = struct{}{}
    }

    out := make([]Candidate, 0, len(snapshot))
    for _, rec := range snapshot {
        if !rec.Available {
            continue
        }
        if _, ok := triedSet[rec.IdentityID]; ok {
            continue
        }
        if !containsString(rec.Capabilities.Models, env.Model) {
            continue
        }
        if !containsTier(rec.Capabilities.Tiers, env.Tier) {
            continue
        }
        if rec.HeadroomEstimate < cfg.HeadroomThreshold {
            continue
        }
        if cfg.LoadThreshold > 0 && rec.Load >= cfg.LoadThreshold {
            continue
        }
        if rep.IsFrozen(rec.IdentityID) {
            continue
        }
        repScore, _ := rep.Score(rec.IdentityID)
        out = append(out, Candidate{
            Record: rec,
            Score:  Score(rec, w, repScore, 0 /* v1 RTT placeholder */),
        })
    }
    sort.SliceStable(out, func(i, j int) bool {
        if out[i].Score != out[j].Score {
            return out[i].Score > out[j].Score
        }
        return bytes.Compare(out[i].Record.IdentityID.Bytes(), out[j].Record.IdentityID.Bytes()) < 0
    })
    return out
}

func containsString(xs []string, want string) bool {
    for _, x := range xs {
        if x == want { return true }
    }
    return false
}

func containsTier(xs []tbproto.PrivacyTier, want tbproto.PrivacyTier) bool {
    for _, x := range xs {
        if x == want { return true }
    }
    return false
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/broker/... -run TestPick -v`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/selector.go tracker/internal/broker/selector_test.go tracker/internal/broker/helpers_test.go
git commit -m "feat(tracker/broker): candidate selector with score+filter"
```

### Task 12: `offer_loop.go` — single-attempt offer roundtrip

**Files:**
- Create: `tracker/internal/broker/offer_loop.go`
- Create: `tracker/internal/broker/offer_loop_test.go`

- [ ] **Step 1: Write failing tests** with a fake `PushService`:

```go
type fakePusher struct {
    offerCh chan *tbproto.OfferDecision
    ok      bool
}
func (f *fakePusher) PushOfferTo(ids.IdentityID, *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
    return f.offerCh, f.ok
}
func (*fakePusher) PushSettlementTo(ids.IdentityID, *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
    return nil, false
}

func TestRunOffer_Accept(t *testing.T) {
    ch := make(chan *tbproto.OfferDecision, 1)
    p := &fakePusher{offerCh: ch, ok: true}
    ch <- &tbproto.OfferDecision{Accept: true, EphemeralPubkey: bytesAll(32, 0xCC)}
    accept, pub, err := runOffer(context.Background(), p, ids.IdentityID{1},
        &tbproto.EnvelopeBody{Model: "x", MaxInputTokens: 1, MaxOutputTokens: 1},
        [32]byte{0xAA}, 1500*time.Millisecond)
    require.NoError(t, err)
    require.True(t, accept)
    require.Len(t, pub, 32)
}

func TestRunOffer_Reject(t *testing.T) {
    ch := make(chan *tbproto.OfferDecision, 1)
    p := &fakePusher{offerCh: ch, ok: true}
    ch <- &tbproto.OfferDecision{Accept: false, RejectReason: "busy"}
    accept, _, err := runOffer(context.Background(), p, ids.IdentityID{1}, &tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, time.Second)
    require.NoError(t, err)
    require.False(t, accept)
}

func TestRunOffer_Unreachable(t *testing.T) {
    p := &fakePusher{offerCh: nil, ok: false}
    _, _, err := runOffer(context.Background(), p, ids.IdentityID{1}, &tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, time.Second)
    require.Error(t, err) // unreachable
}

func TestRunOffer_Timeout(t *testing.T) {
    ch := make(chan *tbproto.OfferDecision) // never sends
    p := &fakePusher{offerCh: ch, ok: true}
    accept, _, err := runOffer(context.Background(), p, ids.IdentityID{1}, &tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, 10*time.Millisecond)
    require.NoError(t, err) // timeout returns accept=false, not an error
    require.False(t, accept)
}

func TestRunOffer_CtxCancel(t *testing.T) {
    ch := make(chan *tbproto.OfferDecision)
    p := &fakePusher{offerCh: ch, ok: true}
    ctx, cancel := context.WithCancel(context.Background())
    go func() { time.Sleep(5*time.Millisecond); cancel() }()
    _, _, err := runOffer(ctx, p, ids.IdentityID{1}, &tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, time.Hour)
    require.ErrorIs(t, err, context.Canceled)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestRunOffer -v`. Expected: FAIL.

- [ ] **Step 3: Implement `offer_loop.go`**

```go
package broker

import (
    "context"
    "errors"
    "time"

    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

var errUnreachable = errors.New("broker: seeder unreachable")

func runOffer(ctx context.Context, pusher PushService, seederID ids.IdentityID,
    body *tbproto.EnvelopeBody, envHash [32]byte, timeout time.Duration,
) (accept bool, ephemeralPub []byte, err error) {

    push := &tbproto.OfferPush{
        ConsumerId:      body.ConsumerId,
        EnvelopeHash:    envHash[:],
        Model:           body.Model,
        MaxInputTokens:  uint32(body.MaxInputTokens),
        MaxOutputTokens: uint32(body.MaxOutputTokens),
    }
    ch, ok := pusher.PushOfferTo(seederID, push)
    if !ok {
        return false, nil, errUnreachable
    }
    timer := time.NewTimer(timeout)
    defer timer.Stop()
    select {
    case dec := <-ch:
        if dec == nil || !dec.Accept {
            return false, nil, nil
        }
        return true, dec.EphemeralPubkey, nil
    case <-timer.C:
        return false, nil, nil
    case <-ctx.Done():
        return false, nil, ctx.Err()
    }
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/broker/... -run TestRunOffer -v`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/offer_loop.go tracker/internal/broker/offer_loop_test.go
git commit -m "feat(tracker/broker): single-attempt offer roundtrip"
```

---

## Phase 5 — Broker subsystem

### Task 13: `*Broker` skeleton + `Open` / `Close`

**Files:**
- Create: `tracker/internal/broker/broker.go`
- Create: `tracker/internal/broker/broker_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestBroker_OpenClose_Idempotent(t *testing.T) {
    b, err := OpenBroker(testBrokerCfg(), testSettlementCfg(), testDeps(t), nil, nil)
    require.NoError(t, err)
    require.NoError(t, b.Close())
    require.NoError(t, b.Close())
}
```

`OpenBroker` signature initially exposed for direct testing; `Subsystems.Open` (Task 21) wraps it.

- [ ] **Step 2: Run, expect FAIL** (signature undefined).

- [ ] **Step 3: Implement** `broker.go`:

```go
package broker

import (
    "errors"
    "sync"
    "time"

    "github.com/token-bay/token-bay/tracker/internal/config"
)

type Broker struct {
    cfg     config.BrokerConfig
    scfg    config.SettlementConfig
    deps    Deps
    inflt   *Inflight
    resv    *Reservations

    pendingMu     sync.Mutex
    pendingQueued map[[16]byte]pendingEnv
    queueDrainCh  chan struct{}

    stop chan struct{}
    wg   sync.WaitGroup
}

type pendingEnv struct {
    body    *tbproto.EnvelopeBody
    deliver chan<- *Result
}

func OpenBroker(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps, inflt *Inflight, resv *Reservations) (*Broker, error) {
    if deps.Registry == nil { return nil, errors.New("broker: Registry required") }
    if deps.Ledger == nil   { return nil, errors.New("broker: Ledger required") }
    if deps.Admission == nil{ return nil, errors.New("broker: Admission required") }
    if deps.Pusher == nil   { return nil, errors.New("broker: Pusher required") }
    if deps.Pricing == nil  { return nil, errors.New("broker: Pricing required") }
    if deps.Now == nil      { deps.Now = time.Now }
    if deps.Reputation == nil { deps.Reputation = fallbackReputation{} }
    if inflt == nil { inflt = NewInflight() }
    if resv == nil  { resv = NewReservations() }

    b := &Broker{
        cfg: cfg, scfg: scfg, deps: deps,
        inflt: inflt, resv: resv,
        pendingQueued: make(map[[16]byte]pendingEnv),
        queueDrainCh:  make(chan struct{}, 1),
        stop:          make(chan struct{}),
    }
    b.startQueueDrain()
    return b, nil
}

func (b *Broker) Close() error {
    select {
    case <-b.stop:
        return nil
    default:
        close(b.stop)
    }
    b.wg.Wait()
    return nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/broker/... -run TestBroker_OpenClose -v`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/broker.go tracker/internal/broker/broker_test.go
git commit -m "feat(tracker/broker): subsystem skeleton with Open/Close"
```

### Task 14: `Submit` happy path + insufficient credits + no eligible seeder

**Files:**
- Modify: `tracker/internal/broker/broker.go`
- Modify: `tracker/internal/broker/broker_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestSubmit_AdmitFirstCandidate(t *testing.T) {
    fakeReg := newFakeRegistry()
    fakeReg.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
    pusher := newFakePusher()
    pusher.QueueOfferDecision(true, bytesAll(32, 0xCC))
    deps := testDepsWith(t, fakeReg, pusher)
    b, _ := OpenBroker(testBrokerCfg(), testSettlementCfg(), deps, nil, nil)
    defer b.Close()

    env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
    res, err := b.Submit(context.Background(), env)
    require.NoError(t, err)
    require.Equal(t, OutcomeAdmit, res.Outcome)
    require.NotNil(t, res.Admit)
    require.Equal(t, ids.IdentityID{1}, res.Admit.AssignedSeeder)
    // reservation held:
    require.Greater(t, deps.Pricing.MaxCostMust(t, env.Body), uint64(0))
}

func TestSubmit_InsufficientCredits(t *testing.T) {
    /* envelope balance < cost */
    res, err := b.Submit(ctx, env)
    require.NoError(t, err)
    require.Equal(t, OutcomeNoCapacity, res.Outcome)
    require.Equal(t, "insufficient_credits", res.NoCap.Reason)
}

func TestSubmit_NoEligibleSeeder(t *testing.T) {
    /* registry empty */
    res, _ := b.Submit(ctx, env)
    require.Equal(t, OutcomeNoCapacity, res.Outcome)
    require.Equal(t, "no_eligible_seeder", res.NoCap.Reason)
}

func TestSubmit_AllRejected(t *testing.T) {
    /* MaxOfferAttempts seeders, all reject */
    res, _ := b.Submit(ctx, env)
    require.Equal(t, OutcomeNoCapacity, res.Outcome)
    require.Equal(t, "all_seeders_rejected", res.NoCap.Reason)
}

func TestSubmit_ReservationReleasedOnNoCapacity(t *testing.T) {
    /* assert b.resv.Reserved(consumer) == 0 after a NoCapacity outcome */
}

func TestSubmit_UnknownModel(t *testing.T) {
    env := makeAdmittedEnvelope(t, "gpt-9", 1, 1, 1_000_000)
    _, err := b.Submit(ctx, env)
    require.ErrorIs(t, err, ErrUnknownModel)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestSubmit -v`. Expected: FAIL (Submit not implemented).

- [ ] **Step 3: Implement `Submit`** in `broker.go`:

```go
func (b *Broker) Submit(ctx context.Context, env *tbproto.EnvelopeSigned) (*Result, error) {
    if env == nil || env.Body == nil {
        return nil, errors.New("broker: Submit nil envelope")
    }
    body := env.Body
    consumer := ids.IdentityIDFromBytes(body.ConsumerId)

    cost, err := b.deps.Pricing.MaxCost(body)
    if err != nil {
        return nil, err
    }
    var requestID [16]byte
    if _, err := rand.Read(requestID[:]); err != nil {
        return nil, err
    }
    envHash := sha256.Sum256(envBodyBytes(body))
    now := b.deps.Now()
    expiresAt := now.Add(time.Duration(b.scfg.ReservationTTLS) * time.Second)

    creds := uint64(0)
    if body.BalanceProof != nil && body.BalanceProof.Body != nil {
        creds = body.BalanceProof.Body.Credits
    }
    if err := b.resv.Reserve(requestID, consumer, cost, creds, expiresAt); err != nil {
        if errors.Is(err, ErrInsufficientCredits) {
            return &Result{Outcome: OutcomeNoCapacity, NoCap: &NoCapacityDetails{Reason: "insufficient_credits"}}, nil
        }
        return nil, err
    }

    req := &Request{
        RequestID: requestID, ConsumerID: consumer,
        EnvelopeBody: body, EnvelopeHash: envHash,
        MaxCostReserved: cost, StartedAt: now, State: StateSelecting,
    }
    b.inflt.Insert(req)

    var tried []ids.IdentityID
    for attempt := 0; attempt < b.cfg.MaxOfferAttempts; attempt++ {
        snap := b.deps.Registry.Match(registry.Filter{
            RequireAvailable: true,
            Model:            body.Model,
            Tier:             body.Tier,
            MinHeadroom:      b.cfg.HeadroomThreshold,
            MaxLoad:          b.cfg.LoadThreshold,
        })
        cands := Pick(snap, body, b.cfg.ScoreWeights, b.deps.Reputation, b.cfg, tried)
        if len(cands) == 0 {
            break
        }
        seeder := cands[0].Record

        if _, err := b.deps.Registry.IncLoad(seeder.IdentityID); err != nil {
            tried = append(tried, seeder.IdentityID)
            continue
        }

        accepted, ephPub, oerr := runOffer(ctx, b.deps.Pusher, seeder.IdentityID, body, envHash,
            time.Duration(b.cfg.OfferTimeoutMs)*time.Millisecond)
        if errors.Is(oerr, context.Canceled) || errors.Is(oerr, context.DeadlineExceeded) {
            _, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
            b.failAndRelease(req)
            return nil, oerr
        }
        if oerr != nil {
            // unreachable / push setup error
            _, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
            b.deps.Reputation.RecordOfferOutcome(seeder.IdentityID, "unreachable")
            tried = append(tried, seeder.IdentityID)
            continue
        }
        if !accepted {
            _, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
            b.deps.Reputation.RecordOfferOutcome(seeder.IdentityID, "reject")
            tried = append(tried, seeder.IdentityID)
            continue
        }

        // Accepted — load stays incremented; settlement releases.
        _ = b.inflt.MarkSeeder(req.RequestID, seeder.IdentityID, ephPub)
        if err := b.inflt.Transition(req.RequestID, StateSelecting, StateAssigned); err != nil {
            _, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
            b.failAndRelease(req)
            return nil, err
        }
        return &Result{
            Outcome: OutcomeAdmit,
            Admit: &Assignment{
                SeederAddr:       seeder.NetCoords.ExternalAddr.String(),
                SeederPubkey:     ephPub,
                ReservationToken: append([]byte(nil), requestID[:]...),
                RequestID:        requestID,
                AssignedSeeder:   seeder.IdentityID,
            },
        }, nil
    }

    b.failAndRelease(req)
    reason := "no_eligible_seeder"
    if len(tried) > 0 {
        reason = "all_seeders_rejected"
    }
    return &Result{Outcome: OutcomeNoCapacity, NoCap: &NoCapacityDetails{Reason: reason}}, nil
}

func (b *Broker) failAndRelease(req *Request) {
    _, _, _ = b.resv.Release(req.RequestID)
    _ = b.inflt.Transition(req.RequestID, StateSelecting, StateFailed)
}

// envBodyBytes returns DeterministicMarshal(body). Helper kept inline so
// callers don't need to import shared/signing.
func envBodyBytes(b *tbproto.EnvelopeBody) []byte {
    out, _ := signing.DeterministicMarshal(b)
    return out
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/broker/... -run TestSubmit -v`. Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/
git commit -m "feat(tracker/broker): Submit with selection + offer loop"
```

### Task 15: `RegisterQueued` + queue-drain goroutine

**Files:**
- Create: `tracker/internal/broker/queue_drain.go`
- Create: `tracker/internal/broker/queue_drain_test.go`
- Modify: `tracker/internal/broker/broker.go` (add `RegisterQueued`)

- [ ] **Step 1: Write failing test**

```go
func TestRegisterQueued_DeliversOnDrain(t *testing.T) {
    fakeReg := newFakeRegistry()
    fakeReg.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "x"))
    pusher := newFakePusher()
    pusher.QueueOfferDecision(true, bytesAll(32, 0xCC))
    fakeAdm := &fakeAdmission{
        popReady: []admission.QueueEntry{{RequestID: [16]byte{0xEE}, CreditScore: 0.9, EnqueuedAt: time.Now()}},
    }
    deps := testDepsWith(t, fakeReg, pusher)
    deps.Admission = fakeAdm

    b, _ := OpenBroker(testBrokerCfg(), testSettlementCfg(), deps, nil, nil)
    defer b.Close()

    env := makeAdmittedEnvelope(t, "x", 1, 1, 1_000_000)
    deliver := make(chan *Result, 1)
    b.RegisterQueued(env, [16]byte{0xEE}, func(r *Result) { deliver <- r })

    // Tick the drain manually:
    b.TriggerQueueDrain()
    select {
    case r := <-deliver:
        require.Equal(t, OutcomeAdmit, r.Outcome)
    case <-time.After(time.Second):
        t.Fatal("queue drain did not deliver")
    }
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./tracker/internal/broker/... -run TestRegisterQueued -v`. Expected: FAIL.

- [ ] **Step 3: Implement `queue_drain.go`**

```go
package broker

import (
    "context"
    "time"

    tbproto "github.com/token-bay/token-bay/shared/proto"
)

func (b *Broker) RegisterQueued(env *tbproto.EnvelopeSigned, requestID [16]byte, deliver func(*Result)) {
    b.pendingMu.Lock()
    defer b.pendingMu.Unlock()
    ch := make(chan *Result, 1)
    go func() {
        r := <-ch
        if r != nil { deliver(r) }
    }()
    b.pendingQueued[requestID] = pendingEnv{body: env.Body, deliver: ch}
}

// TriggerQueueDrain forces one drain iteration. Used by tests; the goroutine
// also fires periodically per cfg.QueueDrainIntervalMs.
func (b *Broker) TriggerQueueDrain() {
    select {
    case b.queueDrainCh <- struct{}{}:
    default:
    }
}

func (b *Broker) startQueueDrain() {
    interval := time.Duration(b.cfg.QueueDrainIntervalMs) * time.Millisecond
    if interval <= 0 {
        interval = time.Second
    }
    b.wg.Add(1)
    go func() {
        defer b.wg.Done()
        t := time.NewTicker(interval)
        defer t.Stop()
        for {
            select {
            case <-b.stop:
                return
            case <-t.C:
                b.drainOnce(context.Background())
            case <-b.queueDrainCh:
                b.drainOnce(context.Background())
            }
        }
    }()
}

func (b *Broker) drainOnce(ctx context.Context) {
    pressure := b.deps.Admission.PressureGauge()
    minPriority := 0.0
    if pressure > 1.0 {
        minPriority = 0.5 * (pressure - 1.0)
    }
    now := b.deps.Now()
    for {
        entry, ok := b.deps.Admission.PopReadyForBroker(now, minPriority)
        if !ok {
            return
        }
        b.pendingMu.Lock()
        p, exists := b.pendingQueued[entry.RequestID]
        if exists {
            delete(b.pendingQueued, entry.RequestID)
        }
        b.pendingMu.Unlock()
        if !exists {
            continue
        }
        signed := &tbproto.EnvelopeSigned{Body: p.body}
        result, err := b.Submit(ctx, signed)
        if err != nil {
            result = &Result{Outcome: OutcomeNoCapacity, NoCap: &NoCapacityDetails{Reason: "broker_error"}}
        }
        select {
        case p.deliver <- result:
        default:
        }
        close(p.deliver)
    }
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/broker/... -run TestRegisterQueued -v`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/queue_drain.go tracker/internal/broker/queue_drain_test.go tracker/internal/broker/broker.go
git commit -m "feat(tracker/broker): queue drain goroutine + RegisterQueued"
```

---

## Phase 6 — Settlement subsystem

### Task 16: `*Settlement` skeleton + `Open` / `Close`

**Files:**
- Create: `tracker/internal/broker/settlement.go`
- Create: `tracker/internal/broker/settlement_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestSettlement_OpenClose(t *testing.T) {
    s, err := OpenSettlement(testSettlementCfg(), testDeps(t), nil, nil)
    require.NoError(t, err)
    require.NoError(t, s.Close())
    require.NoError(t, s.Close())
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement skeleton** in `settlement.go`:

```go
package broker

import (
    "context"
    "errors"
    "sync"
    "time"

    "github.com/token-bay/token-bay/tracker/internal/config"
)

type Settlement struct {
    cfg    config.SettlementConfig
    deps   Deps
    inflt  *Inflight
    resv   *Reservations
    stop   chan struct{}
    wg     sync.WaitGroup
}

func OpenSettlement(cfg config.SettlementConfig, deps Deps, inflt *Inflight, resv *Reservations) (*Settlement, error) {
    if deps.Ledger == nil  { return nil, errors.New("settlement: Ledger required") }
    if deps.Pusher == nil  { return nil, errors.New("settlement: Pusher required") }
    if deps.Now == nil     { deps.Now = time.Now }
    if inflt == nil        { inflt = NewInflight() }
    if resv == nil         { resv = NewReservations() }

    s := &Settlement{
        cfg: cfg, deps: deps, inflt: inflt, resv: resv,
        stop: make(chan struct{}),
    }
    s.startReaper()
    return s, nil
}

func (s *Settlement) Close() error {
    select {
    case <-s.stop: return nil
    default: close(s.stop)
    }
    s.wg.Wait()
    return nil
}
```

- [ ] **Step 4: Run, expect PASS**

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/settlement.go tracker/internal/broker/settlement_test.go
git commit -m "feat(tracker/broker): settlement subsystem skeleton"
```

### Task 17: `HandleUsageReport` happy path + ConsumerSigMissing path

**Files:**
- Modify: `tracker/internal/broker/settlement.go`
- Modify: `tracker/internal/broker/settlement_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestHandleUsageReport_HappyPath_AppendsEntry(t *testing.T) {
    fakeLed := newFakeLedger()
    pusher := newFakePusher()
    pusher.QueueSettleAck()
    deps := testDeps(t)
    deps.Ledger = fakeLed
    deps.Pusher = pusher

    inflt := NewInflight()
    resv := NewReservations()
    seederPriv, seederPub, _ := ed25519.GenerateKey(rand.Reader)
    consumerID := ids.IdentityID{0xCC}
    seederID := ids.IdentityID{0xDD}
    requestID := [16]byte{0xEE}

    inflt.Insert(&Request{
        RequestID: requestID, ConsumerID: consumerID,
        EnvelopeBody: &tbproto.EnvelopeBody{Model: "claude-sonnet-4-6"},
        AssignedSeeder: seederID, SeederPubkey: seederPub,
        State: StateAssigned, MaxCostReserved: 1_000_000,
    })
    _ = resv.Reserve(requestID, consumerID, 1_000, 1_000_000, time.Now().Add(time.Hour))

    s, _ := OpenSettlement(testSettlementCfg(), deps, inflt, resv)
    defer s.Close()

    // Build a usage report whose seeder sig matches what BuildUsageEntry produces.
    report := buildSignedUsageReport(t, seederPriv, requestID, "claude-sonnet-4-6", 100, 50)

    ack, err := s.HandleUsageReport(context.Background(), seederID, report)
    require.NoError(t, err)
    require.NotNil(t, ack)

    // Within settlement_timeout, dispatch consumer sig:
    consumerSig := signPreimage(t, consumerPriv, fakeLed.LastPreimage)
    _, _ = s.HandleSettle(context.Background(), consumerID, &tbproto.SettleRequest{
        PreimageHash: fakeLed.LastPreimageHash[:],
        ConsumerSig:  consumerSig,
    })

    require.Eventually(t, func() bool { return fakeLed.AppendCount() == 1 }, time.Second, 10*time.Millisecond)
    appended := fakeLed.LastUsageRecord()
    require.False(t, appended.ConsumerSigMissing)
    require.Equal(t, uint64(100*3+50*15), appended.CostCredits)
}

func TestHandleUsageReport_ConsumerSigMissingOnTimeout(t *testing.T) {
    /* same setup, but never call HandleSettle; advance clock past SettlementTimeoutS;
       assert AppendUsage called with ConsumerSigMissing=true */
}

func TestHandleUsageReport_SeederMismatch(t *testing.T) {
    /* peerID != req.AssignedSeeder → ErrSeederMismatch */
}

func TestHandleUsageReport_CostOverspend(t *testing.T) {
    /* report tokens producing cost > req.MaxCostReserved+5% → ErrCostOverspend */
}

func TestHandleUsageReport_UnknownRequest(t *testing.T) {
    /* request_id not in inflight → ErrUnknownRequest */
}

func TestHandleUsageReport_SeederSigInvalid(t *testing.T) {
    /* tampered seeder_sig → ErrSeederSigInvalid */
}
```

`buildSignedUsageReport` and `signPreimage` are test helpers that build a report whose `seeder_sig = ed25519.Sign(seederPriv, DeterministicMarshal(BuildUsageEntry(...)))`.

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `HandleUsageReport`** in `settlement.go`:

```go
func (s *Settlement) HandleUsageReport(ctx context.Context, peerID ids.IdentityID, r *tbproto.UsageReport) (*tbproto.UsageAck, error) {
    if r == nil || len(r.RequestId) != 16 {
        return nil, errors.New("settlement: malformed report")
    }
    var reqID [16]byte
    copy(reqID[:], r.RequestId)
    req, ok := s.inflt.Get(reqID)
    if !ok {
        return nil, ErrUnknownRequest
    }
    if req.State != StateAssigned && req.State != StateServing {
        return nil, ErrIllegalTransition
    }
    if req.AssignedSeeder != peerID {
        return nil, ErrSeederMismatch
    }
    if r.Model != req.EnvelopeBody.Model {
        return nil, ErrModelMismatch
    }
    actualCost, err := s.deps.Pricing.ActualCost(r.Model, r.InputTokens, r.OutputTokens)
    if err != nil {
        return nil, err
    }
    // 5% tolerance, integer-safe:
    if actualCost > req.MaxCostReserved+(req.MaxCostReserved/20) {
        return nil, ErrCostOverspend
    }

    // Move to SERVING (idempotent).
    _ = s.inflt.Transition(reqID, StateAssigned, StateServing)

    tipSeq, tipHash, _, err := s.deps.Ledger.Tip(ctx)
    if err != nil {
        return nil, err
    }
    body, err := entry.BuildUsageEntry(entry.UsageInput{
        PrevHash:     tipHash, Seq: tipSeq + 1,
        ConsumerID:   req.ConsumerID.Bytes(),
        SeederID:     peerID.Bytes(),
        Model:        r.Model,
        InputTokens:  r.InputTokens, OutputTokens: r.OutputTokens,
        CostCredits:  actualCost,
        Timestamp:    uint64(s.deps.Now().Unix()),
        RequestID:    reqID[:],
        ConsumerSigMissing: false,
    })
    if err != nil {
        return nil, err
    }
    bodyBytes, err := signing.DeterministicMarshal(body)
    if err != nil {
        return nil, err
    }
    if !ed25519.Verify(req.SeederPubkey, bodyBytes, r.SeederSig) {
        return nil, ErrSeederSigInvalid
    }
    var preimageHash [32]byte = sha256.Sum256(bodyBytes)
    req.SettleSig = make(chan []byte, 1)
    if err := s.inflt.IndexByHash(reqID, preimageHash); err != nil {
        return nil, err
    }

    push := &tbproto.SettlementPush{
        PreimageHash: preimageHash[:],
        PreimageBody: bodyBytes,
    }
    ackCh, _ := s.deps.Pusher.PushSettlementTo(req.ConsumerID, push)

    s.wg.Add(1)
    go s.awaitSettle(ctx, req, body, bodyBytes, r.SeederSig, ackCh)

    return &tbproto.UsageAck{}, nil
}

func (s *Settlement) awaitSettle(ctx context.Context, req *Request, body *tbproto.EntryBody, bodyBytes []byte, seederSig []byte, ackCh <-chan *tbproto.SettleAck) {
    defer s.wg.Done()
    timeout := time.Duration(s.cfg.SettlementTimeoutS) * time.Second
    timer := time.NewTimer(timeout)
    defer timer.Stop()
    var sig []byte
    consumerSigMissing := false
    select {
    case sig = <-req.SettleSig:
    case <-timer.C:
        consumerSigMissing = true
    case <-s.stop:
        return
    case <-ctx.Done():
        return
    }
    s.appendUsageEntry(ctx, req, body, sig, seederSig, consumerSigMissing)
    _ = ackCh // SettleAck is informational; the sig flows via HandleSettle
}

func (s *Settlement) appendUsageEntry(ctx context.Context, req *Request, body *tbproto.EntryBody, consumerSig, seederSig []byte, missing bool) {
    consumerPub := ed25519.PublicKey(req.ConsumerID.Bytes()) // identity_id == pubkey hash; concrete pubkey via registry/ledger lookup
    rec := ledger.UsageRecord{
        PrevHash: body.PrevHash, Seq: body.Seq,
        ConsumerID: req.ConsumerID.Bytes(),
        SeederID:   req.AssignedSeeder.Bytes(),
        Model:      body.Model,
        InputTokens: body.InputTokens, OutputTokens: body.OutputTokens,
        CostCredits: body.CostCredits,
        Timestamp: body.Timestamp, RequestID: body.RequestID,
        ConsumerSigMissing: missing,
        SeederSig: seederSig, SeederPub: req.SeederPubkey,
    }
    if !missing {
        rec.ConsumerSig = consumerSig
        rec.ConsumerPub = consumerPub
    }
    var err error
    for i := 0; i <= s.cfg.StaleTipRetries; i++ {
        _, err = s.deps.Ledger.AppendUsage(ctx, rec)
        if !errors.Is(err, ledger.ErrStaleTip) {
            break
        }
        // Rebuild at fresh tip:
        tipSeq, tipHash, _, terr := s.deps.Ledger.Tip(ctx)
        if terr != nil { err = terr; break }
        rec.PrevHash = tipHash
        rec.Seq = tipSeq + 1
    }
    if err != nil {
        _ = s.inflt.Transition(req.RequestID, StateServing, StateFailed)
        // Reservation NOT released — operator drains via admin endpoint.
        return
    }
    _, _, _ = s.resv.Release(req.RequestID)
    _, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
    _ = s.inflt.Transition(req.RequestID, StateServing, StateCompleted)
}
```

NOTE on consumer pubkey lookup: in v1 the broker treats the 32-byte `IdentityID` as the consumer pubkey *hash*, not the pubkey itself. The ledger's `AppendUsage` requires a real pubkey. The lookup should use `deps.Ledger` (or a new `deps.IdentityResolver` interface) to fetch the consumer's pubkey from the enrollment record. For Task 17 keep a TODO comment + emit the report path with a stub pubkey resolver — Task 17.5 hardens this.

- [ ] **Step 4: Run, expect PASS**

Run: `go test -race ./tracker/internal/broker/... -run TestHandleUsageReport -v`. Expected: PASS for the cases that don't depend on the resolver (sigs use the seeder side; consumer-sig path may be skipped via `ConsumerSigMissing=true`).

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/settlement.go tracker/internal/broker/settlement_test.go
git commit -m "feat(tracker/broker): HandleUsageReport with timer-bounded counter-sig wait"
```

### Task 17.5: Consumer pubkey resolver

**Files:**
- Modify: `tracker/internal/broker/deps.go` (add `IdentityResolver` interface)
- Modify: `tracker/internal/broker/settlement.go` (use it in `appendUsageEntry`)
- Modify: `tracker/internal/ledger/` — add `EnrolledPubkey(ctx, identityID) (ed25519.PublicKey, bool, error)` if not present, or fall back to `consumer_id == pubkey-hash` truncation if the ledger doesn't store pubkeys directly. (Cross-check the ledger spec; if pubkeys aren't ledgered, identityID must come from elsewhere — registry stores them on enroll.)

- [ ] **Step 1**: Verify in `tracker/internal/ledger` and `tracker/internal/registry` whether the consumer's full Ed25519 pubkey is reachable. Run `grep -n 'IdentityPubkey\|ConsumerPub' tracker/internal/{ledger,registry,api}/`.

- [ ] **Step 2**: Pick the source — likely `internal/api`'s enroll handler stores the pubkey in `internal/registry` or `internal/ledger`. If neither, add a new resolver indexed in admission's tlog (or simpler: stash on enroll into a new `internal/identity` registry).

- [ ] **Step 3**: Wire `IdentityResolver.ConsumerPubkey(id)` into `Settlement.appendUsageEntry`.

- [ ] **Step 4**: Run all settlement tests with consumer-sig path enabled.

- [ ] **Step 5**: Commit `feat(tracker): identity pubkey resolver for ledger settlement`.

### Task 18: `HandleSettle` — preimage_hash dispatcher

**Files:**
- Modify: `tracker/internal/broker/settlement.go`
- Modify: `tracker/internal/broker/settlement_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestHandleSettle_DispatchSig(t *testing.T) {
    /* set up an inflight with PreimageHash indexed; HandleSettle delivers sig into req.SettleSig */
}

func TestHandleSettle_UnknownPreimage(t *testing.T) {
    /* random preimage_hash → ErrUnknownPreimage */
}

func TestHandleSettle_Duplicate(t *testing.T) {
    /* two consecutive HandleSettle calls → second returns ErrDuplicateSettle */
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement**

```go
func (s *Settlement) HandleSettle(ctx context.Context, peerID ids.IdentityID, r *tbproto.SettleRequest) (*tbproto.SettleAck, error) {
    if r == nil || len(r.PreimageHash) != 32 {
        return nil, errors.New("settlement: malformed Settle")
    }
    var hash [32]byte
    copy(hash[:], r.PreimageHash)
    req, ok := s.inflt.LookupByHash(hash)
    if !ok {
        return nil, ErrUnknownPreimage
    }
    if req.ConsumerID != peerID {
        return nil, ErrSeederMismatch // wrong identity
    }
    select {
    case req.SettleSig <- r.ConsumerSig:
    default:
        return nil, ErrDuplicateSettle
    }
    return &tbproto.SettleAck{}, nil
}
```

- [ ] **Step 4: Run, expect PASS** (race-clean).

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/settlement.go tracker/internal/broker/settlement_test.go
git commit -m "feat(tracker/broker): HandleSettle dispatcher for consumer counter-sig"
```

### Task 19: Reservation TTL reaper goroutine

**Files:**
- Create: `tracker/internal/broker/reaper.go`
- Create: `tracker/internal/broker/reaper_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestReaper_ExpiresStaleReservation(t *testing.T) {
    /* manually insert an expired reservation + an inflight in StateAssigned;
       advance clock; assert reservation gone, inflight transitioned to Failed,
       seeder load decremented */
}
```

- [ ] **Step 2: Run, expect FAIL**

- [ ] **Step 3: Implement `reaper.go`**

```go
package broker

import "time"

func (s *Settlement) startReaper() {
    s.wg.Add(1)
    go func() {
        defer s.wg.Done()
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-s.stop: return
            case <-ticker.C:
                s.runReap(s.deps.Now())
            }
        }
    }()
}

func (s *Settlement) runReap(now time.Time) {
    expired := s.resv.Sweep(now)
    for _, slot := range expired {
        if req, ok := s.inflt.Get(slot.ReqID); ok {
            switch req.State {
            case StateSelecting:
                _ = s.inflt.Transition(slot.ReqID, StateSelecting, StateFailed)
            case StateAssigned:
                _, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
                _ = s.inflt.Transition(slot.ReqID, StateAssigned, StateFailed)
            }
        }
    }
}
```

For testability, expose `s.runReap(now)` so tests can advance the clock without touching the goroutine. Or use an injected `tickFn`.

- [ ] **Step 4: Run, expect PASS**

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/broker/reaper.go tracker/internal/broker/reaper_test.go
git commit -m "feat(tracker/broker): reservation TTL reaper goroutine"
```

---

## Phase 7 — Composite + admin + metrics

### Task 20: `Subsystems.Open`

**Files:**
- Create: `tracker/internal/broker/subsystems.go`
- Create: `tracker/internal/broker/subsystems_test.go`

- [ ] **Step 1: Implement** — single composite that ties shared state:

```go
package broker

import "github.com/token-bay/token-bay/tracker/internal/config"

type Subsystems struct {
    Broker     *Broker
    Settlement *Settlement
}

func Open(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps) (*Subsystems, error) {
    inflt := NewInflight()
    resv  := NewReservations()
    b, err := OpenBroker(cfg, scfg, deps, inflt, resv)
    if err != nil { return nil, err }
    s, err := OpenSettlement(scfg, deps, inflt, resv)
    if err != nil { _ = b.Close(); return nil, err }
    return &Subsystems{Broker: b, Settlement: s}, nil
}

func (s *Subsystems) Close() error {
    err1 := s.Broker.Close()
    err2 := s.Settlement.Close()
    if err1 != nil { return err1 }
    return err2
}
```

- [ ] **Step 2: Test** the composite — both subsystems share the same `Inflight`/`Reservations`; round-trip via Submit then HandleUsageReport in one process.

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/broker/subsystems.go tracker/internal/broker/subsystems_test.go
git commit -m "feat(tracker/broker): Subsystems composite tying Broker + Settlement"
```

### Task 21: Prometheus metrics

**Files:**
- Create: `tracker/internal/broker/metrics.go`
- Create: `tracker/internal/broker/metrics_test.go`

Mirror the metric set in spec §10.1. Provide a `newBrokerMetrics()` factory; thread through `Deps` or an opt. Counters/histograms wrapped behind the same `metrics` field pattern admission uses (`s.metrics.X.Inc()`). Tests assert metric labels and that hot-path counters increment exactly once per outcome.

Concrete metric names — the **exact** strings from spec §10.1 — must appear unchanged. Smoke-test via `prometheus.testutil.CollectAndCount`.

- [ ] Commit: `feat(tracker/broker): prometheus metrics for decisions/reservations/settlement`.

### Task 22: Admin HTTP endpoints

**Files:**
- Create: `tracker/internal/broker/admin.go`
- Create: `tracker/internal/broker/admin_test.go`
- Modify: `tracker/internal/admin/router.go` (mount `/broker/*`)

Provide `(s *Subsystems) AdminHandler() http.Handler` exposing the routes in spec §10:

| Method | Path |
|---|---|
| GET    | `/broker/inflight` |
| GET    | `/broker/inflight/{request_id}` |
| GET    | `/broker/reservations` |
| POST   | `/broker/reservations/release/{request_id}` |
| POST   | `/broker/inflight/fail/{request_id}` |

Tests: each endpoint returns the expected JSON; `POST` paths emit a structured audit log. Use `httptest.NewRecorder` for request shape, `zerolog`'s test sink for the audit assertion.

- [ ] Commit: `feat(tracker/broker): admin HTTP endpoints for inflight + reservations`.

---

## Phase 8 — `internal/api` integration

### Task 23: api.Deps gains `Settlement`; iface_check updated

**Files:**
- Modify: `tracker/internal/api/router.go`
- Modify: `tracker/internal/api/iface_check_test.go`

- [ ] Add to `Deps`:

```go
Settlement SettlementService
```

- [ ] Add union:

```go
type SettlementService interface {
    usageReportHandler
    settleHandler
}
```

- [ ] Update `iface_check_test.go`:

```go
var _ broker.AdmissionService = (*admission.Subsystem)(nil)
var _ api.BrokerService     = (*broker.Broker)(nil)
var _ api.SettlementService = (*broker.Settlement)(nil)
```

- [ ] Compile + commit.

### Task 24: `api/broker_request.go` — real handler

**Files:**
- Modify: `tracker/internal/api/broker_request.go`
- Modify: `tracker/internal/api/broker_request_test.go`

- [ ] Replace stub. Sequence per spec §3.2 step 1–6:

1. Decode `EnvelopeSigned`.
2. `signing.VerifyEnvelope(consumerPub, env)` — derive consumerPub from `body.ConsumerId` via the new identity resolver from Task 17.5.
3. `proto.ValidateEnvelopeBody(body)`.
4. `signing.VerifyBalanceSnapshot(trackerPub, body.BalanceProof)` + freshness `(body.BalanceProof.Body.IssuedAt+600 ≥ now ≥ body.BalanceProof.Body.IssuedAt)`.
5. `exhaustionproof.Verify(body.ExhaustionProof, body.ConsumerId, deps.Now())`.
6. `outcome := deps.Admission.Decide(consumer, nil, deps.Now())`.
7. Branch on outcome.

Translate to wire `BrokerRequestResponse`:
- `OutcomeAdmit` → `Submit(ctx, env)` → `BrokerResult` → translate.
- `OutcomeQueue` → `Queued{request_id, position_band, eta_band}`. `RegisterQueued(env, request_id, deliver)`. Block on `deliver`'s channel until result arrives or admission emits `QUEUE_TIMEOUT`. Cap by `cfg.Admission.QueueTimeoutS`.
- `OutcomeReject` → `Rejected{reason, retry_after_s}`.

Tests cover each outcome + each rejection code (BALANCE_PROOF_STALE, EXHAUSTION_PROOF_INVALID, UNKNOWN_MODEL).

- [ ] Commit: `feat(tracker/api): wire broker_request through admission + broker.Submit`.

### Task 25: `api/usage_report.go` — real handler

**Files:**
- Modify: `tracker/internal/api/usage_report.go`
- Modify: `tracker/internal/api/usage_report_test.go`

Decode `UsageReport`, call `deps.Settlement.HandleUsageReport(ctx, peerID, r)`, return `UsageAck` or map error to `RpcStatus`:
- `ErrUnknownRequest` → `NOT_FOUND`
- `ErrSeederMismatch`, `ErrModelMismatch`, `ErrCostOverspend`, `ErrSeederSigInvalid`, `ErrIllegalTransition` → `INVALID`
- ledger errors → `INTERNAL`

Tests: each error path returns the right status.

- [ ] Commit: `feat(tracker/api): wire usage_report into settlement`.

### Task 26: `api/settle.go` — real handler

**Files:**
- Modify: `tracker/internal/api/settle.go`
- Modify: `tracker/internal/api/settle_test.go`

Symmetric to Task 25, dispatching to `Settlement.HandleSettle`.

- [ ] Commit: `feat(tracker/api): wire settle into settlement dispatcher`.

---

## Phase 9 — Plugin trackerclient update

### Task 27: `(*Client).BrokerRequest` returns sum-typed result

**Files:**
- Modify: `plugin/internal/trackerclient/types.go`
- Modify: `plugin/internal/trackerclient/rpc.go`
- Modify: `plugin/internal/trackerclient/rpc_methods_test.go`
- Modify: `plugin/internal/trackerclient/test/fakeserver/fakeserver.go`

- [ ] **Step 1**: Define `BrokerOutcome`, `BrokerResult` per spec §3.4. Mark old `BrokerResponse` deleted.

- [ ] **Step 2**: Rewrite `BrokerRequest`:

```go
func (c *Client) BrokerRequest(ctx context.Context, env *tbproto.EnvelopeSigned) (*BrokerResult, error) {
    if env == nil {
        return nil, fmt.Errorf("%w: nil envelope", ErrInvalidResponse)
    }
    var resp tbproto.BrokerRequestResponse
    if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env, &resp); err != nil {
        return nil, err
    }
    if err := tbproto.ValidateBrokerRequestResponse(&resp); err != nil {
        return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
    }
    out := &BrokerResult{}
    switch o := resp.Outcome.(type) {
    case *tbproto.BrokerRequestResponse_SeederAssignment:
        out.Outcome = BrokerOutcomeAssignment
        out.Assignment = &SeederAssignment{
            SeederAddr:       string(o.SeederAssignment.SeederAddr),
            SeederPubkey:     o.SeederAssignment.SeederPubkey,
            ReservationToken: o.SeederAssignment.ReservationToken,
        }
    case *tbproto.BrokerRequestResponse_NoCapacity:
        out.Outcome = BrokerOutcomeNoCapacity
        out.NoCap = &NoCapacityResult{Reason: o.NoCapacity.Reason}
    case *tbproto.BrokerRequestResponse_Queued:
        out.Outcome = BrokerOutcomeQueued
        out.Queued = &QueuedResult{
            RequestID:    [16]byte(o.Queued.RequestId),
            PositionBand: uint8(o.Queued.PositionBand),
            EtaBand:      uint8(o.Queued.EtaBand),
        }
    case *tbproto.BrokerRequestResponse_Rejected:
        out.Outcome = BrokerOutcomeRejected
        out.Rejected = &RejectedResult{
            Reason:      uint8(o.Rejected.Reason),
            RetryAfterS: o.Rejected.RetryAfterS,
        }
    default:
        return nil, fmt.Errorf("%w: unknown outcome", ErrInvalidResponse)
    }
    return out, nil
}
```

- [ ] **Step 3**: Update `fakeserver.go` so `RPC_METHOD_BROKER_REQUEST` returns `*tbproto.BrokerRequestResponse` (not `*tbproto.BrokerResponse`). Update tests in `rpc_methods_test.go` to assert each variant.

- [ ] **Step 4**: Run `cd plugin && make test` — expect PASS.

- [ ] **Step 5**: Commit `refactor(plugin/trackerclient)!: BrokerRequest returns sum-typed BrokerResult`.

---

## Phase 10 — `cmd/run_cmd` wiring + integration tests

### Task 28: Wire `broker.Subsystems` in `cmd/run_cmd.go`

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go`
- Modify: `tracker/cmd/token-bay-tracker/run_cmd_test.go`

- [ ] **Step 1**: After `admission.Open`, `reputation.Open`, etc., construct broker:

```go
adm, err := admission.Open(cfg.Admission, reg, trackerKey, /* opts */)
if err != nil { return fmt.Errorf("admission: %w", err) }
defer adm.Close()

prices := broker.NewPriceTableFromConfig(cfg.Pricing)

brokerSubs, err := broker.Open(cfg.Broker, cfg.Settlement, broker.Deps{
    Logger: logger, Now: time.Now,
    Registry:  reg,
    Ledger:    led,
    Admission: adm,
    Reputation: reputationAdapter{}, // or actual reputation subsystem
    Pusher:    serverPushAdapter{srv: srv}, // satisfies broker.PushService via *server.Server's PushOfferTo + PushSettlementTo
    Pricing:   prices,
    TrackerKey: trackerKey,
})
if err != nil { return fmt.Errorf("broker: %w", err) }
defer brokerSubs.Close()
```

The `Pusher` injection is circular (server needs `api.Router` which needs `broker.Subsystems` which needs `server.PushService`). Resolve by:

1. Construct `srv` first with `api.Router == nil` placeholder OR with a deferred `Pusher` wrapper. The repo convention is: the `*Server` is constructed first; `*Broker` and `*Settlement` are wired into a *new* `api.Router` that is then attached via `srv.SetAPI(router)`. If `*Server` doesn't have `SetAPI`, add it.

OR simpler: construct broker with a `pusherProxy` whose `inner` field is set later, and set it before `srv.Run`. Pattern documented in similar wirings (federation/api).

Pick the option that requires least surgery on `internal/server`; add the smallest change that satisfies it.

- [ ] **Step 2**: Update `api.NewRouter` call to include `Broker: brokerSubs.Broker`, `Settlement: brokerSubs.Settlement`.

- [ ] **Step 3**: Remove the `// Broker / Admission / Federation left nil` comment.

- [ ] **Step 4**: Update `run_cmd_test.go` — assert composition, no nil panics, broker actually wired.

- [ ] **Step 5**: Run `make -C tracker test`. Expect PASS.

- [ ] **Step 6**: Commit `feat(tracker/cmd): wire broker subsystems into run_cmd`.

### Task 29: End-to-end integration test

**File:**
- Create: `tracker/test/integration/broker_e2e_test.go`

- [ ] Drive the full path with a real `*ledger.Ledger` (tempdir SQLite) and an in-process fake `*server.Server` (or the full server with loopback transport — whichever the api+server plan already provides).

Test cases:

1. **Admit → offer accept → usage_report → settle → ledger entry**: assert one `USAGE` entry with `cost_credits = expected`, `consumer_sig_missing=false`.
2. **Admit → consumer never settles**: assert `USAGE` entry with `consumer_sig_missing=true` after `SettlementTimeoutS`.
3. **Queue path**: simulate high admission pressure (mock `PressureGauge` or set thresholds tight); first `Submit` returns `Queued`; admission emits `PopReadyForBroker` after a heartbeat tick; broker delivers `Admit`.
4. **No eligible seeder**: `Submit` returns `NoCapacity{no_eligible_seeder}`; reservation is empty after the call.

- [ ] Commit `test(tracker): broker end-to-end integration`.

---

## Phase 11 — Acceptance gates

### Task 30: Race-clean confirmation

- [ ] Run `make -C tracker test` (which runs `go test -race ./...`) — must be clean.
- [ ] Run `make -C shared test`, `make -C plugin test` — both must pass with the new wire shape.

### Task 31: Performance smoke test

**File:** `tracker/internal/broker/perf_bench_test.go`

- [ ] Benchmark `Submit` p50/p99 against a synthetic registry of 100 seeders with 1 immediate-accept and 99 reject. Target p50 < 50 ms / p99 < 200 ms in CI's reference environment.

```go
func BenchmarkSubmit_AdmitFirst(b *testing.B) { /* … */ }
```

Track results in the PR description; not a CI gate.

### Task 32: CLAUDE.md confirmation

- [ ] Open `tracker/CLAUDE.md`. Confirm `internal/broker` is on the always-`-race` list — already named in the existing list. No edit needed unless missing.

### Task 33: Spec amendment notes

- [ ] Add a one-liner amendment note at the bottom of `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` (or the §11 amendment block) referencing this plan: "Implemented in `docs/superpowers/plans/2026-05-07-tracker-broker.md`; see `docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md`."
- [ ] Add a similar amendment note to `docs/superpowers/specs/admission/2026-04-25-admission-design.md` §11.2.
- [ ] Add a note in `docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md` for the new `pricing:` block.

- [ ] Commit `docs(tracker): cross-reference broker plan in upstream specs`.

### Task 34: Final `make check`

- [ ] Run `make check` from repo root. All three modules must be green.
- [ ] Run `git status` — must be clean (no stray files).

---

## Self-review notes (author)

- Spec §1–§11 mapped: §3 wire (Phase 1) · §4 data structures (Phase 4) · §5.1 Submit (Task 14) · §5.2 settlement (Task 17) · §5.3 timeouts (config Task 4 + reaper Task 19) · §5.4 queue drain (Task 15) · §5.5 reaper (Task 19) · §6 concurrency (`-race` everywhere) · §7 failure handling (per-test cases in Tasks 14, 17, 18) · §8 security (cost overspend Task 17, sig replay Task 18) · §9 config (Phase 2) · §10 admin (Task 22) · §11 acceptance (Phase 11).
- Cross-cutting amendments §12: items 1–4 in Phases 1, 2, 3, 9 respectively; item 5 (api/cmd) in Phases 8, 10.
- Identity-resolver gap (Task 17.5) is a real prerequisite for the consumer-sig path; flagged but kept inside the broker plan so settlement is fully testable.
- `Pusher` injection cycle in `cmd/run_cmd` (Task 28 step 1) is unresolved at plan time — three viable approaches sketched; engineer picks the one that requires least surgery.
- Performance benchmarks (Task 31) are advisory, not CI gates, matching the existing repo norm.
