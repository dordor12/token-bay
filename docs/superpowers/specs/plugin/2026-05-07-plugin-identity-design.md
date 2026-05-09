# `plugin/internal/identity` — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Plugin design spec](2026-04-22-plugin-design.md) — particularly §4 (Enrollment & Identity). |
| Status | Design draft |
| Date | 2026-05-07 |
| Scope | Carve out the long-lived plugin identity primitives — Ed25519 keypair on disk, `claude auth status --json` → account fingerprint, the `token-bay-enroll:v1` preimage builder, and the cached identity-binding metadata file — into a focused Go package. The package is the production source of `trackerclient.Signer` and the single chokepoint for `account_fingerprint = SHA-256(orgId)`. |

## 1. Purpose

The plugin's identity is two pieces of state plus one derivation:

- **Keypair** at `~/.token-bay/identity.key` (mode 0600). Ed25519, stdlib only.
- **Binding metadata** at `~/.token-bay/identity.json` (mode 0600): the orgId-derived account fingerprint at last enrollment, the tracker-issued IdentityID, the role bitmask, and a UTC `enrolled_at` timestamp.
- **Account fingerprint** = `SHA-256(orgId)` derived from `claude auth status --json` (plugin spec §4.2 steps 3–4).

Today `plugin/internal/trackerclient/types.go:60-64` declares a `Signer` interface — its docstring says *"internal/identity provides the production implementation; tests use a fake"* — but no production package satisfies it. This package is that production implementation, plus the surrounding enrollment-payload and binding-record machinery.

## 2. Non-goals

- **Tracker enrollment RPC.** That stays in `trackerclient.Enroll`. This package builds the wire payload; trackerclient sends it.
- **Anthropic auth credentials.** Plugin CLAUDE.md rule #1 — never read, never store. The package never touches `~/.claude/credentials*`.
- **`claude` binary lifecycle.** The package consumes a small `AuthSnapshot` value — production wiring fills it from `internal/ccproxy.ClaudeAuthProber`. This package does not exec.
- **Mid-session redirect / `settings.json` mutation.** That's `internal/settingsjson` + `internal/ccproxy`. Identity is orthogonal.
- **OS keychain integration.** Plugin spec §4.3 marks it optional. v1 ships file-on-disk only.
- **Key rotation.** No rotation API in v1. The key file is replaced only via the explicit re-enrollment flow.

## 3. Public Go surface

