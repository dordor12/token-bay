# Plugin Internal Identity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up `plugin/internal/identity` — the package that owns the plugin's Ed25519 keypair on disk, derives the account fingerprint from `claude auth status --json`, builds the `token-bay-enroll:v1` signed preimage, and persists the cached identity-binding record.

**Architecture:** Single leaf package under `plugin/internal/identity`. Stdlib-only dependencies plus `shared/ids`. Production source of `trackerclient.Signer`. No goroutines, no exec calls — production wiring fills the `AuthSnapshot` struct from the existing `internal/ccproxy.ClaudeAuthProber`.

**Tech Stack:** Go 1.25, stdlib `crypto/ed25519`, `crypto/rand`, `crypto/sha256`, `encoding/hex`, `encoding/json`, `os`, `path/filepath`, `time`. Tests use `github.com/stretchr/testify` (already a plugin module dep).

---

## Reference

- Spec: `docs/superpowers/specs/plugin/2026-05-07-plugin-identity-design.md`
- Parent spec: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` (§4 enrollment & identity)
- Consumer interface: `plugin/internal/trackerclient/types.go:60-64` (`Signer`)
- Sibling pattern (atomic-write conventions): `plugin/internal/auditlog/logger.go`, `plugin/internal/settingsjson/atomic_write.go`

## File map

```
plugin/internal/identity/                                 -- new package
  doc.go                                                  -- new
  CLAUDE.md                                               -- new (small dev-context overview)
  errors.go                                               -- new (7 sentinel errors)
  errors_test.go                                          -- new (sentinel identity check)
  signer.go                                               -- new (Signer + New + Generate + methods)
  signer_test.go                                          -- new
  key.go                                                  -- new (LoadKey + SaveKey + atomic write)
  key_test.go                                             -- new
  fingerprint.go                                          -- new (AuthSnapshot + AccountFingerprint + role consts)
  fingerprint_test.go                                     -- new
  enroll.go                                               -- new (EnrollPreimagePrefix + EnrollPreimage + EnrollPayload + BuildEnrollPayload)
  enroll_test.go                                          -- new
  record.go                                               -- new (Record + LoadRecord + SaveRecord)
  record_test.go                                          -- new
  store.go                                                -- new (Open + VerifyBinding)
  store_test.go                                           -- new
  identity_test.go                                        -- new (compile-time check for trackerclient.Signer)

plugin/internal/identity/.gitkeep                         -- delete (placeholder no longer needed)
```

---

## Phase 1 — Skeleton + sentinels

### Task 1: Package skeleton + CLAUDE.md + errors

**Files:**
- Create: `plugin/internal/identity/doc.go`
- Create: `plugin/internal/identity/CLAUDE.md`
- Create: `plugin/internal/identity/errors.go`
- Create: `plugin/internal/identity/errors_test.go`
- Delete: `plugin/internal/identity/.gitkeep`

- [ ] **Step 1: Write `doc.go`**

```go
// Package identity owns the plugin's long-lived identity primitives:
// the Ed25519 keypair on disk, the SHA-256(orgId) account fingerprint
// derived from `claude auth status --json`, the token-bay-enroll:v1
// signed preimage, and the cached identity-binding record.
//
// The package is the production source of trackerclient.Signer and the
// single chokepoint for the account-fingerprint derivation. It is pure
// state — no goroutines, no exec calls. Production wiring fills the
// AuthSnapshot struct from internal/ccproxy.ClaudeAuthProber.
//
// See docs/superpowers/specs/plugin/2026-05-07-plugin-identity-design.md.
package identity
```

- [ ] **Step 2: Write `CLAUDE.md`**

```markdown
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
   caller-filled AuthSnapshot — this package does not exec.
2. **Stdlib crypto only.** `crypto/ed25519` and `crypto/sha256` —
   nothing third-party. Repo-wide rule.
3. **Atomic writes always.** SaveKey + SaveRecord write to a temp file
   and rename(2). A killed process never leaves a half-written key.
4. **One chokepoint per derivation.** `AccountFingerprint` is the only
   place the SHA-256(orgId) computation lives; `EnrollPreimage` is the
   only place the 99-byte canonical layout lives. Tests pin both with
   hex vectors.
5. **No goroutines.** The package is read-mostly; concurrent access
   to the same file is undefined. Operators call `Open` once at
   sidecar startup.

## Tech stack

- Go 1.25, stdlib only (plus `shared/ids` for the IdentityID type)
- Tests via `testify`
```

- [ ] **Step 3: Write `errors.go`**

```go
package identity

import "errors"

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

- [ ] **Step 4: Write `errors_test.go`** — sentinel-identity check

