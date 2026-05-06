# plugin/internal/config — Development Context

## What this is

YAML config loader for `~/.token-bay/config.yaml`. Leaf module: depends on the standard library and `gopkg.in/yaml.v3` only. Mirrors `tracker/internal/config` — same Load/Parse/ApplyDefaults/Validate split, same FieldError aggregation. Schema source is plugin spec §9; full design at `docs/superpowers/specs/plugin/2026-05-07-config-auditlog-design.md`.

Public API:

| Function | Purpose |
|---|---|
| `Load(path) (*Config, error)` | Parse + ApplyDefaults + Validate |
| `Parse(io.Reader) (*Config, error)` | YAML decode only; rejects unknown fields |
| `ApplyDefaults(*Config)` | Idempotent default-fill + `~` expansion |
| `Validate(*Config) error` | Aggregating invariant check; returns `*ValidationError` |
| `DefaultConfig() *Config` | Defaults only; required fields zero-valued |

## Source layout

| File | Purpose |
|---|---|
| `config.go` | `Config` + per-section types + `Duration` + `DefaultConfig` |
| `load.go` | `Parse(io.Reader)` and `Load(path)` |
| `apply_defaults.go` | `ApplyDefaults` — idempotent; `~` expansion |
| `validate.go` | `Validate` — accumulates every `FieldError` |
| `errors.go` | `ParseError`, `ValidationError`, `FieldError` |

One test file per source file. Fixtures in `testdata/`.

## Non-negotiable rules

1. **YAML is the only source.** No env-var overrides, no flag binding. Adding either changes precedence semantics — discuss in an issue first.
2. **Reject unknown fields.** `yaml.Decoder.KnownFields(true)` is non-negotiable. Typos must fail loudly.
3. **`Validate` accumulates, never fails fast.** Operators must see every problem in one run.
4. **`DefaultConfig` does not populate required fields.** `Role` and `Tracker` stay zero so `Validate` flags them when the operator forgets.
5. **Cross-section invariants live in `validate.go`.** Do not enforce them at parse time or `ApplyDefaults` time — single chokepoint.
6. **No reflection-driven validation or default-fill.** Explicit code is auditable.
7. **No Anthropic API key field.** Plugin CLAUDE.md rule #1 — the architecture eliminates the need entirely. If a contributor proposes one, that is a spec violation, not a config-package change.

## Adding a new field

1. Add it to the section struct in `config.go` with a `yaml:"..."` tag.
2. If it has a default, populate it in `DefaultConfig()`.
3. Add a default-fill stanza in `ApplyDefaults` (skip if it has no default).
4. Add validation rules in `Validate` via the matching `check<Section>` method.
5. Update `testdata/full.yaml` with an explicit value (so the round-trip test exercises the new field).
6. Add a focused test in `validate_test.go` that triggers the new rule.

## Adding a required field

Required = zero value is invalid (e.g., a path with no sane default).

1. Do **NOT** populate in `DefaultConfig()`.
2. Add a "required, non-empty" rule to `Validate` via `checkRequired`.
3. Add to `testdata/minimal.yaml` so the minimal happy path keeps loading.
4. Add a Go-only test that mutates `validConfig()` (in `validate_test.go`) to clear the field.

## Defaults that depend on other config values

Today only the `~` expansion in path fields counts. The pattern:

1. `DefaultConfig()` returns the literal `~/...` path.
2. `ApplyDefaults()` runs `expandTilde` after the zero-value default-fill.
3. `Validate()` runs after `ApplyDefaults()`, so it sees the resolved absolute path.

If you need another derived default, follow this exact pattern. Do not introduce a `${var}` template engine; the substitution is one line of Go.

## Things that look surprising and aren't bugs

- `Parse` does NOT call `ApplyDefaults`. Tests that want to assert "the YAML had X" use `Parse` alone; tests that want "what the runtime sees" use `Load`.
- `Duration` is a custom type because yaml.v3 doesn't natively unmarshal `time.Duration` from a `"15m"` string — only from a nanosecond integer.
- `Validate` does not touch the filesystem. Path existence is checked at use time by the auditlog opener and identity loader. A non-existent home dir at config-load time is fine on a fresh machine.
- `ValidationError.Error()` produces a bare list of indented `FieldError` lines — no header. Callers that want a header (a `validation failed:` line) emit it themselves before iterating `Errors`.
- `tracker == "auto"` is a sentinel that bypasses URL-scheme checks. Any other value must parse as a URL with `https`/`quic` scheme and a non-empty host.
- `idle_policy.window` is required only when `mode=scheduled`. With `always_on`, the window field is ignored.
