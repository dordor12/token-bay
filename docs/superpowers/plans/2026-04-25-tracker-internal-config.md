# `tracker/internal/config` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `tracker/internal/config` per the [design spec](../specs/tracker/2026-04-25-tracker-internal-config-design.md) — YAML loader, defaults, aggregating validator, and a `token-bay-tracker config validate` CLI subcommand. Strict TDD throughout; one red-green-refactor cycle per commit.

**Architecture:** Leaf Go package (no other tracker package depends on it; it depends on stdlib + `gopkg.in/yaml.v3`). Public API: `Load`, `Parse`, `ApplyDefaults`, `Validate`, `DefaultConfig`. Errors expose typed `*ParseError` / `*ValidationError`. CLI subcommand uses these typed errors to choose exit codes.

**Tech Stack:** Go 1.23, `gopkg.in/yaml.v3` (already indirect dep, promoted to direct), `github.com/spf13/cobra` (already in use), `github.com/stretchr/testify` for tests.

**Spec:** `docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md`

**Working directory for every command:** `/Users/dor.amid/.superset/worktrees/token-bay/tracker_internal/config/tracker` (the worktree's tracker module). All `go` commands run there unless noted.

---

## File map

```
tracker/internal/config/
├── CLAUDE.md                     ← CREATE (Task 17)
├── doc.go                        ← CREATE (Task 3)
├── config.go                     ← CREATE (Task 3) + MODIFY (Task 4)
├── errors.go                     ← CREATE (Task 2)
├── load.go                       ← CREATE (Task 6) + MODIFY (Task 7)
├── apply_defaults.go             ← CREATE (Task 8)
├── validate.go                   ← CREATE (Task 9) + MODIFY (Tasks 10–13)
├── config_test.go                ← CREATE (Task 3) + MODIFY (Task 4)
├── errors_test.go                ← CREATE (Task 2)
├── load_test.go                  ← CREATE (Task 6) + MODIFY (Task 7, Task 14)
├── apply_defaults_test.go        ← CREATE (Task 8)
├── validate_test.go              ← CREATE (Task 9) + MODIFY (Tasks 10–13)
├── .gitkeep                      ← REMOVE (Task 3)
└── testdata/
    ├── minimal.yaml              ← CREATE (Task 5)
    ├── full.yaml                 ← CREATE (Task 5)
    ├── unknown_field.yaml        ← CREATE (Task 6)
    ├── invalid_log_level.yaml    ← CREATE (Task 14)
    ├── invalid_listener_collision.yaml  ← CREATE (Task 14)
    ├── invalid_signal_windows.yaml      ← CREATE (Task 14)
    ├── invalid_score_weights_sum.yaml   ← CREATE (Task 14)
    ├── invalid_pressure_inversion.yaml  ← CREATE (Task 14)
    └── invalid_attestation_ttl.yaml     ← CREATE (Task 14)

tracker/cmd/token-bay-tracker/
├── main.go                       ← MODIFY (Task 16) — register newConfigCmd
├── main_test.go                  ← MODIFY (Task 16)
├── config_cmd.go                 ← CREATE (Task 15)
└── config_cmd_test.go            ← CREATE (Task 15)

tracker/go.mod                    ← MODIFY (Task 1) — promote yaml.v3 to direct
tracker/go.sum                    ← MODIFY (Task 1) — via go mod tidy
```

---

## Task 1: Promote `gopkg.in/yaml.v3` to a direct dependency

**Files:**
- Modify: `tracker/go.mod`
- Modify: `tracker/go.sum`

`gopkg.in/yaml.v3` is currently an indirect dependency (pulled in via cobra). The config package imports it directly, so we declare it explicitly. This is a one-line change that `go mod tidy` will perform once the first import lands. We add a tiny throwaway file to trigger the import, then remove it after Task 3 introduces real imports — but to keep this plan honest, we just run `go get` to declare the dep without a temporary file.

- [ ] **Step 1: Declare yaml.v3 as a direct require**

Run from `tracker/`:
```bash
go get gopkg.in/yaml.v3@v3.0.1
```

(`v3.0.1` is the version already pinned in `go.sum`. Pinning explicitly avoids a surprise upgrade.)

- [ ] **Step 2: Verify go.mod now has yaml.v3 in the direct require block**

Run:
```bash
grep -A1 'yaml.v3' go.mod
```
Expected: a line `gopkg.in/yaml.v3 v3.0.1` outside any `// indirect` annotation, or no `// indirect` comment on it.

- [ ] **Step 3: Run go mod tidy**

```bash
go mod tidy
```

- [ ] **Step 4: Verify build still works**

```bash
go build ./...
```
Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
chore(tracker): promote gopkg.in/yaml.v3 to direct dependency

internal/config will import it directly; declare explicitly so the
relationship is auditable and a transitive change in cobra cannot
silently drop it.
EOF
)"
```

---

## Task 2: `errors.go` — `ParseError`, `ValidationError`, `FieldError`

**Files:**
- Create: `tracker/internal/config/errors.go`
- Create: `tracker/internal/config/errors_test.go`

These are pure data types with `Error()` methods. TDD here is short — write the assertions, then write the types.

- [ ] **Step 1: Write the failing tests**

Write to `tracker/internal/config/errors_test.go`:
```go
package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseError_ErrorIncludesPathAndCause(t *testing.T) {
	cause := errors.New("yaml: line 3: mapping values are not allowed in this context")
	pe := &ParseError{Path: "/etc/token-bay/tracker.yaml", Err: cause}

	msg := pe.Error()

	assert.Contains(t, msg, "/etc/token-bay/tracker.yaml")
	assert.Contains(t, msg, "line 3")
}

func TestParseError_EmptyPathOmitsPathSegment(t *testing.T) {
	pe := &ParseError{Err: errors.New("oops")}

	msg := pe.Error()

	assert.NotContains(t, msg, "/")
	assert.Contains(t, msg, "oops")
}

func TestParseError_Unwrap(t *testing.T) {
	cause := errors.New("inner")
	pe := &ParseError{Err: cause}

	assert.Same(t, cause, errors.Unwrap(pe))
}

func TestValidationError_ErrorListsAllFieldErrors(t *testing.T) {
	ve := &ValidationError{
		Errors: []FieldError{
			{Field: "data_dir", Message: "must be non-empty"},
			{Field: "admission.score_weights", Message: "weights sum to 0.97, must be 1.0 ± 0.001"},
		},
	}

	msg := ve.Error()

	assert.Contains(t, msg, "data_dir")
	assert.Contains(t, msg, "admission.score_weights")
	assert.Contains(t, msg, "weights sum to 0.97")
	// Each FieldError on its own line for operator readability:
	assert.Equal(t, 2, strings.Count(msg, "\n")+1-strings.Count(msg, "\n\n"))
}

func TestValidationError_ZeroErrorsStillFormats(t *testing.T) {
	ve := &ValidationError{}

	msg := ve.Error()

	require.NotEmpty(t, msg)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — `undefined: ParseError`, `undefined: ValidationError`, `undefined: FieldError`.

- [ ] **Step 3: Write `errors.go`**

Write to `tracker/internal/config/errors.go`:
```go
package config

import (
	"fmt"
	"strings"
)

// ParseError reports a YAML decode failure or an unknown-key rejection.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("config: parse error: %v", e.Err)
	}
	return fmt.Sprintf("config: parse error in %s: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// FieldError describes a single invariant failure produced by Validate.
type FieldError struct {
	Field   string
	Message string
}

func (e FieldError) String() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationError aggregates every invariant failure from a Validate call.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "config: validation failed (no specific errors recorded)"
	}
	parts := make([]string, 0, len(e.Errors)+1)
	parts = append(parts, fmt.Sprintf("config: validation failed (%d error(s)):", len(e.Errors)))
	for _, fe := range e.Errors {
		parts = append(parts, "  - "+fe.String())
	}
	return strings.Join(parts, "\n")
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/config/errors.go tracker/internal/config/errors_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): typed ParseError, ValidationError, FieldError

Distinct error types let callers distinguish parse failures from
invariant failures via errors.As, and let the CLI choose exit codes.
ValidationError aggregates every FieldError so operators see all
problems in one run.
EOF
)"
```

---

## Task 3: `config.go` — `Config` struct + section types (no `DefaultConfig` yet)

**Files:**
- Create: `tracker/internal/config/doc.go`
- Create: `tracker/internal/config/config.go`
- Create: `tracker/internal/config/config_test.go`
- Remove: `tracker/internal/config/.gitkeep`

This task introduces the type skeleton only — no `DefaultConfig`. The test asserts that the YAML tags match the spec's snake_case naming.

- [ ] **Step 1: Remove the `.gitkeep`**

```bash
rm tracker/internal/config/.gitkeep
```

- [ ] **Step 2: Write the failing test**

Write to `tracker/internal/config/config_test.go`:
```go
package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestConfig_YAMLTags_AllSnakeCase walks every field in Config and its
// embedded section types and asserts the yaml tag is present and uses
// snake_case. Catches typos and forgotten tags.
func TestConfig_YAMLTags_AllSnakeCase(t *testing.T) {
	walkYAMLTags(t, reflect.TypeOf(Config{}), "Config")
}

func walkYAMLTags(t *testing.T, ty reflect.Type, parent string) {
	t.Helper()
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		tag := f.Tag.Get("yaml")
		path := parent + "." + f.Name
		if tag == "" {
			t.Errorf("%s: missing yaml tag", path)
			continue
		}
		assert.Falsef(t, strings.Contains(tag, " "), "%s: yaml tag must not contain spaces", path)
		assert.Equalf(t, strings.ToLower(tag), tag, "%s: yaml tag %q must be snake_case", path, tag)
		if f.Type.Kind() == reflect.Struct {
			walkYAMLTags(t, f.Type, path)
		}
	}
}

func TestConfig_HasAllExpectedSections(t *testing.T) {
	cty := reflect.TypeOf(Config{})
	expected := []string{
		"DataDir", "LogLevel",
		"Server", "Admin", "Ledger",
		"Broker", "Settlement",
		"Federation", "Reputation", "Admission",
		"STUNTURN", "Metrics",
	}
	for _, name := range expected {
		_, ok := cty.FieldByName(name)
		assert.Truef(t, ok, "Config missing field %q", name)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — `undefined: Config`.

- [ ] **Step 4: Write `doc.go`**

Write to `tracker/internal/config/doc.go`:
```go
// Package config loads and validates the token-bay-tracker YAML
// configuration file.
//
// The package is a leaf: it depends only on the standard library and
// gopkg.in/yaml.v3. Every other tracker subsystem reads its section
// from the *Config returned by Load.
//
// Public surface:
//
//   Load(path)         file → *Config | error      (parse + defaults + validate)
//   Parse(io.Reader)   YAML stream → *Config | error  (no defaults, no validate)
//   ApplyDefaults(*Config)
//   Validate(*Config)  → error (typed *ValidationError)
//   DefaultConfig()    *Config with every defaultable field populated
//
// See docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md
// for the full schema and validation rules. See CLAUDE.md in this
// directory for instructions on extending the schema.
package config
```

- [ ] **Step 5: Write `config.go`**

Write to `tracker/internal/config/config.go`:
```go
package config

// Config is the root configuration for token-bay-tracker. Every other
// tracker subsystem reads its section from a *Config produced by Load.
//
// Required fields (data_dir, server.*, ledger.storage_path) have no
// defaults and must be populated by the operator. See the design spec
// §4.2 for the full required-field list.
type Config struct {
	DataDir    string           `yaml:"data_dir"`
	LogLevel   string           `yaml:"log_level"`
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
	ListenAddr      string `yaml:"listen_addr"`
	IdentityKeyPath string `yaml:"identity_key_path"`
	TLSCertPath     string `yaml:"tls_cert_path"`
	TLSKeyPath      string `yaml:"tls_key_path"`
}

type AdminConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}

type LedgerConfig struct {
	StoragePath           string `yaml:"storage_path"`
	MerkleRootIntervalMin int    `yaml:"merkle_root_interval_minutes"`
}

type BrokerConfig struct {
	HeadroomThreshold       float64            `yaml:"headroom_threshold"`
	LoadThreshold           int                `yaml:"load_threshold"`
	ScoreWeights            BrokerScoreWeights `yaml:"score_weights"`
	OfferTimeoutMs          int                `yaml:"offer_timeout_ms"`
	MaxOfferAttempts        int                `yaml:"max_offer_attempts"`
	BrokerRequestRatePerSec float64            `yaml:"broker_request_rate_per_sec"`
}

type BrokerScoreWeights struct {
	Reputation float64 `yaml:"reputation"`
	Headroom   float64 `yaml:"headroom"`
	RTT        float64 `yaml:"rtt"`
	Load       float64 `yaml:"load"`
}

type SettlementConfig struct {
	TunnelSetupMs      int `yaml:"tunnel_setup_ms"`
	StreamIdleS        int `yaml:"stream_idle_s"`
	SettlementTimeoutS int `yaml:"settlement_timeout_s"`
	ReservationTTLS    int `yaml:"reservation_ttl_s"`
}

type FederationConfig struct {
	PeerCountMin          int `yaml:"peer_count_min"`
	PeerCountMax          int `yaml:"peer_count_max"`
	GossipDedupeTTLS      int `yaml:"gossip_dedupe_ttl_s"`
	TransferRetryWindowH  int `yaml:"transfer_retry_window_hours"`
	EnrollRatePerMinPerIP int `yaml:"enroll_rate_per_min_per_ip"`
}