```go
package identity

import (
    "errors"
    "fmt"
    "testing"
)

func TestErrorsAreSentinels(t *testing.T) {
    cases := []error{
        ErrKeyNotFound,
        ErrKeyExists,
        ErrInvalidKey,
        ErrRecordNotFound,
        ErrInvalidRecord,
        ErrIneligibleAuth,
        ErrFingerprintMismatch,
    }
    for _, e := range cases {
        if e == nil {
            t.Fatal("nil sentinel")
        }
        wrapped := fmt.Errorf("ctx: %w", e)
        if !errors.Is(wrapped, e) {
            t.Fatalf("errors.Is broken for %v", e)
        }
    }
}
```

- [ ] **Step 5: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`
Expected: PASS for `TestErrorsAreSentinels`. The package compiles.

- [ ] **Step 6: Commit**

```bash
git rm plugin/internal/identity/.gitkeep
git add plugin/internal/identity/doc.go plugin/internal/identity/CLAUDE.md \
        plugin/internal/identity/errors.go plugin/internal/identity/errors_test.go
git commit -m "feat(plugin/identity): package skeleton + sentinel errors"
```

---

## Phase 2 — Signer

### Task 2: `Signer`, `New`, `Generate`, methods

**Files:**
- Create: `plugin/internal/identity/signer.go`
- Create: `plugin/internal/identity/signer_test.go`

- [ ] **Step 1: Write failing tests**

```go
package identity

import (
    "crypto/ed25519"
    "crypto/sha256"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestNew_RejectsBadKeyLength(t *testing.T) {
    _, err := New(ed25519.PrivateKey{1, 2, 3})
    require.ErrorIs(t, err, ErrInvalidKey)
}

func TestGenerate_DeterministicWithinCall(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)
    require.Len(t, s.PrivateKey(), ed25519.PrivateKeySize)
    require.Len(t, s.PublicKey(), ed25519.PublicKeySize)

    expectedID := sha256.Sum256(s.PublicKey())
    assert.Equal(t, expectedID, [32]byte(s.IdentityID()))
}

func TestSign_RoundTripVerify(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)

    msg := []byte("hello identity")
    sig, err := s.Sign(msg)
    require.NoError(t, err)
    require.Len(t, sig, ed25519.SignatureSize)

    assert.True(t, ed25519.Verify(s.PublicKey(), msg, sig))
}

func TestNew_FromGeneratedKey_RoundTripsSignature(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)
    s2, err := New(s.PrivateKey())
    require.NoError(t, err)
    assert.Equal(t, s.IdentityID(), s2.IdentityID())
}
```

- [ ] **Step 2: Run, expect FAIL** — `New`, `Generate`, `Signer` undefined.

- [ ] **Step 3: Implement `signer.go`**

```go
package identity

import (
    "crypto/ed25519"
    "crypto/rand"
    "crypto/sha256"
    "fmt"

    "github.com/token-bay/token-bay/shared/ids"
)

// Signer satisfies trackerclient.Signer. Holds an Ed25519 keypair and
// caches the IdentityID = SHA-256(public key bytes).
type Signer struct {
    priv ed25519.PrivateKey
    pub  ed25519.PublicKey
    id   ids.IdentityID
}

// New wraps an existing keypair. Returns ErrInvalidKey on length mismatch.
func New(priv ed25519.PrivateKey) (*Signer, error) {
    if len(priv) != ed25519.PrivateKeySize {
        return nil, fmt.Errorf("%w: priv length %d, want %d", ErrInvalidKey, len(priv), ed25519.PrivateKeySize)
    }
    pub, ok := priv.Public().(ed25519.PublicKey)
    if !ok || len(pub) != ed25519.PublicKeySize {
        return nil, fmt.Errorf("%w: derived pub length", ErrInvalidKey)
    }
    return &Signer{priv: priv, pub: pub, id: ids.IdentityID(sha256.Sum256(pub))}, nil
}

// Generate produces a fresh keypair via crypto/rand.
func Generate() (*Signer, error) {
    pub, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil {
        return nil, fmt.Errorf("identity: generate key: %w", err)
    }
    return &Signer{priv: priv, pub: pub, id: ids.IdentityID(sha256.Sum256(pub))}, nil
}

func (s *Signer) Sign(msg []byte) ([]byte, error) {
    return ed25519.Sign(s.priv, msg), nil
}

func (s *Signer) PrivateKey() ed25519.PrivateKey { return s.priv }
func (s *Signer) PublicKey() ed25519.PublicKey   { return s.pub }
func (s *Signer) IdentityID() ids.IdentityID     { return s.id }
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -run TestSigner -v` (and the existing TestSign / TestNew / TestGenerate)
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/identity/signer.go plugin/internal/identity/signer_test.go
git commit -m "feat(plugin/identity): Signer + Generate + New"
```

---

## Phase 3 — Key on disk

### Task 3: `LoadKey` + `SaveKey`

**Files:**
- Create: `plugin/internal/identity/key.go`
- Create: `plugin/internal/identity/key_test.go`

- [ ] **Step 1: Write failing tests**

