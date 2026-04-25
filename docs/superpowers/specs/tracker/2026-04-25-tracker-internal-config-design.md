# `tracker/internal/config` — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Regional Tracker](./2026-04-22-tracker-design.md) |
| Depends on | [Federation](../federation/2026-04-22-federation-design.md), [Ledger](../ledger/2026-04-22-ledger-design.md), [Reputation](../reputation/2026-04-22-reputation-design.md), [Admission](../admission/2026-04-25-admission-design.md) |
| Status | Design draft |
| Date | 2026-04-25 |
| Scope | YAML configuration loader for `token-bay-tracker`. Defines the full tracker config schema, defaults from referenced specs, validation rules, and a `config validate` CLI subcommand. Leaf module — depends only on the Go stdlib + `gopkg.in/yaml.v3`. |

## 1. Purpose

Give the tracker a single, well-typed source of operational configuration. A YAML file at `--config <path>` is parsed into a `*Config`, defaults are filled, and every cross-field invariant is checked before the server boots. Operators get a `config validate` lint command that returns a non-zero exit and a list of every problem at once — loud failure beats silent misconfiguration.

This module is a leaf: every other `internal/<module>` will read its own section from `*Config`, but `config` itself depends on no other tracker package.

## 2. Scope

### 2.1 In scope (v1)

- New tracker module `tracker/internal/config`.
- Top-level `Config` struct with one section per current and near-future tracker subsystem (server, admin, ledger, broker, settlement, federation, reputation, admission, stun_turn, metrics).
- Defaults populated from the published subsystem specs.
- A small required field set with no sane defaults (paths, listener address, identity key).
- Strict YAML parsing — unknown keys rejected.
- Aggregating validator — every invariant failure surfaced together, not fail-fast.
- `token-bay-tracker config validate --config <path>` cobra subcommand.
- Per-package `tracker/internal/config/CLAUDE.md` with extension instructions.

### 2.2 Out of scope (v1)

- Environment variable overrides. The systemd unit and Dockerfile both pass `--config`; no precedence rules to debug.
- Cobra flag binding for individual fields.
- Hot reload / SIGHUP. Process restart is the operational model.
- Schema versioning / migration tooling. The tracker is on `v0.0.0-dev`; breaking changes are coordinated by humans.
- Hierarchical config merging (`base.yaml + overrides.yaml`).
- Secret-handling (key material is loaded from path, not embedded in YAML).

### 2.3 Non-goals

- This module does not *consume* config — it only loads, validates, and returns `*Config`. Feature modules read their section.
- This module does not enforce that referenced files (e.g. `identity_key_path`) actually exist or contain valid keys; that is the concern of `internal/server` startup. The single exception is the `tlog_path` parent-directory check explicitly required by the admission spec §9.3.

## 3. Interfaces

### 3.1 Public Go API

```go
// Load reads the YAML file at path, applies defaults, validates, and returns
// a fully populated *Config. Convenience wrapper over Parse → ApplyDefaults
// → Validate.
func Load(path string) (*Config, error)

// Parse decodes YAML from r into a Config. Rejects unknown fields. Does not
// apply defaults or validate.
func Parse(r io.Reader) (*Config, error)

// ApplyDefaults fills zero-valued fields with DefaultConfig() values and
// expands ${data_dir}-relative paths. Idempotent.
func ApplyDefaults(c *Config)

// Validate accumulates every cross-field invariant failure into a
// *ValidationError. Never fail-fast.
func Validate(c *Config) error

// DefaultConfig returns a Config with every defaultable field populated.
// Required fields are returned zero-valued.
func DefaultConfig() *Config
```

### 3.2 Error types

```go
type ParseError struct {
    Path string  // file path; "" when called via Parse(io.Reader)
    Err  error
}

type ValidationError struct {
    Errors []FieldError
}

type FieldError struct {
    Field   string  // dotted path, e.g. "admission.score_weights"
    Message string  // human-readable invariant description
}
```

`*ParseError` and `*ValidationError` are exposed for `errors.As`; the `config validate` CLI uses them to format multi-line output and choose exit codes.