type ReputationConfig struct {
	EvaluationIntervalS int                     `yaml:"evaluation_interval_s"`
	SignalWindows       ReputationSignalWindows `yaml:"signal_windows"`
	ZScoreThreshold     float64                 `yaml:"z_score_threshold"`
	DefaultScore        float64                 `yaml:"default_score"`
	FreezeListCacheTTLS int                     `yaml:"freeze_list_cache_ttl_s"`
}

type ReputationSignalWindows struct {
	ShortS  int `yaml:"short_s"`
	MediumS int `yaml:"medium_s"`
	LongS   int `yaml:"long_s"`
}

type AdmissionConfig struct {
	PressureAdmitThreshold  float64 `yaml:"pressure_admit_threshold"`
	PressureRejectThreshold float64 `yaml:"pressure_reject_threshold"`
	QueueCap                int     `yaml:"queue_cap"`
	TrialTierScore          float64 `yaml:"trial_tier_score"`

	AgingAlphaPerMinute float64 `yaml:"aging_alpha_per_minute"`
	QueueTimeoutS       int     `yaml:"queue_timeout_s"`

	ScoreWeights AdmissionScoreWeights `yaml:"score_weights"`

	NetFlowNormalizationConstant int `yaml:"net_flow_normalization_constant"`
	TenureCapDays                int `yaml:"tenure_cap_days"`
	StarterGrantCredits          int `yaml:"starter_grant_credits"`
	RollingWindowDays            int `yaml:"rolling_window_days"`

	TrialSettlementsRequired int `yaml:"trial_settlements_required"`
	TrialDurationHours       int `yaml:"trial_duration_hours"`

	AttestationTTLSeconds                 int     `yaml:"attestation_ttl_seconds"`
	AttestationMaxTTLSeconds              int     `yaml:"attestation_max_ttl_seconds"`
	AttestationIssuancePerConsumerPerHour int     `yaml:"attestation_issuance_per_consumer_per_hour"`
	MaxAttestationScoreImported           float64 `yaml:"max_attestation_score_imported"`

	TLogPath           string `yaml:"tlog_path"`
	SnapshotPathPrefix string `yaml:"snapshot_path_prefix"`
	SnapshotIntervalS  int    `yaml:"snapshot_interval_s"`
	SnapshotsRetained  int    `yaml:"snapshots_retained"`
	FsyncBatchWindowMs int    `yaml:"fsync_batch_window_ms"`

	HeartbeatWindowMinutes      int `yaml:"heartbeat_window_minutes"`
	HeartbeatFreshnessDecayMaxS int `yaml:"heartbeat_freshness_decay_max_s"`

	AttestationPeerBlocklist []string `yaml:"attestation_peer_blocklist"`
}

type AdmissionScoreWeights struct {
	SettlementReliability float64 `yaml:"settlement_reliability"`
	InverseDisputeRate    float64 `yaml:"inverse_dispute_rate"`
	Tenure                float64 `yaml:"tenure"`
	NetCreditFlow         float64 `yaml:"net_credit_flow"`
	BalanceCushion        float64 `yaml:"balance_cushion"`
}

type STUNTURNConfig struct {
	STUNListenAddr   string `yaml:"stun_listen_addr"`
	TURNListenAddr   string `yaml:"turn_listen_addr"`
	TURNRelayMaxKbps int    `yaml:"turn_relay_max_kbps"`
}

type MetricsConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}
```

- [ ] **Step 6: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS — both struct tests green.

- [ ] **Step 7: Commit**

```bash
git add tracker/internal/config/doc.go tracker/internal/config/config.go tracker/internal/config/config_test.go
git rm tracker/internal/config/.gitkeep
git commit -m "$(cat <<'EOF'
feat(tracker/config): Config struct + per-section types

Types only — DefaultConfig and validation come in subsequent commits.
Reflect-walking tests assert every field has a snake_case yaml tag and
that all expected sections are present, catching typos at compile-of-test
time rather than at YAML-parse time.
EOF
)"
```

---

## Task 4: `DefaultConfig()` returning spec defaults

**Files:**
- Modify: `tracker/internal/config/config.go`
- Modify: `tracker/internal/config/config_test.go`

`DefaultConfig` populates every field that has a documented default in the spec. Required fields stay zero-valued.

- [ ] **Step 1: Append the failing tests to `config_test.go`**

Append to `tracker/internal/config/config_test.go`:
```go
func TestDefaultConfig_RequiredFieldsAreZero(t *testing.T) {
	c := DefaultConfig()

	assert.Empty(t, c.DataDir, "data_dir is required; default must be empty")
	assert.Empty(t, c.Server.ListenAddr)
	assert.Empty(t, c.Server.IdentityKeyPath)
	assert.Empty(t, c.Server.TLSCertPath)
	assert.Empty(t, c.Server.TLSKeyPath)
	assert.Empty(t, c.Ledger.StoragePath)
}