```go
package identity

import (
    "errors"
    "os"
    "path/filepath"
    "runtime"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestLoadKey_NotFound(t *testing.T) {
    _, err := LoadKey(filepath.Join(t.TempDir(), "missing"))
    require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestLoadKey_BadSize(t *testing.T) {
    p := filepath.Join(t.TempDir(), "identity.key")
    require.NoError(t, os.WriteFile(p, []byte("too short"), 0o600))
    _, err := LoadKey(p)
    require.ErrorIs(t, err, ErrInvalidKey)
}

func TestSaveKey_RoundTripPreservesPrivateKey(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)

    p := filepath.Join(t.TempDir(), "identity.key")
    require.NoError(t, SaveKey(p, s))

    s2, err := LoadKey(p)
    require.NoError(t, err)
    assert.Equal(t, []byte(s.PrivateKey()), []byte(s2.PrivateKey()))
    assert.Equal(t, s.IdentityID(), s2.IdentityID())
}

func TestSaveKey_RefusesOverwrite(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)
    p := filepath.Join(t.TempDir(), "identity.key")
    require.NoError(t, SaveKey(p, s))

    err = SaveKey(p, s)
    require.True(t, errors.Is(err, ErrKeyExists), "got %v", err)
}

func TestSaveKey_FileMode0600(t *testing.T) {
    if runtime.GOOS == "windows" {
        t.Skip("Windows does not honor Unix file modes")
    }
    s, err := Generate()
    require.NoError(t, err)
    p := filepath.Join(t.TempDir(), "identity.key")
    require.NoError(t, SaveKey(p, s))

    info, err := os.Stat(p)
    require.NoError(t, err)
    assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveKey_FileSizeIs32Bytes(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)
    p := filepath.Join(t.TempDir(), "identity.key")
    require.NoError(t, SaveKey(p, s))
    info, err := os.Stat(p)
    require.NoError(t, err)
    assert.EqualValues(t, 32, info.Size())
}
```

- [ ] **Step 2: Run, expect FAIL** — undefined symbols.

- [ ] **Step 3: Implement `key.go`**

```go
package identity

import (
    "crypto/ed25519"
    "errors"
    "fmt"
    "io/fs"
    "os"
    "strconv"
)

// LoadKey reads path, parses the 32-byte Ed25519 seed, and returns a
// *Signer. Returns ErrKeyNotFound when path doesn't exist, ErrInvalidKey
// on size mismatch.
func LoadKey(path string) (*Signer, error) {
    seed, err := os.ReadFile(path)
    if err != nil {
        if errors.Is(err, fs.ErrNotExist) {
            return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, path)
        }
        return nil, fmt.Errorf("identity: read %s: %w", path, err)
    }
    if len(seed) != ed25519.SeedSize {
        return nil, fmt.Errorf("%w: %s: size %d, want %d", ErrInvalidKey, path, len(seed), ed25519.SeedSize)
    }
    priv := ed25519.NewKeyFromSeed(seed)
    return New(priv)
}

// SaveKey atomically writes the 32-byte seed to path, mode 0600.
// Refuses to clobber an existing file (returns ErrKeyExists).
func SaveKey(path string, s *Signer) error {
    if _, err := os.Stat(path); err == nil {
        return fmt.Errorf("%w: %s", ErrKeyExists, path)
    } else if !errors.Is(err, fs.ErrNotExist) {
        return fmt.Errorf("identity: stat %s: %w", path, err)
    }
    seed := s.PrivateKey().Seed()
    tmp := path + ".token-bay-tmp-" + strconv.Itoa(os.Getpid())
    if err := os.WriteFile(tmp, seed, 0o600); err != nil {
        return fmt.Errorf("identity: write tmp: %w", err)
    }
    if err := os.Rename(tmp, path); err != nil {
        _ = os.Remove(tmp)
        return fmt.Errorf("identity: rename: %w", err)
    }
    return nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/identity/key.go plugin/internal/identity/key_test.go
git commit -m "feat(plugin/identity): atomic 0600 SaveKey + size-checked LoadKey"
```

---

## Phase 4 — Account fingerprint

### Task 4: `AuthSnapshot` + `AccountFingerprint`

**Files:**
- Create: `plugin/internal/identity/fingerprint.go`
- Create: `plugin/internal/identity/fingerprint_test.go`

- [ ] **Step 1: Write failing tests**

