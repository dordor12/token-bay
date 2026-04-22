# TEE-Attested Tier — Subsystem Design Spec

> ⚠️ **Status: needs redesign.** This spec was written assuming the seeder plugin held an Anthropic API token directly, which an enclave could take ownership of. Under the current architecture (plugin spec revised 2026-04-22), the plugin no longer holds credentials — all Anthropic-bound calls go through the Claude Code CLI bridge. **That change invalidates the confidentiality premise of this design.** The enclave can no longer be the sole holder of credentials because there are no credentials to hold. The content below is preserved as historical record and may inform the redesign. See the "Redesign directions" block immediately below.

## Redesign directions (to be worked in a follow-up brainstorm)

Three plausible repositionings:

1. **Repurpose TEE as a content-policy enforcement layer.** Enclave runs a policy classifier on the consumer's prompt and a signed attestation that it forwarded only policy-clean content to the host's Claude Code. Standard-tier seeders now can opt into stronger content guarantees without encrypting against the host OS. Loses the "host cannot read the prompt" confidentiality promise.
2. **Run Claude Code inside the enclave.** Possible in principle; very large attack surface; significant engineering work (network, filesystem, auth storage all need enclave-native equivalents). Unclear whether Claude Code's authentication can be initialized inside a fresh enclave. Probably not viable for v1.
3. **Drop the TEE tier for v1.** Acknowledge that under the Claude Code bridge architecture, confidentiality from the seeder host is not achievable. Consumers who need prompt confidentiality must use the Token-Bay network only for non-sensitive work. Revisit TEE when/if Claude Code exposes a primitive that lets authenticated API calls originate from inside an enclave.

The rest of this spec assumes the original token-in-enclave model and should be read with that context.

---

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | **Needs redesign** (see banner above). Original design preserved below. |
| Date | 2026-04-22 |
| Scope | The hardened privacy tier where seeders run the request-forwarding proxy inside a Trusted Execution Environment (TEE). Defines attestation flow, enclave-image distribution, supported hardware, the enclave's runtime contract, and the consumer's verification steps. Complements parent spec §7.1 "Standard tier" with a second class of seeder. |

## 1. Purpose

Give consumers with sensitive prompts a cryptographically-backed option that the seeder host cannot read their request body. The host OS processes and root users on the seeder's machine are explicitly in the threat model; the TEE boundary is the only entity inside which plaintext exists.

## 2. What the TEE tier provides

- **Confidentiality** — the seeder's host OS, hypervisor (where applicable), root user, and malware cannot read the prompt or response body.
- **Integrity** — the code running inside the enclave is verifiably the Token-Bay signed reference image; modifications are detectable via attestation.
- **Content policy enforcement** (optional, see §7) — the enclave can run a pre-forwarding content classifier, reducing the seeder's Anthropic-account ban exposure since obvious policy-violating content can be rejected before forwarding.

## 3. What the TEE tier does NOT provide

- Protection from Anthropic seeing the prompt (Anthropic is the eventual recipient; that's the point).
- Protection against Anthropic's upstream TLS endpoint (the seeder's token identifies the account to Anthropic exactly as in the standard tier).
- Protection from state-level side-channel attacks on enclaves (SGX has a long history of these; the spec acknowledges this and limits scope).
- Guaranteed availability — TEE-capable seeders are a subset of the pool.

## 4. Supported hardware (v1)

### 4.1 Candidates

- **AWS Nitro Enclaves** — VM-based enclaves on EC2 instances. Attestation via AWS-signed cert chain. Relatively easy to deploy; ties the enclave's root of trust to AWS.
- **Intel SGX** — CPU-level enclaves on supporting Xeon. More mature attestation tooling, but major side-channel history and limited memory.
- **AMD SEV-SNP** — VM encryption + attestation. Good integrity but weaker selective-reveal properties than SGX.

### 4.2 v1 choice

Support AWS Nitro Enclaves as the reference TEE because:
- Attestation chain is clean and verifiable offline.
- Enclave image format (EIF) has reasonable tooling.
- Doesn't require consumers to integrate with Intel's attestation service.

SGX and SEV-SNP are listed as future hardware targets.