func TestDefaultConfig_SpecDefaultsPopulated(t *testing.T) {
	c := DefaultConfig()

	// Top-level
	assert.Equal(t, "info", c.LogLevel)

	// Admin / metrics / stun-turn
	assert.Equal(t, "127.0.0.1:9090", c.Admin.ListenAddr)
	assert.Equal(t, ":9100", c.Metrics.ListenAddr)
	assert.Equal(t, ":3478", c.STUNTURN.STUNListenAddr)
	assert.Equal(t, ":3479", c.STUNTURN.TURNListenAddr)
	assert.Equal(t, 1024, c.STUNTURN.TURNRelayMaxKbps)

	// Ledger
	assert.Equal(t, 60, c.Ledger.MerkleRootIntervalMin)

	// Broker (tracker spec §5.1)
	assert.InDelta(t, 0.2, c.Broker.HeadroomThreshold, 1e-9)
	assert.Equal(t, 5, c.Broker.LoadThreshold)
	assert.InDelta(t, 0.4, c.Broker.ScoreWeights.Reputation, 1e-9)
	assert.InDelta(t, 0.3, c.Broker.ScoreWeights.Headroom, 1e-9)
	assert.InDelta(t, 0.2, c.Broker.ScoreWeights.RTT, 1e-9)
	assert.InDelta(t, 0.1, c.Broker.ScoreWeights.Load, 1e-9)
	assert.Equal(t, 1500, c.Broker.OfferTimeoutMs)
	assert.Equal(t, 4, c.Broker.MaxOfferAttempts)
	assert.InDelta(t, 2.0, c.Broker.BrokerRequestRatePerSec, 1e-9)

	// Settlement (tracker spec §5.3)
	assert.Equal(t, 10000, c.Settlement.TunnelSetupMs)
	assert.Equal(t, 60, c.Settlement.StreamIdleS)
	assert.Equal(t, 900, c.Settlement.SettlementTimeoutS)
	assert.Equal(t, 1200, c.Settlement.ReservationTTLS)

	// Federation
	assert.Equal(t, 8, c.Federation.PeerCountMin)
	assert.Equal(t, 16, c.Federation.PeerCountMax)
	assert.Equal(t, 3600, c.Federation.GossipDedupeTTLS)
	assert.Equal(t, 24, c.Federation.TransferRetryWindowH)
	assert.Equal(t, 1, c.Federation.EnrollRatePerMinPerIP)

	// Reputation
	assert.Equal(t, 60, c.Reputation.EvaluationIntervalS)
	assert.Equal(t, 3600, c.Reputation.SignalWindows.ShortS)
	assert.Equal(t, 86400, c.Reputation.SignalWindows.MediumS)
	assert.Equal(t, 604800, c.Reputation.SignalWindows.LongS)
	assert.InDelta(t, 2.5, c.Reputation.ZScoreThreshold, 1e-9)
	assert.InDelta(t, 0.5, c.Reputation.DefaultScore, 1e-9)
	assert.Equal(t, 600, c.Reputation.FreezeListCacheTTLS)

	// Admission (admission spec §9.3)
	assert.InDelta(t, 0.85, c.Admission.PressureAdmitThreshold, 1e-9)
	assert.InDelta(t, 1.5, c.Admission.PressureRejectThreshold, 1e-9)
	assert.Equal(t, 512, c.Admission.QueueCap)
	assert.InDelta(t, 0.4, c.Admission.TrialTierScore, 1e-9)
	assert.InDelta(t, 0.05, c.Admission.AgingAlphaPerMinute, 1e-9)
	assert.Equal(t, 300, c.Admission.QueueTimeoutS)
	assert.InDelta(t, 0.30, c.Admission.ScoreWeights.SettlementReliability, 1e-9)
	assert.InDelta(t, 0.10, c.Admission.ScoreWeights.InverseDisputeRate, 1e-9)
	assert.InDelta(t, 0.20, c.Admission.ScoreWeights.Tenure, 1e-9)
	assert.InDelta(t, 0.30, c.Admission.ScoreWeights.NetCreditFlow, 1e-9)
	assert.InDelta(t, 0.10, c.Admission.ScoreWeights.BalanceCushion, 1e-9)
	assert.Equal(t, 10000, c.Admission.NetFlowNormalizationConstant)
	assert.Equal(t, 30, c.Admission.TenureCapDays)
	assert.Equal(t, 1000, c.Admission.StarterGrantCredits)
	assert.Equal(t, 30, c.Admission.RollingWindowDays)
	assert.Equal(t, 50, c.Admission.TrialSettlementsRequired)
	assert.Equal(t, 72, c.Admission.TrialDurationHours)
	assert.Equal(t, 86400, c.Admission.AttestationTTLSeconds)
	assert.Equal(t, 604800, c.Admission.AttestationMaxTTLSeconds)
	assert.Equal(t, 6, c.Admission.AttestationIssuancePerConsumerPerHour)
	assert.InDelta(t, 0.95, c.Admission.MaxAttestationScoreImported, 1e-9)
	assert.Empty(t, c.Admission.TLogPath, "tlog_path is data_dir-derived; default empty")
	assert.Empty(t, c.Admission.SnapshotPathPrefix)
	assert.Equal(t, 600, c.Admission.SnapshotIntervalS)
	assert.Equal(t, 3, c.Admission.SnapshotsRetained)
	assert.Equal(t, 5, c.Admission.FsyncBatchWindowMs)
	assert.Equal(t, 10, c.Admission.HeartbeatWindowMinutes)
	assert.Equal(t, 300, c.Admission.HeartbeatFreshnessDecayMaxS)
	assert.Empty(t, c.Admission.AttestationPeerBlocklist)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — `undefined: DefaultConfig`.

- [ ] **Step 3: Append `DefaultConfig` to `config.go`**

Append to `tracker/internal/config/config.go`:
```go
// DefaultConfig returns a Config with every defaultable field populated
// per the design spec §4.1. Required fields (see spec §4.2) are returned
// zero-valued so Validate flags them when the operator forgets.
func DefaultConfig() *Config {
	return &Config{
		LogLevel: "info",
		Admin: AdminConfig{
			ListenAddr: "127.0.0.1:9090",
		},
		Ledger: LedgerConfig{
			MerkleRootIntervalMin: 60,
		},
		Broker: BrokerConfig{
			HeadroomThreshold: 0.2,
			LoadThreshold:     5,
			ScoreWeights: BrokerScoreWeights{
				Reputation: 0.4,
				Headroom:   0.3,
				RTT:        0.2,
				Load:       0.1,
			},
			OfferTimeoutMs:          1500,
			MaxOfferAttempts:        4,
			BrokerRequestRatePerSec: 2.0,
		},
		Settlement: SettlementConfig{
			TunnelSetupMs:      10000,
			StreamIdleS:        60,
			SettlementTimeoutS: 900,
			ReservationTTLS:    1200,
		},
		Federation: FederationConfig{
			PeerCountMin:          8,
			PeerCountMax:          16,
			GossipDedupeTTLS:      3600,
			TransferRetryWindowH:  24,
			EnrollRatePerMinPerIP: 1,
		},
		Reputation: ReputationConfig{
			EvaluationIntervalS: 60,
			SignalWindows: ReputationSignalWindows{
				ShortS:  3600,
				MediumS: 86400,
				LongS:   604800,
			},
			ZScoreThreshold:     2.5,
			DefaultScore:        0.5,
			FreezeListCacheTTLS: 600,
		},
		Admission: AdmissionConfig{
			PressureAdmitThreshold:  0.85,
			PressureRejectThreshold: 1.5,
			QueueCap:                512,
			TrialTierScore:          0.4,
			AgingAlphaPerMinute:     0.05,
			QueueTimeoutS:           300,
			ScoreWeights: AdmissionScoreWeights{
				SettlementReliability: 0.30,
				InverseDisputeRate:    0.10,
				Tenure:                0.20,
				NetCreditFlow:         0.30,
				BalanceCushion:        0.10,
			},
			NetFlowNormalizationConstant:          10000,
			TenureCapDays:                         30,
			StarterGrantCredits:                   1000,
			RollingWindowDays:                     30,
			TrialSettlementsRequired:              50,
			TrialDurationHours:                    72,
			AttestationTTLSeconds:                 86400,
			AttestationMaxTTLSeconds:              604800,
			AttestationIssuancePerConsumerPerHour: 6,
			MaxAttestationScoreImported:           0.95,
			SnapshotIntervalS:                     600,
			SnapshotsRetained:                     3,
			FsyncBatchWindowMs:                    5,
			HeartbeatWindowMinutes:                10,
			HeartbeatFreshnessDecayMaxS:           300,
		},
		STUNTURN: STUNTURNConfig{
			STUNListenAddr:   ":3478",
			TURNListenAddr:   ":3479",
			TURNRelayMaxKbps: 1024,
		},
		Metrics: MetricsConfig{
			ListenAddr: ":9100",
		},
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/config/config.go tracker/internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): DefaultConfig with spec defaults

Every defaultable field gets its value from the matching subsystem
spec — tracker (broker/settlement), federation, reputation, admission §9.3.
Required fields stay zero-valued; Validate will flag them.
EOF
)"
```

---

## Task 5: Testdata fixtures — `minimal.yaml` and `full.yaml`

**Files:**
- Create: `tracker/internal/config/testdata/minimal.yaml`
- Create: `tracker/internal/config/testdata/full.yaml`

`minimal.yaml` is the smallest valid config: only required fields. `full.yaml` sets every field explicitly to its default value — the round-trip golden fixture.

- [ ] **Step 1: Write `minimal.yaml`**

Write to `tracker/internal/config/testdata/minimal.yaml`:
```yaml
data_dir: /var/lib/token-bay
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /var/lib/token-bay/ledger.sqlite
```

- [ ] **Step 2: Write `full.yaml`**

Write to `tracker/internal/config/testdata/full.yaml`:
```yaml
data_dir: /var/lib/token-bay
log_level: info

server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key

admin:
  listen_addr: "127.0.0.1:9090"

ledger:
  storage_path: /var/lib/token-bay/ledger.sqlite
  merkle_root_interval_minutes: 60

broker:
  headroom_threshold: 0.2
  load_threshold: 5
  score_weights:
    reputation: 0.4
    headroom: 0.3
    rtt: 0.2
    load: 0.1
  offer_timeout_ms: 1500
  max_offer_attempts: 4
  broker_request_rate_per_sec: 2.0

settlement:
  tunnel_setup_ms: 10000
  stream_idle_s: 60
  settlement_timeout_s: 900
  reservation_ttl_s: 1200

federation:
  peer_count_min: 8
  peer_count_max: 16
  gossip_dedupe_ttl_s: 3600
  transfer_retry_window_hours: 24
  enroll_rate_per_min_per_ip: 1

reputation:
  evaluation_interval_s: 60
  signal_windows:
    short_s: 3600
    medium_s: 86400
    long_s: 604800
  z_score_threshold: 2.5
  default_score: 0.5
  freeze_list_cache_ttl_s: 600

admission:
  pressure_admit_threshold: 0.85
  pressure_reject_threshold: 1.5
  queue_cap: 512
  trial_tier_score: 0.4
  aging_alpha_per_minute: 0.05
  queue_timeout_s: 300
  score_weights:
    settlement_reliability: 0.30
    inverse_dispute_rate: 0.10
    tenure: 0.20
    net_credit_flow: 0.30
    balance_cushion: 0.10
  net_flow_normalization_constant: 10000
  tenure_cap_days: 30
  starter_grant_credits: 1000
  rolling_window_days: 30
  trial_settlements_required: 50
  trial_duration_hours: 72
  attestation_ttl_seconds: 86400
  attestation_max_ttl_seconds: 604800
  attestation_issuance_per_consumer_per_hour: 6
  max_attestation_score_imported: 0.95
  tlog_path: /var/lib/token-bay/admission.tlog
  snapshot_path_prefix: /var/lib/token-bay/admission.snapshot
  snapshot_interval_s: 600
  snapshots_retained: 3
  fsync_batch_window_ms: 5
  heartbeat_window_minutes: 10
  heartbeat_freshness_decay_max_s: 300
  attestation_peer_blocklist: []

stun_turn:
  stun_listen_addr: ":3478"
  turn_listen_addr: ":3479"
  turn_relay_max_kbps: 1024

metrics:
  listen_addr: ":9100"
```

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/config/testdata/minimal.yaml tracker/internal/config/testdata/full.yaml
git commit -m "$(cat <<'EOF'
test(tracker/config): minimal and full YAML testdata fixtures

minimal.yaml has only required fields; full.yaml sets every field to
its default value — round-trip golden for Parse.
EOF
)"
```

---

## Task 6: `Parse(io.Reader)` with strict unknown-field rejection

**Files:**
- Create: `tracker/internal/config/load.go`
- Create: `tracker/internal/config/load_test.go`
- Create: `tracker/internal/config/testdata/unknown_field.yaml`

- [ ] **Step 1: Write the failing tests**

Write to `tracker/internal/config/load_test.go`:
```go
package config

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_FullYAMLRoundTrip(t *testing.T) {
	f, err := os.Open("testdata/full.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "/var/lib/token-bay", c.DataDir)
	assert.Equal(t, "info", c.LogLevel)
	assert.Equal(t, 512, c.Admission.QueueCap)
	assert.InDelta(t, 0.30, c.Admission.ScoreWeights.SettlementReliability, 1e-9)
}

func TestParse_MinimalYAMLLoadsRequiredOnly(t *testing.T) {
	f, err := os.Open("testdata/minimal.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.NoError(t, err)
	assert.Equal(t, "/var/lib/token-bay", c.DataDir)
	// Optional fields stay zero — Parse does not apply defaults.
	assert.Empty(t, c.LogLevel)
	assert.Equal(t, 0, c.Admission.QueueCap)
}

func TestParse_RejectsUnknownField(t *testing.T) {
	f, err := os.Open("testdata/unknown_field.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
	assert.Contains(t, pe.Error(), "score_weight")
}

func TestParse_RejectsMalformedYAML(t *testing.T) {
	r := strings.NewReader("data_dir: [this is: not a string]\n")

	c, err := Parse(r)

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
}
```

- [ ] **Step 2: Write `unknown_field.yaml`**

Write to `tracker/internal/config/testdata/unknown_field.yaml`:
```yaml
data_dir: /var/lib/token-bay
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /var/lib/token-bay/ledger.sqlite
broker:
  score_weight:           # typo — should be score_weights
    reputation: 0.4
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — `undefined: Parse`.

- [ ] **Step 4: Write `load.go`**

Write to `tracker/internal/config/load.go`:
```go
package config

import (
	"io"

	"gopkg.in/yaml.v3"
)

// Parse decodes a YAML document from r into a *Config. Unknown fields are
// rejected — typos must fail loudly. Parse does not apply defaults or
// validate; for the full pipeline use Load.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, &ParseError{Err: err}
	}
	return &c, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/config/load.go tracker/internal/config/load_test.go tracker/internal/config/testdata/unknown_field.yaml
git commit -m "$(cat <<'EOF'
feat(tracker/config): Parse with strict unknown-field rejection

yaml.Decoder.KnownFields(true) catches operator typos at parse time
rather than letting them silently fall back to defaults. Errors are
wrapped in *ParseError for errors.As discrimination by callers.
EOF
)"
```

---

## Task 7: `Load(path)` — file open + Parse

**Files:**
- Modify: `tracker/internal/config/load.go`
- Modify: `tracker/internal/config/load_test.go`

- [ ] **Step 1: Append the failing tests**

Append to `tracker/internal/config/load_test.go`:
```go
func TestLoad_FullYAMLLoadsAndAppliesDefaultsAndValidates(t *testing.T) {
	// At this point in the plan, ApplyDefaults and Validate are not yet
	// wired into Load — Task 12 will complete the wiring. For now we only
	// assert the file-open + Parse path. The test file will be extended
	// as the wiring lands; this initial form is intentionally loose.
	c, err := Load("testdata/full.yaml")

	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "/var/lib/token-bay", c.DataDir)
}

func TestLoad_MissingFileReturnsErrorPathPresent(t *testing.T) {
	c, err := Load("testdata/this_file_does_not_exist.yaml")

	require.Error(t, err)
	assert.Nil(t, c)
	assert.True(t, os.IsNotExist(err) || strings.Contains(err.Error(), "no such file"))
}

func TestLoad_DirectoryReturnsError(t *testing.T) {
	dir := t.TempDir()

	c, err := Load(dir)

	require.Error(t, err)
	assert.Nil(t, c)
}

func TestLoad_PermissionDeniedReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := dir + "/forbidden.yaml"
	require.NoError(t, os.WriteFile(path, []byte("data_dir: /tmp\n"), 0o000))

	c, err := Load(path)

	require.Error(t, err)
	assert.Nil(t, c)
}

func TestLoad_UnknownFieldYAMLReturnsParseError(t *testing.T) {
	c, err := Load("testdata/unknown_field.yaml")

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "testdata/unknown_field.yaml", pe.Path)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — `undefined: Load`.

- [ ] **Step 3: Replace `load.go` contents with full file**

Overwrite `tracker/internal/config/load.go` with:
```go
package config

import (
	"errors"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Parse decodes a YAML document from r into a *Config. Unknown fields are
// rejected — typos must fail loudly. Parse does not apply defaults or
// validate; for the full pipeline use Load.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, &ParseError{Err: err}
	}
	return &c, nil
}

// Load reads the YAML config file at path and decodes it. ApplyDefaults
// and Validate are wired in by Task 12; this task only exposes the file-
// open boundary and the *ParseError.Path enrichment.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c, err := Parse(f)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			pe.Path = path
		}
		return nil, err
	}
	return c, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/config/load.go tracker/internal/config/load_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): Load(path) — file open + Parse

ApplyDefaults and Validate wiring lands in Task 12. For now Load only
exposes the file-open boundary so the FS error matrix (missing,
directory, permission denied) gets covered under the file-only contract.
EOF
)"
```

---

## Task 8: `ApplyDefaults`

**Files:**
- Create: `tracker/internal/config/apply_defaults.go`
- Create: `tracker/internal/config/apply_defaults_test.go`

`ApplyDefaults` fills zero-valued fields and expands `${data_dir}` paths. Must be idempotent.

- [ ] **Step 1: Write the failing tests**

Write to `tracker/internal/config/apply_defaults_test.go`:
```go
package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyDefaults_EmptyConfigGetsAllDefaults(t *testing.T) {
	c := &Config{}

	ApplyDefaults(c)

	expected := DefaultConfig()
	expected.Admission.TLogPath = ""           // data_dir empty → no expansion
	expected.Admission.SnapshotPathPrefix = "" // ditto
	assert.Equal(t, expected, c)
}

func TestApplyDefaults_DataDirSubstitutesAdmissionPaths(t *testing.T) {
	c := &Config{DataDir: "/var/lib/token-bay"}

	ApplyDefaults(c)

	assert.Equal(t, "/var/lib/token-bay/admission.tlog", c.Admission.TLogPath)
	assert.Equal(t, "/var/lib/token-bay/admission.snapshot", c.Admission.SnapshotPathPrefix)
}

func TestApplyDefaults_ExplicitValuesPreserved(t *testing.T) {
	c := &Config{
		LogLevel: "debug",
		Broker: BrokerConfig{
			ScoreWeights: BrokerScoreWeights{
				Reputation: 0.5, Headroom: 0.2, RTT: 0.2, Load: 0.1,
			},
			MaxOfferAttempts: 7,
		},
		Admission: AdmissionConfig{
			TLogPath: "/custom/admission.tlog",
		},
		DataDir: "/var/lib/token-bay",
	}

	ApplyDefaults(c)

	assert.Equal(t, "debug", c.LogLevel)
	assert.InDelta(t, 0.5, c.Broker.ScoreWeights.Reputation, 1e-9)
	assert.Equal(t, 7, c.Broker.MaxOfferAttempts)
	assert.Equal(t, "/custom/admission.tlog", c.Admission.TLogPath, "explicit value not overwritten")
	assert.Equal(t, "/var/lib/token-bay/admission.snapshot", c.Admission.SnapshotPathPrefix, "snapshot still expanded")
}

func TestApplyDefaults_PartialBrokerWeightsAllZeroFilledFromDefault(t *testing.T) {
	// If the entire score_weights block is zero, treat as "operator did
	// not supply weights" and use defaults.
	c := &Config{}

	ApplyDefaults(c)

	d := DefaultConfig()
	assert.Equal(t, d.Broker.ScoreWeights, c.Broker.ScoreWeights)
}

func TestApplyDefaults_Idempotent(t *testing.T) {
	cases := map[string]*Config{
		"empty":    {},
		"defaults": DefaultConfig(),
		"partial":  {DataDir: "/var/lib/token-bay", LogLevel: "warn"},
	}
	for name, base := range cases {
		t.Run(name, func(t *testing.T) {
			a := cloneConfig(base)
			b := cloneConfig(base)

			ApplyDefaults(a)
			ApplyDefaults(b)
			ApplyDefaults(b)

			assert.True(t, reflect.DeepEqual(a, b), "ApplyDefaults must be idempotent")
		})
	}
}

// cloneConfig is a deep copy via gob-free pointer chasing — the only
// pointer-typed fields are slices, which we copy explicitly.
func cloneConfig(c *Config) *Config {
	cp := *c
	if c.Admission.AttestationPeerBlocklist != nil {
		cp.Admission.AttestationPeerBlocklist = append([]string(nil), c.Admission.AttestationPeerBlocklist...)
	}
	return &cp
}