```go
package identity

import (
    "crypto/sha256"
    "encoding/hex"
    "errors"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

const specExampleOrgID = "cd2c6c26-fdea-44af-b14f-11e283737e33"

func validSnapshot() AuthSnapshot {
    return AuthSnapshot{
        LoggedIn:    true,
        AuthMethod:  "claude.ai",
        APIProvider: "firstParty",
        OrgID:       specExampleOrgID,
    }
}

func TestAccountFingerprint_KnownVector(t *testing.T) {
    fp, err := AccountFingerprint(validSnapshot())
    require.NoError(t, err)

    expected := sha256.Sum256([]byte(specExampleOrgID))
    assert.Equal(t, expected, fp)

    // Lock as hex so we catch any future drift.
    expectedHex := hex.EncodeToString(expected[:])
    assert.Equal(t, expectedHex, hex.EncodeToString(fp[:]))
}

func TestAccountFingerprint_LoggedInFalseRefused(t *testing.T) {
    s := validSnapshot()
    s.LoggedIn = false
    _, err := AccountFingerprint(s)
    require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}

func TestAccountFingerprint_BedrockProviderRefused(t *testing.T) {
    s := validSnapshot()
    s.APIProvider = "bedrock"
    _, err := AccountFingerprint(s)
    require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}

func TestAccountFingerprint_APIKeyMethodRefused(t *testing.T) {
    s := validSnapshot()
    s.AuthMethod = "api_key"
    _, err := AccountFingerprint(s)
    require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}

func TestAccountFingerprint_EmptyOrgIDRefused(t *testing.T) {
    s := validSnapshot()
    s.OrgID = ""
    _, err := AccountFingerprint(s)
    require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}
```

- [ ] **Step 2: Run, expect FAIL** — undefined.

- [ ] **Step 3: Implement `fingerprint.go`**

```go
package identity

import (
    "crypto/sha256"
    "fmt"
)

// Role bitmask values match tbproto.EnrollRequest.role.
const (
    RoleConsumer uint32 = 1 << 0
    RoleSeeder   uint32 = 1 << 1
)

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
// Returns ErrIneligibleAuth when the snapshot fails the eligibility check.
func AccountFingerprint(s AuthSnapshot) ([32]byte, error) {
    if !s.LoggedIn {
        return [32]byte{}, fmt.Errorf("%w: not logged in", ErrIneligibleAuth)
    }
    if s.AuthMethod != "claude.ai" {
        return [32]byte{}, fmt.Errorf("%w: auth method %q (want claude.ai)", ErrIneligibleAuth, s.AuthMethod)
    }
    if s.APIProvider != "firstParty" {
        return [32]byte{}, fmt.Errorf("%w: api provider %q (want firstParty)", ErrIneligibleAuth, s.APIProvider)
    }
    if s.OrgID == "" {
        return [32]byte{}, fmt.Errorf("%w: missing orgId", ErrIneligibleAuth)
    }
    return sha256.Sum256([]byte(s.OrgID)), nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/identity/fingerprint.go plugin/internal/identity/fingerprint_test.go
git commit -m "feat(plugin/identity): AccountFingerprint = SHA-256(orgId) with eligibility gates"
```

---

## Phase 5 — Enroll preimage + payload

### Task 5: `EnrollPreimage` + `EnrollPayload` + `BuildEnrollPayload`

**Files:**
- Create: `plugin/internal/identity/enroll.go`
- Create: `plugin/internal/identity/enroll_test.go`

- [ ] **Step 1: Write failing tests**

```go
package identity

import (
    "bytes"
    "crypto/ed25519"
    "encoding/hex"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestEnrollPreimage_LayoutAndLength(t *testing.T) {
    var nonce [16]byte
    for i := range nonce {
        nonce[i] = byte(i + 1)
    }
    var fp [32]byte
    for i := range fp {
        fp[i] = 0xAA
    }
    pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
    for i := range pub {
        pub[i] = 0xBB
    }

    pre, err := EnrollPreimage(nonce, fp, pub)
    require.NoError(t, err)
    assert.Len(t, pre, len(EnrollPreimagePrefix)+16+32+32)
    assert.Equal(t, 99, len(pre))

    assert.True(t, bytes.HasPrefix(pre, []byte(EnrollPreimagePrefix)))
    off := len(EnrollPreimagePrefix)
    assert.Equal(t, nonce[:], pre[off:off+16])
    assert.Equal(t, fp[:],    pre[off+16:off+16+32])
    assert.Equal(t, []byte(pub), pre[off+16+32:])
}

func TestEnrollPreimage_KnownHexVector(t *testing.T) {
    var nonce [16]byte
    var fp [32]byte
    pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
    pre, err := EnrollPreimage(nonce, fp, pub)
    require.NoError(t, err)

    // 19 bytes prefix ("token-bay-enroll:v1") + 80 bytes of zeros.
    expected := append([]byte(EnrollPreimagePrefix), make([]byte, 80)...)
    assert.Equal(t, hex.EncodeToString(expected), hex.EncodeToString(pre))
}

func TestEnrollPreimage_RejectsBadPubKeyLength(t *testing.T) {
    _, err := EnrollPreimage([16]byte{}, [32]byte{}, ed25519.PublicKey{1, 2, 3})
    require.ErrorIs(t, err, ErrInvalidKey)
}

func TestBuildEnrollPayload_SignatureVerifies(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)

    var fp [32]byte
    for i := range fp {
        fp[i] = 0xCD
    }

    p, err := BuildEnrollPayload(s, fp, RoleConsumer|RoleSeeder)
    require.NoError(t, err)
    assert.Equal(t, []byte(s.PublicKey()), []byte(p.IdentityPubkey))
    assert.Equal(t, fp, p.AccountFingerprint)
    assert.Equal(t, RoleConsumer|RoleSeeder, p.Role)
    assert.Len(t, p.Sig, ed25519.SignatureSize)

    pre, err := EnrollPreimage(p.Nonce, p.AccountFingerprint, p.IdentityPubkey)
    require.NoError(t, err)
    assert.True(t, ed25519.Verify(s.PublicKey(), pre, p.Sig))
}

func TestBuildEnrollPayload_NonceIsRandom(t *testing.T) {
    s, err := Generate()
    require.NoError(t, err)

    var fp [32]byte
    p1, err := BuildEnrollPayload(s, fp, RoleConsumer)
    require.NoError(t, err)
    p2, err := BuildEnrollPayload(s, fp, RoleConsumer)
    require.NoError(t, err)
    assert.NotEqual(t, p1.Nonce, p2.Nonce, "nonce should be random per call")
}
```

