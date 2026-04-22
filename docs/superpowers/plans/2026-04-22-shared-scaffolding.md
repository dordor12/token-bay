# Shared Library — Scaffolding Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scaffold `shared/` as a Go module that holds wire-format types, exhaustion-proof bundle types, signing helpers, and identity types — the contract surface used by both `plugin/` and `tracker/`. No feature logic yet; just the module structure and a smoke test per package.

**Architecture:** Small Go library, no external dependencies beyond stdlib + testify (for tests). Every type that crosses the network between plugin and tracker, or between trackers, lives here. Breaking changes require coordinated PR updates across consumers.

**Tech Stack:**
- Go 1.23+
- stdlib `crypto/ed25519` for signing
- stdlib `encoding/json` + stdlib `crypto/sha256`
- `stretchr/testify` for test assertions
- No third-party dependencies otherwise

**Dependency order:** Runs after the **monorepo foundation plan**. Must complete before plugin and tracker plans (they'll `require shared`).

---

## Table of contents

1. [Package layout](#1-package-layout)
2. [shared/CLAUDE.md](#2-sharedclaudemd)
3. [Scaffolding tasks](#3-scaffolding-tasks)

---

## 1. Package layout

```
shared/
├── CLAUDE.md
├── Makefile
├── go.mod
├── go.sum
├── ids/                     # identity ID types: Ed25519 pubkey hashes, opaque bytes32 wrappers
│   ├── id.go
│   └── id_test.go
├── signing/                 # Ed25519 sign + verify helpers; canonical serialization primitives
│   ├── ed25519.go
│   ├── ed25519_test.go
│   ├── canonical.go
│   └── canonical_test.go
├── proto/                   # network-wire message types (consumer↔tracker + tracker↔tracker)
│   ├── envelope.go          # broker_request envelope
│   ├── envelope_test.go
│   ├── ledger.go            # ledger Entry
│   ├── ledger_test.go
│   ├── federation.go        # peer handshake, root attestation, transfer proof
│   └── federation_test.go
└── exhaustionproof/         # v1 two-signal bundle; v2 Claude-Code-attestation stub
    ├── v1.go
    ├── v1_test.go
    ├── v2.go                # types only; upstream primitive TBD
    └── v2_test.go
```

### 1.1 Package responsibilities

| Package | What lives here | What does NOT live here |
|---|---|---|
| `ids` | `IdentityID`, `RequestID`, `NonceID`, `TrackerID` — strong-typed wrappers around opaque byte arrays. | Keypairs; those are in `signing`. |
| `signing` | Ed25519 sign/verify helpers. Canonical-serialization utilities (deterministic bytes for signing). | Key storage, keychain integration — plugin-local concern. |
| `proto` | Serializable structs for broker-request envelopes, ledger entries, federation messages. Includes `MarshalJSON` / `UnmarshalJSON` if needed, plus canonical-bytes helpers. | Tracker-specific state (e.g., in-flight request tracking); broker selection logic. |
| `exhaustionproof` | `ProofV1` struct (two-signal bundle), `ProofV2` struct (Claude-Code-attestation — types exist even if upstream primitive doesn't yet). | The plugin-side capture logic (that's in `plugin/internal/ratelimit`); validator logic (that's in `tracker/internal/broker`). |

### 1.2 What stays in `shared` vs what splits

**Stays in shared:** any struct that is serialized to the wire by one module and deserialized by another. Every spec-defined message format.

**Does NOT go in shared:**
- Business logic (broker selection, reputation scoring).
- State machines (offer state, tunnel state).
- I/O (network, disk).
- Component-specific config structures.

Rule of thumb: if renaming a field in `shared` breaks both `plugin/` and `tracker/`, it belongs in `shared`. If a type is used in one module only, it stays in that module.

### 1.3 Module boundary discipline

`shared/` has **zero dependencies on `plugin/` or `tracker/`** (it's a leaf library). Try to import and you'll create a cycle — Go will refuse.

---

## 2. shared/CLAUDE.md

The file that goes at `shared/CLAUDE.md` — Claude Code reads this when working inside the shared module.

````markdown
# shared — Development Context

## What this is

The shared Go library used by `plugin/` and `tracker/`. Everything in here is a contract: changes are breaking by default and require updating both consumers in the same PR.

## Non-negotiable rules

1. **No imports from `plugin/` or `tracker/`.** This is a leaf module.
2. **No state, no I/O.** Pure types + pure helper functions. Nothing that reads a file, opens a socket, or holds a mutable struct beyond the span of a function call.
3. **No third-party crypto.** Ed25519 uses stdlib. No libsodium, no OpenSSL.
4. **Canonical serialization is canonical.** If two parties serialize the same struct, the bytes must be byte-identical — that's what "canonical" means, and it's what signatures rely on. Tests in `signing/canonical_test.go` enforce this with round-trip + re-encode equality.
5. **Breaking change checklist.** Renaming or removing a field in `proto/` or `exhaustionproof/` requires: update all callers in `plugin/` and `tracker/`; add a test ensuring the new format can be parsed; bump the module version comment in `go.mod`; note the change in the commit message with `!` (e.g., `refactor!: rename IdentityID → NodeID`).

## Tech stack

Just Go stdlib + `github.com/stretchr/testify` for tests. That's it.

## What TDD looks like here

Every exported type has a test. Every helper function has a test. The tests usually follow one of three shapes:

1. **Construct → canonical-bytes → verify equality.** For any type that gets signed.
2. **Round-trip serialization.** `Marshal(x)` then `Unmarshal(bytes)` must produce `x` exactly.
3. **Rejection of tampered input.** Flip a bit, ensure the validator catches it.

Tests live in the same directory as the code, as `*_test.go` files.

## Commands

| Command | Effect |
|---|---|
| `make test` | `go test -race ./...` |
| `make lint` | `golangci-lint run ./...` |
| `make check` | test + lint |
````

---

## 3. Scaffolding tasks

### Task 1: Initialize the shared module

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/go.mod`
- Remove: `/Users/dor.amid/git/token-bay/shared/.gitkeep`

- [ ] **Step 1: Remove the placeholder `.gitkeep`**

Run:
```bash
cd /Users/dor.amid/git/token-bay
rm shared/.gitkeep
```

- [ ] **Step 2: Initialize the Go module**

Run:
```bash
cd /Users/dor.amid/git/token-bay/shared
go mod init github.com/YOUR_ORG/token-bay/shared
```

(Replace `YOUR_ORG` with the eventual GitHub org; same placeholder used across the repo.)

- [ ] **Step 3: Add testify**

Run:
```bash
go get github.com/stretchr/testify@latest
go mod tidy
```

- [ ] **Step 4: Register in `go.work`**

Edit `/Users/dor.amid/git/token-bay/go.work` — append:
```
use ./shared
```

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/go.mod shared/go.sum go.work
git rm shared/.gitkeep
git commit -m "chore(shared): initialize Go module"
```

### Task 2: Write `shared/CLAUDE.md`

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/CLAUDE.md`

- [ ] **Step 1: Write CLAUDE.md**

Copy the content from §2 of this plan verbatim into `/Users/dor.amid/git/token-bay/shared/CLAUDE.md`.

- [ ] **Step 2: Commit**

```bash
git add shared/CLAUDE.md
git commit -m "docs(shared): add module CLAUDE.md"
```

### Task 3: Add shared Makefile

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/Makefile`

- [ ] **Step 1: Write Makefile**

Write to `/Users/dor.amid/git/token-bay/shared/Makefile`:
```makefile
.PHONY: test lint check clean build

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

check: test lint

build:
	@echo "shared is a library; nothing to build"

clean:
	rm -f coverage.out
```

- [ ] **Step 2: Verify the empty test target runs**

Run: `make -C shared test`
Expected: `no test files` warnings — acceptable.

- [ ] **Step 3: Commit**

```bash
git add shared/Makefile
git commit -m "chore(shared): add Makefile"
```

### Task 4: Scaffold `ids` package with a smoke test

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/ids/id.go`
- Create: `/Users/dor.amid/git/token-bay/shared/ids/id_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/git/token-bay/shared/ids/id_test.go`:
```go
package ids

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityID_ZeroValue_IsZeroBytes(t *testing.T) {
	var id IdentityID
	assert.Equal(t, [32]byte{}, id.Bytes())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./shared/ids/...`
Expected: FAIL — `undefined: IdentityID`.

- [ ] **Step 3: Write minimal `id.go`**

Write to `/Users/dor.amid/git/token-bay/shared/ids/id.go`:
```go
// Package ids defines opaque identifier types used on the Token-Bay wire.
//
// Each ID is a strongly-typed wrapper around a fixed-size byte array. Strong
// typing prevents accidentally passing a tracker ID where an identity ID is
// expected, even though both are 32 bytes.
package ids

// IdentityID is an Ed25519 pubkey hash identifying a plugin instance.
type IdentityID [32]byte

// Bytes returns the underlying byte array.
func (i IdentityID) Bytes() [32]byte { return i }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./shared/ids/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/ids/
git commit -m "feat(shared/ids): add IdentityID type"
```

### Task 5: Scaffold `signing` package with a smoke test

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/signing/ed25519.go`
- Create: `/Users/dor.amid/git/token-bay/shared/signing/ed25519_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/git/token-bay/shared/signing/ed25519_test.go`:
```go
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify_ValidSignature_ReturnsTrue(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	msg := []byte("token-bay test")
	sig := ed25519.Sign(priv, msg)

	assert.True(t, Verify(pub, msg, sig))
}

func TestVerify_TamperedMessage_ReturnsFalse(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	msg := []byte("token-bay test")
	sig := ed25519.Sign(priv, msg)
	tampered := []byte("token-bay xest")

	assert.False(t, Verify(pub, tampered, sig))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./shared/signing/...`
Expected: FAIL — `undefined: Verify`.

- [ ] **Step 3: Write minimal `ed25519.go`**

Write to `/Users/dor.amid/git/token-bay/shared/signing/ed25519.go`:
```go
// Package signing provides Ed25519 sign and verify helpers plus
// canonical serialization primitives used throughout Token-Bay.
//
// Using stdlib crypto/ed25519 directly would work, but these wrappers
// exist so call sites can't accidentally use an unchecked signature
// or a non-canonical byte preimage.
package signing

import "crypto/ed25519"

// Verify returns true iff sig is a valid Ed25519 signature over msg
// under pub. Returns false on any length mismatch or signature error.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./shared/signing/...`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add shared/signing/
git commit -m "feat(shared/signing): add Ed25519 Verify helper"
```

### Task 6: Scaffold `proto` package with a smoke placeholder

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/proto/proto.go`
- Create: `/Users/dor.amid/git/token-bay/shared/proto/proto_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/git/token-bay/shared/proto/proto_test.go`:
```go
package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProtocolVersion_IsOne(t *testing.T) {
	assert.Equal(t, uint16(1), ProtocolVersion)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./shared/proto/...`
Expected: FAIL — `undefined: ProtocolVersion`.

- [ ] **Step 3: Write minimal `proto.go`**

Write to `/Users/dor.amid/git/token-bay/shared/proto/proto.go`:
```go
// Package proto holds the wire-format types exchanged across the Token-Bay network:
//   - broker_request envelopes (consumer → tracker)
//   - ledger Entry structs (written by trackers)
//   - federation messages (tracker ↔ tracker)
//
// Actual message types are added as they are implemented in subsequent
// feature plans. This file exists so the package compiles and carries
// the protocol version constant.
package proto

// ProtocolVersion is the current wire-format version.
// Bump when making breaking changes to any message in this package.
const ProtocolVersion uint16 = 1
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./shared/proto/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/proto/
git commit -m "feat(shared/proto): scaffold package with ProtocolVersion"
```

### Task 7: Scaffold `exhaustionproof` package with a smoke placeholder

**Files:**
- Create: `/Users/dor.amid/git/token-bay/shared/exhaustionproof/exhaustionproof.go`
- Create: `/Users/dor.amid/git/token-bay/shared/exhaustionproof/exhaustionproof_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/git/token-bay/shared/exhaustionproof/exhaustionproof_test.go`:
```go
package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersionV1_IsOne(t *testing.T) {
	assert.Equal(t, uint8(1), VersionV1)
}

func TestVersionV2_IsTwo(t *testing.T) {
	assert.Equal(t, uint8(2), VersionV2)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./shared/exhaustionproof/...`
Expected: FAIL — `undefined: VersionV1`.

- [ ] **Step 3: Write minimal package file**

Write to `/Users/dor.amid/git/token-bay/shared/exhaustionproof/exhaustionproof.go`:
```go
// Package exhaustionproof holds the types for the rate-limit exhaustion proof
// attached to broker_request envelopes.
//
// v1 is the two-signal bundle (StopFailure payload + /usage probe).
// v2 is the Claude-Code-issued signed attestation (pending upstream primitive).
//
// See docs/superpowers/specs/exhaustion-proof/ for the authoritative spec.
package exhaustionproof

// Version constants for the exhaustion proof format.
const (
	VersionV1 uint8 = 1
	VersionV2 uint8 = 2
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./shared/exhaustionproof/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/exhaustionproof/
git commit -m "feat(shared/exhaustionproof): scaffold package with version constants"
```

### Task 8: Run full module check

**Files:**
- None (verification only)

- [ ] **Step 1: Run module tests**

Run: `make -C shared test`
Expected: all tests pass across `ids`, `signing`, `proto`, `exhaustionproof`.

- [ ] **Step 2: Run module lint**

Run: `make -C shared lint`
Expected: clean (no lint errors).

- [ ] **Step 3: Run root `make check`**

Run: `make check` (from repo root)
Expected: delegates to `shared` module, all green. plugin and tracker still unscaffolded — their Makefiles don't exist yet, root Makefile skips them.

- [ ] **Step 4: Tag**

```bash
git tag -a shared-v0 -m "Shared library scaffolding complete"
```

---

## Self-review

- **Spec coverage:** `ids` covers identity IDs used throughout; `signing` covers Ed25519 + canonical bytes framework; `proto` is the home for all wire structs (stubs only at this stage); `exhaustionproof` houses v1 and v2 proof types (stubs only).
- **Placeholder scan:** Each package ships with a real working test, not a `t.Skip`. Full feature types are intentionally deferred to feature plans.
- **Dependencies:** Only stdlib + testify. No external crypto, no third-party serialization.
- **Module boundary discipline:** Zero imports from `plugin/` or `tracker/`. Enforced by package layout — those modules don't exist as Go packages until their plans complete.

## Next plan

After this plan completes, proceed to the **plugin scaffolding plan** (`2026-04-22-plugin-scaffolding.md`, revised). The plugin's `go.mod` will `require shared` so plugin code can import `github.com/YOUR_ORG/token-bay/shared/ids`, etc.