### 3.3 CLI surface

```
token-bay-tracker config validate --config <path>
```

Exit codes:

| Code | Condition |
|---|---|
| 0 | Loaded and validated successfully |
| 1 | Other (file missing, permission denied, …) |
| 2 | `*ParseError` (malformed YAML or unknown key) |
| 3 | `*ValidationError` (one or more invariant failures) |

Success line on stdout: `OK: <path> (N sections, M fields)`.
Failure: per-error lines on stderr.

## 4. Data structures

### 4.1 `Config` schema

Top-level type plus per-section types. YAML tags use `snake_case`. Required fields are documented inline.

```go
type Config struct {
    DataDir    string           `yaml:"data_dir"`     // REQUIRED — base for relative paths
    LogLevel   string           `yaml:"log_level"`    // default "info"
    Server     ServerConfig     `yaml:"server"`
    Admin      AdminConfig      `yaml:"admin"`
    Ledger     LedgerConfig     `yaml:"ledger"`
    Broker     BrokerConfig     `yaml:"broker"`
    Settlement SettlementConfig `yaml:"settlement"`
    Federation FederationConfig `yaml:"federation"`
    Reputation ReputationConfig `yaml:"reputation"`
    Admission  AdmissionConfig  `yaml:"admission"`
    STUNTURN   STUNTURNConfig   `yaml:"stun_turn"`
    Metrics    MetricsConfig    `yaml:"metrics"`
}

type ServerConfig struct {
    ListenAddr      string `yaml:"listen_addr"`        // REQUIRED
    IdentityKeyPath string `yaml:"identity_key_path"`  // REQUIRED
    TLSCertPath     string `yaml:"tls_cert_path"`      // REQUIRED
    TLSKeyPath      string `yaml:"tls_key_path"`       // REQUIRED
}

type AdminConfig struct {
    ListenAddr string `yaml:"listen_addr"` // default "127.0.0.1:9090"
}

type LedgerConfig struct {
    StoragePath           string `yaml:"storage_path"`                  // REQUIRED
    MerkleRootIntervalMin int    `yaml:"merkle_root_interval_minutes"`  // default 60
}

type BrokerConfig struct {
    HeadroomThreshold       float64            `yaml:"headroom_threshold"`        // 0.2
    LoadThreshold           int                `yaml:"load_threshold"`            // 5
    ScoreWeights            BrokerScoreWeights `yaml:"score_weights"`
    OfferTimeoutMs          int                `yaml:"offer_timeout_ms"`          // 1500
    MaxOfferAttempts        int                `yaml:"max_offer_attempts"`        // 4
    BrokerRequestRatePerSec float64            `yaml:"broker_request_rate_per_sec"` // 2.0
}

type BrokerScoreWeights struct {
    Reputation float64 `yaml:"reputation"` // α 0.4
    Headroom   float64 `yaml:"headroom"`   // β 0.3
    RTT        float64 `yaml:"rtt"`        // γ 0.2
    Load       float64 `yaml:"load"`       // δ 0.1
}

type SettlementConfig struct {
    TunnelSetupMs      int `yaml:"tunnel_setup_ms"`      // 10000
    StreamIdleS        int `yaml:"stream_idle_s"`        // 60
    SettlementTimeoutS int `yaml:"settlement_timeout_s"` // 900
    ReservationTTLS    int `yaml:"reservation_ttl_s"`    // 1200
}

type FederationConfig struct {
    PeerCountMin          int `yaml:"peer_count_min"`               // 8
    PeerCountMax          int `yaml:"peer_count_max"`               // 16
    GossipDedupeTTLS      int `yaml:"gossip_dedupe_ttl_s"`          // 3600
    TransferRetryWindowH  int `yaml:"transfer_retry_window_hours"`  // 24
    EnrollRatePerMinPerIP int `yaml:"enroll_rate_per_min_per_ip"`   // 1
}

type ReputationConfig struct {
    EvaluationIntervalS int                     `yaml:"evaluation_interval_s"` // 60
    SignalWindows       ReputationSignalWindows `yaml:"signal_windows"`
    ZScoreThreshold     float64                 `yaml:"z_score_threshold"`     // 2.5
    DefaultScore        float64                 `yaml:"default_score"`         // 0.5
    FreezeListCacheTTLS int                     `yaml:"freeze_list_cache_ttl_s"` // 600
}

type ReputationSignalWindows struct {
    ShortS  int `yaml:"short_s"`  // 3600
    MediumS int `yaml:"medium_s"` // 86400
    LongS   int `yaml:"long_s"`   // 604800
}

type AdmissionConfig struct {
    PressureAdmitThreshold  float64 `yaml:"pressure_admit_threshold"`  // 0.85
    PressureRejectThreshold float64 `yaml:"pressure_reject_threshold"` // 1.5
    QueueCap                int     `yaml:"queue_cap"`                 // 512
    TrialTierScore          float64 `yaml:"trial_tier_score"`          // 0.4

    AgingAlphaPerMinute float64 `yaml:"aging_alpha_per_minute"` // 0.05
    QueueTimeoutS       int     `yaml:"queue_timeout_s"`        // 300

    ScoreWeights AdmissionScoreWeights `yaml:"score_weights"` // must sum to 1.0

    NetFlowNormalizationConstant int `yaml:"net_flow_normalization_constant"` // 10000
    TenureCapDays                int `yaml:"tenure_cap_days"`                 // 30
    StarterGrantCredits          int `yaml:"starter_grant_credits"`           // 1000
    RollingWindowDays            int `yaml:"rolling_window_days"`             // 30

    TrialSettlementsRequired int `yaml:"trial_settlements_required"` // 50
    TrialDurationHours       int `yaml:"trial_duration_hours"`       // 72

    AttestationTTLSeconds                 int     `yaml:"attestation_ttl_seconds"`                    // 86400
    AttestationMaxTTLSeconds              int     `yaml:"attestation_max_ttl_seconds"`                // 604800
    AttestationIssuancePerConsumerPerHour int     `yaml:"attestation_issuance_per_consumer_per_hour"` // 6
    MaxAttestationScoreImported           float64 `yaml:"max_attestation_score_imported"`             // 0.95

    TLogPath           string `yaml:"tlog_path"`            // default "${data_dir}/admission.tlog"
    SnapshotPathPrefix string `yaml:"snapshot_path_prefix"` // default "${data_dir}/admission.snapshot"
    SnapshotIntervalS  int    `yaml:"snapshot_interval_s"`  // 600
    SnapshotsRetained  int    `yaml:"snapshots_retained"`   // 3
    FsyncBatchWindowMs int    `yaml:"fsync_batch_window_ms"`// 5

    HeartbeatWindowMinutes      int `yaml:"heartbeat_window_minutes"`        // 10
    HeartbeatFreshnessDecayMaxS int `yaml:"heartbeat_freshness_decay_max_s"` // 300

    AttestationPeerBlocklist []string `yaml:"attestation_peer_blocklist"` // []
}

type AdmissionScoreWeights struct {
    SettlementReliability float64 `yaml:"settlement_reliability"` // 0.30
    InverseDisputeRate    float64 `yaml:"inverse_dispute_rate"`   // 0.10
    Tenure                float64 `yaml:"tenure"`                 // 0.20
    NetCreditFlow         float64 `yaml:"net_credit_flow"`        // 0.30
    BalanceCushion        float64 `yaml:"balance_cushion"`        // 0.10
}

type STUNTURNConfig struct {
    STUNListenAddr   string `yaml:"stun_listen_addr"`    // ":3478"
    TURNListenAddr   string `yaml:"turn_listen_addr"`    // ":3479"
    TURNRelayMaxKbps int    `yaml:"turn_relay_max_kbps"` // 1024
}

type MetricsConfig struct {
    ListenAddr string `yaml:"listen_addr"` // ":9100"
}
```