- [ ] **Step 2: Run, expect FAIL** — undefined.

- [ ] **Step 3: Implement `enroll.go`**

```go
package identity

import (
    "crypto/ed25519"
    "crypto/rand"
    "fmt"
)

// EnrollPreimagePrefix is the domain-separation tag prefixed to every
// enroll preimage. Bytes-stable across plugin versions.
const EnrollPreimagePrefix = "token-bay-enroll:v1"

// enrollPreimageLen = len(prefix) + 16 (nonce) + 32 (fingerprint) + 32 (pubkey)
const enrollPreimageLen = len(EnrollPreimagePrefix) + 16 + 32 + 32

// EnrollPreimage assembles the canonical bytes signed by the consumer
// per plugin spec §4.2:
//
//   "token-bay-enroll:v1" || nonce[16] || account_fingerprint[32] || identity_pubkey[32]
//
// Total length: 99 bytes. Returns ErrInvalidKey if pub is not 32 bytes.
func EnrollPreimage(nonce [16]byte, fingerprint [32]byte, pub ed25519.PublicKey) ([]byte, error) {
    if len(pub) != ed25519.PublicKeySize {
        return nil, fmt.Errorf("%w: pub length %d, want %d", ErrInvalidKey, len(pub), ed25519.PublicKeySize)
    }
    out := make([]byte, 0, enrollPreimageLen)
    out = append(out, EnrollPreimagePrefix...)
    out = append(out, nonce[:]...)
    out = append(out, fingerprint[:]...)
    out = append(out, pub...)
    return out, nil
}

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
// builds the preimage, and signs it. Returns the payload alongside the
// chosen nonce so callers can log/audit the enroll attempt.
func BuildEnrollPayload(s *Signer, fingerprint [32]byte, role uint32) (*EnrollPayload, error) {
    var nonce [16]byte
    if _, err := rand.Read(nonce[:]); err != nil {
        return nil, fmt.Errorf("identity: nonce: %w", err)
    }
    pre, err := EnrollPreimage(nonce, fingerprint, s.PublicKey())
    if err != nil {
        return nil, err
    }
    sig, err := s.Sign(pre)
    if err != nil {
        return nil, fmt.Errorf("identity: sign preimage: %w", err)
    }
    return &EnrollPayload{
        IdentityPubkey:     s.PublicKey(),
        Role:               role,
        AccountFingerprint: fingerprint,
        Nonce:              nonce,
        Sig:                sig,
    }, nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/identity/enroll.go plugin/internal/identity/enroll_test.go
git commit -m "feat(plugin/identity): token-bay-enroll:v1 preimage + signed payload"
```

---

## Phase 6 — Identity record file

### Task 6: `Record` + `LoadRecord` + `SaveRecord`

**Files:**
- Create: `plugin/internal/identity/record.go`
- Create: `plugin/internal/identity/record_test.go`

- [ ] **Step 1: Write failing tests**