## 5. Attestation flow

### 5.1 Seeder-side enrollment

1. Seeder operator provisions a Nitro-enabled EC2 instance.
2. Pulls the Token-Bay signed enclave image (EIF) from the distribution channel (see §6).
3. Boots the enclave; enclave generates an ephemeral Ed25519 keypair internal to itself.
4. Enclave requests an **attestation document** from Nitro: a document signed by AWS that includes:
   - Enclave image measurement (PCR values) — proves "this enclave is running *this* image."
   - The enclave's ephemeral pubkey — binds the key to this enclave instance.
   - Timestamp.
5. Seeder plugin advertises to the tracker: `capabilities.tiers = [tee_attested]` and attaches the attestation document.
6. Tracker validates the attestation chain (AWS cert → Nitro root → enclave image pubkey) against its known-good image measurements. Only whitelisted PCR values are accepted.

### 5.2 Consumer-side verification (per request)

At `broker_request` time, if the consumer requested `privacy_tier: tee_required`, the tracker returns the assigned seeder's attestation document along with the seeder's ephemeral key. Consumer's plugin:

1. Validates the AWS cert chain.
2. Validates PCR values against the consumer's local known-good list (shipped with plugin, updated via signed release channel).
3. Validates the ephemeral pubkey is bound to the attestation.
4. Opens the tunnel with the ephemeral key — TLS handshake uses the attested key as the server cert.

If any step fails, consumer aborts and reports back to tracker for reputation.

## 6. Enclave image distribution

### 6.1 Build

Enclave image is built reproducibly from the open-source Token-Bay plugin repo with a narrow "enclave profile" that excludes host-side code (idle policy, config management, audit log persistence to host disk — these run in the host companion process).

### 6.2 Signing

Official builds are signed by the Token-Bay project release key. Signed build artifact includes:
- The EIF.
- A manifest with expected PCR values.
- Release notes and commit SHA.
- Signature over all above.

### 6.3 PCR value distribution

Plugins (both consumer and seeder) ship with a bundled list of known-good PCR values. List updated via signed release channel. Trackers maintain their own list (which may lag plugin updates slightly).

### 6.4 Reproducible builds

All builds must be reproducible: anyone can clone the repo at a tagged commit, run the build, and produce the exact same PCR values. This is the defense against a compromised release pipeline — independent parties can verify the image matches its claimed source.

## 7. Runtime contract (what the enclave does)

### 7.1 Narrow interface

The enclave process exposes exactly two IO streams to its host companion:

- **Outbound to network**: HTTPS to `api.anthropic.com` (only; no other destinations).
- **Inbound from network**: tunnel to the consumer (via the host companion which shuttles bytes; host does not decrypt).

Host companion does nothing interpretive — it's a dumb byte pipe. All cryptographic state (consumer's ephemeral session, Anthropic API token) lives inside the enclave.

### 7.2 Token handling

Seeder's Anthropic API token is **not** held by the host. On enclave boot, the host passes the token into the enclave through a sealed channel (encrypted with the enclave's ephemeral pubkey so only the enclave can decrypt). From that point, the host cannot read the token back.

On seeder restart, the user must re-supply the token interactively (or from OS keychain with an explicit unlock). There is no persistent at-rest encryption of the token on the host — the host is untrusted.

### 7.3 Request handling inside enclave

1. Receive consumer's encrypted request body. Decrypt.
2. (Optional, see §8) Run content policy check. On violation: reject with specific error code.
3. Forward to `api.anthropic.com` with the seeder's token.
4. Stream response back to the tunnel.
5. On stream complete: zero all request/response buffers. Emit `usage_report` through the host (with seeder_sig produced inside enclave).
6. No request content is logged. No state persists between requests.

### 7.4 Memory and logging

- Enclave has no persistent storage.
- Logs are minimal: counters and timings, no content. Exported via the host for the seeder's audit log (which is the same content as standard tier §4.3).
- Memory for each request is zeroed before the next.

## 8. Content policy enforcement (optional module)

### 8.1 Motivation