### 4.2 Required vs defaultable

Required (`Validate` flags zero values):

- `data_dir`
- `server.listen_addr`
- `server.identity_key_path`
- `server.tls_cert_path`
- `server.tls_key_path`
- `ledger.storage_path`

Every other field has a documented default in §4.1.

### 4.3 Derived defaults

`admission.tlog_path` and `admission.snapshot_path_prefix` default to empty strings in `DefaultConfig()`. `ApplyDefaults` substitutes `${data_dir}/admission.tlog` and `${data_dir}/admission.snapshot` when they are empty *and* `data_dir` is set. The substitution is plain Go string concatenation — no template engine.

## 5. Algorithms

### 5.1 `Load(path)`

```
file path
   │
   ▼
os.Open ──► Parse(r)            ◄── rejects unknown YAML keys
   │
   ▼
*Config (raw user values, zero values where omitted)
   │
   ▼
ApplyDefaults(c)                ◄── fills zeros, expands ${data_dir} paths
   │
   ▼
*Config (fully populated)
   │
   ▼
Validate(c)                     ◄── all cross-field invariants
   │
   ▼
*Config, nil   OR   nil, error
```

Errors from any phase short-circuit subsequent phases. The `Load` wrapper preserves the underlying typed error (`*ParseError`, `*ValidationError`) so callers can `errors.As`.

