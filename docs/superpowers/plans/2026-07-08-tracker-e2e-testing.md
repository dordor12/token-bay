# Tracker End-to-End Testing (Docker) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the tracker's real end-to-end gaps (P1–P7) test-first, then prove the fixes with a Docker-based e2e suite that stands up two real regional trackers, a controllable consumer + seeder actor, and a Byzantine federation neighbor, driving 20 protocol scenarios to verify the tracker is deploy-ready.

**Architecture:** Product fixes land first in ascending-risk order (each with module-level unit/integration coverage before any Docker work). Then a multi-module actor/harness layer: a consumer/seeder actor binary in the plugin module (imports `plugin/internal/{trackerclient,tunnel,identity,envelopebuilder,exhaustionproofbuilder}`), a Byzantine `fedactor` and Go test driver in the tracker module (`tracker/test/e2e/`, `//go:build e2e`), a key/config generator (`e2egen`), and a `docker compose` topology. The driver shells out to `docker compose` (no testcontainers dependency) and asserts via admin HTTP, the BALANCE RPC, `/metrics`, `docker exec sqlite3`, and `docker logs`.

**Tech Stack:** Go 1.25/1.26 (toolchain go1.26.2), QUIC via `quic-go`, protobuf (`google.golang.org/protobuf`), SQLite via `modernc.org/sqlite` (pure-Go), Ed25519 stdlib only, cobra/viper+YAML, zerolog, prometheus/promhttp, testify, Docker + `docker compose` v2.

## Global Constraints

- **No third-party crypto.** Ed25519 via stdlib `crypto/ed25519` only. No libsodium/OpenSSL. (repo rule 1)
- **No Anthropic API key handling** anywhere — actors must not need a `claude` binary or Anthropic key. (repo rule 2)
- **Shared wire-format types live in `shared/`** — never duplicate a wire struct between `plugin/` and `tracker/`. (repo rule 3)
- **Breaking `shared/` changes are coordinated**: a PR that modifies `shared/` MUST update `plugin/` and `tracker/` callers in the SAME commit, and mark breaking proto changes with `!` in the conventional-commit subject. (repo rule 4, shared rule 5)
- **Append-only ledger and audit logs**: never rewrite/truncate; rotation-by-file only. (repo rule 5)
- **All sign/verify of proto messages routes through `shared/signing.DeterministicMarshal`** — the single canonical-bytes choke point. (shared rule 6)
- **TDD**: failing test first, green, refactor, commit. One conventional-commit per red-green cycle (`feat:`/`fix:`/`test:`/`refactor:`/`docs:`/`chore:`/`ci:`). Commits small; do not cross component boundaries unless a coordinated `shared/` change requires it.
- **Race detector always on** for `broker`, `session`, `federation`, `admission`, `reputation`, `ledger` — flakes there are real bugs. `make -C tracker test` runs `go test -race`.
- **Proto regen**: after any `.proto` edit run `make -C shared proto-gen` (requires `protoc` + `protoc-gen-go` on PATH; the target hard-fails if absent) and commit the regenerated `.pb.go`; CI `proto-check` fails on any diff. Then `go work sync` and rebuild all three modules.
- **Config loader rejects unknown YAML fields** (`KnownFields(true)`); base all node configs on `tracker/internal/config/testdata/full.yaml`.
- **Two distinct 32-byte identity encodings** — never interchange them:
  - **Federation `tracker_id` / transfer ids** = `sha256(raw Ed25519 pubkey)`.
  - **mTLS pin / plugin `IdentityID`** = `sha256(x509 DER SubjectPublicKeyInfo)` (`server.SPKIToIdentityID`).
- **e2e Go tests carry `//go:build e2e`** so `make -C tracker test` (`go test ./...` on the 3-OS CI matrix incl. Docker-less macOS/Windows) never compiles them.
- **Authoritative spec:** `docs/superpowers/specs/2026-07-08-tracker-e2e-testing-design.md`. Read it before starting. Every code citation in this plan was verified against the working tree at commit `56316c2`.

---

## Phase A — Product fixes (P1–P7), test-first

### Task 1: P1 — Dockerfile builds (base bump, .dockerignore, data dir chown)

**Files:**
- Modify: `tracker/deployments/docker/Dockerfile`
- Create: `.dockerignore` (repo root — the build context root, since `make -C tracker docker` builds with context `../`)

**Interfaces:**
- Consumes: nothing.
- Produces: a `token-bay-tracker:dev` image that builds successfully and whose runtime `/data` is writable by uid 1000; consumed by Tasks 20–24 (harness/compose).

- [ ] **Step 1: Reproduce the failure**

Run: `make -C tracker docker`
Expected: FAIL with `go: go.work requires go >= 1.25.0 (running go 1.23.12; GOTOOLCHAIN=local)`.

- [ ] **Step 2: Read the current Dockerfile**

Read `tracker/deployments/docker/Dockerfile`. Confirm line 3 (or wherever) is `FROM golang:1.23-alpine ... AS builder` and the runtime stage is `FROM alpine:3.20` with `USER 1000:1000` and `ENTRYPOINT ["/usr/local/bin/token-bay-tracker"]`.

- [ ] **Step 3: Bump the builder base image**

Change the builder `FROM` line:
```dockerfile
FROM golang:1.26-alpine AS builder
```
(Was `golang:1.23-alpine`. `golang:1.26-alpine` ships go1.26.x ≥ the `go 1.25.0` workspace floor, so `go work sync` no longer aborts and no toolchain download is needed.)

- [ ] **Step 4: Add data-dir creation + chown to the runtime stage**

