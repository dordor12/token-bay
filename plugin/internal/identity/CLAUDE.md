# plugin/internal/identity — Development Context

## What this is

The plugin's identity layer. Two pieces of state:

- `~/.token-bay/identity.key` — raw 32-byte Ed25519 seed, mode 0600.
- `~/.token-bay/identity.json` — cached binding record (IdentityID,
  account fingerprint, role bitmask, enrolled_at). JSON, mode 0600.

Plus one derivation: `account_fingerprint = SHA-256(orgId)` from
`claude auth status --json` (plugin spec §4.2).

Authoritative spec: `docs/superpowers/specs/plugin/2026-05-07-plugin-identity-design.md`.

## Non-negotiable rules

1. **No Anthropic credentials.** Plugin CLAUDE.md rule #1. The package
   never reads `~/.claude/credentials*`. The orgId arrives via a
   caller-filled `AuthSnapshot` — this package does not exec.
2. **Stdlib crypto only.** `crypto/ed25519` and `crypto/sha256` —
   nothing third-party. Repo-wide rule.
3. **Atomic writes always.** `SaveKey` and `SaveRecord` write to a
   temp file and `rename(2)`. A killed process never leaves a
   half-written key.
4. **One chokepoint per derivation.** `AccountFingerprint` is the only
   place the SHA-256(orgId) computation lives; `EnrollPreimage` is the
   only place the 99-byte canonical layout lives. Tests pin both with
   hex vectors.
5. **No goroutines.** The package is read-mostly; concurrent access
   to the same file is undefined. Operators call `Open` once at
   sidecar startup.

## Tech stack

- Go 1.25, stdlib only (plus `shared/ids` for the `IdentityID` type)
- Tests via `testify`
