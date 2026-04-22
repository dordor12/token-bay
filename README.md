# Token-Bay

> **A BitTorrent-style peer-to-peer network for sharing Claude Code rate-limit capacity.** Educational / research exercise in distributed-systems design.

## What is this?

Token-Bay is an educational design project that asks: *what if we applied BitTorrent's architecture — trackers, seeders, peer-to-peer transfer, reputation — to a very different problem: sharing Claude Code rate-limit capacity between consenting users?*

The problem it explores:

- You pay for Claude Code. Your rate-limit window resets every few hours.
- Most of the day, your account is idle — that window is heavily underused.
- Somewhere else, another user has burned through their quota and can't work.
- **What if their Claude Code could transparently borrow your idle capacity during time you'd otherwise waste — and you could borrow theirs when you're the one who's stuck?**

Token-Bay isn't meant to be *deployed* — doing so would violate Anthropic's Terms of Service and raises real privacy concerns. But the **protocol patterns** that appear when you try to design such a system are a rich playground for distributed-systems concepts: federated coordination, signed reputation, cryptographic proofs of resource state, NAT traversal, idempotent credit ledgers, tamper-evident history.

**This repo is the design specs, implementation plans, and (eventually) the reference code.**

## The BitTorrent mental model

If you know BitTorrent, the pieces map almost one-to-one:

| BitTorrent | Token-Bay |
|---|---|
| File chunks | Claude API calls (`/v1/messages`) |
| Tracker | Regional tracker server |
| Seeder | User with idle Claude Code capacity |
| Leecher | User whose rate limit is exhausted |
| Peer-to-peer chunk transfer | Peer-to-peer request proxying |
| Share ratio / karma | Signed credit ledger (tracker + both peers sign) |
| DHT / multi-tracker | Tracker federation (Merkle-root gossip between regions) |
| `.torrent` announce | `broker_request` to regional tracker |

Users install a **Claude Code plugin** that can play two roles at once:

- **Consumer** — when Claude Code hits a rate-limit error, the plugin detects it via Claude Code's `StopFailure{rate_limit}` hook, asks the user for confirmation, and routes the request through the network.
- **Seeder** — during idle windows the user configures (e.g. `02:00–06:00` local time), the plugin accepts forwarded requests from other users and serves them via `claude -p "<prompt>"` with **all tool access disabled** (no `Bash`, no `Read`, no `Write`, no MCP, no hooks — see "safety" below).

**Trackers** are lightweight coordination servers. One per region. They maintain a live registry of available seeders, broker incoming requests, and own a tamper-evident **credit ledger** of settled requests. Trackers peer with each other — Merkle-root gossip lets you detect a tracker that rewrites its history, and signed transfer proofs let credits earned in one region be spent in another.

## How a request flows (end-to-end)

```mermaid
sequenceDiagram
    autonumber
    participant CC as Consumer's Claude Code
    participant CP as Consumer Plugin
    participant T as Regional Tracker
    participant SP as Seeder Plugin
    participant SCC as Seeder's Claude Code
    participant A as api.anthropic.com

    CC->>A: POST /v1/messages
    A-->>CC: 429 Too Many Requests
    CC->>CP: StopFailure{matcher: rate_limit} hook fires
    CP->>CP: claude -p "/usage" (independent verification)
    CP->>CC: "Fallback via Token-Bay network? (~N credits, balance M)"
    Note right of CP: user confirms
    CP->>T: broker_request(envelope + two-signal exhaustion proof)
    T->>SP: offer(envelope hash, terms)
    SP-->>T: accept (ephemeral pubkey)
    T-->>CP: seeder address + pubkey
    CP<<->>SP: QUIC tunnel (NAT hole-punched via tracker)
    CP->>SP: encrypted conversation body
    SP->>SCC: claude -p "<prompt>" --disallowedTools "*"
    SCC->>A: POST /v1/messages (seeder's Claude Code auth)
    A-->>SCC: streaming SSE response
    SCC-->>SP: stdout stream (stream-json)
    SP-->>CP: tunnel stream
    CP-->>CC: response injected into conversation
    SP->>T: usage_report(tokens, seeder_sig)
    T->>CP: settlement request (entry preimage)
    CP->>T: consumer_sig on entry
    T->>T: append to signed ledger; credit seeder, debit consumer
```