Standard-tier seeders carry the Anthropic-account ban risk because they can't filter content. TEE-tier seeders *could* run a content policy classifier inside the enclave, rejecting policy-violating content before it leaves the enclave. This would materially reduce the ban risk without compromising consumer privacy (classifier runs on plaintext in the enclave only).

### 8.2 Design

- A small classifier model embedded in the enclave image. Candidates: distilled content-policy classifier, rule-based filter, or a lightweight embedding-based matcher against policy categories.
- Runs pre-forwarding. Verdict: `allow`, `reject`, `allow_with_flag` (forward but increment per-consumer flag counter — feeds reputation).
- Consumer sees specific error if rejected (`CONTENT_POLICY_REJECTION`), with optional category hint.
- Rejected requests are not billed (no Anthropic call was made) but cost consumer a small reputation event to disincentivize probing.

### 8.3 False-positive handling

Consumer can appeal via dispute (reputation spec §7). Operator-gated, same process.

### 8.4 v1 status

Content policy is **optional in v1**. Enclaves ship with a permissive default (allow) and a hook for seeders who want to opt into a specific policy pack. Aggressive enforcement across the network is a v2 discussion.

## 9. Attack surface and mitigations

| Attack | Mitigation |
|---|---|
| Malicious seeder modifies enclave image | PCR value check rejects. Only Token-Bay-signed images accepted. |
| Malicious seeder installs host-side interception | Host cannot access enclave memory; TLS keys inside enclave; direct tunnel goes from consumer to enclave via the dumb-pipe host. |
| Nitro attestation compromised (AWS compromise) | Trust reduction to "AWS integrity"; acceptable for v1 pragmatism. |
| Side-channel on Nitro | Out of scope; Nitro's VM boundary makes these harder than SGX but not impossible. Document as residual risk. |
| Operator reads memory via debugger | Nitro enclaves block this; if it becomes possible, PCR of known-good enclaves rejects the modified image. |
| Consumer picks wrong (malicious) notary | Tracker-enforced accepted-PCR list; consumer also verifies. |
| Replay of attestation document | Freshness: attestation documents include a timestamp and enclave ephemeral key; each session uses a fresh ephemeral key, replay cannot bind to a new session. |

## 10. Open questions

- **SGX and SEV-SNP support.** Each has different attestation flows. v1 punts; v2 should generalize the "TEE abstraction" so new hardware can slot in with scheme-specific verifiers.
- **Non-cloud TEEs.** Home seeders on SGX-capable workstations? Attestation without AWS-style cert chain requires Intel's DCAP or equivalent. Deferred.
- **Content policy packs.** Formalized pack format, governance of who writes packs. Cross-cutting concern with reputation subsystem.
- **Enclave updates.** Rolling new enclave images: plugins and trackers must update PCR lists in lockstep. Bootstrapping newly-signed images has a delicate rollout — stale trackers reject fresh enclaves. Operator runbook item.
- **Metering honesty.** Enclave reports token counts; consumer trusts the signature. Since the signature is from a known-good enclave, this is trustworthy by construction — a nice property the standard tier lacks.

## 11. Failure handling

| Failure | Behavior |
|---|---|
| Attestation validation fails at tracker | Seeder not accepted into the TEE-tier registry. Operator notified. |
| Attestation validation fails at consumer | Consumer aborts; broker_request retries with a different seeder if available, else `NO_CAPACITY`. |
| Enclave crashes mid-request | Host companion detects; reports to tracker; request fails (parent Q7c(i)). |
| PCR list on tracker is stale relative to a new seeder's image | Seeder rejected. Operator upgrades tracker's list. |
| Content policy classifier raises false positives | Dispute per §8.3. |

## 12. Acceptance criteria

- A Nitro-enclave-based seeder can enroll and serve TEE-tier requests end-to-end.
- A consumer requesting `tee_required` is only routed to attested seeders.
- Modifying the enclave binary changes PCR values and fails attestation.
- Host companion cannot dump enclave memory or intercept plaintext (demonstrable via a dedicated security test).
- On enclave crash or attestation failure, request falls back cleanly to "error to user" without leaking state.
- Reproducible-build check passes: independent build from the same source produces bit-identical EIF.