```go
package identity

import (
    "encoding/hex"
    "encoding/json"
    "errors"
    "os"
    "path/filepath"
    "runtime"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "github.com/token-bay/token-bay/shared/ids"
)

func sampleRecord() *Record {
    var idid ids.IdentityID
    for i := range idid {
        idid[i] = 0x11
    }
    var fp [32]byte
    for i := range fp {
        fp[i] = 0x22
    }
    return &Record{
        Version:            1,
        IdentityID:         idid,
        AccountFingerprint: fp,
        Role:               RoleConsumer | RoleSeeder,
        EnrolledAt:         time.Date(2026, 5, 7, 19, 48, 0, 0, time.UTC),
    }
}

func TestSaveRecord_LoadRecord_RoundTrip(t *testing.T) {
    p := filepath.Join(t.TempDir(), "identity.json")
    require.NoError(t, SaveRecord(p, sampleRecord()))

    got, err := LoadRecord(p)
    require.NoError(t, err)
    assert.Equal(t, sampleRecord(), got)
}

func TestSaveRecord_AllowsOverwrite(t *testing.T) {
    p := filepath.Join(t.TempDir(), "identity.json")
    require.NoError(t, SaveRecord(p, sampleRecord()))

    r2 := sampleRecord()
    r2.Role = RoleSeeder
    require.NoError(t, SaveRecord(p, r2))

    got, err := LoadRecord(p)
    require.NoError(t, err)
    assert.Equal(t, RoleSeeder, got.Role)
}

func TestSaveRecord_FileMode0600(t *testing.T) {
    if runtime.GOOS == "windows" {
        t.Skip("Windows does not honor Unix file modes")
    }
    p := filepath.Join(t.TempDir(), "identity.json")
    require.NoError(t, SaveRecord(p, sampleRecord()))
    info, err := os.Stat(p)
    require.NoError(t, err)
    assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveRecord_NormalizesUTC(t *testing.T) {
    r := sampleRecord()
    r.EnrolledAt = time.Date(2026, 5, 7, 12, 0, 0, 0, time.FixedZone("X", 5*3600))
    p := filepath.Join(t.TempDir(), "identity.json")
    require.NoError(t, SaveRecord(p, r))

    got, err := LoadRecord(p)
    require.NoError(t, err)
    assert.True(t, got.EnrolledAt.Equal(r.EnrolledAt))
    assert.Equal(t, time.UTC, got.EnrolledAt.Location())
}

func TestLoadRecord_NotFound(t *testing.T) {
    _, err := LoadRecord(filepath.Join(t.TempDir(), "missing"))
    require.True(t, errors.Is(err, ErrRecordNotFound), "got %v", err)
}

func TestLoadRecord_BadVersion(t *testing.T) {
    p := filepath.Join(t.TempDir(), "identity.json")
    raw := map[string]any{
        "version":             99,
        "identity_id":         hex.EncodeToString(make([]byte, 32)),
        "account_fingerprint": hex.EncodeToString(make([]byte, 32)),
        "role":                1,
        "enrolled_at":         "2026-05-07T19:48:00Z",
    }
    b, _ := json.Marshal(raw)
    require.NoError(t, os.WriteFile(p, b, 0o600))
    _, err := LoadRecord(p)
    require.True(t, errors.Is(err, ErrInvalidRecord), "got %v", err)
}

func TestLoadRecord_BadHexLength(t *testing.T) {
    p := filepath.Join(t.TempDir(), "identity.json")
    raw := map[string]any{
        "version":             1,
        "identity_id":         "abcd",
        "account_fingerprint": hex.EncodeToString(make([]byte, 32)),
        "role":                1,
        "enrolled_at":         "2026-05-07T19:48:00Z",
    }
    b, _ := json.Marshal(raw)
    require.NoError(t, os.WriteFile(p, b, 0o600))
    _, err := LoadRecord(p)
    require.True(t, errors.Is(err, ErrInvalidRecord), "got %v", err)
}
```

- [ ] **Step 2: Run, expect FAIL** — undefined.

- [ ] **Step 3: Implement `record.go`**