```go
package identity

// Signer satisfies trackerclient.Signer. Holds an Ed25519 keypair and
// the IdentityID derived from SHA-256 of the 32-byte raw public key.
type Signer struct{ /* unexported */ }

// New wraps an existing keypair without disk I/O. Returns ErrInvalidKey
// on length mismatch.
func New(priv ed25519.PrivateKey) (*Signer, error)

// Generate produces a fresh keypair via crypto/rand.
func Generate() (*Signer, error)

func (s *Signer) Sign(msg []byte) ([]byte, error)
func (s *Signer) PrivateKey() ed25519.PrivateKey
func (s *Signer) PublicKey() ed25519.PublicKey
func (s *Signer) IdentityID() ids.IdentityID

// LoadKey reads path and parses the Ed25519 seed. Returns
// ErrKeyNotFound if path doesn't exist, ErrInvalidKey on size != 32.
func LoadKey(path string) (*Signer, error)

// SaveKey atomically writes the seed to path, mode 0600. Returns
// ErrKeyExists if path is already present — overwriting a key is
// destructive and is only allowed via an explicit Delete + SaveKey
// sequence in the re-enrollment flow.
func SaveKey(path string, s *Signer) error

// AuthSnapshot is the identity-relevant subset of `claude auth status --json`.
// Production callers map *ccproxy.AuthState onto this struct at the call
// site; tests construct it directly. Keeping the dependency one-way
// prevents an identity↔ccproxy import cycle.
type AuthSnapshot struct {
    LoggedIn    bool
    AuthMethod  string // "claude.ai" required
    APIProvider string // "firstParty" required
    OrgID       string // non-empty UUID required
}

// AccountFingerprint computes SHA-256(orgId) per plugin spec §4.2 step 4.
// Returns ErrIneligibleAuth when the snapshot fails the eligibility
// check (not logged in, wrong auth method, raw API key, empty orgId,
// non-firstParty provider).
func AccountFingerprint(s AuthSnapshot) ([32]byte, error)

// EnrollPreimagePrefix is the domain-separation tag prefixed to every
// enroll preimage. Bytes-stable across plugin versions.
const EnrollPreimagePrefix = "token-bay-enroll:v1"

// EnrollPreimage is the canonical bytes signed by the consumer per
// plugin spec §4.2:
//   "token-bay-enroll:v1" || nonce[16] || account_fingerprint[32] || identity_pubkey[32]
//
// Total length: 19 + 16 + 32 + 32 = 99 bytes.
func EnrollPreimage(nonce [16]byte, fingerprint [32]byte, pub ed25519.PublicKey) ([]byte, error)

// EnrollPayload is the self-contained payload the caller copies into
// trackerclient.EnrollRequest. Sig is the 64-byte Ed25519 signature
// over EnrollPreimage.
type EnrollPayload struct {
    IdentityPubkey     ed25519.PublicKey
    Role               uint32
    AccountFingerprint [32]byte
    Nonce              [16]byte
    Sig                []byte
}

// BuildEnrollPayload generates a fresh 16-byte nonce via crypto/rand,
// computes the preimage, and signs it. Returns the payload alongside
// the chosen nonce so callers can log/audit the enroll attempt.
func BuildEnrollPayload(s *Signer, fingerprint [32]byte, role uint32) (*EnrollPayload, error)

// Record is the cached identity binding. Persisted as JSON to
// ~/.token-bay/identity.json so subsequent plugin starts can detect
// orgId switches without re-enrolling on every run.
type Record struct {
    Version            int
    IdentityID         ids.IdentityID
    AccountFingerprint [32]byte
    Role               uint32
    EnrolledAt         time.Time // UTC; encoded RFC3339Nano
}

// LoadRecord reads path. Returns ErrRecordNotFound when path doesn't
// exist, ErrInvalidRecord on schema or hex-decode failure.
func LoadRecord(path string) (*Record, error)

// SaveRecord atomically writes path, mode 0600. Overwrite-allowed:
// re-enrollment on a new orgId rewrites this file (the key file is
// preserved).
func SaveRecord(path string, r *Record) error

// Open is the convenience that loads (signer, record) from a directory:
//   <dir>/identity.key   — read by LoadKey
//   <dir>/identity.json  — read by LoadRecord
//
// Returned errors are typed:
//   - ErrKeyNotFound:    identity.key missing → caller runs enroll flow
//   - ErrRecordNotFound: key present but record missing → caller runs enroll flow
//   - nil:               both present → caller passes the loaded snapshot
//                         through VerifyBinding before trusting the cache.
func Open(dir string) (*Signer, *Record, error)

// VerifyBinding compares the cached record's fingerprint with the live
// AuthSnapshot. Returns ErrFingerprintMismatch when the orgIds differ —
// caller surfaces the re-enrollment prompt described in plugin spec §4.2.
func VerifyBinding(r *Record, s AuthSnapshot) error

// Role bitmask values match `tbproto.EnrollRequest.role`.
const (
    RoleConsumer uint32 = 1 << 0
    RoleSeeder   uint32 = 1 << 1
)
```

## 4. Errors

```go
var (
    ErrKeyNotFound         = errors.New("identity: key file not found")
    ErrKeyExists           = errors.New("identity: key file already exists")
    ErrInvalidKey          = errors.New("identity: invalid key file")
    ErrRecordNotFound      = errors.New("identity: record file not found")
    ErrInvalidRecord       = errors.New("identity: invalid record file")
    ErrIneligibleAuth      = errors.New("identity: claude auth state not eligible")
    ErrFingerprintMismatch = errors.New("identity: account fingerprint changed since enrollment")
)
```

All sentinels work with `errors.Is`. Wrapping callers may attach context with `fmt.Errorf("...: %w", err)`.

## 5. File formats

### 5.1 `identity.key`

Raw 32-byte Ed25519 seed (the seed half of the 64-byte private key, byte-identical to `priv.Seed()`). No PEM, no base64, no JSON. Mode `0600`.

- `LoadKey` rejects any file whose length != 32 with `ErrInvalidKey`.
- `SaveKey` writes to `<path>.token-bay-tmp-<pid>` then `os.Rename` (POSIX-atomic). Refuses to clobber an existing file (`ErrKeyExists`).