// Sanity: DefaultConfig has no slice values, so after one ApplyDefaults call
// against an empty config the slice fields stay nil (not empty slices).
func TestApplyDefaults_BlocklistStaysNilWhenNotSet(t *testing.T) {
	c := &Config{}

	ApplyDefaults(c)

	require.Nil(t, c.Admission.AttestationPeerBlocklist)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — `undefined: ApplyDefaults`.

- [ ] **Step 3: Write `apply_defaults.go`**

Write to `tracker/internal/config/apply_defaults.go`:
```go
package config

// ApplyDefaults fills every zero-valued field with the value from
// DefaultConfig() and expands ${data_dir}-relative paths. Idempotent:
// running it twice produces the same Config.
//
// Implementation policy: explicit per-field code, no reflection. The
// rule lives in CLAUDE.md; future contributors who add fields must add
// a corresponding default-fill stanza here.
func ApplyDefaults(c *Config) {
	d := DefaultConfig()

	// Top-level
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}

	// Admin
	if c.Admin.ListenAddr == "" {
		c.Admin.ListenAddr = d.Admin.ListenAddr
	}

	// Ledger
	if c.Ledger.MerkleRootIntervalMin == 0 {
		c.Ledger.MerkleRootIntervalMin = d.Ledger.MerkleRootIntervalMin
	}

	// Broker
	if c.Broker.HeadroomThreshold == 0 {
		c.Broker.HeadroomThreshold = d.Broker.HeadroomThreshold
	}
	if c.Broker.LoadThreshold == 0 {
		c.Broker.LoadThreshold = d.Broker.LoadThreshold
	}
	// Score-weights block: if every weight is zero, fill all from default;
	// otherwise leave operator's values alone (Validate enforces sum=1.0).
	bw := c.Broker.ScoreWeights
	if bw.Reputation == 0 && bw.Headroom == 0 && bw.RTT == 0 && bw.Load == 0 {
		c.Broker.ScoreWeights = d.Broker.ScoreWeights
	}
	if c.Broker.OfferTimeoutMs == 0 {
		c.Broker.OfferTimeoutMs = d.Broker.OfferTimeoutMs
	}
	if c.Broker.MaxOfferAttempts == 0 {
		c.Broker.MaxOfferAttempts = d.Broker.MaxOfferAttempts
	}
	if c.Broker.BrokerRequestRatePerSec == 0 {
		c.Broker.BrokerRequestRatePerSec = d.Broker.BrokerRequestRatePerSec
	}

	// Settlement
	if c.Settlement.TunnelSetupMs == 0 {
		c.Settlement.TunnelSetupMs = d.Settlement.TunnelSetupMs
	}
	if c.Settlement.StreamIdleS == 0 {
		c.Settlement.StreamIdleS = d.Settlement.StreamIdleS
	}
	if c.Settlement.SettlementTimeoutS == 0 {
		c.Settlement.SettlementTimeoutS = d.Settlement.SettlementTimeoutS
	}
	if c.Settlement.ReservationTTLS == 0 {
		c.Settlement.ReservationTTLS = d.Settlement.ReservationTTLS
	}

	// Federation
	if c.Federation.PeerCountMin == 0 {
		c.Federation.PeerCountMin = d.Federation.PeerCountMin
	}
	if c.Federation.PeerCountMax == 0 {
		c.Federation.PeerCountMax = d.Federation.PeerCountMax
	}
	if c.Federation.GossipDedupeTTLS == 0 {
		c.Federation.GossipDedupeTTLS = d.Federation.GossipDedupeTTLS
	}
	if c.Federation.TransferRetryWindowH == 0 {
		c.Federation.TransferRetryWindowH = d.Federation.TransferRetryWindowH
	}
	if c.Federation.EnrollRatePerMinPerIP == 0 {
		c.Federation.EnrollRatePerMinPerIP = d.Federation.EnrollRatePerMinPerIP
	}

	// Reputation
	if c.Reputation.EvaluationIntervalS == 0 {
		c.Reputation.EvaluationIntervalS = d.Reputation.EvaluationIntervalS
	}
	if c.Reputation.SignalWindows.ShortS == 0 {
		c.Reputation.SignalWindows.ShortS = d.Reputation.SignalWindows.ShortS
	}
	if c.Reputation.SignalWindows.MediumS == 0 {
		c.Reputation.SignalWindows.MediumS = d.Reputation.SignalWindows.MediumS
	}
	if c.Reputation.SignalWindows.LongS == 0 {
		c.Reputation.SignalWindows.LongS = d.Reputation.SignalWindows.LongS
	}
	if c.Reputation.ZScoreThreshold == 0 {
		c.Reputation.ZScoreThreshold = d.Reputation.ZScoreThreshold
	}
	if c.Reputation.DefaultScore == 0 {
		c.Reputation.DefaultScore = d.Reputation.DefaultScore
	}
	if c.Reputation.FreezeListCacheTTLS == 0 {
		c.Reputation.FreezeListCacheTTLS = d.Reputation.FreezeListCacheTTLS
	}

	// Admission — non-path fields
	if c.Admission.PressureAdmitThreshold == 0 {
		c.Admission.PressureAdmitThreshold = d.Admission.PressureAdmitThreshold
	}
	if c.Admission.PressureRejectThreshold == 0 {
		c.Admission.PressureRejectThreshold = d.Admission.PressureRejectThreshold
	}
	if c.Admission.QueueCap == 0 {
		c.Admission.QueueCap = d.Admission.QueueCap
	}
	if c.Admission.TrialTierScore == 0 {
		c.Admission.TrialTierScore = d.Admission.TrialTierScore
	}
	if c.Admission.AgingAlphaPerMinute == 0 {
		c.Admission.AgingAlphaPerMinute = d.Admission.AgingAlphaPerMinute
	}
	if c.Admission.QueueTimeoutS == 0 {
		c.Admission.QueueTimeoutS = d.Admission.QueueTimeoutS
	}
	asw := c.Admission.ScoreWeights
	if asw.SettlementReliability == 0 && asw.InverseDisputeRate == 0 &&
		asw.Tenure == 0 && asw.NetCreditFlow == 0 && asw.BalanceCushion == 0 {
		c.Admission.ScoreWeights = d.Admission.ScoreWeights
	}
	if c.Admission.NetFlowNormalizationConstant == 0 {
		c.Admission.NetFlowNormalizationConstant = d.Admission.NetFlowNormalizationConstant
	}
	if c.Admission.TenureCapDays == 0 {
		c.Admission.TenureCapDays = d.Admission.TenureCapDays
	}
	if c.Admission.StarterGrantCredits == 0 {
		c.Admission.StarterGrantCredits = d.Admission.StarterGrantCredits
	}
	if c.Admission.RollingWindowDays == 0 {
		c.Admission.RollingWindowDays = d.Admission.RollingWindowDays
	}
	if c.Admission.TrialSettlementsRequired == 0 {
		c.Admission.TrialSettlementsRequired = d.Admission.TrialSettlementsRequired
	}
	if c.Admission.TrialDurationHours == 0 {
		c.Admission.TrialDurationHours = d.Admission.TrialDurationHours
	}
	if c.Admission.AttestationTTLSeconds == 0 {
		c.Admission.AttestationTTLSeconds = d.Admission.AttestationTTLSeconds
	}
	if c.Admission.AttestationMaxTTLSeconds == 0 {
		c.Admission.AttestationMaxTTLSeconds = d.Admission.AttestationMaxTTLSeconds
	}
	if c.Admission.AttestationIssuancePerConsumerPerHour == 0 {
		c.Admission.AttestationIssuancePerConsumerPerHour = d.Admission.AttestationIssuancePerConsumerPerHour
	}
	if c.Admission.MaxAttestationScoreImported == 0 {
		c.Admission.MaxAttestationScoreImported = d.Admission.MaxAttestationScoreImported
	}
	if c.Admission.SnapshotIntervalS == 0 {
		c.Admission.SnapshotIntervalS = d.Admission.SnapshotIntervalS
	}
	if c.Admission.SnapshotsRetained == 0 {
		c.Admission.SnapshotsRetained = d.Admission.SnapshotsRetained
	}
	// FsyncBatchWindowMs default is non-zero (5); zero is invalid only if
	// the operator wrote 0 explicitly. We default-fill the zero case.
	if c.Admission.FsyncBatchWindowMs == 0 {
		c.Admission.FsyncBatchWindowMs = d.Admission.FsyncBatchWindowMs
	}
	if c.Admission.HeartbeatWindowMinutes == 0 {
		c.Admission.HeartbeatWindowMinutes = d.Admission.HeartbeatWindowMinutes
	}
	if c.Admission.HeartbeatFreshnessDecayMaxS == 0 {
		c.Admission.HeartbeatFreshnessDecayMaxS = d.Admission.HeartbeatFreshnessDecayMaxS
	}

	// Admission — paths derive from data_dir
	if c.Admission.TLogPath == "" && c.DataDir != "" {
		c.Admission.TLogPath = c.DataDir + "/admission.tlog"
	}
	if c.Admission.SnapshotPathPrefix == "" && c.DataDir != "" {
		c.Admission.SnapshotPathPrefix = c.DataDir + "/admission.snapshot"
	}

	// STUN/TURN
	if c.STUNTURN.STUNListenAddr == "" {
		c.STUNTURN.STUNListenAddr = d.STUNTURN.STUNListenAddr
	}
	if c.STUNTURN.TURNListenAddr == "" {
		c.STUNTURN.TURNListenAddr = d.STUNTURN.TURNListenAddr
	}
	if c.STUNTURN.TURNRelayMaxKbps == 0 {
		c.STUNTURN.TURNRelayMaxKbps = d.STUNTURN.TURNRelayMaxKbps
	}

	// Metrics
	if c.Metrics.ListenAddr == "" {
		c.Metrics.ListenAddr = d.Metrics.ListenAddr
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/config/apply_defaults.go tracker/internal/config/apply_defaults_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): ApplyDefaults — idempotent default-fill

Explicit per-field code; no reflection. Score-weights blocks (broker and
admission) default-fill only when every weight is zero, so partial-
operator-input doesn't get silently overridden. ${data_dir}-relative
paths expand here, after parsing, before validation.
EOF
)"
```

---

## Task 9: `Validate` skeleton — top-level required + log_level + listeners

**Files:**
- Create: `tracker/internal/config/validate.go`
- Create: `tracker/internal/config/validate_test.go`

This task lays down the `Validate` function shell and covers spec §6.1 (top-level required) and §6.2 (listener invariants). Subsequent tasks extend `validate.go` and `validate_test.go` per section.

- [ ] **Step 1: Write the failing tests**

Write to `tracker/internal/config/validate_test.go`:
```go
package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns a config that passes Validate. Subtests mutate one
// field at a time and assert exactly one FieldError surfaces.
func validConfig(t *testing.T) *Config {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = "/var/lib/token-bay"
	c.Server = ServerConfig{
		ListenAddr:      "0.0.0.0:7777",
		IdentityKeyPath: "/etc/token-bay/identity.key",
		TLSCertPath:     "/etc/token-bay/cert.pem",
		TLSKeyPath:      "/etc/token-bay/cert.key",
	}
	c.Ledger.StoragePath = "/var/lib/token-bay/ledger.sqlite"
	ApplyDefaults(c) // fills tlog_path / snapshot_path_prefix

	// tlog parent dir must exist for §6.8's filesystem check; use the
	// already-existing /var which is universal on linux+darwin. (Tests
	// for the missing-parent branch override this.)
	c.Admission.TLogPath = "/var/admission.tlog"
	c.Admission.SnapshotPathPrefix = "/var/admission.snapshot"
	return c
}

func assertOneFieldError(t *testing.T, err error, field string) {
	t.Helper()
	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve), "expected *ValidationError, got %T", err)
	require.Lenf(t, ve.Errors, 1, "expected exactly one FieldError, got %v", ve.Errors)
	assert.Equal(t, field, ve.Errors[0].Field)
}

func TestValidate_DefaultConfigFlagsRequiredFields(t *testing.T) {
	err := Validate(DefaultConfig())

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	fields := make(map[string]bool, len(ve.Errors))
	for _, fe := range ve.Errors {
		fields[fe.Field] = true
	}
	assert.True(t, fields["data_dir"])
	assert.True(t, fields["server.listen_addr"])
	assert.True(t, fields["server.identity_key_path"])
	assert.True(t, fields["server.tls_cert_path"])
	assert.True(t, fields["server.tls_key_path"])
	assert.True(t, fields["ledger.storage_path"])
}

func TestValidate_HappyPath(t *testing.T) {
	c := validConfig(t)

	err := Validate(c)

	assert.NoError(t, err)
}

func TestValidate_DataDirMustBeAbsolute(t *testing.T) {
	c := validConfig(t)
	c.DataDir = "var/lib/token-bay" // relative

	err := Validate(c)

	assertOneFieldError(t, err, "data_dir")
}

func TestValidate_LogLevelRejectsBogusValue(t *testing.T) {
	c := validConfig(t)
	c.LogLevel = "chatty"

	err := Validate(c)

	assertOneFieldError(t, err, "log_level")
}

func TestValidate_LogLevelAcceptsAllFour(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		t.Run(lvl, func(t *testing.T) {
			c := validConfig(t)
			c.LogLevel = lvl

			err := Validate(c)

			assert.NoError(t, err)
		})
	}
}

func TestValidate_UnparseableListenerAddr(t *testing.T) {
	c := validConfig(t)
	c.Server.ListenAddr = "not-a-host-port"

	err := Validate(c)

	assertOneFieldError(t, err, "server.listen_addr")
}

func TestValidate_ListenerCollision_ServerAndAdmin(t *testing.T) {
	c := validConfig(t)
	c.Admin.ListenAddr = c.Server.ListenAddr

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	// Collision is reported once, against the second offender:
	require.Len(t, ve.Errors, 1)
	assert.Equal(t, "admin.listen_addr", ve.Errors[0].Field)
	assert.Contains(t, ve.Errors[0].Message, "collides")
}

func TestValidate_ListenerCollision_StunAndTurn(t *testing.T) {
	c := validConfig(t)
	c.STUNTURN.TURNListenAddr = c.STUNTURN.STUNListenAddr

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	require.Len(t, ve.Errors, 1)
	assert.Equal(t, "stun_turn.turn_listen_addr", ve.Errors[0].Field)
}
```

- [ ] **Step 2: Write `validate.go`**

Write to `tracker/internal/config/validate.go`:
```go
package config

import (
	"net"
	"path/filepath"
)

// Validate runs every cross-field invariant declared in the design spec
// §6 against c and returns a *ValidationError with every failure
// recorded. Returns nil when c is valid.
//
// Order: rules run in section order so error output is stable for tests.
// Validation never short-circuits on first error — operators must see
// every problem in one run.
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkLogLevel(c)
	v.checkListeners(c)
	return v.toError()
}

type validator struct {
	errs []FieldError
}

func (v *validator) add(field, msg string) {
	v.errs = append(v.errs, FieldError{Field: field, Message: msg})
}

func (v *validator) toError() error {
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: v.errs}
}

// §6.1
func (v *validator) checkRequired(c *Config) {
	if c.DataDir == "" {
		v.add("data_dir", "must be non-empty")
	} else if !filepath.IsAbs(c.DataDir) {
		v.add("data_dir", "must be an absolute path")
	}
	if c.Server.ListenAddr == "" {
		v.add("server.listen_addr", "must be non-empty")
	}
	if c.Server.IdentityKeyPath == "" {
		v.add("server.identity_key_path", "must be non-empty")
	}
	if c.Server.TLSCertPath == "" {
		v.add("server.tls_cert_path", "must be non-empty")
	}
	if c.Server.TLSKeyPath == "" {
		v.add("server.tls_key_path", "must be non-empty")
	}
	if c.Ledger.StoragePath == "" {
		v.add("ledger.storage_path", "must be non-empty")
	}
}

// §6.1 — log level enum
func (v *validator) checkLogLevel(c *Config) {
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		// "" is allowed because ApplyDefaults fills it; if Validate is
		// called without ApplyDefaults the empty value is still valid.
	default:
		v.add("log_level", "must be one of: debug, info, warn, error")
	}
}

// §6.2 — every listener parses; all five collectively distinct.
func (v *validator) checkListeners(c *Config) {
	listeners := []struct {
		field string
		addr  string
	}{
		{"server.listen_addr", c.Server.ListenAddr},
		{"admin.listen_addr", c.Admin.ListenAddr},
		{"metrics.listen_addr", c.Metrics.ListenAddr},
		{"stun_turn.stun_listen_addr", c.STUNTURN.STUNListenAddr},
		{"stun_turn.turn_listen_addr", c.STUNTURN.TURNListenAddr},
	}
	seen := make(map[string]string, len(listeners))
	for _, l := range listeners {
		if l.addr == "" {
			// Required listeners are caught by checkRequired; optional
			// listeners with empty addr are not parsed.
			continue
		}
		if _, _, err := net.SplitHostPort(l.addr); err != nil {
			v.add(l.field, "must be a valid host:port — "+err.Error())
			continue
		}
		if prev, ok := seen[l.addr]; ok {
			v.add(l.field, "collides with "+prev+" (both "+l.addr+")")
			continue
		}
		seen[l.addr] = l.field
	}
}
```

- [ ] **Step 3: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/config/validate.go tracker/internal/config/validate_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): Validate skeleton — required + log_level + listeners