### 5.2 `Parse(r)`

Uses `yaml.NewDecoder(r)` with `dec.KnownFields(true)`. On any decode error, wraps in `*ParseError`.

### 5.3 `ApplyDefaults(c)`

Walks each section; for every field whose Go zero value is set and which has a documented default in §4.1, writes the default in. After per-field passes, runs the `${data_dir}` substitution from §4.3. The function is idempotent: running it twice produces the same `*Config`.

Implementation note: explicit code, one pass per section. No reflection-driven default mechanism — the rule from §6 of the embedded CLAUDE.md.

### 5.4 `Validate(c)`

Iterates the rules in §6 in section order. Each failure appends a `FieldError` to a local slice. After all rules, returns `nil` if the slice is empty, else `&ValidationError{Errors: …}`. Order matches struct field order so error output is stable for testing.

The single filesystem touch — checking that `admission.tlog_path`'s parent directory exists — uses `os.Stat`. Failure is reported as a `FieldError`, not a Go error.

## 6. Validation rules

### 6.1 Top-level / required

- `data_dir` non-empty
- `data_dir` is an absolute path
- `log_level` ∈ {`debug`, `info`, `warn`, `error`}
- `server.listen_addr`, `server.identity_key_path`, `server.tls_cert_path`, `server.tls_key_path` non-empty
- `ledger.storage_path` non-empty

### 6.2 Listener invariants

- `server.listen_addr`, `admin.listen_addr`, `metrics.listen_addr`, `stun_turn.stun_listen_addr`, `stun_turn.turn_listen_addr` each parseable as `host:port`
- All five collectively distinct (no two share the same `host:port`)

### 6.3 Ledger

- `ledger.merkle_root_interval_minutes` > 0

### 6.4 Broker

- `broker.headroom_threshold` ∈ [0.0, 1.0]
- `broker.load_threshold` ≥ 1
- `broker.score_weights.{reputation, headroom, rtt, load}` each ≥ 0
- `broker.score_weights` sum ≈ 1.0 (±0.001)
- `broker.offer_timeout_ms` > 0
- `broker.max_offer_attempts` ≥ 1
- `broker.broker_request_rate_per_sec` > 0

### 6.5 Settlement

- `settlement.tunnel_setup_ms`, `stream_idle_s`, `settlement_timeout_s`, `reservation_ttl_s` each > 0
- `tunnel_setup_ms` < `settlement_timeout_s × 1000`
- `reservation_ttl_s` ≥ `settlement_timeout_s`

### 6.6 Federation

- `federation.peer_count_min` ≥ 1
- `federation.peer_count_max` ≥ `peer_count_min`
- `federation.gossip_dedupe_ttl_s` > 0
- `federation.transfer_retry_window_hours` > 0
- `federation.enroll_rate_per_min_per_ip` ≥ 1

### 6.7 Reputation

