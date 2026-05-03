# shared — Development Context

## What this is

The shared Go library used by `plugin/` and `tracker/`. Everything in here is a contract: changes are breaking by default and require updating both consumers in the same PR.

## Non-negotiable rules

1. **No imports from `plugin/` or `tracker/`.** This is a leaf module.
2. **No state, no I/O.** Pure types + pure helper functions. Nothing that reads a file, opens a socket, or holds a mutable struct beyond the span of a function call.
3. **No third-party crypto.** Ed25519 uses stdlib. No libsodium, no OpenSSL.
4. **Canonical serialization is canonical.** If two parties serialize the same struct, the bytes must be byte-identical — that's what "canonical" means, and it's what signatures rely on. When canonical serialization helpers are added to `signing/`, tests must enforce this with round-trip + re-encode equality.
5. **Breaking change checklist.** Renaming or removing a field in `proto/`, `exhaustionproof/`, or `admission/` requires: update all callers in `plugin/` and `tracker/`; add a test ensuring the new format can be parsed; bump the module version comment in `go.mod`; note the change in the commit message with `!` (e.g., `refactor!: rename IdentityID → NodeID`).
6. **All signing/verifying of proto messages goes through `shared/signing.DeterministicMarshal`.** This is the single choke point for canonical bytes. Calling `proto.Marshal` directly on something that gets signed bypasses the determinism guarantee — which is precisely the bug Ed25519 verification surfaces in production rather than at code-review time.

## Tech stack

- Go stdlib
- `github.com/stretchr/testify` (tests only)
- `google.golang.org/protobuf` (wire-format types — single approved third-party runtime dependency)

No other runtime dependencies.

## What TDD looks like here

Every exported type has a test. Every helper function has a test. The tests usually follow one of three shapes:

1. **Construct → canonical-bytes → verify equality.** For any type that gets signed.
2. **Round-trip serialization.** `Marshal(x)` then `Unmarshal(bytes)` must produce `x` exactly.
3. **Rejection of tampered input.** Flip a bit, ensure the validator catches it.

Tests live in the same directory as the code, as `*_test.go` files.

## Commands

| Command | Effect |
|---|---|
| `make test` | `go test -race -coverprofile=coverage.out ./...` |
| `make lint` | `golangci-lint run ./...` |
| `make check` | test + lint |