validator accumulates FieldErrors across every section; never fail-fast.
This commit covers spec §6.1 (required + log_level) and §6.2 (listener
parse + collision). Subsequent commits add ledger / broker / settlement /
federation / reputation / admission / stun_turn rules.
EOF
)"
```

---

## Task 10: `Validate` — ledger + broker + settlement

**Files:**
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/validate_test.go`

- [ ] **Step 1: Append the failing tests**

Append to `tracker/internal/config/validate_test.go`:
```go
func TestValidate_LedgerMerkleIntervalMustBePositive(t *testing.T) {
	c := validConfig(t)
	c.Ledger.MerkleRootIntervalMin = 0

	err := Validate(c)

	assertOneFieldError(t, err, "ledger.merkle_root_interval_minutes")
}

func TestValidate_BrokerHeadroomThresholdRange(t *testing.T) {
	c := validConfig(t)
	c.Broker.HeadroomThreshold = 1.5

	err := Validate(c)

	assertOneFieldError(t, err, "broker.headroom_threshold")
}

func TestValidate_BrokerLoadThresholdMin(t *testing.T) {
	c := validConfig(t)
	c.Broker.LoadThreshold = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.load_threshold")
}

func TestValidate_BrokerScoreWeightsSum(t *testing.T) {
	c := validConfig(t)
	c.Broker.ScoreWeights.Reputation = 0.5 // sum becomes 1.1

	err := Validate(c)

	assertOneFieldError(t, err, "broker.score_weights")
}

func TestValidate_BrokerScoreWeightsNegative(t *testing.T) {
	c := validConfig(t)
	c.Broker.ScoreWeights.RTT = -0.2
	c.Broker.ScoreWeights.Reputation = 0.6 // keep sum at 1.0

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	// Negative-weight rule fires first; sum check separate.
	found := false
	for _, fe := range ve.Errors {
		if fe.Field == "broker.score_weights.rtt" {
			found = true
		}
	}
	assert.True(t, found, "expected broker.score_weights.rtt error in %v", ve.Errors)
}

func TestValidate_BrokerOfferTimeoutPositive(t *testing.T) {
	c := validConfig(t)
	c.Broker.OfferTimeoutMs = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.offer_timeout_ms")
}

func TestValidate_BrokerMaxOfferAttemptsMin(t *testing.T) {
	c := validConfig(t)
	c.Broker.MaxOfferAttempts = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.max_offer_attempts")
}

func TestValidate_BrokerRequestRatePositive(t *testing.T) {
	c := validConfig(t)
	c.Broker.BrokerRequestRatePerSec = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.broker_request_rate_per_sec")
}

func TestValidate_SettlementTimeoutsPositive(t *testing.T) {
	cases := map[string]func(*Config){
		"settlement.tunnel_setup_ms":      func(c *Config) { c.Settlement.TunnelSetupMs = 0 },
		"settlement.stream_idle_s":        func(c *Config) { c.Settlement.StreamIdleS = 0 },
		"settlement.settlement_timeout_s": func(c *Config) { c.Settlement.SettlementTimeoutS = 0 },
		"settlement.reservation_ttl_s":    func(c *Config) { c.Settlement.ReservationTTLS = 0 },
	}
	for field, mut := range cases {
		t.Run(field, func(t *testing.T) {
			c := validConfig(t)
			mut(c)
			err := Validate(c)
			// Setting one timeout to zero may also break the cross-rule
			// (tunnel_setup < settlement_timeout * 1000); allow either
			// the single-field error or the field plus the cross-rule.
			require.Error(t, err)
			var ve *ValidationError
			require.True(t, errors.As(err, &ve))
			matched := false
			for _, fe := range ve.Errors {
				if fe.Field == field {
					matched = true
				}
			}
			assert.True(t, matched, "expected %s in errors %v", field, ve.Errors)
		})
	}
}

func TestValidate_SettlementTunnelSetupBelowSettlementTimeout(t *testing.T) {
	c := validConfig(t)
	c.Settlement.TunnelSetupMs = 1_000_000 // 1000s > 900s
	c.Settlement.SettlementTimeoutS = 900

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "settlement.tunnel_setup_ms" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_SettlementReservationOutlivesSettlement(t *testing.T) {
	c := validConfig(t)
	c.Settlement.SettlementTimeoutS = 900
	c.Settlement.ReservationTTLS = 600 // shorter than settlement

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "settlement.reservation_ttl_s" {
			matched = true
		}
	}
	assert.True(t, matched)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — most ledger/broker/settlement tests fail because the validator doesn't yet check those.

- [ ] **Step 3: Add ledger/broker/settlement rules to `validate.go`**

Modify `tracker/internal/config/validate.go` — extend `Validate` to call new methods:
```go
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkLogLevel(c)
	v.checkListeners(c)
	v.checkLedger(c)
	v.checkBroker(c)
	v.checkSettlement(c)
	return v.toError()
}
```

Append these methods to `validate.go`:
```go
// §6.3
func (v *validator) checkLedger(c *Config) {
	if c.Ledger.MerkleRootIntervalMin <= 0 {
		v.add("ledger.merkle_root_interval_minutes", "must be > 0")
	}
}

// §6.4
func (v *validator) checkBroker(c *Config) {
	if c.Broker.HeadroomThreshold < 0 || c.Broker.HeadroomThreshold > 1 {
		v.add("broker.headroom_threshold", "must be in [0.0, 1.0]")
	}
	if c.Broker.LoadThreshold < 1 {
		v.add("broker.load_threshold", "must be >= 1")
	}
	w := c.Broker.ScoreWeights
	checkNonNeg := func(field string, val float64) {
		if val < 0 {
			v.add(field, "must be >= 0")
		}
	}
	checkNonNeg("broker.score_weights.reputation", w.Reputation)
	checkNonNeg("broker.score_weights.headroom", w.Headroom)
	checkNonNeg("broker.score_weights.rtt", w.RTT)
	checkNonNeg("broker.score_weights.load", w.Load)

	sum := w.Reputation + w.Headroom + w.RTT + w.Load
	if !approxEqual(sum, 1.0, 1e-3) {
		v.add("broker.score_weights",
			"weights must sum to 1.0 ± 0.001 (got "+ftoa(sum)+")")
	}

	if c.Broker.OfferTimeoutMs <= 0 {
		v.add("broker.offer_timeout_ms", "must be > 0")
	}
	if c.Broker.MaxOfferAttempts < 1 {
		v.add("broker.max_offer_attempts", "must be >= 1")
	}
	if c.Broker.BrokerRequestRatePerSec <= 0 {
		v.add("broker.broker_request_rate_per_sec", "must be > 0")
	}
}

// §6.5
func (v *validator) checkSettlement(c *Config) {
	if c.Settlement.TunnelSetupMs <= 0 {
		v.add("settlement.tunnel_setup_ms", "must be > 0")
	}
	if c.Settlement.StreamIdleS <= 0 {
		v.add("settlement.stream_idle_s", "must be > 0")
	}
	if c.Settlement.SettlementTimeoutS <= 0 {
		v.add("settlement.settlement_timeout_s", "must be > 0")
	}
	if c.Settlement.ReservationTTLS <= 0 {
		v.add("settlement.reservation_ttl_s", "must be > 0")
	}
	// Cross-field rules — only meaningful when the underlying values are positive.
	if c.Settlement.TunnelSetupMs > 0 && c.Settlement.SettlementTimeoutS > 0 {
		if c.Settlement.TunnelSetupMs >= c.Settlement.SettlementTimeoutS*1000 {
			v.add("settlement.tunnel_setup_ms",
				"must be < settlement_timeout_s × 1000")
		}
	}
	if c.Settlement.ReservationTTLS > 0 && c.Settlement.SettlementTimeoutS > 0 {
		if c.Settlement.ReservationTTLS < c.Settlement.SettlementTimeoutS {
			v.add("settlement.reservation_ttl_s",
				"must be >= settlement_timeout_s (TTL must outlast settlement)")
		}
	}
}

// approxEqual returns true iff |a - b| < tolerance.
func approxEqual(a, b, tolerance float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tolerance
}

// ftoa renders a float in a stable, operator-readable form. Local helper —
// fmt.Sprintf("%g", …) would be fine, but using a tight wrapper keeps
// validation messages dependency-free of fmt's locale variations.
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
```

Add `"strconv"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/config/validate.go tracker/internal/config/validate_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): Validate ledger + broker + settlement

Spec §6.3–§6.5. Broker score-weights sum and settlement cross-field
relations (tunnel < settlement × 1000, reservation >= settlement) are
the load-bearing checks; the rest are simple range bounds.
EOF
)"
```

---

## Task 11: `Validate` — federation + reputation

**Files:**
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/validate_test.go`

- [ ] **Step 1: Append the failing tests**