```go
package identity

import (
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io/fs"
    "os"
    "strconv"
    "time"

    "github.com/token-bay/token-bay/shared/ids"
)

const recordCurrentVersion = 1

// Record is the cached identity binding persisted to disk after enrollment.
type Record struct {
    Version            int
    IdentityID         ids.IdentityID
    AccountFingerprint [32]byte
    Role               uint32
    EnrolledAt         time.Time // UTC
}

type recordWire struct {
    Version            int    `json:"version"`
    IdentityID         string `json:"identity_id"`
    AccountFingerprint string `json:"account_fingerprint"`
    Role               uint32 `json:"role"`
    EnrolledAt         string `json:"enrolled_at"`
}

// LoadRecord reads path. Returns ErrRecordNotFound when path doesn't
// exist, ErrInvalidRecord on schema or hex-decode failure.
func LoadRecord(path string) (*Record, error) {
    raw, err := os.ReadFile(path)
    if err != nil {
        if errors.Is(err, fs.ErrNotExist) {
            return nil, fmt.Errorf("%w: %s", ErrRecordNotFound, path)
        }
        return nil, fmt.Errorf("identity: read %s: %w", path, err)
    }
    var w recordWire
    if err := json.Unmarshal(raw, &w); err != nil {
        return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
    }
    if w.Version != recordCurrentVersion {
        return nil, fmt.Errorf("%w: version %d, want %d", ErrInvalidRecord, w.Version, recordCurrentVersion)
    }
    idHash, err := decodeHex32(w.IdentityID)
    if err != nil {
        return nil, fmt.Errorf("%w: identity_id: %v", ErrInvalidRecord, err)
    }
    fp, err := decodeHex32(w.AccountFingerprint)
    if err != nil {
        return nil, fmt.Errorf("%w: account_fingerprint: %v", ErrInvalidRecord, err)
    }
    enrolledAt, err := time.Parse(time.RFC3339Nano, w.EnrolledAt)
    if err != nil {
        return nil, fmt.Errorf("%w: enrolled_at: %v", ErrInvalidRecord, err)
    }
    return &Record{
        Version:            w.Version,
        IdentityID:         ids.IdentityID(idHash),
        AccountFingerprint: fp,
        Role:               w.Role,
        EnrolledAt:         enrolledAt.UTC(),
    }, nil
}

// SaveRecord atomically writes path, mode 0600. Overwrite-allowed.
func SaveRecord(path string, r *Record) error {
    if r == nil {
        return fmt.Errorf("identity: SaveRecord nil record")
    }
    version := r.Version
    if version == 0 {
        version = recordCurrentVersion
    }
    w := recordWire{
        Version:            version,
        IdentityID:         hex.EncodeToString(r.IdentityID[:]),
        AccountFingerprint: hex.EncodeToString(r.AccountFingerprint[:]),
        Role:               r.Role,
        EnrolledAt:         r.EnrolledAt.UTC().Format(time.RFC3339Nano),
    }
    blob, err := json.Marshal(w)
    if err != nil {
        return fmt.Errorf("identity: marshal record: %w", err)
    }
    tmp := path + ".token-bay-tmp-" + strconv.Itoa(os.Getpid())
    if err := os.WriteFile(tmp, blob, 0o600); err != nil {
        return fmt.Errorf("identity: write tmp: %w", err)
    }
    if err := os.Rename(tmp, path); err != nil {
        _ = os.Remove(tmp)
        return fmt.Errorf("identity: rename: %w", err)
    }
    return nil
}

func decodeHex32(s string) ([32]byte, error) {
    b, err := hex.DecodeString(s)
    if err != nil {
        return [32]byte{}, err
    }
    if len(b) != 32 {
        return [32]byte{}, fmt.Errorf("expected 32 bytes, got %d", len(b))
    }
    var out [32]byte
    copy(out[:], b)
    return out, nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/identity/record.go plugin/internal/identity/record_test.go
git commit -m "feat(plugin/identity): atomic 0600 SaveRecord + versioned LoadRecord"
```

---

## Phase 7 — Open + VerifyBinding

### Task 7: `Open` + `VerifyBinding`

**Files:**
- Create: `plugin/internal/identity/store.go`
- Create: `plugin/internal/identity/store_test.go`

- [ ] **Step 1: Write failing tests**

```go
package identity

import (
    "errors"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestOpen_BothFilesPresent(t *testing.T) {
    dir := t.TempDir()
    s, err := Generate()
    require.NoError(t, err)
    require.NoError(t, SaveKey(filepath.Join(dir, "identity.key"), s))

    rec := sampleRecord()
    require.NoError(t, SaveRecord(filepath.Join(dir, "identity.json"), rec))

    gotS, gotR, err := Open(dir)
    require.NoError(t, err)
    assert.Equal(t, s.IdentityID(), gotS.IdentityID())
    assert.Equal(t, rec, gotR)
}

func TestOpen_KeyMissing(t *testing.T) {
    dir := t.TempDir()
    _, _, err := Open(dir)
    require.True(t, errors.Is(err, ErrKeyNotFound), "got %v", err)
}

func TestOpen_RecordMissing(t *testing.T) {
    dir := t.TempDir()
    s, err := Generate()
    require.NoError(t, err)
    require.NoError(t, SaveKey(filepath.Join(dir, "identity.key"), s))

    _, _, err = Open(dir)
    require.True(t, errors.Is(err, ErrRecordNotFound), "got %v", err)
}

func TestOpen_KeyCorrupt(t *testing.T) {
    dir := t.TempDir()
    require.NoError(t, os.WriteFile(filepath.Join(dir, "identity.key"), []byte("xxx"), 0o600))
    _, _, err := Open(dir)
    require.True(t, errors.Is(err, ErrInvalidKey), "got %v", err)
}

func TestVerifyBinding_Match(t *testing.T) {
    snap := validSnapshot()
    fp, err := AccountFingerprint(snap)
    require.NoError(t, err)
    rec := &Record{Version: 1, AccountFingerprint: fp, EnrolledAt: time.Now().UTC()}
    require.NoError(t, VerifyBinding(rec, snap))
}

func TestVerifyBinding_Mismatch(t *testing.T) {
    snap := validSnapshot()
    rec := &Record{Version: 1, AccountFingerprint: [32]byte{0xFF}, EnrolledAt: time.Now().UTC()}
    err := VerifyBinding(rec, snap)
    require.True(t, errors.Is(err, ErrFingerprintMismatch), "got %v", err)
}

func TestVerifyBinding_IneligibleSnapshot(t *testing.T) {
    snap := validSnapshot()
    snap.LoggedIn = false
    rec := &Record{Version: 1, AccountFingerprint: [32]byte{}, EnrolledAt: time.Now().UTC()}
    err := VerifyBinding(rec, snap)
    require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}
```

- [ ] **Step 2: Run, expect FAIL** — undefined.

- [ ] **Step 3: Implement `store.go`**