### 5.2 `identity.json`

```json
{
  "version": 1,
  "identity_id":         "<hex 32 bytes>",
  "account_fingerprint": "<hex 32 bytes>",
  "role":                1,
  "enrolled_at":         "2026-05-07T19:48:00.000Z"
}
```

Mode `0600`. Atomic write via temp + rename. `SaveRecord` allows overwrite. `LoadRecord` requires `version == 1`; later versions add a migration switch the same way `auditlog/records.go` dispatches on `kind`.

## 6. Concurrency model

The package is stateless beyond per-call I/O. `*Signer` is read-only after construction; methods take no locks. Two goroutines writing the same path concurrently is undefined — operators are expected to call `Open` once at sidecar startup and thread the result through the rest of the process.

## 7. Failure handling

| Failure | Behavior |
|---|---|
| `~/.token-bay/identity.key` missing | `LoadKey` returns `ErrKeyNotFound`; caller (enrollment flow) `Generate`s + `SaveKey`s. |
| Key file corrupt (size != 32) | `LoadKey` returns `ErrInvalidKey` wrapping the size-mismatch detail. |
| `claude auth status --json` says `loggedIn:false` | `AccountFingerprint` returns `ErrIneligibleAuth` wrapping `"not logged in"`. |
| `apiProvider != "firstParty"` (Bedrock / Vertex / Foundry) | `AccountFingerprint` returns `ErrIneligibleAuth` wrapping the provider name. Plugin spec §10. |
| `authMethod == "api_key"` (or anything other than `"claude.ai"`) | `AccountFingerprint` returns `ErrIneligibleAuth` wrapping the method. Plugin spec §4.2 caveat. |
| `orgId` empty | `AccountFingerprint` returns `ErrIneligibleAuth("missing orgId")`. |
| Cached record's fingerprint != live fingerprint | `VerifyBinding` returns `ErrFingerprintMismatch`. Caller surfaces re-enrollment prompt. |
| Atomic write fails mid-rename | `rename(2)` is atomic — the target is either the new contents or the prior contents, never partial. Caller surfaces the error and retries on next launch. |

## 8. Acceptance criteria

The package is complete when:

1. `*Signer` satisfies `trackerclient.Signer` (compile-time assertion in a test: `var _ trackerclient.Signer = (*Signer)(nil)`).
2. Round-trip: `s := Generate(); SaveKey(p, s); s2 := LoadKey(p)` ⇒ `s.PrivateKey()` byte-identical to `s2.PrivateKey()` and `s.IdentityID() == s2.IdentityID()`.
3. `EnrollPreimage` matches the canonical 99-byte layout in §3 — locked by a hex-vector test using the spec's example orgId.
4. `AccountFingerprint` of orgId `cd2c6c26-fdea-44af-b14f-11e283737e33` (plugin spec §4.2 example) equals `SHA-256` of the literal UTF-8 bytes — locked by an explicit hex constant.
5. Eligibility refused for: `loggedIn:false`, `apiProvider:bedrock`, `authMethod:api_key`, empty `orgId`. One focused test per case.
6. `Record` round-trip: `SaveRecord` → `LoadRecord` preserves all fields byte-identical (with UTC normalization on `EnrolledAt`).
7. `Open` smoke test: prepare dir, write key + record, expect both back; missing each file produces the typed error.
8. `VerifyBinding` returns `nil` for matching fingerprints, `ErrFingerprintMismatch` for differing.
9. Race-clean under `go test -race ./plugin/internal/identity/...`.
10. `make check` from `plugin/` is green; the trackerclient consumer wiring asserts `*identity.Signer` satisfies its `Signer` interface.

## 9. Open questions

- **Key rotation.** Plugin spec §4.3 doesn't define rotation. v1 leaves the key file untouchable except via manual operator action; rotation is deferred to a follow-up plan.
- **OS keychain.** Spec §4.3 marks optional. A keychain-backed `Signer` could plug in without changing call sites — deferred until a user requests it.
- **`identity.json` schema versioning.** The `version: 1` field is present from the start; once we ship v2, `LoadRecord` dispatches on the version field the same way `auditlog/records.go` dispatches on `kind`. No code today.