Append to `tracker/internal/config/validate_test.go`:
```go
func TestValidate_FederationPeerCountMin(t *testing.T) {
	c := validConfig(t)
	c.Federation.PeerCountMin = 0

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "federation.peer_count_min" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_FederationPeerCountOrdering(t *testing.T) {
	c := validConfig(t)
	c.Federation.PeerCountMin = 20
	c.Federation.PeerCountMax = 10

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "federation.peer_count_max" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_FederationGossipDedupePositive(t *testing.T) {
	c := validConfig(t)
	c.Federation.GossipDedupeTTLS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "federation.gossip_dedupe_ttl_s")
}

func TestValidate_FederationTransferRetryPositive(t *testing.T) {
	c := validConfig(t)
	c.Federation.TransferRetryWindowH = 0

	err := Validate(c)

	assertOneFieldError(t, err, "federation.transfer_retry_window_hours")
}

func TestValidate_FederationEnrollRateMin(t *testing.T) {
	c := validConfig(t)
	c.Federation.EnrollRatePerMinPerIP = 0

	err := Validate(c)

	assertOneFieldError(t, err, "federation.enroll_rate_per_min_per_ip")
}

func TestValidate_ReputationEvalIntervalPositive(t *testing.T) {
	c := validConfig(t)
	c.Reputation.EvaluationIntervalS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.evaluation_interval_s")
}

func TestValidate_ReputationSignalWindowOrdering_ShortGEMedium(t *testing.T) {
	c := validConfig(t)
	c.Reputation.SignalWindows.ShortS = 100000 // > medium

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "reputation.signal_windows" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_ReputationZScoreThresholdPositive(t *testing.T) {
	c := validConfig(t)
	c.Reputation.ZScoreThreshold = 0

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.z_score_threshold")
}

func TestValidate_ReputationDefaultScoreRange(t *testing.T) {
	c := validConfig(t)
	c.Reputation.DefaultScore = 1.5

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.default_score")
}

func TestValidate_ReputationFreezeListCacheTTLPositive(t *testing.T) {
	c := validConfig(t)
	c.Reputation.FreezeListCacheTTLS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.freeze_list_cache_ttl_s")
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — federation/reputation rules not present.

- [ ] **Step 3: Extend `validate.go`**

Modify `Validate` to call the new methods:
```go
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkLogLevel(c)
	v.checkListeners(c)
	v.checkLedger(c)
	v.checkBroker(c)
	v.checkSettlement(c)
	v.checkFederation(c)
	v.checkReputation(c)
	return v.toError()
}
```

Append to `validate.go`:
```go
// §6.6
func (v *validator) checkFederation(c *Config) {
	if c.Federation.PeerCountMin < 1 {
		v.add("federation.peer_count_min", "must be >= 1")
	}
	if c.Federation.PeerCountMax < c.Federation.PeerCountMin {
		v.add("federation.peer_count_max", "must be >= peer_count_min")
	}
	if c.Federation.GossipDedupeTTLS <= 0 {
		v.add("federation.gossip_dedupe_ttl_s", "must be > 0")
	}
	if c.Federation.TransferRetryWindowH <= 0 {
		v.add("federation.transfer_retry_window_hours", "must be > 0")
	}
	if c.Federation.EnrollRatePerMinPerIP < 1 {
		v.add("federation.enroll_rate_per_min_per_ip", "must be >= 1")
	}
}

// §6.7
func (v *validator) checkReputation(c *Config) {
	if c.Reputation.EvaluationIntervalS <= 0 {
		v.add("reputation.evaluation_interval_s", "must be > 0")
	}
	w := c.Reputation.SignalWindows
	if !(w.ShortS < w.MediumS && w.MediumS < w.LongS) {
		v.add("reputation.signal_windows",
			"must have short_s < medium_s < long_s")
	}
	if c.Reputation.ZScoreThreshold <= 0 {
		v.add("reputation.z_score_threshold", "must be > 0")
	}
	if c.Reputation.DefaultScore < 0 || c.Reputation.DefaultScore > 1 {
		v.add("reputation.default_score", "must be in [0.0, 1.0]")
	}
	if c.Reputation.FreezeListCacheTTLS <= 0 {
		v.add("reputation.freeze_list_cache_ttl_s", "must be > 0")
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/config/validate.go tracker/internal/config/validate_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): Validate federation + reputation

Spec §6.6–§6.7. Peer-count ordering and signal-window monotonicity are
the load-bearing cross-field rules; the rest are range bounds.
EOF
)"
```

---

## Task 12: `Validate` — admission (incl. filesystem check) and `Load` wiring

**Files:**
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/validate_test.go`
- Modify: `tracker/internal/config/load.go`
- Modify: `tracker/internal/config/load_test.go`

- [ ] **Step 1: Append the failing admission tests**

Append to `tracker/internal/config/validate_test.go`:
```go
func TestValidate_AdmissionPressureOrdering(t *testing.T) {
	c := validConfig(t)
	c.Admission.PressureAdmitThreshold = 1.5
	c.Admission.PressureRejectThreshold = 1.0

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.pressure_admit_threshold" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionScoreWeightsSum(t *testing.T) {
	c := validConfig(t)
	c.Admission.ScoreWeights.Tenure = 0.3 // sum becomes 1.1

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.score_weights" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionAttestationTTLOrdering(t *testing.T) {
	c := validConfig(t)
	c.Admission.AttestationTTLSeconds = 100000
	c.Admission.AttestationMaxTTLSeconds = 50000

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.attestation_max_ttl_seconds" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionTrialTierScoreRange(t *testing.T) {
	c := validConfig(t)
	c.Admission.TrialTierScore = 1.5

	err := Validate(c)

	assertOneFieldError(t, err, "admission.trial_tier_score")
}

func TestValidate_AdmissionMaxImportedScoreRange(t *testing.T) {
	c := validConfig(t)
	c.Admission.MaxAttestationScoreImported = 2.0

	err := Validate(c)

	assertOneFieldError(t, err, "admission.max_attestation_score_imported")
}

func TestValidate_AdmissionTLogParentExists(t *testing.T) {
	tmp := t.TempDir()
	c := validConfig(t)
	c.Admission.TLogPath = tmp + "/sub/admission.tlog" // /sub does not exist

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.tlog_path" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionTLogParentExistsHappy(t *testing.T) {
	tmp := t.TempDir()
	c := validConfig(t)
	c.Admission.TLogPath = tmp + "/admission.tlog" // tmp exists

	err := Validate(c)

	assert.NoError(t, err)
}

func TestValidate_AdmissionEmptyBlocklistEntry(t *testing.T) {
	c := validConfig(t)
	c.Admission.AttestationPeerBlocklist = []string{"peer-a", "", "peer-c"}

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.attestation_peer_blocklist[1]" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionFsyncBatchWindowAllowsZero(t *testing.T) {
	// Spec §6.8 says fsync_batch_window_ms ≥ 0 (zero = synchronous fsync).
	c := validConfig(t)
	c.Admission.FsyncBatchWindowMs = 0

	err := Validate(c)

	assert.NoError(t, err)
}

func TestValidate_AdmissionStunTurnRelayKbps(t *testing.T) {
	c := validConfig(t)
	c.STUNTURN.TURNRelayMaxKbps = 0

	err := Validate(c)

	assertOneFieldError(t, err, "stun_turn.turn_relay_max_kbps")
}
```

- [ ] **Step 2: Append `Load`-wiring tests**

Append to `tracker/internal/config/load_test.go`:
```go
func TestLoad_FullYAMLPassesValidation(t *testing.T) {
	c, err := Load("testdata/full.yaml")

	require.NoError(t, err)
	require.NotNil(t, c)
	// ApplyDefaults expanded data_dir-relative paths:
	assert.Equal(t, "/var/lib/token-bay/admission.tlog", c.Admission.TLogPath)
	assert.Equal(t, "/var/lib/token-bay/admission.snapshot", c.Admission.SnapshotPathPrefix)
}

// Wait — full.yaml's tlog parent /var/lib/token-bay may not exist on CI.
// Override below uses tmp dirs.
func TestLoad_MinimalFailsValidationOnMissingTLogParent(t *testing.T) {
	c, err := Load("testdata/minimal.yaml")

	// minimal.yaml's data_dir is /var/lib/token-bay; if that parent is
	// missing on this host we should see a tlog_path FieldError. Either
	// way, the test asserts Load returns either nil-error (parent exists)
	// or a *ValidationError naming admission.tlog_path.
	if err == nil {
		require.NotNil(t, c)
		return
	}
	var ve *ValidationError
	require.True(t, errors.As(err, &ve), "got %v", err)
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.tlog_path" {
			matched = true
		}
	}
	assert.True(t, matched, "expected admission.tlog_path error: %v", ve.Errors)
}
```

The first appended test relies on `/var/lib/token-bay` existing. To make this test deterministic, we rewrite it to use a tempdir-based YAML. Replace the just-appended `TestLoad_FullYAMLPassesValidation` with:

```go
func TestLoad_TempdirYAMLPassesValidation(t *testing.T) {
	tmp := t.TempDir()
	yaml := "data_dir: " + tmp + "\n" +
		"server:\n" +
		"  listen_addr: \"0.0.0.0:7777\"\n" +
		"  identity_key_path: " + tmp + "/identity.key\n" +
		"  tls_cert_path: " + tmp + "/cert.pem\n" +
		"  tls_key_path: " + tmp + "/cert.key\n" +
		"ledger:\n" +
		"  storage_path: " + tmp + "/ledger.sqlite\n"
	path := tmp + "/tracker.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	c, err := Load(path)

	require.NoError(t, err, "expected clean Load against tmp dir")
	require.NotNil(t, c)
	assert.Equal(t, tmp+"/admission.tlog", c.Admission.TLogPath)
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/config/...
```
Expected: FAIL — admission rules not present; Load doesn't yet call ApplyDefaults+Validate.

- [ ] **Step 4: Add admission + stun_turn checks to `validate.go`**

Modify `Validate` to call the new methods:
```go
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkLogLevel(c)
	v.checkListeners(c)
	v.checkLedger(c)
	v.checkBroker(c)
	v.checkSettlement(c)
	v.checkFederation(c)
	v.checkReputation(c)
	v.checkAdmission(c)
	v.checkSTUNTURN(c)
	return v.toError()
}
```

Append to `validate.go`:
```go
import (
	// (existing imports)
	"os"
)

// §6.8
func (v *validator) checkAdmission(c *Config) {
	a := c.Admission

	if a.PressureAdmitThreshold <= 0 {
		v.add("admission.pressure_admit_threshold", "must be > 0")
	}
	if a.PressureRejectThreshold <= 0 {
		v.add("admission.pressure_reject_threshold", "must be > 0")
	}
	if a.PressureAdmitThreshold > 0 && a.PressureRejectThreshold > 0 &&
		a.PressureAdmitThreshold >= a.PressureRejectThreshold {
		v.add("admission.pressure_admit_threshold",
			"must be < pressure_reject_threshold")
	}

	if a.QueueCap < 1 {
		v.add("admission.queue_cap", "must be >= 1")
	}
	if a.TrialTierScore < 0 || a.TrialTierScore > 1 {
		v.add("admission.trial_tier_score", "must be in [0.0, 1.0]")
	}
	if a.AgingAlphaPerMinute < 0 {
		v.add("admission.aging_alpha_per_minute", "must be >= 0")
	}
	if a.QueueTimeoutS <= 0 {
		v.add("admission.queue_timeout_s", "must be > 0")
	}

	w := a.ScoreWeights
	checkNonNeg := func(field string, val float64) {
		if val < 0 {
			v.add(field, "must be >= 0")
		}
	}
	checkNonNeg("admission.score_weights.settlement_reliability", w.SettlementReliability)
	checkNonNeg("admission.score_weights.inverse_dispute_rate", w.InverseDisputeRate)
	checkNonNeg("admission.score_weights.tenure", w.Tenure)
	checkNonNeg("admission.score_weights.net_credit_flow", w.NetCreditFlow)
	checkNonNeg("admission.score_weights.balance_cushion", w.BalanceCushion)
	sum := w.SettlementReliability + w.InverseDisputeRate + w.Tenure + w.NetCreditFlow + w.BalanceCushion
	if !approxEqual(sum, 1.0, 1e-3) {
		v.add("admission.score_weights",
			"weights must sum to 1.0 ± 0.001 (got "+ftoa(sum)+")")
	}

	if a.NetFlowNormalizationConstant <= 0 {
		v.add("admission.net_flow_normalization_constant", "must be > 0")
	}
	if a.TenureCapDays < 1 {
		v.add("admission.tenure_cap_days", "must be >= 1")
	}
	if a.StarterGrantCredits < 1 {
		v.add("admission.starter_grant_credits", "must be >= 1")
	}
	if a.RollingWindowDays < 1 {
		v.add("admission.rolling_window_days", "must be >= 1")
	}
	if a.TrialSettlementsRequired < 0 {
		v.add("admission.trial_settlements_required", "must be >= 0")
	}
	if a.TrialDurationHours < 0 {
		v.add("admission.trial_duration_hours", "must be >= 0")
	}
	if a.AttestationTTLSeconds <= 0 {
		v.add("admission.attestation_ttl_seconds", "must be > 0")
	}
	if a.AttestationMaxTTLSeconds < a.AttestationTTLSeconds {
		v.add("admission.attestation_max_ttl_seconds",
			"must be >= attestation_ttl_seconds")
	}
	if a.AttestationIssuancePerConsumerPerHour < 1 {
		v.add("admission.attestation_issuance_per_consumer_per_hour", "must be >= 1")
	}
	if a.MaxAttestationScoreImported < 0 || a.MaxAttestationScoreImported > 1 {
		v.add("admission.max_attestation_score_imported", "must be in [0.0, 1.0]")
	}

	if a.TLogPath == "" {
		v.add("admission.tlog_path", "must be non-empty")
	} else {
		if dir := filepath.Dir(a.TLogPath); dir != "" {
			if _, err := os.Stat(dir); err != nil {
				v.add("admission.tlog_path",
					"parent directory does not exist: "+dir)
			}
		}
	}
	if a.SnapshotPathPrefix == "" {
		v.add("admission.snapshot_path_prefix", "must be non-empty")
	}
	if a.SnapshotIntervalS <= 0 {
		v.add("admission.snapshot_interval_s", "must be > 0")
	}
	if a.SnapshotsRetained < 1 {
		v.add("admission.snapshots_retained", "must be >= 1")
	}
	if a.FsyncBatchWindowMs < 0 {
		v.add("admission.fsync_batch_window_ms", "must be >= 0")
	}
	if a.HeartbeatWindowMinutes < 1 {
		v.add("admission.heartbeat_window_minutes", "must be >= 1")
	}
	if a.HeartbeatFreshnessDecayMaxS <= 0 {
		v.add("admission.heartbeat_freshness_decay_max_s", "must be > 0")
	}
	for i, peer := range a.AttestationPeerBlocklist {
		if peer == "" {
			v.add("admission.attestation_peer_blocklist["+strconv.Itoa(i)+"]",
				"must be non-empty")
		}
	}
}

// §6.9 (listener parse + collision already handled in §6.2)
func (v *validator) checkSTUNTURN(c *Config) {
	if c.STUNTURN.TURNRelayMaxKbps <= 0 {
		v.add("stun_turn.turn_relay_max_kbps", "must be > 0")
	}
}
```