```go
package identity

import (
    "fmt"
    "path/filepath"
)

const (
    keyFilename    = "identity.key"
    recordFilename = "identity.json"
)

// Open loads (signer, record) from a directory:
//
//   <dir>/identity.key   read by LoadKey
//   <dir>/identity.json  read by LoadRecord
//
// Returns the typed errors LoadKey / LoadRecord surface (ErrKeyNotFound,
// ErrInvalidKey, ErrRecordNotFound, ErrInvalidRecord) so callers can
// branch into the enrollment flow.
func Open(dir string) (*Signer, *Record, error) {
    s, err := LoadKey(filepath.Join(dir, keyFilename))
    if err != nil {
        return nil, nil, err
    }
    r, err := LoadRecord(filepath.Join(dir, recordFilename))
    if err != nil {
        return nil, nil, err
    }
    return s, r, nil
}

// VerifyBinding compares the cached record's fingerprint with the live
// AuthSnapshot. Returns nil on match, ErrFingerprintMismatch on
// divergence, ErrIneligibleAuth if the snapshot itself is ineligible
// (caller should treat that as a re-enrollment trigger as well).
func VerifyBinding(r *Record, s AuthSnapshot) error {
    if r == nil {
        return fmt.Errorf("identity: VerifyBinding nil record")
    }
    fp, err := AccountFingerprint(s)
    if err != nil {
        return err
    }
    if fp != r.AccountFingerprint {
        return fmt.Errorf("%w: cached vs live differ", ErrFingerprintMismatch)
    }
    return nil
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/identity/store.go plugin/internal/identity/store_test.go
git commit -m "feat(plugin/identity): Open dir + VerifyBinding orgId-mismatch detection"
```

---

## Phase 8 — trackerclient.Signer compile assertion

### Task 8: Verify `*Signer` satisfies `trackerclient.Signer`

**Files:**
- Create: `plugin/internal/identity/identity_test.go`

- [ ] **Step 1: Write the assertion test**

```go
package identity_test

import (
    "testing"

    "github.com/token-bay/token-bay/plugin/internal/identity"
    "github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// Compile-time check: *identity.Signer satisfies trackerclient.Signer.
var _ trackerclient.Signer = (*identity.Signer)(nil)

func TestSigner_SatisfiesTrackerClientSigner(t *testing.T) {
    s, err := identity.Generate()
    if err != nil {
        t.Fatal(err)
    }
    var iface trackerclient.Signer = s
    if iface.IdentityID() != s.IdentityID() {
        t.Fatal("IdentityID round-trip via interface failed")
    }
}
```

- [ ] **Step 2: Run, expect PASS**

Run: `cd plugin && go test -race ./internal/identity/... -v`
Expected: PASS. Compilation of the `var _ ...` line proves the interface is satisfied.

- [ ] **Step 3: Commit**

```bash
git add plugin/internal/identity/identity_test.go
git commit -m "test(plugin/identity): assert *Signer satisfies trackerclient.Signer"
```

---

## Phase 9 — Final acceptance

### Task 9: `make check`

- [ ] **Step 1: Race-clean confirmation**

Run: `cd plugin && go test -race ./internal/identity/... -v`
Expected: all PASS, race-clean.

- [ ] **Step 2: Plugin-wide test**

Run: `make -C plugin test`
Expected: all unit tests in plugin module green; the new `internal/identity` package contributes additional tests.

- [ ] **Step 3: Lint**

Run: `make -C plugin lint`
Expected: `golangci-lint` clean. Resolve any nits before merging.

- [ ] **Step 4: Cross-module sanity**

Run: `make check` from repo root.
Expected: all three modules green.

- [ ] **Step 5: PR**

```bash
git push -u origin plugin/internal/identity
gh pr create --base main --fill --title "plugin/identity: Ed25519 keypair, account fingerprint, enroll preimage"
gh pr checks --watch
```

Merge with `gh pr merge --squash --delete-branch` only when CI is green.

---

## Self-review notes (author)

- Stdlib-only, no third-party crypto. Repo rule.
- One package, no goroutines, no exec. The user-visible enroll flow lives elsewhere; this package is the primitive.
- Atomic writes mirror `internal/auditlog/logger.go`'s `Rotate` and `internal/settingsjson/atomic_write.go` patterns: temp + rename, mode 0600, refuse-clobber for the key.
- The `AuthSnapshot` mirror struct keeps identity↔ccproxy decoupled. Production wiring does the field-by-field map at the call site (one or two lines of code) — not hidden inside this package.
- The hex vector in `TestAccountFingerprint_KnownVector` and `TestEnrollPreimage_KnownHexVector` are spec-pin tests: a future drift in either definition forces an explicit code review.
- `Record.Version` is present from day one so `LoadRecord` can dispatch on a version field when v2 lands. No code today.
- Open question deferred: OS-keychain-backed Signer plugs into the existing `Signer` interface without changing call sites. Future plan, no scaffolding now.