- `reputation.evaluation_interval_s` > 0
- `reputation.signal_windows.short_s` < `medium_s` < `long_s`
- `reputation.z_score_threshold` > 0
- `reputation.default_score` ∈ [0.0, 1.0]
- `reputation.freeze_list_cache_ttl_s` > 0

### 6.8 Admission

- `admission.pressure_admit_threshold` > 0
- `admission.pressure_reject_threshold` > 0
- `admission.pressure_admit_threshold` < `pressure_reject_threshold`
- `admission.queue_cap` ≥ 1
- `admission.trial_tier_score` ∈ [0.0, 1.0]
- `admission.aging_alpha_per_minute` ≥ 0
- `admission.queue_timeout_s` > 0
- `admission.score_weights.{settlement_reliability, inverse_dispute_rate, tenure, net_credit_flow, balance_cushion}` each ≥ 0
- `admission.score_weights` sum ≈ 1.0 (±0.001)
- `admission.net_flow_normalization_constant` > 0
- `admission.tenure_cap_days` ≥ 1
- `admission.starter_grant_credits` ≥ 1
- `admission.rolling_window_days` ≥ 1
- `admission.trial_settlements_required` ≥ 0
- `admission.trial_duration_hours` ≥ 0
- `admission.attestation_ttl_seconds` > 0
- `admission.attestation_max_ttl_seconds` ≥ `attestation_ttl_seconds`
- `admission.attestation_issuance_per_consumer_per_hour` ≥ 1
- `admission.max_attestation_score_imported` ∈ [0.0, 1.0]
- `admission.tlog_path` non-empty (after `ApplyDefaults`)
- parent directory of `admission.tlog_path` exists
- `admission.snapshot_path_prefix` non-empty (after `ApplyDefaults`)
- `admission.snapshot_interval_s` > 0
- `admission.snapshots_retained` ≥ 1
- `admission.fsync_batch_window_ms` ≥ 0
- `admission.heartbeat_window_minutes` ≥ 1
- `admission.heartbeat_freshness_decay_max_s` > 0
- each entry in `admission.attestation_peer_blocklist` non-empty

### 6.9 STUN/TURN

- `stun_turn.turn_relay_max_kbps` > 0

(Listener parse + collision rules covered in §6.2.)

## 7. Source layout

```
tracker/internal/config/
├── CLAUDE.md
├── doc.go
├── config.go                 // Config + section types + DefaultConfig
├── load.go                   // Load(path), Parse(io.Reader)
├── apply_defaults.go
├── validate.go
├── errors.go
├── config_test.go
├── load_test.go
├── apply_defaults_test.go
├── validate_test.go
└── testdata/
    ├── minimal.yaml
    ├── full.yaml
    ├── invalid_score_weights_sum.yaml
    ├── invalid_pressure_inversion.yaml
    ├── invalid_attestation_ttl.yaml
    ├── invalid_signal_windows.yaml
    ├── invalid_log_level.yaml
    ├── invalid_listener_collision.yaml
    └── unknown_field.yaml

tracker/cmd/token-bay-tracker/
├── main.go                   // MODIFY: register newConfigCmd()
├── main_test.go              // MODIFY: assert config subcommand registered
├── config_cmd.go             // NEW
└── config_cmd_test.go        // NEW
```

## 8. Testing

### 8.1 Unit tests (per source file)

| File | Coverage |
|---|---|
| `config_test.go` | `DefaultConfig()` returns expected values; struct yaml tags follow snake_case. |
| `load_test.go` | `Parse` round-trips `testdata/full.yaml`; rejects `unknown_field.yaml`; `Load` happy path; `Load` on missing file / directory / permission denied; typed errors `errors.As`-comparable. |
| `apply_defaults_test.go` | Idempotence; zero-valued sections fully populated; `${data_dir}` expansion; explicit values not overwritten. |
| `validate_test.go` | One subtest per invariant in §6. Each starts from `DefaultConfig()`, mutates one field, asserts exactly one `FieldError` with the expected `Field` path. |

### 8.2 CLI tests