- [ ] **Step 5: Wire `Load` to call `ApplyDefaults` and `Validate`**

Modify `tracker/internal/config/load.go` — replace `Load` body:
```go
// Load reads the YAML config file at path, applies defaults, and validates.
// Returns a typed *ParseError on parse failure or *ValidationError on
// invariant failure (both errors.As-comparable).
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c, err := Parse(f)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			pe.Path = path
		}
		return nil, err
	}
	ApplyDefaults(c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	return c, nil
}
```

- [ ] **Step 6: Run tests to verify they pass**

```bash
go test ./internal/config/...
```
Expected: PASS — including the new Load wiring tests.

- [ ] **Step 7: Run with race detector**

```bash
go test -race ./internal/config/...
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add tracker/internal/config/validate.go tracker/internal/config/validate_test.go tracker/internal/config/load.go tracker/internal/config/load_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/config): Validate admission + stun_turn; wire Load pipeline

Spec §6.8–§6.9. Pressure-threshold ordering, score-weights sum, TTL
ordering, and the tlog-parent-exists filesystem check are the operator-
critical guards from admission spec §9.3. Load now runs the full
pipeline: Parse → ApplyDefaults → Validate.
EOF
)"
```

---

## Task 13: Coverage check on the validation matrix

**Files:**
- None (verification only)

- [ ] **Step 1: Run with coverage**

```bash
go test -race -coverprofile=coverage.out ./internal/config/...
```

- [ ] **Step 2: Check coverage**

```bash
go tool cover -func=coverage.out | tail -5
```

Expected: total ≥ 90% on `tracker/internal/config`. If below, identify the uncovered lines via:
```bash
go tool cover -func=coverage.out | grep -v '100.0%' | head -20
```
and add targeted tests in the appropriate `*_test.go` file. Common gaps after the previous tasks: `*ParseError` paths in `Load` for permission-denied (already covered by `TestLoad_PermissionDeniedReturnsError` if running as non-root), edge-cases in `ApplyDefaults` when only a single field of a section is zero.

- [ ] **Step 3: Commit any gap-filling tests**

If any new tests were added:
```bash
git add tracker/internal/config/
git commit -m "$(cat <<'EOF'
test(tracker/config): close coverage gap to 90%+

Added [describe gap] coverage. Validation, parse, apply-defaults,
and Load file-error paths all now exercised.
EOF
)"
```

If coverage was already ≥ 90% with no changes, skip this commit.

---

## Task 14: Invalid testdata fixtures + integration tests through `Load`

**Files:**
- Create: `tracker/internal/config/testdata/invalid_log_level.yaml`
- Create: `tracker/internal/config/testdata/invalid_listener_collision.yaml`
- Create: `tracker/internal/config/testdata/invalid_signal_windows.yaml`
- Create: `tracker/internal/config/testdata/invalid_score_weights_sum.yaml`
- Create: `tracker/internal/config/testdata/invalid_pressure_inversion.yaml`
- Create: `tracker/internal/config/testdata/invalid_attestation_ttl.yaml`
- Modify: `tracker/internal/config/load_test.go`

These fixtures back the CLI subcommand tests in Task 15 and provide an end-to-end check that Load surfaces the right `FieldError` for each.

The fixtures share a common base. The pattern: write a base block, then override the field that should be invalid.

- [ ] **Step 1: Write the six fixture files**

Each starts from `minimal.yaml`. To fail on the tlog_path FS check, we set the `data_dir` to a path whose parent likely exists (`/tmp`) on Linux/macOS so only the *targeted* invariant fires.

`tracker/internal/config/testdata/invalid_log_level.yaml`:
```yaml
data_dir: /tmp
log_level: chatty
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /tmp/ledger.sqlite
```

`tracker/internal/config/testdata/invalid_listener_collision.yaml`:
```yaml
data_dir: /tmp
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
admin:
  listen_addr: "0.0.0.0:7777"
ledger:
  storage_path: /tmp/ledger.sqlite
```

`tracker/internal/config/testdata/invalid_signal_windows.yaml`:
```yaml
data_dir: /tmp
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /tmp/ledger.sqlite
reputation:
  signal_windows:
    short_s: 1000000
    medium_s: 86400
    long_s: 604800
```

`tracker/internal/config/testdata/invalid_score_weights_sum.yaml`:
```yaml
data_dir: /tmp
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /tmp/ledger.sqlite
admission:
  score_weights:
    settlement_reliability: 0.30
    inverse_dispute_rate: 0.10
    tenure: 0.20
    net_credit_flow: 0.30
    balance_cushion: 0.20    # sum = 1.10
```

`tracker/internal/config/testdata/invalid_pressure_inversion.yaml`:
```yaml
data_dir: /tmp
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /tmp/ledger.sqlite
admission:
  pressure_admit_threshold: 1.5
  pressure_reject_threshold: 0.85
```

`tracker/internal/config/testdata/invalid_attestation_ttl.yaml`:
```yaml
data_dir: /tmp
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
ledger:
  storage_path: /tmp/ledger.sqlite
admission:
  attestation_ttl_seconds: 600000
  attestation_max_ttl_seconds: 300000
```

- [ ] **Step 2: Append the integration tests**

Append to `tracker/internal/config/load_test.go`:
```go
func TestLoad_InvalidFixtures_SurfaceExpectedFieldErrors(t *testing.T) {
	cases := []struct {
		fixture string
		field   string
	}{
		{"testdata/invalid_log_level.yaml", "log_level"},
		{"testdata/invalid_listener_collision.yaml", "admin.listen_addr"},
		{"testdata/invalid_signal_windows.yaml", "reputation.signal_windows"},
		{"testdata/invalid_score_weights_sum.yaml", "admission.score_weights"},
		{"testdata/invalid_pressure_inversion.yaml", "admission.pressure_admit_threshold"},
		{"testdata/invalid_attestation_ttl.yaml", "admission.attestation_max_ttl_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c, err := Load(tc.fixture)

			require.Error(t, err)
			assert.Nil(t, c)
			var ve *ValidationError
			require.True(t, errors.As(err, &ve), "got %T: %v", err, err)
			matched := false
			for _, fe := range ve.Errors {
				if fe.Field == tc.field {
					matched = true
				}
			}
			assert.Truef(t, matched, "expected %s in errors %v", tc.field, ve.Errors)
		})
	}
}
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/config/...
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/config/testdata/invalid_*.yaml tracker/internal/config/load_test.go
git commit -m "$(cat <<'EOF'
test(tracker/config): invalid YAML fixtures + Load integration matrix

Six fixtures, one per non-trivial invariant. Each round-trips through
Load and asserts the expected FieldError surfaces. Backs the CLI
subcommand tests in the next commit.
EOF
)"
```

---

## Task 15: CLI subcommand `config validate`

**Files:**
- Create: `tracker/cmd/token-bay-tracker/config_cmd.go`
- Create: `tracker/cmd/token-bay-tracker/config_cmd_test.go`

This task creates the cobra subcommand and its tests. Wiring into `newRootCmd()` happens in Task 16.

- [ ] **Step 1: Write the failing tests**

Write to `tracker/cmd/token-bay-tracker/config_cmd_test.go`:
```go
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runConfigValidate(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := newConfigCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"validate"}, args...))

	exit := -1
	exitFn := func(c int) { exit = c }
	cmd.SetContext(withExitFunc(t.Context(), exitFn))
	_ = cmd.Execute()
	return outBuf.String(), errBuf.String(), exit
}

func TestConfigValidate_FullYAMLExitsZero(t *testing.T) {
	// full.yaml's data_dir is /var/lib/token-bay; rewrite to a tempdir
	// so the tlog parent-exists check passes deterministically.
	tmp := t.TempDir()
	yaml := "data_dir: " + tmp + "\n" +
		"server:\n" +
		"  listen_addr: \"0.0.0.0:7777\"\n" +
		"  identity_key_path: " + tmp + "/identity.key\n" +
		"  tls_cert_path: " + tmp + "/cert.pem\n" +
		"  tls_key_path: " + tmp + "/cert.key\n" +
		"ledger:\n" +
		"  storage_path: " + tmp + "/ledger.sqlite\n"
	path := tmp + "/tracker.yaml"
	require.NoError(t, writeFile(t, path, yaml))

	stdout, stderr, code := runConfigValidate(t, "--config", path)

	assert.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "OK")
	assert.Contains(t, stdout, path)
}

func TestConfigValidate_MissingFileExits1(t *testing.T) {
	_, stderr, code := runConfigValidate(t, "--config", "/nonexistent/path.yaml")

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
}

func TestConfigValidate_UnknownFieldExits2(t *testing.T) {
	_, stderr, code := runConfigValidate(t, "--config", "../../internal/config/testdata/unknown_field.yaml")

	assert.Equal(t, 2, code)
	assert.Contains(t, strings.ToLower(stderr), "parse")
}

func TestConfigValidate_InvalidExits3(t *testing.T) {
	_, stderr, code := runConfigValidate(t, "--config", "../../internal/config/testdata/invalid_score_weights_sum.yaml")

	assert.Equal(t, 3, code)
	assert.Contains(t, stderr, "admission.score_weights")
	assert.Contains(t, strings.ToLower(stderr), "validation")
}

func TestConfigValidate_NoConfigFlagExits1(t *testing.T) {
	_, stderr, code := runConfigValidate(t)

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
}

// writeFile is a tiny test helper: os.WriteFile via Read so we don't
// import os in the test directly.
func writeFile(t *testing.T, path, body string) error {
	t.Helper()
	return writeFileImpl(path, body)
}
```

`writeFileImpl` and the context-based exit hook need to live in the test file too — they're test-only plumbing:

```go
// (Append to config_cmd_test.go)
import (
	"context"
	"os"
)

type exitFuncKey struct{}

func withExitFunc(parent context.Context, fn func(int)) context.Context {
	return context.WithValue(parent, exitFuncKey{}, fn)
}

func writeFileImpl(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
```

(Move all `import` blocks to a single grouped block at the top of the file when you write it; the snippets above are split for readability.)

- [ ] **Step 2: Run tests to verify they fail**

From `tracker/`:
```bash
go test ./cmd/token-bay-tracker/...
```
Expected: FAIL — `undefined: newConfigCmd`, `undefined: withExitFunc`.

- [ ] **Step 3: Write `config_cmd.go`**

Write to `tracker/cmd/token-bay-tracker/config_cmd.go`:
```go
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// newConfigCmd returns the `config` subcommand tree. Today only `validate`
// is exposed; future subcommands (`show`, `defaults`) belong here.
func newConfigCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate tracker configuration",
	}
	root.AddCommand(newConfigValidateCmd())
	return root
}

func newConfigValidateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate a tracker config file; exit non-zero on failure",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "error: --config is required")
				exitWithCode(cmd.Context(), 1)
				return nil
			}
			c, err := config.Load(configPath)
			if err != nil {
				return reportConfigError(cmd, err, configPath)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "OK: %s (%d sections)\n", configPath, sectionCount(c))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to tracker.yaml (required)")
	return cmd
}

// reportConfigError dispatches a Load error to the right exit code per the
// design spec §3.3:
//   - *ParseError  → 2
//   - *ValidationError → 3
//   - other (file missing, permission, …) → 1
func reportConfigError(cmd *cobra.Command, err error, path string) error {
	stderr := cmd.ErrOrStderr()
	var pe *config.ParseError
	if errors.As(err, &pe) {
		fmt.Fprintf(stderr, "parse error: %s\n", pe.Error())
		exitWithCode(cmd.Context(), 2)
		return nil
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		fmt.Fprintln(stderr, "validation failed:")
		for _, fe := range ve.Errors {
			fmt.Fprintf(stderr, "  - %s\n", fe.String())
		}
		exitWithCode(cmd.Context(), 3)
		return nil
	}
	fmt.Fprintln(stderr, err.Error())
	exitWithCode(cmd.Context(), 1)
	return nil
}

// sectionCount returns the number of top-level Config sections. Cheap helper
// for the success line; counts non-string top-level fields.
func sectionCount(_ *config.Config) int {
	// Server, Admin, Ledger, Broker, Settlement, Federation, Reputation,
	// Admission, STUNTURN, Metrics — ten composite sections, plus
	// data_dir + log_level scalars not counted.
	return 10
}

// exitWithCode looks up the exit hook on cmd.Context() and calls it. In
// tests the hook is set by withExitFunc so the test captures the code.
// In production the root command sets it to os.Exit (Task 16 wires this).
func exitWithCode(ctx context.Context, code int) {
	if ctx == nil {
		return
	}
	v := ctx.Value(exitFuncKey{})
	if fn, ok := v.(func(int)); ok {
		fn(code)
	}
}
```