Each settled request produces one ledger entry carrying **three signatures** — consumer, seeder, tracker — so any party can later prove what they did or did not agree to.

## Non-obvious architecture properties

- **The plugin never handles an Anthropic API key.** All Anthropic traffic goes through the user's own `claude` CLI. Token-Bay sits next to Claude Code, not in front of it.
- **Seeder-side safety.** When you seed, the consumer's prompt reaches a `claude -p` subprocess with **every side-effecting primitive disabled** — no tool use, no MCP, no hooks. A malicious prompt can steer Claude's text output, but it cannot touch the seeder's filesystem, shell, or network. A conformance test suite runs adversarial prompts against the configured flags and enforces zero observable side effects.
- **Two-signal exhaustion proof.** The network only engages when (a) Claude Code says `StopFailure{rate_limit}`, *and* (b) an independent `claude -p "/usage"` probe agrees. Both signals are signed and travel with the request, so forgery requires fabricating both coherently.
- **Tamper-evident credit ledger.** Every settled request is an append-only, hash-chained, triple-signed ledger entry. Each tracker commits an hourly Merkle root, gossiped federation-wide — a tracker that tries to rewrite history gets exposed by peers holding its archived roots.
- **Identity is tied to Claude Code accounts.** The plugin can't just mint 10,000 fake identities to farm credits because identity binding uses a signed challenge mediated by the Claude Code bridge. One Claude Code account → one network identity. Sybil resistance is ≈ the cost of Claude Code accounts.

## Repo layout

Monorepo with three Go modules linked by a Go workspace:

```
token-bay/
├── plugin/                  — Claude Code plugin (consumer + seeder roles)
├── tracker/                 — regional coordination server
├── shared/                  — shared Go library (wire formats, crypto helpers, common types)
├── docs/superpowers/
│   ├── specs/               — architecture + subsystem design specs
│   └── plans/               — implementation plans
└── CLAUDE.md                — repo-level development context
```

Each component has its own `CLAUDE.md` (development rules) and `Makefile`. The root `Makefile` orchestrates `make test / lint / build / check` across all three.

## Status

**Design phase.** The following are complete:

- Top-level architecture spec: `docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md`
- Subsystem specs: plugin, tracker, federation, ledger, exhaustion-proof, reputation.
- Scaffolding plans for the monorepo, shared library, plugin, and tracker: `docs/superpowers/plans/`.

The TEE-tier subsystem spec is **paused pending redesign** — its original premise (passing an API token into an enclave) no longer applies under the Claude-Code-bridge architecture.

**Not production software.** This is an educational exploration. Deploying it against live Anthropic accounts would violate Anthropic's Terms of Service.

## Getting started (once scaffolded)

Prerequisites:

- Go 1.23+
- `make`
- `golangci-lint`
- `lefthook`
- `claude` CLI (required for seeder role and for bridge conformance tests)

```bash
git clone <repo-url> token-bay
cd token-bay
go work sync
make check         # test + lint across all three modules
```

Component-specific commands live in each subdirectory's `Makefile`:

- `make -C plugin conformance` — bridge safety suite (adversarial prompts vs `claude -p` flags)
- `make -C tracker run-local` — spin up a local tracker with a throwaway keypair and SQLite DB
- `make -C shared test` — shared library tests

## Where to read next

If you want to understand the design: start with the [root architecture spec](docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md). It documents every architectural decision with its rationale and trade-offs.

If you want to contribute: read the root [`CLAUDE.md`](CLAUDE.md), then the component-specific `CLAUDE.md` under whichever directory you're touching.

Subsystem specs live under `docs/superpowers/specs/<subsystem>/`. Implementation plans under `docs/superpowers/plans/`. Each plan is a series of bite-sized, TDD-structured tasks that produce commits — no prose-heavy "do this later" placeholders.

## License

TBD.

## Pre-commit hooks

Install [lefthook](https://github.com/evilmartians/lefthook), then run `lefthook install` in the repo root.
