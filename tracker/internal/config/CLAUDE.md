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
- `ValidationError.Error()` produces a bare list of indented `FieldError` lines, no header. Callers that want a header (the CLI's `validation failed:` line) emit it themselves before iterating `Errors`.