`cmd/token-bay-tracker/config_cmd_test.go` is a table-driven test over the testdata fixtures. Each row asserts exit code (0/1/2/3) and a stderr/stdout substring. Output captured via `cobra.Command.SetOut`/`SetErr`; the real binary is not invoked.

### 8.3 Coverage and discipline

- Coverage target: ≥ 90% on `internal/config/`. Achievable for a leaf module.
- `go test -race ./internal/config/...` clean. No goroutines today; the flag stays on for repo consistency.
- Any test that needs a real path uses `t.TempDir()`. Never a hardcoded path.
- The "parent of `tlog_path` exists" rule is the only filesystem-touching invariant; both branches (parent exists, parent missing) are exercised via `t.TempDir()`.

## 9. Failure handling

| Failure | Behavior |
|---|---|
| File not found at `--config <path>` | `Load` returns the underlying `os.PathError`. CLI exits 1 with message. |
| Permission denied | Same as above. |
| Malformed YAML (parse error) | `*ParseError` wrapping yaml.v3 error. CLI exits 2. |
| Unknown YAML key | `*ParseError` (yaml.v3 reports the offending key + line). CLI exits 2. |
| One or more invariant failures | `*ValidationError` with all `FieldError`s. CLI exits 3 and prints every error. |
| `tlog_path` parent missing | Reported as a `FieldError`. Loud, but does not crash. |
| `data_dir` empty AND `tlog_path`/`snapshot_path_prefix` empty | The `data_dir` required-field rule fires; the empty-path rules also fire. Both surface together — the operator sees the root cause and the consequence. |

The design property: every malformed config fails *loudly* before the server boots. The validator never returns success on partial data.

## 10. Security model

Configuration is operator-owned, read from a path the operator chose. The package does not read secrets directly — the four `*_path` fields point at files that other packages will open. Nothing in this module uploads, logs (other than via the operator's own log level), or transmits config values.

`config validate` is a local-only CLI. It does not connect to anything.

## 11. Acceptance criteria

The module is complete when:

1. `Load("testdata/full.yaml")` returns a fully populated `*Config` with no error.
2. `Load("testdata/minimal.yaml")` returns a `*Config` whose required fields match the YAML and whose other fields equal `DefaultConfig()` post-substitution.
3. `Load("testdata/unknown_field.yaml")` returns `*ParseError` whose message names the offending key.
4. `Load("testdata/invalid_*.yaml")` returns `*ValidationError` whose `Errors` slice contains the expected `FieldError` for that fixture.
5. `ApplyDefaults(ApplyDefaults(c)) == ApplyDefaults(c)` holds for `c := DefaultConfig()`, for `c := &Config{}`, and for the parsed `testdata/minimal.yaml` config.
6. `Validate(DefaultConfig())` returns a `*ValidationError` whose `Errors` slice lists exactly the six required fields from §4.2 and nothing else.
7. `token-bay-tracker config validate --config <path>` exits with the codes from §3.3 for each fixture.
8. `go test -race -coverprofile=coverage.out ./internal/config/...` reports ≥ 90% coverage.
9. `golangci-lint run ./internal/config/...` clean.

## 12. Open questions

- **Log level enum vs free string.** v1 takes a string and validates against a fixed set; later we may switch to a typed enum if zerolog's `level.ParseLevel` is wired in. Minor; not load-bearing.
- **Per-section interface.** Future module plans may want a `(*Config) BrokerSection() BrokerConfig` accessor for ergonomics. Not added in v1 — direct field access is fine.
- **Schema versioning.** When the first incompatible change lands, we'll add a `schema_version: 1` field and a migration path. Out of scope until then.

## 13. Cross-cutting amendments

None. This module introduces no changes to other specs. The fields it exposes are sourced from existing specs (tracker §5, federation, ledger, reputation, admission §9.3); this spec does not redefine them.

## 14. Future work

- Environment-variable overrides if container deployments demand it (would need a documented precedence rule).
- A `config show` subcommand that prints the resolved `*Config` (defaults applied) as YAML — useful for debugging "what did the server actually load?".
- A `config defaults` subcommand that emits a fully-populated YAML template for operators starting from scratch.