Note: `exitFuncKey{}` is referenced from both `config_cmd.go` and `config_cmd_test.go`. Move it to `config_cmd.go` so both compile against the same type. Then `config_cmd_test.go`'s `withExitFunc` uses the type from `config_cmd.go`.

Adjust the test file to drop the duplicate `exitFuncKey` declaration:
```go
// (config_cmd_test.go top of file)
package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withExitFunc(parent context.Context, fn func(int)) context.Context {
	return context.WithValue(parent, exitFuncKey{}, fn)
}

// (rest of tests from Step 1, but without re-declaring exitFuncKey)
```

- [ ] **Step 4: Run tests to verify they pass**

From `tracker/`:
```bash
go test ./cmd/token-bay-tracker/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/cmd/token-bay-tracker/config_cmd.go tracker/cmd/token-bay-tracker/config_cmd_test.go
git commit -m "$(cat <<'EOF'
feat(tracker): config validate subcommand with exit-code matrix

Exit codes per design spec §3.3: 0 success, 1 file/other, 2 parse error,
3 validation. Wiring into the root command lands in the next commit.
EOF
)"
```

---

## Task 16: Wire `config` into the root command

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/main.go`
- Modify: `tracker/cmd/token-bay-tracker/main_test.go`

- [ ] **Step 1: Append the failing test to `main_test.go`**

Append to `tracker/cmd/token-bay-tracker/main_test.go`:
```go
func TestRootCmd_ConfigSubcommandRegistered(t *testing.T) {
	cmd := newRootCmd()

	found := false
	for _, sub := range cmd.Commands() {
		if sub.Name() == "config" {
			found = true
			// 'validate' must be a child of config:
			haveValidate := false
			for _, sub2 := range sub.Commands() {
				if sub2.Name() == "validate" {
					haveValidate = true
				}
			}
			assert.True(t, haveValidate, "expected 'validate' under 'config'")
		}
	}
	assert.True(t, found, "expected 'config' subcommand registered")
}
```

- [ ] **Step 2: Run tests to verify failure**

From `tracker/`:
```bash
go test ./cmd/token-bay-tracker/...
```
Expected: FAIL — `config` not yet registered.

- [ ] **Step 3: Modify `main.go`**

In `tracker/cmd/token-bay-tracker/main.go`, modify `newRootCmd()` to register the config subcommand and to install the production exit hook:
```go
import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "token-bay-tracker",
		Short: "Token-Bay regional tracker server",
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tracker version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "token-bay-tracker %s\n", version)
		},
	}
}

func main() {
	root := newRootCmd()
	ctx := withExitFunc(context.Background(), os.Exit)
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

From `tracker/`:
```bash
go test ./cmd/token-bay-tracker/...
```
Expected: PASS.

- [ ] **Step 5: Smoke-test the binary**

```bash
go build -o bin/token-bay-tracker ./cmd/token-bay-tracker
./bin/token-bay-tracker config validate --help
./bin/token-bay-tracker config validate --config internal/config/testdata/full.yaml || echo "non-zero exit (expected if /var/lib/token-bay missing)"
```
Expected: the `--help` output describes the subcommand. The validate run on `full.yaml` may exit non-zero on machines without `/var/lib/token-bay` — the next smoke creates a tempdir version:

```bash
TMP=$(mktemp -d)
cat > "$TMP/tracker.yaml" <<EOF
data_dir: $TMP
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: $TMP/identity.key
  tls_cert_path: $TMP/cert.pem
  tls_key_path: $TMP/cert.key
ledger:
  storage_path: $TMP/ledger.sqlite
EOF
./bin/token-bay-tracker config validate --config "$TMP/tracker.yaml"
```
Expected: `OK: <path> (10 sections)` and exit 0.

- [ ] **Step 6: Commit**

```bash
git add tracker/cmd/token-bay-tracker/main.go tracker/cmd/token-bay-tracker/main_test.go
git commit -m "$(cat <<'EOF'
feat(tracker): wire config subcommand into root command

Production wiring: root command installs os.Exit as the exit hook on
its context; the validate subcommand reads it from cmd.Context() to
emit exit codes 0/1/2/3 per design spec §3.3.
EOF
)"
```

---

## Task 17: `tracker/internal/config/CLAUDE.md`

**Files:**
- Create: `tracker/internal/config/CLAUDE.md`

- [ ] **Step 1: Write the file**

Write to `tracker/internal/config/CLAUDE.md`:
```markdown
# tracker/internal/config — Development Context

## What this is

YAML config loader for `token-bay-tracker`. Leaf module: depends only on the standard library and `gopkg.in/yaml.v3`. Every other tracker subsystem reads its section from the `*Config` returned by `Load`. See the design spec at `docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md`.

Public API (do not break without coordinating callers):

| Function | Purpose |
|---|---|
| `Load(path) (*Config, error)` | Parse + ApplyDefaults + Validate |
| `Parse(io.Reader) (*Config, error)` | YAML decode only; rejects unknown fields |
| `ApplyDefaults(*Config)` | Idempotent default-fill + `${data_dir}` expansion |
| `Validate(*Config) error` | Aggregating invariant check; returns `*ValidationError` |
| `DefaultConfig() *Config` | Defaults only; required fields zero-valued |

## Source layout

| File | Purpose |
|---|---|
| `config.go` | `Config` + per-section types + `DefaultConfig` |
| `load.go` | `Parse(io.Reader)` and `Load(path)` |
| `apply_defaults.go` | `ApplyDefaults` — idempotent; `${data_dir}` expansion |
| `validate.go` | `Validate` — accumulates every `FieldError` |
| `errors.go` | `ParseError`, `ValidationError`, `FieldError` |

One test file per source file. Fixtures in `testdata/`.

## Non-negotiable rules

1. **YAML is the only source.** No env-var overrides, no flag binding. Adding either changes precedence semantics — discuss in an issue first.
2. **Reject unknown fields.** `yaml.Decoder.KnownFields(true)` is non-negotiable. Typos must fail loudly.
3. **`Validate` accumulates, never fails fast.** Operators must see every problem in one run.
4. **`DefaultConfig` does not populate required fields.** Required fields stay zero-valued; `Validate` flags them. Defaults are for *optional* fields only.
5. **Cross-section invariants live in `validate.go`.** Do not enforce them at parse time or `ApplyDefaults` time — single chokepoint.
6. **No reflection-driven validation.** Explicit code is auditable; reflection rules drift over time.

## Adding a new section

When a feature plan introduces a new `internal/<module>`:

1. Add a section type (e.g., `BrokerConfig`) to `config.go` alongside the existing types.
2. Add the field to the top-level `Config` struct with a `yaml` tag.
3. Populate spec defaults in `DefaultConfig()`.
4. Add the section's invariants to `Validate()` via a new `check<Section>` method called from the top-level dispatcher. One `FieldError` per invariant.
5. Add per-field default-fill stanzas to `ApplyDefaults`.
6. Update `testdata/full.yaml` with explicit values for every new field. The byte-stable round-trip will fail if you forgot any.
7. Add at least one `testdata/invalid_<section>_<rule>.yaml` per nontrivial invariant.

## Adding a required field

Required = zero value is invalid (e.g., a path with no sane default).

1. Do **NOT** populate in `DefaultConfig()` — leave it zero.
2. Add a "required, non-empty" rule to `Validate()`.
3. Add to `testdata/minimal.yaml` so the minimal happy path keeps loading.
4. Add a Go-only test that mutates `validConfig` (in `validate_test.go`) to clear the field.

## Adding a CLI subcommand

Subcommands live in `tracker/cmd/token-bay-tracker/`, not here. This package exposes the API; the CLI consumes it.

## Defaults that depend on other config values

Today only `admission.tlog_path` and `admission.snapshot_path_prefix` derive from `data_dir`. The pattern:

1. Default value is empty string in `DefaultConfig()`.
2. `ApplyDefaults()` fills it from `c.DataDir` when empty.
3. `Validate()` runs after `ApplyDefaults()`, so it sees the resolved path.

If you need another derived default, follow this exact pattern. Do not introduce a `${var}` template engine; the substitution is one line of Go.

## Things that look surprising and aren't bugs

- `Parse` does NOT call `ApplyDefaults`. Tests that want to assert "the YAML had X" use `Parse` alone; tests that want "what the runtime sees" use `Load`.
- The validation order in `validate.go` matches the order of fields in `Config`. Don't reorder unless you also reorder the test cases — diff readability matters.
- `Validate` touches the filesystem exactly once (the `tlog_path` parent-dir existence check). Tests for this rule must use `t.TempDir()`; never a hardcoded path.
- `BrokerScoreWeights` / `AdmissionScoreWeights` blocks default-fill *only* when every weight is zero. Partial operator input is left alone so `Validate`'s sum check fires loudly.
- The `config validate` CLI in `cmd/token-bay-tracker/` reads `os.Exit` from `cmd.Context()` so tests can intercept the exit code. Don't call `os.Exit` directly from inside command handlers.
```

- [ ] **Step 2: Commit**

```bash
git add tracker/internal/config/CLAUDE.md
git commit -m "$(cat <<'EOF'
docs(tracker/config): per-package CLAUDE.md with extension instructions

Documents the leaf-module rules, the section-extension and required-
field-extension recipes, and the surprises (Parse-vs-Load, FS check,
score-weights all-zero behaviour, exit-via-context).
EOF
)"
```

---

## Task 18: Final verification + tag

**Files:**
- None (verification only)

- [ ] **Step 1: Full test pass with race + coverage**

From `tracker/`:
```bash
go test -race -coverprofile=coverage.out ./...
```
Expected: PASS, no race warnings.

- [ ] **Step 2: Coverage check on internal/config**

```bash
go tool cover -func=coverage.out | grep 'internal/config' | tail -3
```
Expected: total coverage on the package ≥ 90%.

- [ ] **Step 3: Lint**

```bash
make lint
```
Expected: clean. If `golangci-lint` is not installed, install per repo CI guidance and rerun.

- [ ] **Step 4: Build the tracker binary**

```bash
make build
ls -la bin/token-bay-tracker
```

- [ ] **Step 5: Smoke the CLI end-to-end**

```bash
TMP=$(mktemp -d)
cat > "$TMP/tracker.yaml" <<EOF
data_dir: $TMP
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: $TMP/identity.key
  tls_cert_path: $TMP/cert.pem
  tls_key_path: $TMP/cert.key
ledger:
  storage_path: $TMP/ledger.sqlite
EOF
./bin/token-bay-tracker config validate --config "$TMP/tracker.yaml"
echo "exit=$?"

# Now break it and confirm exit code 3 + multi-line error output:
echo "log_level: chatty" >> "$TMP/tracker.yaml"
./bin/token-bay-tracker config validate --config "$TMP/tracker.yaml"
echo "exit=$?"
```
Expected: first run exits 0 with `OK: …`. Second run exits 3 with `validation failed:` + a `log_level` line on stderr.

- [ ] **Step 6: Run repo-root `make check`**

From repo root:
```bash
make check
```
Expected: shared, plugin, tracker — all green.

- [ ] **Step 7: Tag**

```bash
git tag -a tracker-internal-config-v0 -m "tracker/internal/config v0 — schema + Load + Validate + CLI"
```

- [ ] **Step 8: No commit needed**

Verification only — no source changes.

---

## Self-review

**Spec coverage:**
- §3.1 public API (`Load`, `Parse`, `ApplyDefaults`, `Validate`, `DefaultConfig`) — Tasks 2, 3, 4, 6, 7, 8, 12.
- §3.2 error types — Task 2.
- §3.3 CLI exit codes — Tasks 15, 16.
- §4.1 schema — Task 3.
- §4.2 required fields — Tasks 3, 9.
- §4.3 derived defaults — Task 8.
- §5 algorithms (Load / Parse / ApplyDefaults / Validate) — Tasks 6, 7, 8, 9–12.
- §6 validation rules — Tasks 9–12 (one section group per task).
- §7 source layout — exact paths in the file map.
- §8 testing — Tasks 2–14, plus coverage gate in Tasks 13 + 18.
- §9 failure handling — Task 7 (file errors), Task 12 (validation aggregation), Task 15 (CLI exit codes).
- §11 acceptance criteria — Task 18 step 5 walks the CLI end-to-end matrix; coverage gate covers AC 8.

**Placeholder scan:** Task 13 has a conditional ("if any new tests were added") — that's a verification fork, not a placeholder. No `TBD` / `TODO` / "implement later" remains.

**Type consistency:** `newConfigCmd` / `newConfigValidateCmd` are referenced in Tasks 15, 16. `exitFuncKey{}` declared in Task 15's `config_cmd.go` and consumed by both `config_cmd_test.go` and `main.go` (Task 16). `validConfig` helper defined in Task 9 and reused in Tasks 10–12. `assertOneFieldError` defined in Task 9 and reused in Tasks 10–12. `approxEqual` and `ftoa` defined in Task 10 and reused in Task 12.

**Cross-task dependencies:** Tasks 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10 → 11 → 12 → 13 → 14 → 15 → 16 → 17 → 18. Strict linear order; subagent-driven execution is straightforward.