In the runtime stage, BEFORE the `USER 1000:1000` line, add:
```dockerfile
RUN mkdir -p /data && chown 1000:1000 /data
```
(Docker's copy-on-first-use then makes fresh named volumes mounted at `/data` writable by uid 1000. Verified empirically that named volumes are otherwise root-owned and unwritable by the non-root user.)

- [ ] **Step 5: Create the repo-root `.dockerignore`**

Create `.dockerignore`:
```
.git
.github
.claude
.superpowers
docs
bin
**/bin
coverage.out
**/*.test
.token-bay-local
**/.token-bay-local
go.work.local
node_modules
```
(Keeps `COPY . .` from busting the single build-cache layer on every doc/commit edit. `go.work`, `shared/`, `plugin/`, `tracker/` sources are NOT ignored — the build needs them.)

- [ ] **Step 6: Verify the image builds**

Run: `make -C tracker docker`
Expected: PASS — build completes, `token-bay-tracker:dev` tagged. (Takes ~20s locally.)

- [ ] **Step 7: Verify the data dir is writable by uid 1000**

Run: `docker run --rm --entrypoint sh token-bay-tracker:dev -c 'id -u && touch /data/x && echo OK'`
Expected: prints `1000` then `OK`.

- [ ] **Step 8: Commit**

```bash
git add tracker/deployments/docker/Dockerfile .dockerignore
git commit -m "fix(tracker): Dockerfile builds on go1.26 base + writable data dir"
```

---

### Task 2: P2 — Seeder register-on-connect

**Files:**
- Modify: `tracker/internal/server/server.go` (`serveConn`, ~lines 190–204)
- Test: `tracker/internal/server/server_test.go` (black-box `package server_test`)

**Interfaces:**
- Consumes: `server.Deps.Registry *registry.Registry` (already present, `server.go:42`); `registry.SeederRecord`, `(*registry.Registry).Register`, `.Deregister`, `.Get` (`registry.go:57,68,63`); `server.Connection.PeerID()/RemoteAddr()` (`connection.go:55,62`).
- Produces: every plugin peer that connects is upserted into the registry as a `SeederRecord{IdentityID, NetCoords.ExternalAddr=observed addr, Available=false, LastHeartbeat=now}`, and deregistered on disconnect. This makes `ADVERTISE`/`Heartbeat`/`Match` reachable — consumed by Task 3 (offer loop) and the broker-assignment path.

**Design decision (from spec §3 P2):** register-on-connect (not upsert-on-advertise) so the reflexive address is captured from the live QUIC connection and `ADVERTISE`/`Heartbeat` stay pure updates. The server cannot distinguish role at connect (mTLS gives only identity+pubkey; `EnrollRequest.Role` is never persisted), so EVERY connecting peer is registered with `Available=false`. A consumer that never advertises stays `Available=false` and is invisible to broker selection (`Match` with `RequireAvailable=true`); only a seeder that sends `ADVERTISE` becomes selectable. This is the intended two-step: Register makes a peer addressable, Advertise makes it selectable.

- [ ] **Step 1: Write the failing test**

Add to `tracker/internal/server/server_test.go` (mirror `TestServer_PeerPubkey`'s real-listener style — `go srv.Run`, `waitForListen`, `dialClientWithCert`, `waitForPeers`; derive `peerID` via `sha256` of the parsed client cert's `RawSubjectPublicKeyInfo` exactly as that test does; NO testify):
```go
func TestServer_RegistersPeerOnConnect(t *testing.T) {
	reg, err := registry.New(8)
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	srv, clientKey := newTestServerWithRegistry(t, reg) // helper: validDeps() + Registry: reg
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	waitForListen(t, srv)

	conn := dialClientWithCert(t, srv.ListenAddr(), clientKey)
	defer conn.CloseWithError(0, "done")
	waitForPeers(t, srv, 1, 2*time.Second)

	peerID := identityIDFromKey(t, clientKey) // sha256(cert RawSubjectPublicKeyInfo)
	rec, ok := reg.Get(peerID)
	if !ok {
		t.Fatal("expected seeder record after connect")
	}
	if rec.Available {
		t.Error("record must be Available=false until ADVERTISE")
	}
	if !rec.NetCoords.ExternalAddr.IsValid() {
		t.Error("record must carry the observed reflexive addr")
	}

	// Disconnect → deregister.
	conn.CloseWithError(0, "bye")
	waitForPeers(t, srv, 0, 2*time.Second)
	if _, ok := reg.Get(peerID); ok {
		t.Error("expected deregister on disconnect")
	}
}
```
Add the `newTestServerWithRegistry` and `identityIDFromKey` helpers if not present (copy the cert-parse from `TestServer_PeerPubkey`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/server/ -run TestServer_RegistersPeerOnConnect -v` (from `tracker/`)
Expected: FAIL — `expected seeder record after connect` (nothing registers today).

- [ ] **Step 3: Implement register-on-connect in `serveConn`**

In `tracker/internal/server/server.go`, right after the `connsByPeer[peerID] = c` insert and the `"server: peer connected"` log (~line 196), add:
```go
if s.deps.Registry != nil {
	s.deps.Registry.Register(registry.SeederRecord{
		IdentityID:    peerID,
		NetCoords:     registry.NetCoords{ExternalAddr: addr},
		Available:     false,
		LastHeartbeat: s.deps.Now(),
	})
}
```
In the `defer` disconnect-cleanup block (right after `delete(s.connsByPeer, peerID)`), add:
```go
if s.deps.Registry != nil {
	s.deps.Registry.Deregister(peerID)
}
```
Add the `registry` import if not already present. (Guard for nil because unit tests build `Deps` without a Registry — the heartbeat path already nil-checks the same way.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/server/ -run TestServer_RegistersPeerOnConnect -v`
Expected: PASS.

- [ ] **Step 5: Run the full server + registry suites (no regressions)**

Run: `go test -race ./internal/server/... ./internal/registry/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/server/server.go tracker/internal/server/server_test.go
git commit -m "fix(tracker): register seeders in registry on plugin connect"
```

---

### Task 3: P2 (cont.) — Broker-assignment integration test

**Files:**
- Test: `tracker/test/integration/broker_assign_test.go` (new, `//go:build integration`, `package integration_test`)

**Interfaces:**
- Consumes: the `newFixture` harness (`tracker/test/integration/helpers_test.go:68`) which brings up a real `*server.Server` + real ledger SQLite + registry + `api.Router` on `127.0.0.1:0` with a SPKI-pinned QUIC `dial`; `registry.Register` now happens on connect (Task 2).
- Produces: proof that a connected+advertised seeder becomes selectable by the broker and a `BrokerRequest` yields a `SeederAssignment` (not `NoCapacity`).

- [ ] **Step 1: Write the failing test**

Create `tracker/test/integration/broker_assign_test.go`. Using the fixture pattern: enroll+connect a seeder client, send `ADVERTISE` (Available=true, headroom≥0.2, tier bit0, matching model), then enroll+connect a consumer, build a valid `EnvelopeSigned` (fresh balance snapshot, priced model, credits covering max cost), send `BROKER_REQUEST`, assert the response oneof is `SeederAssignment` with a non-empty `SeederAddr` and the advertised seeder's pubkey. (Follow `broker_e2e_test.go` for the envelope-build + dispatch idiom.)

- [ ] **Step 2: Run to verify it fails or passes**

Run: `make -C tracker test-integration` (or `go test -race -tags=integration ./test/integration/ -run TestIntegration_BrokerAssign -v`)
Expected: After Task 2, this should PASS if the seeder registers, advertises, and is matched. If it FAILS with `NoCapacity`, the gap is real — debug registry population/match before proceeding.

- [ ] **Step 3: Commit**

```bash
git add tracker/test/integration/broker_assign_test.go
git commit -m "test(tracker): integration proof of broker seeder assignment"
```

---

### Task 4: P3 — Add `consumer_ephemeral_pub` to `EnvelopeBody` (shared proto change)

**Files:**
- Modify: `shared/proto/envelope.proto`
- Regenerate: `shared/proto/envelope.pb.go` (via `make -C shared proto-gen`)
- Modify: `shared/proto/validate.go` (`ValidateEnvelopeBody`)
- Modify: `shared/proto/testdata/envelope_signed.golden.hex` (regen), `shared/proto/envelope_test.go` (fixture)
- Test: `shared/proto/validate_test.go`

**Interfaces:**
- Consumes: current `EnvelopeBody` (highest field = 11 `nonce`; next free = 12).
- Produces: `EnvelopeBody.ConsumerEphemeralPub []byte` (field 12, 0-or-32 bytes, part of the signed body). Consumed by Task 5 (broker copies it into `OfferPush`), the consumer actor (Task 18 sets it), and the seeder actor (uses it as tunnel `PeerPin`).

**Coordinated `shared/` change (repo rule 4):** this breaks `EnvelopeBody` wire bytes → the envelope golden vector AND every plugin/tracker caller compiling against `EnvelopeBody`. All caller updates (Task 5) go in the SAME commit; mark the subject with `!`.

- [ ] **Step 1: Write the failing validate test**

In `shared/proto/validate_test.go` add:
```go
func TestValidateEnvelopeBody_EphemeralPubLength(t *testing.T) {
	b := fixtureEnvelopeBody() // existing helper
	b.ConsumerEphemeralPub = make([]byte, 31) // wrong length
	require.Error(t, ValidateEnvelopeBody(b))
	b.ConsumerEphemeralPub = make([]byte, 32)  // valid
	require.NoError(t, ValidateEnvelopeBody(b))
	b.ConsumerEphemeralPub = nil                // absent is allowed (0)
	require.NoError(t, ValidateEnvelopeBody(b))
}
```

- [ ] **Step 2: Run — expect COMPILE failure**

Run: `go test ./proto/ -run TestValidateEnvelopeBody_EphemeralPubLength` (from `shared/`)
Expected: FAIL to compile — `b.ConsumerEphemeralPub undefined`.

- [ ] **Step 3: Add the proto field**

In `shared/proto/envelope.proto`, inside `message EnvelopeBody`, after the `nonce = 11` field:
```proto
  // consumer_ephemeral_pub is the consumer's per-session Ed25519 pubkey for
  // the seeder tunnel TLS handshake. 0 bytes (legacy) or 32. Signed as part
  // of the body so the tracker can forward an authenticated ephemeral key to
  // the seeder (OfferPush.consumer_ephemeral_pub) for tunnel pinning.
  bytes consumer_ephemeral_pub = 12;
```

- [ ] **Step 4: Regenerate the proto**

Run: `make -C shared proto-gen`
Expected: `shared/proto/envelope.pb.go` now has `ConsumerEphemeralPub []byte`. (If `protoc`/`protoc-gen-go` are missing, install them first — the target hard-fails otherwise.)

- [ ] **Step 5: Enforce the length in `ValidateEnvelopeBody`**

In `shared/proto/validate.go`, inside `ValidateEnvelopeBody`, add (near the other length checks):
```go
if n := len(b.ConsumerEphemeralPub); n != 0 && n != 32 {
	return fmt.Errorf("proto: consumer_ephemeral_pub length %d, want 0 or 32", n)
}
```

- [ ] **Step 6: Run the validate test**

Run: `go test ./proto/ -run TestValidateEnvelopeBody_EphemeralPubLength -v`
Expected: PASS.

- [ ] **Step 7: Refresh the golden vector**

Add `ConsumerEphemeralPub` to `fixtureEnvelopeBody` (`envelope_test.go:21`) with a deterministic 32-byte value, then regenerate:
Run: `UPDATE_GOLDEN=1 go test ./proto/ -run TestEnvelopeSigned_GoldenBytes` (from `shared/`)
Then review the hex diff in `shared/proto/testdata/envelope_signed.golden.hex` manually. Re-run without the env var to confirm PASS.

- [ ] **Step 8: Commit is deferred** — this compiles in `shared/` but breaks plugin/tracker callers; the caller update in Task 5 lands in the SAME commit.

Run: `go test ./proto/...` (from `shared/`) — Expected: PASS. Do NOT commit yet.

---

### Task 5: P3 (cont.) — Broker copies ephemeral pub + request_id into `OfferPush`; consumer/seeder wire-up

**Files:**
- Modify: `tracker/internal/broker/offer_loop.go` (`runOffer` — build `OfferPush`)
- Modify: `tracker/internal/broker/broker.go` (pass `request_id`/reservation token + ephemeral pub into the offer)
- Modify: `plugin/internal/envelopebuilder/envelopebuilder.go` (`RequestSpec` + `Build` set `ConsumerEphemeralPub`)
- Test: `tracker/internal/broker/offer_loop_test.go`; `plugin/internal/envelopebuilder/envelopebuilder_test.go`

**Interfaces:**
- Consumes: `EnvelopeBody.ConsumerEphemeralPub` (Task 4); `OfferPush.consumer_ephemeral_pub` (field 6, already exists in `rpc.proto`); `OfferPush.request_id` — **check if it exists**; if not, add it (field 7) in this task (coordinated shared change, same commit).
- Produces: `OfferPush` carrying the consumer's authenticated ephemeral pubkey and the tracker's `request_id`, so the seeder can pin the tunnel and reference the correct request in `UsageReport`.

- [ ] **Step 1: Confirm/produce `OfferPush.request_id`**

Read `shared/proto/rpc.proto` `message OfferPush`. It has `consumer_ephemeral_pub = 6` but NO `request_id`. Add:
```proto
  bytes request_id = 7; // 16 bytes; the tracker's reservation token for this offer
```
Regenerate: `make -C shared proto-gen`. Update `ValidateOfferPush` (`shared/proto/validate.go:317`) to accept `request_id` of length 0-or-16 (0 for legacy). This is part of the SAME coordinated commit as Task 4.

- [ ] **Step 2: Write the failing broker offer-loop test**

In `tracker/internal/broker/offer_loop_test.go`, extend the `fakePusher` capture (it records the `*OfferPush` it received). Add:
```go
func TestRunOffer_PopulatesEphemeralAndRequestID(t *testing.T) {
	fp := &fakePusher{offerCh: make(chan *tbproto.OfferDecision, 1), ok: true}
	fp.offerCh <- &tbproto.OfferDecision{Accept: true, EphemeralPubkey: make([]byte, 32)}
	body := &tbproto.EnvelopeBody{ /* ...valid... */ ConsumerEphemeralPub: bytes.Repeat([]byte{7}, 32)}
	var reqID [16]byte
	reqID[0] = 0xAB
	_, _, err := runOffer(context.Background(), fp, someSeederID, body, someEnvHash, reqID, time.Second)
	require.NoError(t, err)
	require.Equal(t, body.ConsumerEphemeralPub, fp.lastPush.ConsumerEphemeralPub)
	require.Equal(t, reqID[:], fp.lastPush.RequestId)
}
```
(Note: `runOffer`'s signature gains a `reqID [16]byte` param — update the test call accordingly. Capture `lastPush` in `fakePusher.PushOfferTo`.)

- [ ] **Step 3: Run to verify it fails (compile)**

Run: `go test -race ./internal/broker/ -run TestRunOffer_PopulatesEphemeralAndRequestID` (from `tracker/`)
Expected: FAIL to compile — `runOffer` arity mismatch / `lastPush` undefined.

- [ ] **Step 4: Implement**

In `tracker/internal/broker/offer_loop.go`, change `runOffer` to accept `reqID [16]byte` and set the new `OfferPush` fields:
```go
func runOffer(ctx context.Context, pusher PushService, seederID ids.IdentityID,
	body *tbproto.EnvelopeBody, envHash [32]byte, reqID [16]byte, timeout time.Duration,
) (accept bool, ephemeralPub []byte, err error) {
	push := &tbproto.OfferPush{
		ConsumerId:          body.ConsumerId,
		EnvelopeHash:        envHash[:],
		Model:               body.Model,
		MaxInputTokens:      uint32(body.MaxInputTokens),
		MaxOutputTokens:     uint32(body.MaxOutputTokens),
		ConsumerEphemeralPub: body.ConsumerEphemeralPub,
		RequestId:           reqID[:],
	}
	// ... unchanged from here ...
}
```
In `tracker/internal/broker/broker.go`, update the `runOffer(...)` call site to pass the reservation token / request id it already has in scope (the `reqID [16]byte` used for the reservation). Trace where `Submit` derives the request id (the reservation token becomes `SeederAssignment.ReservationToken`, `broker.go:280`) and pass that same value.

- [ ] **Step 5: Run broker tests**

Run: `go test -race ./internal/broker/`
Expected: PASS (fix any other `runOffer` callers/tests broken by the arity change).

- [ ] **Step 6: Write + pass the envelopebuilder test**

In `plugin/internal/envelopebuilder/envelopebuilder_test.go` add a test asserting `Build` copies `RequestSpec.ConsumerEphemeralPub` into the signed `EnvelopeBody.ConsumerEphemeralPub`. Then add a `ConsumerEphemeralPub []byte` field to `RequestSpec` and set `body.ConsumerEphemeralPub = spec.ConsumerEphemeralPub` in `Build` (`envelopebuilder.go:77`). Run `go test ./internal/envelopebuilder/` (from `plugin/`) → PASS.

- [ ] **Step 7: Build all three modules**

Run (from repo root): `go work sync && go build ./... || (cd shared && go build ./...) ` — build each module: `cd shared && go build ./... && cd ../plugin && go build ./... && cd ../tracker && go build ./...`
Expected: all compile. Fix any remaining `EnvelopeBody`/`OfferPush` callers.

- [ ] **Step 8: Full test sweep for the coordinated change**

Run: `make test` (from repo root — runs all three modules with `-race`).
Expected: PASS.

- [ ] **Step 9: Commit (coordinated, breaking) — Task 4 + Task 5 together**

```bash
git add shared/proto/envelope.proto shared/proto/envelope.pb.go shared/proto/rpc.proto shared/proto/rpc.pb.go \
        shared/proto/validate.go shared/proto/validate_test.go shared/proto/envelope_test.go \
        shared/proto/testdata/envelope_signed.golden.hex \
        tracker/internal/broker/offer_loop.go tracker/internal/broker/offer_loop_test.go tracker/internal/broker/broker.go \
        plugin/internal/envelopebuilder/envelopebuilder.go plugin/internal/envelopebuilder/envelopebuilder_test.go
git commit -m "feat(shared)!: carry consumer ephemeral pubkey + request_id through offer

EnvelopeBody gains consumer_ephemeral_pub (field 12); OfferPush gains
request_id (field 7). Broker forwards both to the seeder so the tunnel
listener can pin the consumer and the usage report references the right
request. Updates plugin envelopebuilder + tracker broker callers."
```

---

### Task 6: P4 — Canonical usage-assertion preimage in `shared/signing`

**Files:**
- Create/Modify: `shared/signing/usage_assertion.go` (new)
- Modify: `shared/proto/rpc.proto` if a `UsageAssertion` sub-message is needed, OR define the canonical byte layout over existing `UsageReport` fields (preferred — no new proto message)
- Test: `shared/signing/usage_assertion_test.go`

**Interfaces:**
- Consumes: `shared/signing.DeterministicMarshal`; the `UsageReport` fields `{request_id, model, input_tokens, output_tokens}` plus tracker-supplied `{consumer_id, seeder_id, cost_credits}`.
- Produces:
  - `signing.CanonicalUsageAssertionPreSig(a UsageAssertion) ([]byte, error)` — sequencing-independent preimage (NO `prev_hash`/`seq`/`timestamp`/`flags`).
  - `signing.SignUsageAssertion(priv ed25519.PrivateKey, a UsageAssertion) ([]byte, error)`
  - `signing.VerifyUsageAssertion(pub ed25519.PublicKey, a UsageAssertion, sig []byte) bool`
  - `type UsageAssertion struct { RequestID []byte; ConsumerID []byte; SeederID []byte; Model string; InputTokens, OutputTokens uint32; CostCredits uint64 }`
  Consumed by Task 7 (tracker settlement verify) and Task 8 (seeder actor / plugin seederflow sign).

**Design (spec §3 P4):** both participant sigs are over the sequencing-independent usage-assertion. The seeder signs it in `UsageReport`; the `SettlementPush` preimage the consumer counter-signs is the same assertion. The tracker verifies both, THEN builds the full `EntryBody` (assigning prev_hash/seq/timestamp) and tracker-signs the chain hash.

- [ ] **Step 1: Write the failing signing test**

Create `shared/signing/usage_assertion_test.go`:
```go
func TestUsageAssertion_SignVerify_RoundTrip(t *testing.T) {
	seed := bytes.Repeat([]byte{9}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	a := UsageAssertion{
		RequestID: bytes.Repeat([]byte{1}, 16), ConsumerID: bytes.Repeat([]byte{2}, 32),
		SeederID: bytes.Repeat([]byte{3}, 32), Model: "claude-sonnet-4-6",
		InputTokens: 100, OutputTokens: 50, CostCredits: 1500,
	}
	sig, err := SignUsageAssertion(priv, a)
	require.NoError(t, err)
	require.True(t, VerifyUsageAssertion(priv.Public().(ed25519.PublicKey), a, sig))
	a.OutputTokens = 51 // tamper
	require.False(t, VerifyUsageAssertion(priv.Public().(ed25519.PublicKey), a, sig))
}

func TestCanonicalUsageAssertion_Deterministic(t *testing.T) {
	a := UsageAssertion{RequestID: bytes.Repeat([]byte{1}, 16), ConsumerID: bytes.Repeat([]byte{2}, 32),
		SeederID: bytes.Repeat([]byte{3}, 32), Model: "m", InputTokens: 1, OutputTokens: 2, CostCredits: 3}
	b1, err := CanonicalUsageAssertionPreSig(a)
	require.NoError(t, err)
	b2, _ := CanonicalUsageAssertionPreSig(a)
	require.Equal(t, b1, b2)
	require.NotEmpty(t, b1)
}
```

- [ ] **Step 2: Run — expect compile FAIL**

Run: `go test ./signing/ -run TestUsageAssertion` (from `shared/`)
Expected: FAIL to compile — undefined symbols.

- [ ] **Step 3: Implement `shared/signing/usage_assertion.go`**

Model the shape EXACTLY on `SignEntry`/`VerifyEntry` (`shared/signing/proto.go:94,112`): `len(priv)!=ed25519.PrivateKeySize` → `fmt.Errorf`; nil/short fields → `errors.New`; canonical bytes via a domain-tagged layout; then `ed25519.Sign`. `Verify` is nil-safe, returns bool, never panics.
```go
package signing

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
)

// usageAssertionDomain namespaces the usage-assertion preimage so a signature
// over it can never be replayed as any other Token-Bay message.
const usageAssertionDomain = "token-bay/usage-assertion:v1"

// UsageAssertion is the sequencing-independent settlement authorization both
// the seeder (in UsageReport) and the consumer (counter-sig) sign. It omits
// prev_hash/seq/timestamp/flags — those are ledger sequencing the tracker
// assigns at append time.
type UsageAssertion struct {
	RequestID    []byte // 16
	ConsumerID   []byte // 32
	SeederID     []byte // 32
	Model        string
	InputTokens  uint32
	OutputTokens uint32
	CostCredits  uint64
}

func CanonicalUsageAssertionPreSig(a UsageAssertion) ([]byte, error) {
	if len(a.RequestID) != 16 {
		return nil, errors.New("signing: usage assertion request_id must be 16 bytes")
	}
	if len(a.ConsumerID) != 32 || len(a.SeederID) != 32 {
		return nil, errors.New("signing: usage assertion consumer_id/seeder_id must be 32 bytes")
	}
	if a.Model == "" {
		return nil, errors.New("signing: usage assertion model empty")
	}
	// domain \0 request_id \0 consumer_id \0 seeder_id \0 model \0 in(8) out(8) cost(8)
	var buf []byte
	buf = append(buf, usageAssertionDomain...)
	buf = append(buf, 0)
	buf = append(buf, a.RequestID...)
	buf = append(buf, 0)
	buf = append(buf, a.ConsumerID...)
	buf = append(buf, 0)
	buf = append(buf, a.SeederID...)
	buf = append(buf, 0)
	buf = append(buf, a.Model...)
	buf = append(buf, 0)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(a.InputTokens))
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], uint64(a.OutputTokens))
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], a.CostCredits)
	buf = append(buf, n[:]...)
	return buf, nil
}

func SignUsageAssertion(priv ed25519.PrivateKey, a UsageAssertion) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: bad private key size %d", len(priv))
	}
	msg, err := CanonicalUsageAssertionPreSig(a)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, msg), nil
}

func VerifyUsageAssertion(pub ed25519.PublicKey, a UsageAssertion, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg, err := CanonicalUsageAssertionPreSig(a)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}
```
(Uses a documented byte layout rather than a proto message, avoiding a new wire type. The layout mirrors the existing plugin `usageReportPreimage` convention but adds `consumer_id`/`seeder_id`/`cost_credits` so both parties bind the full economic intent.)

- [ ] **Step 4: Run — PASS**

Run: `go test -race ./signing/ -run TestUsageAssertion -v` and `-run TestCanonicalUsageAssertion`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/signing/usage_assertion.go shared/signing/usage_assertion_test.go
git commit -m "feat(shared): canonical usage-assertion signing helper for settlement"
```

---

### Task 7: P4 (cont.) — Tracker settlement verifies usage-assertion + honest consumer-sig entry

**Files:**
- Modify: `tracker/internal/broker/settlement.go` (`HandleUsageReport`, `awaitSettle`, `appendUsageEntry`, `verifyConsumerSig`)
- Test: `tracker/internal/broker/settlement_test.go`

**Interfaces:**
- Consumes: `signing.VerifyUsageAssertion` (Task 6); `ledger.UsageRecord` (already has `ConsumerSig`/`ConsumerPub`/`ConsumerSigMissing` fields, `usage.go`); `deps.Identity.PeerPubkey` (consumer pubkey resolver, wired in `run_cmd.go`).
- Produces: both settlement paths write a valid ledger entry — happy path (consumer counter-signs → entry with `consumer_sig` present, `ConsumerSigMissing=false`) and dispute path (timeout → entry with empty `consumer_sig`, `ConsumerSigMissing=true`). The seeder sig is verified over the usage-assertion, not the sequencing-dependent `EntryBody`.

**Root cause (verified):** `settlement.go:189` hardcodes `ConsumerSigMissing:true` into the body the seeder signs, and `appendUsageEntry` (`settlement.go:309`) never sets `UsageRecord.ConsumerSig`/`ConsumerPub`. So a verified consumer sig is dropped, and the ledger re-verifies the seeder sig over a body whose flag bit differs → `AppendUsage` fails. Fix: verify participant sigs over the usage-assertion; let the ledger own the `EntryBody` flag; pass the consumer sig+pub through when verified.

- [ ] **Step 1: Write the failing happy-path test**

In `tracker/internal/broker/settlement_test.go` (follow the existing settlement test harness — a fake ledger/`session.Manager`/`Identity` resolver). Add a test that: registers an inflight request with a known consumer pubkey resolver, has the seeder sign the **usage-assertion** (via `signing.SignUsageAssertion`), calls `HandleUsageReport`, then simulates the consumer `Settle` with a valid usage-assertion counter-sig, and asserts the ledger received a `UsageRecord` with `ConsumerSigMissing=false`, non-empty `ConsumerSig`, and a valid `ConsumerPub`. Add a second test for the timeout path asserting `ConsumerSigMissing=true` and empty `ConsumerSig`. Add a third for tampered consumer sig → `ErrConsumerSig`, no ledger append.

- [ ] **Step 2: Run — expect FAIL**

Run: `go test -race ./internal/broker/ -run TestSettlement_HappyPath_ConsumerSigPresent -v`
Expected: FAIL — today the happy path drops the consumer sig / append errors.

- [ ] **Step 3: Change seeder-sig verification to the usage-assertion**

In `HandleUsageReport` (`settlement.go`), replace the `entry.BuildUsageEntry(...ConsumerSigMissing:true...)` + `signing.VerifyEntry(seederPub, body, r.SeederSig)` block (steps 6–7, lines ~166–198) with:
- Build the `signing.UsageAssertion{RequestID:r.RequestId, ConsumerID:req.ConsumerID[:], SeederID:req.AssignedSeeder[:], Model:r.Model, InputTokens:r.InputTokens, OutputTokens:r.OutputTokens, CostCredits:actualCost}`.
- Verify `signing.VerifyUsageAssertion(ed25519.PublicKey(req.SeederPubkey), assertion, r.SeederSig)`; on failure return `ErrSeederSigInvalid`.
- The `SettlementPush` preimage becomes `CanonicalUsageAssertionPreSig(assertion)`; `preimage_hash = sha256(that)`. Store the assertion (or its canonical bytes) in `pendingSettle` so `HandleSettle` verifies the consumer's counter-sig over the same bytes.

- [ ] **Step 4: Make `appendUsageEntry` honest about the consumer sig**

Change `awaitSettle`/`appendUsageEntry` so that when the consumer sig verified, the `UsageRecord` carries `ConsumerSigMissing:false, ConsumerSig:<consumer sig over assertion>, ConsumerPub:<resolved pub>`; on timeout/refused-unverifiable it carries `ConsumerSigMissing:true` and empty `ConsumerSig`.

**Important ledger-contract note:** `ledger.AppendUsage` currently verifies `ConsumerSig` via `signing.VerifyEntry(ConsumerPub, EntryBody, sig)` (`append.go:124`) — i.e. over the `EntryBody`, NOT the usage-assertion. Since the participant now signs the assertion, the ledger must verify the consumer/seeder authorization over the **assertion**, not the `EntryBody`. Update `ledger.AppendUsage` (Task 7b below) to verify participant sigs over `CanonicalUsageAssertionPreSig` and keep only the tracker sig over the `EntryBody`. Do this in the same task.

- [ ] **Step 4b: Update ledger AppendUsage to verify participant sigs over the assertion**

In `tracker/internal/ledger/usage.go` + `append.go`: `AppendUsage` builds the `UsageAssertion` from the `UsageRecord` fields, verifies `signing.VerifyUsageAssertion(SeederPub, assertion, SeederSig)` and (when `!ConsumerSigMissing`) `VerifyUsageAssertion(ConsumerPub, assertion, ConsumerSig)`, then builds the `EntryBody` (with `flags` bit0 = `ConsumerSigMissing`), tracker-signs the body, and stores. Remove the `EntryBody`-domain participant verification from `appendLocked` for the USAGE kind (keep it for kinds that still use it, or route USAGE through a dedicated path). Add a ledger unit test in `tracker/internal/ledger/usage_test.go` for both paths (use `openTempLedger`, `IssueStarterGrant` to fund the consumer).

- [ ] **Step 5: Run the settlement + ledger tests**

Run: `go test -race ./internal/broker/ ./internal/ledger/...`
Expected: PASS (all three settlement scenarios + ledger both-paths).

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/broker/settlement.go tracker/internal/broker/settlement_test.go \
        tracker/internal/ledger/usage.go tracker/internal/ledger/append.go tracker/internal/ledger/usage_test.go
git commit -m "fix(tracker): settlement produces valid ledger entry on both consumer-sig paths"
```

---

### Task 8: P4 (cont.) — Plugin seederflow + consumerflow sign the usage-assertion

**Files:**
- Modify: `plugin/internal/seederflow/serve.go` (`reportUsage`, replace `usageReportPreimage`)
- Modify: `plugin/internal/consumerflow/settlement.go` (`HandleSettlement` counter-signs the assertion)
- Test: `plugin/internal/seederflow/serve_test.go`, `plugin/internal/consumerflow/settlement_test.go`

**Interfaces:**
- Consumes: `signing.SignUsageAssertion` / `UsageAssertion` (Task 6). The seeder knows `request_id` (from `OfferPush.request_id`, Task 5), `model`, token counts; it must also know `consumer_id`, `seeder_id`, and `cost_credits` to build the assertion. **Gap:** the seeder does not know `cost_credits` (tracker-side pricing) or `consumer_id` reliably. Resolve: the assertion the seeder signs uses the fields it has; the tracker recomputes `cost_credits` and rejects if the seeder's differ. **Simplify:** have the seeder sign the assertion with `CostCredits` it computes from a mirrored price table OR redefine the shared assertion so the seeder-signed portion excludes `cost_credits` (tracker binds cost separately). **Decision:** keep `cost_credits` OUT of the seeder's signed assertion is cleaner, but the consumer must also sign the same fields. Adopt: the canonical assertion includes `cost_credits`; the seeder obtains it via a mirrored `pricing` table (the actor mirrors `broker.DefaultPriceTable`) — matches spec intent that both sides sign the economic intent. If mirroring proves brittle, fall back to excluding cost from the signature and binding it via the tracker-only `EntryBody`.
- Produces: real plugin seeder/consumer that produce assertion sigs the fixed tracker accepts. (The e2e actor in Task 18 reuses this logic; the real plugin is updated for consistency per repo rule 4 since `shared/signing` changed.)

- [ ] **Step 1: Write failing seederflow test**

In `plugin/internal/seederflow/serve_test.go`, assert `reportUsage` produces a `UsageReport.SeederSig` that `signing.VerifyUsageAssertion` accepts under the seeder's signing key for the assertion built from the report fields.

- [ ] **Step 2: Run — FAIL**

Run: `go test ./internal/seederflow/ -run TestReportUsage_SignsAssertion -v` (from `plugin/`)
Expected: FAIL (today it signs the custom `usageReportPreimage`).

- [ ] **Step 3: Implement**

Replace `usageReportPreimage` usage in `serve.go` with `signing.SignUsageAssertion(c.cfg.Signer.PrivateKey()...)` — but note the seeder signs with the **ephemeral offer key** (the tracker verifies with `req.SeederPubkey` = ephemeral pub, Task 7). So sign with the per-offer ephemeral private key, not the long-lived identity key. Thread the ephemeral key from the offer/serve path. Build the assertion from `{request_id (from offer), consumer_id (from offer), seeder_id (own identity id), model, tokens, cost_credits (mirrored price table)}`.

- [ ] **Step 4: Update consumerflow settlement counter-sign**

In `plugin/internal/consumerflow/settlement.go` `HandleSettlement`: the `SettlementPush.preimage_body` is now the canonical usage-assertion bytes; verify `sha256(body)==preimage_hash`, then sign `body` with the consumer's **identity** key (the tracker resolves the consumer pubkey via mTLS) and send via `Settle`. (Confirm which key the tracker verifies the consumer sig under in Task 7 — the mTLS-resolved consumer pubkey = identity key.)

- [ ] **Step 5: Run plugin tests**

Run: `go test -race ./internal/seederflow/ ./internal/consumerflow/` (from `plugin/`)
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugin/internal/seederflow/serve.go plugin/internal/seederflow/serve_test.go \
        plugin/internal/consumerflow/settlement.go plugin/internal/consumerflow/settlement_test.go
git commit -m "fix(plugin): sign/verify usage-assertion in settlement flow"
```

---

### Task 9: P5 — Ledger transfer verifies consumer authz over the transfer-proof-request intent

**Files:**
- Modify: `tracker/internal/ledger/transfer.go` (`AppendTransferOut` — verify consumer sig over `CanonicalTransferProofRequestPreSig`, add identity binding)
- Test: `tracker/internal/ledger/transfer_test.go`

**Interfaces:**
- Consumes: `fed.CanonicalTransferProofRequestPreSig` (`shared/federation/signing_transfer.go:22`); `ledger.TransferOutRecord` (has `ConsumerSig`/`ConsumerPub`).
- Produces: `AppendTransferOut` that verifies the consumer's authorization as the sig over the transfer-proof-request intent (sequencing-independent), binds `sha256(x509 DER SPKI of ConsumerPub) == IdentityID`, and debits that identity. Consumed by Task 10 (federation LedgerHooks adapter).

**Root cause (verified):** `AppendTransferOut` verifies `ConsumerSig` over the `EntryBody` (append.go domain), but federation only holds the consumer sig over `CanonicalTransferProofRequestPreSig`. A pass-through always fails `ledger: consumer_sig invalid`. Same principle as P4: participant authz is over a sequencing-independent canonical intent.

- [ ] **Step 1: Write the failing test**

In `tracker/internal/ledger/transfer_test.go`: fund an identity via `IssueStarterGrant`; build a `TransferProofRequest`; sign `CanonicalTransferProofRequestPreSig` with the consumer key; call `AppendTransferOut` with `ConsumerSig`/`ConsumerPub` set and `IdentityID = sha256(DER SPKI of pub)`; assert success + balance debited. Add negative tests: wrong `ConsumerPub` (binding fails), insufficient balance (no partial apply).

- [ ] **Step 2: Run — FAIL**

Run: `go test -race ./internal/ledger/ -run TestAppendTransferOut_VerifiesIntent -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `AppendTransferOut`: (a) verify `sha256(x509.MarshalPKIXPublicKey(r.ConsumerPub)) == r.IdentityID` else return a binding error; (b) verify `signing.Verify(r.ConsumerPub, fed.CanonicalTransferProofRequestPreSig(<reconstructed request>), r.ConsumerSig)` — the record must carry enough fields to reconstruct the request (amount, nonce/ref, source tracker id, identity). Extend `TransferOutRecord` with the fields needed to rebuild the canonical request, or accept the pre-computed canonical bytes. Keep the tracker-only sig over the `EntryBody`. Debit `r.IdentityID`'s balance.

- [ ] **Step 4: Run — PASS**

Run: `go test -race ./internal/ledger/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/ledger/transfer.go tracker/internal/ledger/transfer_test.go
git commit -m "fix(tracker): transfer_out verifies consumer intent + binds identity to pubkey"
```

---

### Task 10: P5 (cont.) — Wire `federation.Deps.Ledger` with a real LedgerHooks adapter + dest idempotency

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/federation_adapters.go` (new `ledgerHooksAdapter`)
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go` (set `Ledger:` in the federation `Deps` literal, ~lines 203–219)
- Modify: `tracker/internal/federation/transfer.go` (dest-side completed-transfer cache keyed by `ref=nonce`)
- Test: `tracker/internal/federation/transfer_test.go`; `tracker/test/integration/transfer_integration_test.go`

**Interfaces:**
- Consumes: `federation.LedgerHooks` interface (`ledger_hooks.go:18`: `AppendTransferOut(ctx, TransferOutHookIn) (TransferOutHookOut, error)`, `AppendTransferIn(ctx, TransferInHookIn) error`); `ledger.AppendTransferOut`/`AppendTransferIn` (Task 9); `led.Tip` for prev/seq; `entry.Hash` for `TransferOutHookOut.ChainTipHash`.
- Produces: a production `LedgerHooks` implementer (the first one — only test fakes exist today) that bridges federation transfer hooks to the real ledger with a bounded `ErrStaleTip` retry loop, and idempotent replay at both ends. Enables `Federation.StartTransfer` (dest) and inbound `TRANSFER_PROOF_REQUEST` (source).

- [ ] **Step 1: Write the failing federation transfer test (real ledger)**

In `tracker/internal/federation/transfer_test.go` or a new integration test, use the in-proc transport (`NewInprocHub`/`NewInprocTransport`) with TWO `federation.Open` instances, BOTH with `Deps.Ledger` set to the new adapter over real `openTempLedger`-backed ledgers. Fund the source identity. Drive `StartTransfer`; assert `transfer_out` at source + `transfer_in` at dest, balances move by N, `TransferProof` returned. (Copy the `transfer_api_integration_test.go` / `newTwoTrackerWithLedgers` pattern but swap fakes for the real adapter.)

- [ ] **Step 2: Run — FAIL**

Run: `go test -race ./internal/federation/ -run TestTransfer_RealLedger_HappyPath -v`
Expected: FAIL — `ErrTransferDisabled` (Ledger not wired) or adapter missing.

- [ ] **Step 3: Implement the `ledgerHooksAdapter`**

In `tracker/cmd/token-bay-tracker/federation_adapters.go`, add (follow `transferFederationAdapter` conventions, `federation_adapters.go:88`):
```go
type ledgerHooksAdapter struct{ led *ledger.Ledger }

func (a ledgerHooksAdapter) AppendTransferOut(ctx context.Context, in federation.TransferOutHookIn) (federation.TransferOutHookOut, error) {
	for i := 0; i < staleTipRetries; i++ {
		seq, tipHash, hasTip, err := a.led.Tip(ctx)
		if err != nil {
			return federation.TransferOutHookOut{}, err
		}
		prev := tipHash
		next := seq + 1
		if !hasTip { prev = make([]byte, 32); next = 1 }
		e, err := a.led.AppendTransferOut(ctx, ledger.TransferOutRecord{
			PrevHash: prev, Seq: next, ConsumerID: in.IdentityID[:], Amount: in.Amount,
			Timestamp: in.Timestamp, TransferRef: in.TransferRef[:],
			ConsumerSig: in.ConsumerSig, ConsumerPub: in.ConsumerPub,
		})
		if errors.Is(err, ledger.ErrStaleTip) { continue }
		if err != nil { return federation.TransferOutHookOut{}, err }
		h, err := entry.Hash(e.Body)
		if err != nil { return federation.TransferOutHookOut{}, err }
		return federation.TransferOutHookOut{ChainTipHash: h, Seq: e.Body.Seq}, nil
	}
	return federation.TransferOutHookOut{}, ledger.ErrStaleTip
}

func (a ledgerHooksAdapter) AppendTransferIn(ctx context.Context, in federation.TransferInHookIn) error {
	for i := 0; i < staleTipRetries; i++ {
		seq, tipHash, hasTip, err := a.led.Tip(ctx)
		if err != nil { return err }
		prev := tipHash; next := seq + 1
		if !hasTip { prev = make([]byte, 32); next = 1 }
		_, err = a.led.AppendTransferIn(ctx, ledger.TransferInRecord{
			PrevHash: prev, Seq: next, IdentityID: in.IdentityID[:], Amount: in.Amount,
			Timestamp: in.Timestamp, TransferRef: in.TransferRef[:],
		})
		if errors.Is(err, ledger.ErrStaleTip) { continue }
		return err
	}
	return ledger.ErrStaleTip
}
```
Add the compile-time assertion to the `var ( _ ... )` block (`federation_adapters.go:125`): `_ federation.LedgerHooks = ledgerHooksAdapter{}`.

- [ ] **Step 4: Wire it in `run_cmd.go`**

In the federation `Deps` literal (`run_cmd.go:203-219`) add:
```go
Ledger: ledgerHooksAdapter{led: led},
```
(`led` is the `*ledger.Ledger` created at `run_cmd.go:74`.)

- [ ] **Step 5: Add dest-side idempotency cache**

In `tracker/internal/federation/transfer.go`, add a `completed` map (keyed by `ref=nonce`) on the dest side that short-circuits a replayed `StartTransfer` to return the cached proof/ack without a second `AppendTransferIn`. Add the on-chain `ref` existence check path (the ledger already reserves `ErrTransferRefExists`). Add a replay test asserting no double-credit.

- [ ] **Step 6: Run — PASS**

Run: `go test -race ./internal/federation/... ./internal/ledger/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add tracker/cmd/token-bay-tracker/federation_adapters.go tracker/cmd/token-bay-tracker/run_cmd.go \
        tracker/internal/federation/transfer.go tracker/internal/federation/transfer_test.go \
        tracker/test/integration/transfer_integration_test.go
git commit -m "feat(tracker): enable cross-region transfer (ledger hooks + idempotency)"
```

---

### Task 11: P6 — Serve `/metrics`

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go` (add a third HTTP listener on `cfg.Metrics.ListenAddr`)
- Test: `tracker/test/integration/metrics_test.go` (new, `//go:build integration`) OR a `run_cmd` test

**Interfaces:**
- Consumes: `promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{})` (`prometheus/client_golang` already a direct dep, `tracker/go.mod:11`); `cfg.Metrics.ListenAddr` (default `:9100`, always non-empty at runtime).
- Produces: an unauthenticated `/metrics` endpoint exposing all `DefaultRegisterer` collectors (broker, federation, reputation, admission, ledger-integrity, bootstrap). Consumed by scenarios 8, 12, 20.

- [ ] **Step 1: Write the failing test**

In a new integration test, boot a tracker (or the run composition) with `metrics.listen_addr: 127.0.0.1:0`, GET `/metrics`, assert HTTP 200 and body contains a known counter name (e.g. `tokenbay_federation_` or a broker counter). Assert NO auth is required (no bearer token).

- [ ] **Step 2: Run — FAIL**

Expected: connection refused / 404 — no listener today.

- [ ] **Step 3: Implement**

In `run_cmd.go`, mirror the admin-server lifecycle (`run_cmd.go:349-375`): build `metricsMux := http.NewServeMux(); metricsMux.Handle("/metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))`, `metricsSrv := &http.Server{Addr: cfg.Metrics.ListenAddr, Handler: metricsMux}`, launch `go func(){ metricsErrCh <- metricsSrv.ListenAndServe() }()`, add a `case err := <-metricsErrCh:` select branch, and `_ = metricsSrv.Shutdown(graceCtx)` in every exit branch. Treat `http.ErrServerClosed` as nil. Add the `promhttp` import. Do NOT put `/metrics` behind the admin bearer guard.

- [ ] **Step 4: Run — PASS**

Run: `make -C tracker test-integration` (or the specific test)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/cmd/token-bay-tracker/run_cmd.go tracker/test/integration/metrics_test.go
git commit -m "feat(tracker): serve prometheus /metrics on metrics.listen_addr"
```

---

### Task 12: P7 — Reputation Freeze/Unfreeze methods

**Files:**
- Modify: `tracker/internal/reputation/ingest.go` (add exported `Freeze`/`Unfreeze`)
- Modify: `tracker/internal/reputation/storage_state.go` (add `clearFrozen` bypassing `canTransition` for unfreeze)
- Test: `tracker/internal/reputation/*_test.go`

**Interfaces:**
- Consumes: `s.breachMu`, `s.store.transition`, `s.ensureState`, `s.refreshOne`, `s.notifyFreeze` (all unexported in-package); `ReasonRecord`; `State` (OK=0, AUDIT=1, FROZEN=2).
- Produces:
  - `(*Subsystem).Freeze(ctx context.Context, id ids.IdentityID, operator string) error`
  - `(*Subsystem).Unfreeze(ctx context.Context, id ids.IdentityID, operator string) error`
  Consumed by Task 13 (admin route). Freeze emits the federation REVOCATION via `notifyFreeze`.

- [ ] **Step 1: Write the failing test**

In `tracker/internal/reputation/` (in-package test, tempdir SQLite, `WithClock`): assert `Freeze` sets state FROZEN, `IsFrozen` returns true after (calls `refreshOne`), a `FreezeListener` receives `OnFreeze`. Assert `Unfreeze` returns state to OK and `IsFrozen` false. Assert `reasons` stays append-only (a `manual` reason appended, none rewritten).

- [ ] **Step 2: Run — FAIL (compile)**

Run: `go test -race ./internal/reputation/ -run TestFreezeUnfreeze -v`
Expected: FAIL — `Freeze`/`Unfreeze` undefined.

- [ ] **Step 3: Implement `Freeze`**

Model on `RecordCategoricalBreach` (`ingest.go:167`): guard `s.closed.Load()`; `s.breachMu.Lock()`/defer unlock; `s.ensureState(ctx,id,now)`; build `ReasonRecord{Kind:"manual", Operator:operator, At:now.Unix()}`; `s.store.transition(ctx, id, StateFrozen, reason, now)`; `s.refreshOne(ctx,id)`; then `s.notifyFreeze(ctx, id, "operator", now)` to gossip the REVOCATION. Reason `"operator"` → `REVOCATION_REASON_MANUAL` via `mapReputationReasonToProto`.

- [ ] **Step 4: Implement `Unfreeze` + `clearFrozen`**

`canTransition` blocks leaving FROZEN (`state.go:56`), so add a `storage.clearFrozen(ctx,id,reason,now)` method that runs the same UPDATE to `StateOK` but bypasses `canTransition` (append a `manual` reason, never rewrite). `Unfreeze` calls it under `breachMu`, then `refreshOne`. No revocation emit on unfreeze (v1: revocations are not un-gossiped; document).

- [ ] **Step 5: Run — PASS**

Run: `go test -race ./internal/reputation/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/reputation/ingest.go tracker/internal/reputation/storage_state.go tracker/internal/reputation/*_test.go
git commit -m "feat(tracker): reputation Freeze/Unfreeze with revocation on freeze"
```

---

### Task 13: P7 (cont.) — Admin freeze/unfreeze routes

**Files:**
- Modify: `tracker/internal/admin/server.go` (add `ReputationActions` interface + optional Deps field + routes in `buildMux`)
- Create: `tracker/cmd/token-bay-tracker/admin_reputation_adapter.go` (hex→id adapter)
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go` (`buildAdminServer` signature + call site to thread `rep`)
- Test: `tracker/internal/admin/*_test.go`

**Interfaces:**
- Consumes: `reputation.Subsystem.Freeze/Unfreeze` (Task 12); admin patterns `bearerGuard`, `writeJSON`, `writeError`, `parseHexID`, Go 1.22 `mux.Handle("POST /identity/{id}/freeze", ...)`.
- Produces: authenticated `POST /identity/{id}/freeze` and `POST /identity/{id}/unfreeze` (202 on success, 501 when `Reputation` nil). Consumed by scenario 13.

- [ ] **Step 1: Write the failing admin test**

In `tracker/internal/admin/` (in-package, `httptest.NewServer(srv.Handler())`): with a fake `ReputationActions`, POST `/identity/<hex>/freeze` with a valid bearer → 202 and the fake's `Freeze(idHex, op)` was called; unauthenticated → 401; nil `Reputation` Deps → 501.

- [ ] **Step 2: Run — FAIL**

Run: `go test -race ./internal/admin/ -run TestFreezeRoute -v`
Expected: FAIL — route not registered.

- [ ] **Step 3: Implement admin side**

Follow `FederationActions` verbatim (admin stays free of `internal/reputation` imports): define `type ReputationActions interface { Freeze(idHex, operator string) error; Unfreeze(idHex, operator string) error }`; add optional `Reputation ReputationActions` to `admin.Deps`; in `buildMux` gate the two routes on `if s.deps.Reputation != nil` (else the routes return 501 or are absent — mirror the FederationActions gating). Handlers read `r.PathValue("id")`, call the action, log `admin_freeze`/`admin_unfreeze`, return 202 via `writeJSON`.

- [ ] **Step 4: Implement the cmd-side adapter + wiring**

Create `admin_reputation_adapter.go`: a struct wrapping `rep *reputation.Subsystem` translating `idHex → ids.IdentityID` (via `parseHexID` equivalent) and calling `rep.Freeze`/`rep.Unfreeze` with a fixed operator string. Thread `rep` into `buildAdminServer` (add the param to its signature `run_cmd.go:506` and the call site `run_cmd.go:342` — `rep` is already in scope) and set `Reputation: reputationAdminActions{rep}` in the `admin.Deps` literal.

- [ ] **Step 5: Run — PASS**

Run: `go test -race ./internal/admin/... && cd .. && go build ./tracker/...`
Expected: PASS + compiles.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/admin/server.go tracker/internal/admin/*_test.go \
        tracker/cmd/token-bay-tracker/admin_reputation_adapter.go tracker/cmd/token-bay-tracker/run_cmd.go
git commit -m "feat(tracker): admin freeze/unfreeze routes for identities"
```

---

### Task 14: Phase A gate — full `make check` green

- [ ] **Step 1: Run the whole repo suite + lint**

Run: `make check` (from repo root — `make test` + `make lint` across all three modules).
Expected: PASS. Fix any lint/test failures before proceeding to the harness.

- [ ] **Step 2: Confirm the tracker image still builds after all fixes**

Run: `make -C tracker docker`
Expected: PASS.

- [ ] **Step 3: Commit any lint fixes**

```bash
git add -A && git commit -m "chore(tracker): lint/test fixups after P1-P7"  # only if needed
```

---

## Phase B — E2e harness

### Task 15: `e2egen` — key + config generator

**Files:**
- Create: `tracker/test/e2e/cmd/e2egen/main.go`
- Create: `tracker/test/e2e/cmd/e2egen/render.go` (config templating)
- Create: `tracker/test/e2e/cmd/e2egen/render_test.go`
- Reference: `tracker/internal/config/testdata/full.yaml` (base for the rendered configs)

**Interfaces:**
- Consumes: `crypto/ed25519`, `crypto/sha256`, `shared/ids`; the config YAML schema (base on `full.yaml`).
- Produces: a CLI `e2egen --out <dir> --seed-a <hex> --seed-b <hex> --seed-fed <hex>` that writes: `identity-a.key`, `identity-b.key`, `identity-fed.key` (raw 64-byte Ed25519, mode 0644 so container uid 1000 can read a bind-mounted file), `tracker-a.yaml`, `tracker-b.yaml`. Deterministic given seeds (no `Date.now`/random). Consumed by the compose build (Task 22) and the driver (Task 24).

**Key facts (verified):**
- Identity key file = **raw 64 bytes** from `ed25519.GenerateKey` (or `NewKeyFromSeed`), NOT PEM/hex — matches `helpers_test.go` (`os.WriteFile(path, priv, 0o600)`).
- Federation `tracker_id = sha256(rawPubkey)` (hex, 64 chars); peer entry = `{tracker_id, pubkey (hex raw pub), addr, region}`.
- The 5 listeners (`server`, `admin`, `metrics`, `stun`, `turn`) must be collectively distinct per container. `federation.listen_addr` is separate.
- `admission.tlog_path` parent dir must exist before `run`/`config validate`.
- Timings (spec §6): `reputation.evaluation_interval_s: 1`, `min_population_for_z_score: 3`, `z_score_threshold: 2.5`; `settlement.settlement_timeout_s: 3`, `tunnel_setup_ms: 500` (< timeout*1000), `reservation_ttl_s: 5` (>= timeout); `broker.offer_timeout_ms: 1500`; `federation.publish_cadence_s: 2`; `admin.listen_addr: 0.0.0.0:9090`; `metrics.listen_addr: 0.0.0.0:9100`; pricing table includes the models the consumer actor requests (`claude-sonnet-4-6`, `claude-opus-4-7`, `claude-haiku-4-5-20251001`).

- [ ] **Step 1: Write the failing render test**

Create `render_test.go`:
```go
func TestRender_ProducesValidConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, generate(genOpts{
		OutDir: dir,
		SeedA:  bytes.Repeat([]byte{1}, 32),
		SeedB:  bytes.Repeat([]byte{2}, 32),
		SeedFed: bytes.Repeat([]byte{3}, 32),
		AddrA:  "tracker-a", AddrB: "tracker-b", FedAddrActor: "fedactor",
	}))
	// keys are raw 64 bytes
	kb, _ := os.ReadFile(filepath.Join(dir, "identity-a.key"))
	require.Len(t, kb, ed25519.PrivateKeySize)
	// config parses + validates via the real loader
	raw, _ := os.ReadFile(filepath.Join(dir, "tracker-a.yaml"))
	cfg, err := config.Parse(bytes.NewReader(raw))
	require.NoError(t, err)
	config.ApplyDefaults(cfg)
	// data_dir parent must be present for Validate; generate() mkdir's it
	require.NoError(t, config.Validate(cfg))
	// A's peer list contains B with tracker_id == sha256(B pub)
	// ... assert federation.peers[0].tracker_id == hex(sha256(pubB))
}
```

- [ ] **Step 2: Run — FAIL (compile)**

Run: `go test ./test/e2e/cmd/e2egen/ -run TestRender` (from `tracker/`)
Expected: FAIL — `generate` undefined.

- [ ] **Step 3: Implement key generation**

`main.go`: parse flags; for each node derive `priv := ed25519.NewKeyFromSeed(seed)`; write raw priv (64 bytes) to `identity-<x>.key` mode 0644; compute `pub`, `trackerID := sha256.Sum256(pub)`.

- [ ] **Step 4: Implement config rendering**

`render.go`: build a `config.Config` struct programmatically (do NOT hand-write YAML — marshal a struct so unknown-field drift is impossible), OR render from a template string based on `full.yaml`. Prefer building the struct and `yaml.Marshal`. Set data_dir, all 5 distinct listeners (bind `0.0.0.0`), `federation.listen_addr: 0.0.0.0:7443`, each node's `federation.peers` = the other node (+ fedactor allowlisted on tracker-a: `{tracker_id: sha256(fedPub), pubkey: fedPub, addr: fedactor:0, region: A}` — note the fedactor dials out, so its listen addr is unused by the tracker; the tracker only needs it in the allowlist to accept the inbound handshake). Apply all §6 timings. `mkdir -p` the data_dir + admission tlog parent inside `generate` so `config validate` passes.

- [ ] **Step 5: Run — PASS**

Run: `go test ./test/e2e/cmd/e2egen/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/test/e2e/cmd/e2egen/
git commit -m "feat(e2e): key + config generator for the docker topology"
```

---

### Task 16: Consumer/seeder actor — skeleton, identity, enroll, control API

**Files:**
- Create: `plugin/cmd/tokenbay-e2e-actor/main.go` (flag parsing: `--role consumer|seeder`, `--tracker-addr`, `--tracker-hash-file` (path to the hex SPKI-hash file emitted by `e2egen`; a `--tracker-hash` hex flag is also accepted for non-Docker unit tests), `--tracker-b-addr`, `--tracker-b-hash-file` (consumer transfer target), `--data-dir`, `--ctrl-addr`)
- Create: `plugin/cmd/tokenbay-e2e-actor/actor.go` (shared lifecycle: identity, trackerclient connect, enroll)
- Create: `plugin/cmd/tokenbay-e2e-actor/control.go` (HTTP control server)
- Create: `plugin/cmd/tokenbay-e2e-actor/actor_test.go`

**Interfaces:**
- Consumes: `identity.Generate`/`identity.LoadKey`/`identity.SaveKey`/`identity.BuildEnrollPayload`; `trackerclient.New/Start/WaitConnected/Close/Enroll`; `trackerclient.TrackerEndpoint{Addr, IdentityHash, Region}`, `trackerclient.Config`.
- Produces: a binary that on boot generates/loads its identity, builds a `TrackerEndpoint` literal from `--tracker-addr` + the SPKI hash read from `--tracker-hash-file` (hex), connects, enrolls (role per flag), and serves `GET /healthz` (200 once enrolled+connected) and `GET /identity` (`{identity_id_hex, pubkey_hex}`). Consumed by all consumer/seeder scenarios.

**Key facts (verified):**
- `trackerclient.New` does NOT dial — must `Start(ctx)` then `WaitConnected(ctx)` (10s timeout) before RPCs.
- Actor bypasses ALL ccbridge/consumerflow/seederflow/ccproxy: use `identity` + `trackerclient` + `tunnel` + `envelopebuilder`/`exhaustionproofbuilder` directly. The cmd-layer build funcs are `package main` unexported and NOT importable — re-implement minimal wiring.
- `TrackerEndpoint.IdentityHash` = SHA-256 of the tracker's Ed25519 **SPKI DER** (the mTLS pin) — the actor gets this from `--tracker-hash` (hex). `e2egen` computes it: `sha256(x509.MarshalPKIXPublicKey(trackerPub))`. **`e2egen` must also emit each tracker's SPKI hash hex** (add to Task 15 output, e.g. `tracker-a.spki`, `tracker-b.spki`).
- Enroll needs an `AccountFingerprint`; the real path calls `claude auth status`. The actor fabricates a fingerprint (e.g. `sha256("e2e-org-<role>")`) since the tracker does NOT verify the enroll sig/preimage/fingerprint (confirmed: `enroll.go` checks only `len(identity_pubkey)==32` and `len(account_fingerprint)==32`).

- [ ] **Step 1: Update `e2egen` to also emit SPKI-hash files**

Add to Task 15's `generate`: write `tracker-a.spki` / `tracker-b.spki` = hex(`sha256(x509.MarshalPKIXPublicKey(pub))`). Add a render_test assertion. (Small addendum commit to e2egen, or fold into this task's commit.)

- [ ] **Step 2: Write the failing actor test (against the fakeserver)**

In `actor_test.go`, reuse `plugin/internal/trackerclient/test/fakeserver` to stand up a fake tracker; boot the actor lifecycle pointed at it; assert `/healthz` returns 200 after connect+enroll and `/identity` returns a 64-hex id. (Keeps this unit test hermetic — no Docker.)

- [ ] **Step 3: Run — FAIL**

Run: `go test ./cmd/tokenbay-e2e-actor/ -run TestActor_HealthzAfterEnroll -v` (from `plugin/`)
Expected: FAIL.

- [ ] **Step 4: Implement the lifecycle + control server**

`actor.go`: load-or-generate identity into `--data-dir`; build `trackerclient.Config{Endpoints: []TrackerEndpoint{{Addr, IdentityHash, Region}}, Identity: signer, ...}`; `Start`+`WaitConnected`; build enroll payload (`identity.BuildEnrollPayload(signer, fingerprint, role)`), call `Enroll`; record `EnrollResponse.IdentityId`. `control.go`: `net/http` server on `--ctrl-addr` with `/healthz`, `/identity`. `main.go`: wire flags → lifecycle → block.

- [ ] **Step 5: Run — PASS**

Run: `go test ./cmd/tokenbay-e2e-actor/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugin/cmd/tokenbay-e2e-actor/ tracker/test/e2e/cmd/e2egen/
git commit -m "feat(e2e): consumer/seeder actor skeleton (identity, enroll, control API)"
```

---

### Task 17: Seeder actor — advertise, offer handling, tunnel serve, usage report

**Files:**
- Create: `plugin/cmd/tokenbay-e2e-actor/seeder.go`
- Modify: `plugin/cmd/tokenbay-e2e-actor/control.go` (add `/config`, `/offers/last`, `/usage/last`)
- Test: `plugin/cmd/tokenbay-e2e-actor/seeder_test.go`

**Interfaces:**
- Consumes: `trackerclient.Advertise`, `trackerclient.OfferHandler` (registered via `Config.OfferHandler`), `trackerclient.UsageReport`; `tunnel.Listen`/`Listener.Accept`/`Tunnel.ReadRequest`/`SendOK`/`ResponseWriter`/`CloseWrite`; `signing.SignUsageAssertion` (Task 6); `OfferPush.consumer_ephemeral_pub` + `request_id` (Task 5).
- Produces: seeder behavior — advertise loop (Available/headroom/tier/models from `/config`), `OfferHandler` that generates a per-offer ephemeral key, binds a `tunnel.Listen` pinned to `(seederEphemeralPriv, consumerEphemeralPub)`, returns `OfferDecision{Accept, EphemeralPubkey}` within `offer_timeout_ms`, serves canned SSE over the tunnel, and sends `UsageReport` signed with the ephemeral key over the usage-assertion.

**Key facts (verified):**
- Advertise eligibility: `Available=true`, `headroom >= 0.2` (broker `HeadroomThreshold`), `Tiers` bit0 set (STANDARD), matching model, `Load < 5`; heartbeat is automatic (15s) — keeps the record fresh vs the 30s sweeper's 10-min window.
- Own `OfferHandler` (do NOT reuse production `seederflow.HandleOffer`, which needs its own config); it must respond within `offer_timeout_ms` (1500ms default) with `Accept=true` + 32-byte `EphemeralPubkey`.
- Tunnel wire protocol (half-duplex, ALPN `tb-tun/1`): consumer→seeder `[4-byte BE len][body]`; seeder→consumer `[1-byte status][content]` (Status 0x00 OK = verbatim SSE, 0x01 error). Seeder: `Listen → Accept → ReadRequest → SendOK → write via ResponseWriter → CloseWrite`.
- UsageReport: sign the usage-assertion with the **ephemeral offer key** (tracker verifies with `req.SeederPubkey` = ephemeral pub, per Task 7). `request_id` from `OfferPush.request_id`; `consumer_id` from `OfferPush.consumer_id`; `cost_credits` from a mirrored price table (mirror `broker.DefaultPriceTable`: `claude-opus-4-7`, `claude-sonnet-4-6`, `claude-haiku-4-5-20251001`).

- [ ] **Step 1: Write the failing test**

In `seeder_test.go`, against the fakeserver: drive an offer push, assert the actor accepts with a 32-byte ephemeral pub, binds a tunnel listener, and — when a tunnel client connects and sends a body — serves the configured canned SSE and then calls `UsageReport` with a signature that `signing.VerifyUsageAssertion` accepts under the ephemeral pub.

- [ ] **Step 2: Run — FAIL**

Run: `go test ./cmd/tokenbay-e2e-actor/ -run TestSeeder -v` (from `plugin/`)
Expected: FAIL.

- [ ] **Step 3: Implement seeder mode**

`seeder.go`: advertise loop reading `/config` state; `OfferHandler.HandleOffer` generates `ed25519` ephemeral key, `tunnel.Listen(bind 0.0.0.0:0, Config{EphemeralPriv: seederEph, PeerPin: offer.ConsumerEphemeralPub})`, spawns an accept goroutine that serves the canned SSE, returns `OfferDecision{Accept:true, EphemeralPubkey: seederEphPub}`. After serving, `UsageReport` with the ephemeral-key assertion sig. Record last offer/usage for introspection.

- [ ] **Step 4: Run — PASS**

Run: `go test ./cmd/tokenbay-e2e-actor/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/cmd/tokenbay-e2e-actor/seeder.go plugin/cmd/tokenbay-e2e-actor/control.go plugin/cmd/tokenbay-e2e-actor/seeder_test.go
git commit -m "feat(e2e): seeder actor (advertise, offer, tunnel serve, usage report)"
```

---

### Task 18: Consumer actor — request flow, tunnel dial, settlement, transfer

**Files:**
- Create: `plugin/cmd/tokenbay-e2e-actor/consumer.go`
- Modify: `plugin/cmd/tokenbay-e2e-actor/control.go` (add `/request`, `/settlement/last`, `/transfer`, `/balance`)
- Test: `plugin/cmd/tokenbay-e2e-actor/consumer_test.go`

**Interfaces:**
- Consumes: `trackerclient.Balance/BalanceCached`, `BrokerRequest`, `Settle`, `TransferRequest`; `trackerclient.SettlementHandler` (via `Config.SettlementHandler`); `envelopebuilder.NewBuilder/Build` + `RequestSpec` (now with `ConsumerEphemeralPub`, Task 5); `exhaustionproofbuilder.NewBuilder/Build`; `tunnel.Dial`/`Tunnel.Send`/`Receive`; `signing.VerifyUsageAssertion`/counter-sign helper (Task 8 logic).
- Produces: consumer behavior — `POST /request` runs Balance → build `ExhaustionProofV1` + signed `EnvelopeSigned` (with a fresh per-session ephemeral key set as `ConsumerEphemeralPub`) → `BrokerRequest`; on `SeederAssignment` dial the seeder tunnel pinned by `(consumerEphemeralPriv, assignment.SeederPubkey)`, send a canned `/v1/messages` body, read the SSE; the `SettlementHandler` counter-signs the usage-assertion and calls `Settle`. `POST /transfer` drives `TransferRequest` against the destination tracker; `GET /balance` returns the Balance RPC result.

**Key facts (verified):**
- Envelope must be accepted by the tracker: `protocol_version==1`, valid `ExhaustionProofV1` (matcher `rate_limit`, `|stop.At - probe.At| <= 60s`, 16-byte nonce — fabricated bytes are fine, tracker never inspects content), fresh (<=10min) tracker-signed balance snapshot (obtained via `Balance` RPC — the actor cannot fabricate it), priced model, credits covering `MaxInput*inPrice + MaxOutput*outPrice`.
- **Identity encoding trap:** the envelope `ConsumerId` must be the **Enroll-returned `IdentityId`** (the SPKI hash), NOT `identity.Signer.IdentityID()` (raw-pubkey hash), because the tracker resolves `PeerPubkey(env.Body.ConsumerId)` against its SPKI-keyed mTLS table. Wrap the signer so `IdentityID()` returns the enroll id while `Sign()` uses the same Ed25519 key. (Same for the `Balance` lookup key.)
- Tunnel dial: `tunnel.Dial(ctx, seederAddrPort, Config{EphemeralPriv: consumerEph, PeerPin: assignment.SeederPubkey})` → `Send(body)` → `Receive(ctx)` returns `(Status, io.Reader, err)`; read to EOF.

- [ ] **Step 1: Write the failing test**

In `consumer_test.go`, against the fakeserver (which can push a `SettlementPush`): `POST /request`, assert the actor builds a valid envelope, calls `BrokerRequest`, and (fakeserver returns a `SeederAssignment` and later pushes settlement) the actor counter-signs and calls `Settle`; assert `/settlement/last` reflects it. (Full tunnel data-plane is covered by the Docker scenario; here assert the RPC sequence.)

- [ ] **Step 2: Run — FAIL**

Run: `go test ./cmd/tokenbay-e2e-actor/ -run TestConsumer -v` (from `plugin/`)
Expected: FAIL.

- [ ] **Step 3: Implement consumer mode**

`consumer.go`: the `IdentityID`-wrapping signer; `/request` handler builds proof + envelope (fresh ephemeral key, set `ConsumerEphemeralPub`), Balance RPC, `BrokerRequest`; on assignment, `tunnel.Dial` + `Send` + `Receive`; register a `SettlementHandler` that verifies `sha256(body)==hash`, counter-signs the usage-assertion, and calls `Settle`; `/transfer` builds a `TransferRequest` and calls the RPC against the target tracker; `/balance` returns Balance.

- [ ] **Step 4: Run — PASS**

Run: `go test ./cmd/tokenbay-e2e-actor/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/cmd/tokenbay-e2e-actor/consumer.go plugin/cmd/tokenbay-e2e-actor/control.go plugin/cmd/tokenbay-e2e-actor/consumer_test.go
git commit -m "feat(e2e): consumer actor (request, tunnel dial, settlement, transfer)"
```

---

### Task 19: `fedactor` — Byzantine federation neighbor

**Files:**
- Create: `tracker/test/e2e/cmd/fedactor/main.go`
- Create: `tracker/test/e2e/cmd/fedactor/actor.go` (handshake + send helpers)
- Create: `tracker/test/e2e/cmd/fedactor/control.go`
- Test: `tracker/test/e2e/cmd/fedactor/actor_test.go`

**Interfaces:**
- Consumes (all importable — `tracker/test/e2e` is under the tracker module root, so `tracker/internal/federation` is visible): `federation.NewQUICTransport`, `QUICConfig`, `(*QUICTransport).Dial`, `PeerConn.Send/Recv`, `federation.CertFromIdentity`, `RunHandshakeDialer`, `SignEnvelope`, `MarshalFrame`, `UnmarshalFrame`; `shared/federation` message types + `fed.CanonicalRevocationPreSig`; `shared/ids.TrackerID`.
- Produces: a binary with control API `/handshake`, `/send/root-attestation`, `/send/equivocation-evidence`, `/send/revocation`, `/received`. Consumed by scenarios 12 (equivocation→depeer) and 13 (revocation propagation — as the alternative to the admin-freeze path).

**Key facts (verified):**
- `tracker_id = sha256(rawPubkey)` (must equal `Envelope.SenderId` for attestations); mTLS pin = `sha256(DER SPKI)` computed internally by `Dial(addr, expectedPeerPub)`.
- Honest attach: `NewQUICTransport(QUICConfig{Cert: CertFromIdentity(fedPriv)}).Dial(ctx, targetAddr, targetPub)` → `RunHandshakeDialer(ctx, conn, fedID, fedPriv, targetID, targetPub, timeout)`. Reuse the SAME `PeerConn` for subsequent frames.
- Envelope build: inner proto → `proto.Marshal` (NOT DeterministicMarshal for the envelope payload) → `federation.SignEnvelope(fedPriv, fedID.Bytes()[:], kind, payload)` → `MarshalFrame` → `conn.Send`.
- Equivocation: send two `KIND_ROOT_ATTESTATION` with same `TrackerId`(=own)+`Hour`(>0), DIFFERENT `MerkleRoot` (32 bytes). `TrackerSig` is never verified on receipt (arbitrary 64 bytes OK). Second conflicting root → victim `PutPeerRoot` returns `ErrPeerRootConflict` → broadcasts `EQUIVOCATION_EVIDENCE` + depeers. Send exactly TWO (rate limit ~1/s, burst covers two).
- Revocation: `Revocation.TrackerSig` IS verified — set `TrackerId`(=own), `IdentityId`(32 non-zero), `Reason` in {ABUSE=1,MANUAL=2,EXPIRED=3}, `RevokedAt>0`; `TrackerSig = ed25519.Sign(fedPriv, CanonicalRevocationPreSig(rev))`; wrap `Kind_KIND_REVOCATION`. Issuer must be in the victim's allowlist (fedactor is, via e2egen).
- The fedactor must be allowlisted on tracker-a (e2egen adds it).

- [ ] **Step 1: Write the failing test**

In `actor_test.go` (package `main` or a `_test` sibling), stand up a real `federation.Open`-based victim over the in-proc hub OR a real QUIC listener with the fedactor allowlisted; drive `/handshake`; send two conflicting root attestations; assert the victim depeers (poll the victim's `Peers()` state or a metric). (This is a heavier test — acceptable to keep it in the e2e Docker suite instead if in-proc setup is awkward; if so, assert the send helpers build valid frames here and defer the depeer assertion to scenario 12.)

- [ ] **Step 2: Run — FAIL**

Run: `go test ./test/e2e/cmd/fedactor/ -v` (from `tracker/`)
Expected: FAIL.

- [ ] **Step 3: Implement handshake + send helpers + control API**

`actor.go`: `handshake(targetAddr, targetPubHex)` dials + runs the dialer handshake, stores the live `PeerConn`; `sendRootAttestation(hour, rootHex)`, `sendEquivocationEvidence(...)`, `sendRevocation(identityHex, reason)` each build the inner proto, sign the envelope, and `conn.Send`. A background goroutine drains `conn.Recv()` into a `received` slice for `/received`. `control.go`: HTTP handlers. `main.go`: flags (`--priv-key-path`, `--ctrl-addr`) → identity load → block.

- [ ] **Step 4: Run — PASS (or defer depeer assertion to scenario 12)**

Run: `go test ./test/e2e/cmd/fedactor/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/test/e2e/cmd/fedactor/
git commit -m "feat(e2e): byzantine federation neighbor actor (fedactor)"
```

---

### Task 20: Actor Dockerfile

**Files:**
- Create: `tracker/test/e2e/Dockerfile.actors` (multi-stage, repo-root context, builds both `tokenbay-e2e-actor` and `fedactor`)

**Interfaces:**
- Consumes: repo-root build context (`go.work` + all three modules); the P1 base image pattern.
- Produces: an image containing both actor binaries (static `CGO_ENABLED=0`). Consumed by `compose.e2e.yaml` (Task 22).

- [ ] **Step 1: Write the Dockerfile**

```dockerfile
# syntax=docker/dockerfile:1.6
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY . .
RUN mkdir -p /out \
 && go work sync \
 && CGO_ENABLED=0 go build -o /out/tokenbay-e2e-actor ./plugin/cmd/tokenbay-e2e-actor \
 && CGO_ENABLED=0 go build -o /out/fedactor ./tracker/test/e2e/cmd/fedactor

FROM alpine:3.20
RUN mkdir -p /data && chown 1000:1000 /data
COPY --from=builder /out/tokenbay-e2e-actor /usr/local/bin/tokenbay-e2e-actor
COPY --from=builder /out/fedactor /usr/local/bin/fedactor
USER 1000:1000
# entrypoint set per-service in compose
```

- [ ] **Step 2: Verify both build**

Run (from repo root): `docker build -f tracker/test/e2e/Dockerfile.actors -t tokenbay-e2e-actors:dev .`
Expected: PASS; image contains both binaries (`docker run --rm --entrypoint ls tokenbay-e2e-actors:dev /usr/local/bin`).

- [ ] **Step 3: Commit**

```bash
git add tracker/test/e2e/Dockerfile.actors
git commit -m "build(e2e): docker image for actor + fedactor binaries"
```

---

### Task 21: `e2egen` build into an image + entrypoint wiring

**Files:**
- Modify: `tracker/test/e2e/Dockerfile.actors` (also build `e2egen`) OR run `e2egen` on the host in the Makefile before compose.
- Decision: run `e2egen` on the **host** (via `go run ./test/e2e/cmd/e2egen`) in the `test-e2e` make target, writing to a generated dir bind-mounted into the containers. Simpler than an init container; deterministic.

**Interfaces:**
- Consumes: `e2egen` (Task 15).
- Produces: a `tracker/test/e2e/.gen/` dir (gitignored) with keys + configs + SPKI hashes, bind-mounted read-only into the tracker/actor containers.

- [ ] **Step 1: Add `.gen/` to `.gitignore`**

Append to repo `.gitignore`: `tracker/test/e2e/.gen/`.

- [ ] **Step 2: Verify `e2egen` writes a usable dir**

Run (from `tracker/`): `go run ./test/e2e/cmd/e2egen --out test/e2e/.gen --seed-a 01..01 --seed-b 02..02 --seed-fed 03..03 --addr-a tracker-a --addr-b tracker-b`
Expected: `.gen/` contains `identity-{a,b,fed}.key`, `tracker-{a,b}.yaml`, `tracker-{a,b}.spki`.

- [ ] **Step 3: Validate the generated configs with the real binary**

Run: `docker run --rm -v "$PWD/test/e2e/.gen:/gen:ro" token-bay-tracker:dev config validate --config /gen/tracker-a.yaml`
Expected: exit 0. (Catches any unknown-field/listener-collision/data-dir issues before compose.)

- [ ] **Step 4: Commit**

```bash
git add .gitignore
git commit -m "chore(e2e): gitignore generated e2e artifacts"
```

---

### Task 22: `compose.e2e.yaml` topology

**Files:**
- Create: `tracker/test/e2e/compose.e2e.yaml`

**Interfaces:**
- Consumes: `token-bay-tracker:dev` (P1), `tokenbay-e2e-actors:dev` (Task 20), the `.gen/` dir (Task 21).
- Produces: the topology in spec §2 — `tracker-a`, `tracker-b` (federated), `consumer`, `seeder`, `fedactor`, one network, per-tracker named data volumes, healthchecks, `depends_on: service_healthy` ordering. Consumed by the driver (Task 24).

**Key facts (verified):**
- Healthcheck against `/health` MUST inject the token (health is NOT auth-exempt): `wget -q -O- --header "Authorization: Bearer $$TOKEN_BAY_ADMIN_TOKEN" http://127.0.0.1:9090/health` (busybox `wget --header` present in alpine; no curl). Admin bound `0.0.0.0:9090`.
- `TOKEN_BAY_ADMIN_TOKEN` set per tracker container.
- Data volume mounted at `/data` (uid-1000-writable via P1 chown). Config + keys bind-mounted read-only from `.gen/`.
- Each tracker uses distinct in-container ports but they're in separate containers so no collision; expose admin/metrics to the host for driver assertions.

- [ ] **Step 1: Write the compose file**

```yaml
name: tokenbay-e2e
networks:
  tokenbay-e2e:
services:
  tracker-a:
    image: token-bay-tracker:dev
    command: ["run", "--config", "/gen/tracker-a.yaml"]
    environment: { TOKEN_BAY_ADMIN_TOKEN: "e2e-admin-token-a" }
    volumes:
      - ./.gen:/gen:ro
      - tracker-a-data:/data
    ports: ["9090:9090", "9100:9100"]
    networks: [tokenbay-e2e]
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O- --header \"Authorization: Bearer $$TOKEN_BAY_ADMIN_TOKEN\" http://127.0.0.1:9090/health || exit 1"]
      interval: 2s
      timeout: 3s
      retries: 20
  tracker-b:
    image: token-bay-tracker:dev
    command: ["run", "--config", "/gen/tracker-b.yaml"]
    environment: { TOKEN_BAY_ADMIN_TOKEN: "e2e-admin-token-b" }
    volumes:
      - ./.gen:/gen:ro
      - tracker-b-data:/data
    ports: ["9091:9090", "9101:9100"]
    networks: [tokenbay-e2e]
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O- --header \"Authorization: Bearer $$TOKEN_BAY_ADMIN_TOKEN\" http://127.0.0.1:9090/health || exit 1"]
      interval: 2s
      timeout: 3s
      retries: 20
  seeder:
    image: tokenbay-e2e-actors:dev
    entrypoint: ["/usr/local/bin/tokenbay-e2e-actor"]
    command: ["--role", "seeder", "--tracker-addr", "tracker-a:7777", "--tracker-hash-file", "/gen/tracker-a.spki", "--data-dir", "/data", "--ctrl-addr", "0.0.0.0:8082"]
    volumes: ["./.gen:/gen:ro"]
    ports: ["8082:8082"]
    networks: [tokenbay-e2e]
    depends_on: { tracker-a: { condition: service_healthy } }
  consumer:
    image: tokenbay-e2e-actors:dev
    entrypoint: ["/usr/local/bin/tokenbay-e2e-actor"]
    command: ["--role", "consumer", "--tracker-addr", "tracker-a:7777", "--tracker-hash-file", "/gen/tracker-a.spki", "--data-dir", "/data", "--ctrl-addr", "0.0.0.0:8081"]
    volumes: ["./.gen:/gen:ro"]
    ports: ["8081:8081"]
    networks: [tokenbay-e2e]
    depends_on: { tracker-a: { condition: service_healthy }, seeder: { condition: service_started } }
  fedactor:
    image: tokenbay-e2e-actors:dev
    entrypoint: ["/usr/local/bin/fedactor"]
    command: ["--priv-key-path", "/gen/identity-fed.key", "--ctrl-addr", "0.0.0.0:8083"]
    volumes: ["./.gen:/gen:ro"]
    ports: ["8083:8083"]
    networks: [tokenbay-e2e]
    depends_on: { tracker-a: { condition: service_healthy } }
volumes:
  tracker-a-data:
  tracker-b-data:
```
(The consumer `POST /transfer` targets tracker-b; the actor opens a second trackerclient to tracker-b on demand — the consumer container can reach `tracker-b:7777` on the shared network. The `--tracker-b-addr`/`--tracker-b-hash-file` flags feed that; add them.)

- [ ] **Step 2: Bring the stack up manually and confirm health**

Run (from `tracker/`, after `e2egen` + both image builds): `docker compose -f test/e2e/compose.e2e.yaml up -d --build` then `docker compose -f test/e2e/compose.e2e.yaml ps`
Expected: `tracker-a`, `tracker-b` healthy; actors up.

- [ ] **Step 3: Confirm federation reaches Steady**

Run: `curl -H "Authorization: Bearer e2e-admin-token-a" http://localhost:9090/peers`
Expected: JSON showing tracker-b as `Steady` (may take a few seconds). Tear down: `docker compose -f test/e2e/compose.e2e.yaml down -v`.

- [ ] **Step 4: Commit**

```bash
git add tracker/test/e2e/compose.e2e.yaml
git commit -m "feat(e2e): docker compose topology (2 trackers, actors, fedactor)"
```

---

### Task 23: Driver package — admin client, BALANCE RPC client, sqlite/logs helpers, compose control

**Files:**
- Create: `tracker/test/e2e/driver/admin.go` (admin HTTP client)
- Create: `tracker/test/e2e/driver/balance.go` (thin BALANCE-RPC QUIC client)
- Create: `tracker/test/e2e/driver/compose.go` (`docker compose` up/down/exec/logs shell-outs)
- Create: `tracker/test/e2e/driver/actors.go` (consumer/seeder/fedactor control HTTP clients)
- Create: `tracker/test/e2e/driver/sqlite.go` (`docker exec <ctr> sqlite3` query helper)
- Test: `tracker/test/e2e/driver/driver_test.go` (unit tests for URL building / JSON parsing — no Docker)

**Interfaces:**
- Consumes: `os/exec` (`docker compose`, `docker exec`); `net/http`; `shared/proto` + `shared/signing` for BALANCE verification; the actor control APIs.
- Produces: helper types the scenario tests call: `Admin{baseURL, token}.Health()/Stats()/Peers()/Identity(id)/BrokerInflight(id)/Reservations()/Freeze(id)`; `Compose{file}.Up()/Down()/Exec(svc, args)/Logs(svc)`; `ConsumerCtl`/`SeederCtl`/`FedactorCtl`; `SQLiteQuery(ctr, dbPath, sql)`. Consumed by all scenario tests (Task 24).

- [ ] **Step 1: Write failing unit tests for pure helpers**

`driver_test.go`: test admin URL construction + JSON decoding of a `/stats` fixture; test the compose command builder produces the right `docker compose -f <file> ...` args. (No live Docker — assert string/parse logic.)

- [ ] **Step 2: Run — FAIL**

Run: `go test -tags=e2e ./test/e2e/driver/ -v` (from `tracker/`)
Expected: FAIL (compile).

- [ ] **Step 3: Implement the driver helpers**

All files carry `//go:build e2e`. `admin.go`: `http.Client` with the bearer header; typed structs for `/stats`, `/peers`, `/identity/{id}`, `/broker/inflight/{id}`, `/broker/reservations`. `balance.go`: minimal QUIC dial (mTLS with a generated client cert, ALPN `tokenbay/1`, SPKI-pin the tracker) sending `RPC_METHOD_BALANCE` and verifying the snapshot with `signing.VerifyBalanceSnapshot`. `compose.go`: `exec.Command("docker","compose","-f",file, ...)`; `Exec` = `docker compose exec -T <svc> <args...>`; `Logs` = `docker compose logs <svc>`. `sqlite.go`: `docker compose exec -T <svc> sqlite3 <dbPath> "<sql>"` (sqlite3 must be in the tracker image — **add `sqlite` to the runtime `apk add` in the tracker Dockerfile** in this task, small P1 addendum). `actors.go`: control-API clients.

- [ ] **Step 4: Add sqlite3 to the tracker runtime image**

In `tracker/deployments/docker/Dockerfile` runtime stage: `RUN apk add --no-cache ca-certificates sqlite` (was just `ca-certificates`). Rebuild `make -C tracker docker`.

- [ ] **Step 5: Run — PASS**

Run: `go test -tags=e2e ./test/e2e/driver/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/test/e2e/driver/ tracker/deployments/docker/Dockerfile
git commit -m "feat(e2e): driver helpers (admin/balance/compose/sqlite clients) + sqlite in image"
```

---

## Phase C — Scenarios

### Task 24: Scenario suite — bring-up, harness lifecycle, scenarios 1–2

**Files:**
- Create: `tracker/test/e2e/main_test.go` (`TestMain`: compose up + wait-healthy + defer down)
- Create: `tracker/test/e2e/bringup_test.go` (scenarios 1–2)

**Interfaces:**
- Consumes: the driver (Task 23), the compose file (Task 22), `e2egen` (run in `TestMain` or the make target).
- Produces: a working `//go:build e2e` suite entrypoint that all scenario files share; scenarios 1 (health & topology) + 2 (enrollment & starter grant) green.

- [ ] **Step 1: Implement `TestMain`**

`main_test.go` (`//go:build e2e`): in `TestMain`, run `e2egen` (or assume the make target did), `Compose.Up()`, poll all containers healthy (admin `/health` 200), run `m.Run()`, `defer Compose.Down(volumes=true)`. Provide package-level `Admin` clients for tracker-a/-b and control clients for the actors.

- [ ] **Step 2: Scenario 1 — health & topology**

Assert all containers healthy; `docker logs tracker-a` contains the startup ledger-integrity line; `Admin(a).Peers()` shows tracker-b `Steady` within a timeout.

- [ ] **Step 3: Scenario 2 — enrollment & starter grant**

`ConsumerCtl.Identity()` + `SeederCtl.Identity()`; `Admin(a).Identity(consumerID).Credits == 1000`; `/stats` tip advances; verify the actor exposes `EnrollResponse.starter_grant_entry` and it passes `signing.VerifyEntry`.

- [ ] **Step 4: Run the suite (Docker required)**

Run (from `tracker/`): `go run ./test/e2e/cmd/e2egen --out test/e2e/.gen ...` then `go test -race -tags=e2e ./test/e2e/ -run 'TestScenario_(Health|Enroll)' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/test/e2e/main_test.go tracker/test/e2e/bringup_test.go
git commit -m "test(e2e): harness lifecycle + bring-up/enrollment scenarios"
```

---

### Task 25: Scenarios 3–7 — consumer→seeder→settlement + reservation lifecycle

**Files:**
- Create: `tracker/test/e2e/settlement_test.go`

**Interfaces:**
- Consumes: driver + control clients; requires P2/P3/P4 landed.
- Produces: scenarios 3 (broker assignment), 4 (tunnel data plane + wrong-pin negative), 5 (settlement happy path), 6 (dispute/timeout), 7 (abandoned assignment).

- [ ] **Step 1: Scenario 3 — broker assignment**

Configure the seeder (`POST /config` available+headroom+model+tier); `ConsumerCtl.Request({model, max_input, max_output})`; assert outcome `SeederAssignment` with `SeederAddr`/`SeederPubkey`/`ReservationToken`; `Admin(a).BrokerInflight(reqID).State == "assigned"`.

- [ ] **Step 2: Scenario 4 — tunnel data plane + negative**

Assert the `/request` response includes the canned SSE the seeder was configured to return (proves the tunnel round-trip). Negative: point a second consumer with a wrong ephemeral pin at the seeder addr → tunnel handshake fails. (Drive via a control endpoint or assert the pinning at the actor level.)

- [ ] **Step 3: Scenario 5 — settlement happy path**

After the request, poll `Admin(a).BrokerInflight(reqID).State == "completed"`; assert the ledger `entries` table has a USAGE row with BOTH `consumer_sig` and `seeder_sig` non-empty (via `SQLiteQuery`); assert consumer debited + seeder credited (BALANCE RPC + `balances` table).

- [ ] **Step 4: Scenario 6 — dispute/timeout**

`SeederCtl` configured normally but `ConsumerCtl` set to stay silent on settlement (add a `/config {settle: false}` toggle to the consumer actor); after `settlement_timeout_s` (3s) + margin, assert a USAGE entry with empty `consumer_sig` and moved balances.

- [ ] **Step 5: Scenario 7 — abandoned assignment**

Consumer requests but never dials the tunnel (`/request {dial: false}` toggle); wait ~35s (reaper tick is 30s hardcoded); assert `/broker/reservations` slot gone + `/broker/inflight` state `failed`. Mark this subtest with a comment about the 35s budget.

- [ ] **Step 6: Run**

Run: `go test -race -tags=e2e ./test/e2e/ -run TestScenario_Settlement -v` (stack already up via TestMain)
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add tracker/test/e2e/settlement_test.go plugin/cmd/tokenbay-e2e-actor/
git commit -m "test(e2e): consumer->seeder->settlement + reservation lifecycle scenarios"
```

---

### Task 26: Scenarios 8–10 — ledger integrity & durability

**Files:**
- Create: `tracker/test/e2e/ledger_test.go`

**Interfaces:**
- Consumes: `Compose.Exec`/restart, `SQLiteQuery`, admin `/stats`, `/metrics`, `docker logs`.
- Produces: scenarios 8 (restart integrity), 9 (corruption tripwire — negative), 10 (graceful drain).

- [ ] **Step 1: Scenario 8 — restart integrity**

After scenarios that wrote entries, `docker compose restart tracker-a`; assert the startup integrity gate passes on the non-empty chain (log line + `/metrics` integrity counter increment).

- [ ] **Step 2: Scenario 9 — corruption tripwire (negative)**

`SQLiteQuery(tracker-a, ledger.db, "UPDATE entries SET cost_credits=cost_credits+1 WHERE seq=(SELECT MAX(seq) FROM entries)")` to break the chain hash; restart tracker-a; assert the container exits non-zero / stays unhealthy and `docker logs` shows an integrity error. Then restore (down -v resets; or run this scenario last / on a throwaway node).

- [ ] **Step 3: Scenario 10 — graceful drain**

`Admin(a).Maintenance()` (or `docker compose kill -s SIGTERM tracker-a`); assert clean exit; restart and assert the integrity gate passes (tip intact).

- [ ] **Step 4: Run**

Run: `go test -race -tags=e2e ./test/e2e/ -run TestScenario_Ledger -v`
Expected: PASS. (Order matters — corruption test should not poison later scenarios; isolate via a dedicated compose project or run it last.)

- [ ] **Step 5: Commit**

```bash
git add tracker/test/e2e/ledger_test.go
git commit -m "test(e2e): ledger integrity, corruption tripwire, graceful drain"
```

---

### Task 27: Scenarios 11–13 — federation (real neighbor + Byzantine)

**Files:**
- Create: `tracker/test/e2e/federation_test.go`

**Interfaces:**
- Consumes: `FedactorCtl`, admin `/peers`, `/metrics`, `SQLiteQuery` (`peer_root_archive`, `peer_revocations`), admin `/identity/{id}/freeze` (P7).
- Produces: scenarios 11 (root-attestation exchange), 12 (equivocation→depeer), 13 (revocation propagation).

- [ ] **Step 1: Scenario 11 — root-attestation exchange**

Wait for a `publish_cadence_s` tick; assert both trackers' `peer_root_archive` tables gained the counterpart's row (`SQLiteQuery`).

- [ ] **Step 2: Scenario 12 — equivocation → depeer**

`FedactorCtl.Handshake(tracker-a addr + pub)`; `FedactorCtl.SendRootAttestation(hour, rootX)` then `SendRootAttestation(hour, rootY)`; assert `Admin(a).Peers()` no longer lists the fedactor as Steady and `/metrics` `equivocations_detected` incremented.

- [ ] **Step 3: Scenario 13 — revocation propagation**

`Admin(a).Freeze(identityID)` (P7) for a consumer enrolled at A; assert a `REVOCATION` reaches tracker-b (`peer_revocations` row on B via `SQLiteQuery`); a `BrokerRequest` from that identity on tracker-b returns `FROZEN` (drive via a consumer actor pointed at B, or the balance/broker RPC through the driver). Alternatively assert via the fedactor's `/received` that the revocation gossiped.

- [ ] **Step 4: Run**

Run: `go test -race -tags=e2e ./test/e2e/ -run TestScenario_Federation -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/test/e2e/federation_test.go
git commit -m "test(e2e): federation exchange, equivocation depeer, revocation propagation"
```

---

### Task 28: Scenarios 14–16 — cross-region transfer

**Files:**
- Create: `tracker/test/e2e/transfer_test.go`

**Interfaces:**
- Consumes: `ConsumerCtl.Transfer(destTrackerID, amount)`, BALANCE RPC at both trackers, `SQLiteQuery` (`entries` kinds 2/3, `ref`), admin `/identity/{id}`.
- Produces: scenarios 14 (happy path), 15 (idempotency), 16 (authz negative). Requires P5.

- [ ] **Step 1: Scenario 14 — transfer happy path**

Consumer enrolled at A (also enrolled at B if the dest requires it — confirm from P5). `ConsumerCtl.Transfer({dest_tracker_id: B_fed_id, amount: N})`; assert `TransferProof` returned; `transfer_out` at A + `transfer_in` at B (`SQLiteQuery`); balances move by N at both (BALANCE RPC / `/identity`).

- [ ] **Step 2: Scenario 15 — idempotency**

Replay the same transfer (same nonce) via a control toggle that reuses the nonce; assert no double credit at either end (dest cache + on-chain ref check from P5).

- [ ] **Step 3: Scenario 16 — authz negative**

Drive a `TransferRequest` whose `ConsumerPub` doesn't match `IdentityId` (control toggle) → `UNAUTHENTICATED`; and a transfer exceeding source balance → fails without partial application (source balance unchanged, no `transfer_out` row).

- [ ] **Step 4: Run**

Run: `go test -race -tags=e2e ./test/e2e/ -run TestScenario_Transfer -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/test/e2e/transfer_test.go plugin/cmd/tokenbay-e2e-actor/
git commit -m "test(e2e): cross-region transfer happy path, idempotency, authz"
```

---

### Task 29: Scenarios 17–20 — negative/security/robustness + metrics

**Files:**
- Create: `tracker/test/e2e/robustness_test.go`

**Interfaces:**
- Consumes: raw QUIC dial helpers (for oversize-frame / TLS-version / cert negatives), admin client, `/metrics`.
- Produces: scenarios 17 (auth & framing), 18 (envelope validation), 19 (reconnect integrity), 20 (`/metrics` exposition).

- [ ] **Step 1: Scenario 17 — auth & framing**

Unauthenticated admin GET → 401; oversize QUIC frame (>1 MiB) rejected; TLS 1.2 / non-Ed25519 client cert rejected at handshake. (Reuse the raw-dial helpers from `tracker/test/integration`; adapt for the container address.)

- [ ] **Step 2: Scenario 18 — envelope validation**

Via a consumer-actor control toggle producing a tampered consumer sig → `BrokerRequest` rejected; unknown model → `UNKNOWN_MODEL`; force a stale (>10 min) balance proof (toggle: reuse an old snapshot) → rejected.

- [ ] **Step 3: Scenario 19 — reconnect integrity**

Bounce the federation link (`docker network disconnect`/`connect` or restart tracker-b); on reconnect assert the reconnect-integrity gate runs (log/metric) and the peer re-attaches (Steady) — or depeers on injected local corruption.

- [ ] **Step 4: Scenario 20 — /metrics exposition**

Scrape `:9100/metrics` on both trackers; assert the broker/federation/reputation counters used by earlier scenarios are present and non-zero where expected.

- [ ] **Step 5: Run the FULL suite**

Run (from `tracker/`): `make test-e2e` (Task 30) or `go test -race -tags=e2e ./test/e2e/ -v`
Expected: all 20 scenarios PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/test/e2e/robustness_test.go
git commit -m "test(e2e): auth/framing, envelope validation, reconnect, metrics scenarios"
```

---

## Phase D — Make/CI integration + readiness report

### Task 30: `make -C tracker test-e2e` target

**Files:**
- Modify: `tracker/Makefile`

**Interfaces:**
- Consumes: `e2egen`, `docker compose`, the `e2e` build tag.
- Produces: `make -C tracker test-e2e` — one command that generates artifacts, builds images, runs the tagged suite (which manages up/down), and cleans up.

- [ ] **Step 1: Add the target**

In `tracker/Makefile`, add `test-e2e` to `.PHONY` and (tab-indented, mirroring `test-integration`):
```make
test-e2e:
	go run ./test/e2e/cmd/e2egen --out test/e2e/.gen --seed-a $(E2E_SEED_A) --seed-b $(E2E_SEED_B) --seed-fed $(E2E_SEED_FED) --addr-a tracker-a --addr-b tracker-b
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
	docker build -f test/e2e/Dockerfile.actors -t tokenbay-e2e-actors:dev ../
	go test -race -tags=e2e ./test/e2e/...
```
Define default seeds at the top of the Makefile (fixed hex constants) so runs are deterministic. The test's `TestMain` runs `compose up`/`down -v` itself.

- [ ] **Step 2: Run the target end-to-end**

Run: `make -C tracker test-e2e`
Expected: images build, 20 scenarios PASS, stack torn down.

- [ ] **Step 3: Commit**

```bash
git add tracker/Makefile
git commit -m "build(tracker): make test-e2e target"
```

---

### Task 31: CI job

**Files:**
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: the `test-e2e` target; ubuntu-latest runners (Docker + compose v2 preinstalled).
- Produces: a dedicated ubuntu-only `e2e` job that does NOT join the 3-OS matrix.

- [ ] **Step 1: Add the job**

Mirror the existing `integration` job block (checkout@v4, setup-go@v5 go-version `1.23`, `go work sync`), then `make -C tracker test-e2e`. `runs-on: ubuntu-latest` only. Optionally add a step `sudo sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000` to silence quic-go UDP-buffer warnings. Do NOT add it to the `test` matrix (the `e2e` build tag keeps it out of `go test ./...`).

- [ ] **Step 2: Validate the workflow YAML locally**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))"` (or `actionlint` if available)
Expected: parses cleanly.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add ubuntu-only tracker e2e job"
```

---

### Task 32: Deploy-readiness report + final gate

**Files:**
- Create: `docs/superpowers/specs/2026-07-08-tracker-e2e-readiness-report.md` (or fold into the PR body)

**Interfaces:**
- Consumes: the green suite.
- Produces: the deliverable per spec §9.5 — what the e2e proves, plus documented residual limitations.

- [ ] **Step 1: Write the report**

Summarize: the 20 scenarios and what each proves; the P1–P7 fixes landed; and the documented residual limitations: (a) automatic z-score→FROZEN unreachable in v1 (P7 gives the operator lever); (b) STUN/TURN hole-punch path not stress-tested (Docker gives direct reachability); (c) plugin `transfer_cmd.go` CLI encoding still a v1 placeholder (e2e drives `trackerclient.TransferRequest` directly); (d) transfer idempotency caches are in-memory (persistent idempotency is follow-up).

- [ ] **Step 2: Final full gate**

Run (from repo root): `make check` (all module unit+integration+lint) then `make -C tracker test-e2e` (Docker e2e).
Expected: everything PASS.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/specs/2026-07-08-tracker-e2e-readiness-report.md
git commit -m "docs(tracker): e2e deploy-readiness report"
```

- [ ] **Step 4: Open the PR** (per repo CLAUDE.md: only after all tasks done and `make check` green)

```bash
git push -u origin HEAD
gh pr create --base main --title "Tracker e2e (Docker) + P1-P7 deploy-readiness fixes" --body "<summary + readiness report>"
gh pr checks --watch
```
Merge only when CI (including the new e2e job) is green.

---

## Self-review notes

- **Spec coverage:** P1–P7 → Tasks 1–13; harness (e2egen/actor/fedactor/dockerfiles/compose/driver) → Tasks 15–23; 20 scenarios → Tasks 24–29; make/CI → Tasks 30–31; readiness report → Task 32. All spec §3–§9 items map to a task.
- **Coordinated shared changes** (rule 4) are called out where they occur: Task 4+5 (EnvelopeBody/OfferPush) commit together with `!`; Task 6 (new signing helper, additive) is safe alone; Tasks 7–8 update tracker + plugin callers of the settlement scheme.
- **Two open design risks flagged inline** for the implementer (do NOT skip): (1) P4 seeder `cost_credits` in the usage-assertion requires a mirrored price table — Task 8 documents the fallback (exclude cost from the participant sig, bind via tracker-only EntryBody) if mirroring proves brittle; (2) P5 `TransferOutRecord` must carry enough fields to reconstruct `CanonicalTransferProofRequestPreSig` — Task 9 extends the record. Both were surfaced by the code audit and resolved in the spec's "sequencing-independent canonical intent" principle.
- **Test conventions preserved:** server tests black-box `package server_test` + raw `t.Fatalf`; registry/reputation/broker/admin in-package + testify; federation `_test` external + `t.Fatalf`; all with `-race`.
