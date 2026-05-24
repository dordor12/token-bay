# Slice 6 — Plugin reroute via signed bootstrap list

> Plugin consumes slice 4's `Client.FetchBootstrapPeers` to dynamically extend its endpoint set with tracker-recommended peers ranked by `health_score`. Closes the §7.2 chain end-to-end: bootstrap config (3-5 trackers) → fetch ranked peers → reconnect to better-ranked alternatives on drop.

## 1. Goal

Make slices 4+5 actually do something. Today `Client.FetchBootstrapPeers` returns a parsed `[]BootstrapPeer` but no caller invokes it. After this slice, the plugin's reconnect supervisor cycles through both operator-configured endpoints AND tracker-published peers, ordered by `health_score`.

## 2. Non-goals

- **Latency-based ranking tiebreak.** Slice 8 will add measured-RTT to the candidate selection. v1 ranks by `health_score` only.
- **Pinning a new "home" tracker for identity registration.** That's an admission concern; plugin keeps its existing identity bind.
- **Auto-fetching across rebinds.** Identity rebind requires re-enrollment which is a separate slice.
- **Multi-region failover policy.** The list is region-agnostic; operator's bootstrap config still drives geography.
- **Cross-restart persistence to disk.** v1 caches the list in memory only. A future slice can persist to `~/.token-bay/bootstrap.cache` if cold-start latency becomes an issue.

## 3. Architecture

A new `*peerCandidates` type in `plugin/internal/trackerclient/peercandidates.go` owns:

- An in-memory ranked list of `[]TrackerEndpoint` sourced from the most recent `FetchBootstrapPeers` call.
- An `expires_at time.Time` from the signed list.
- A `mu sync.Mutex` guarding both.

The `supervisor` (reconnect.go) gains two new hooks:

1. **On first successful connect** — kicks a one-shot `FetchBootstrapPeers` in a goroutine; the result feeds `peerCandidates.Update(...)`.
2. **On endpoint selection** — instead of `s.cfg.Endpoints[epIdx % len]`, the supervisor calls `peerCandidates.NextEndpoint(epIdx, baseEndpoints)` which returns:
   - The next operator-configured endpoint if the candidate set is empty or expired
   - Otherwise, a merged list: `[baseEndpoints..., candidates_by_health_desc...]`, picking by `epIdx % len(merged)`

Re-fetch policy: each successful reconnect re-triggers the fetch (one-shot per connect event). No periodic refresh in v1 — the per-connect fetch is enough since reconnects fire frequently enough that staleness is bounded.

## 4. Configuration

`Config` gains one new field:

```go
RerouteEnabled bool // default false; opt-in for v1
```

When `false`, the supervisor never calls `FetchBootstrapPeers` and `peerCandidates` stays empty. Existing behavior unchanged.

When `true`, the supervisor fetches on connect and merges into endpoint selection.

No other knobs — TTL is dictated by the server's signed `expires_at`, and merge order is fixed.

## 5. Wire format

Unchanged. Reuses slice 4's `BootstrapPeerList` / `RPC_METHOD_BOOTSTRAP_PEERS` end-to-end.

## 6. Data flow

```
Plugin Start → Supervisor.run() →
  → ep = peerCandidates.NextEndpoint(epIdx, baseEndpoints)
  → Transport.Dial(ep)
  → ConnectedSuccess →
       → go fetchAndUpdateCandidates(c)
       → fetchAndUpdateCandidates calls c.FetchBootstrapPeers(ctx)
       → on success, calls peerCandidates.Update(list, expiresAt)
       → on error, logs Warn and leaves the prior candidates intact
  → on disconnect, supervisor loops, epIdx++ → next NextEndpoint() pick
```

## 7. Endpoint translation

A `BootstrapPeer` from the wire has `{TrackerID ids.IdentityID, Addr string, RegionHint, HealthScore, LastSeen}`. The supervisor's `Transport.Dial` needs a `TrackerEndpoint{Addr, IdentityHash}`. The translation is direct:

```go
ep := TrackerEndpoint{
    Addr:         p.Addr,
    IdentityHash: p.TrackerID, // already a sha256(SPKI) under slice 4
}
```

No new identity-hash derivation. The server's signed list IS the identity binding.

## 8. Ranking & merge

When merging:

1. Sort `candidates` by `HealthScore DESC` (stable; tiebreak by `Addr` lexicographic).
2. Drop any candidate whose `Addr+IdentityHash` matches a configured base endpoint (avoid duplicate dialing).
3. Result list: `[baseEndpoints..., sortedCandidates...]`.
4. Picker: `merged[epIdx % len(merged)]`.

This keeps operator-configured trackers first — they're the trust root. Candidates are only consulted after the base set rolls over.

## 9. Edge cases

- **Empty candidates / expired list.** Return next base endpoint. Identical to pre-slice behavior.
- **All endpoints unreachable.** Same as today: supervisor backs off and retries; `epIdx` keeps incrementing.
- **Candidate with same Addr as base endpoint, different IdentityHash.** Drop the candidate (operator config wins; mismatched IdentityHash is a signal of a misconfigured fetch — silent drop, metric).
- **Server returns empty list.** Cache an empty list with `expires_at`; behaves identical to no fetch until TTL.

## 10. Metrics

New optional Prometheus counters on the existing `Config.Metrics` interface (slice 4 already widened it for `IncBootstrapPeersFetched`):

- `IncRerouteCandidatesUpdated()` — bumped on each successful `peerCandidates.Update`.
- `IncRerouteEndpointPicked(source)` — `source ∈ {base, candidate}`.

`BootstrapMetrics` interface gets these two new methods; existing impls are extended.

## 11. Concurrency

`peerCandidates` is mutex-guarded. `Update` and `NextEndpoint` both take `mu.Lock()`. `NextEndpoint` copies the merged list under the lock and returns it by value so the supervisor doesn't hold the lock during a dial.

## 12. Testing

| File | What |
|---|---|
| `peercandidates_test.go` (new) | Unit: Update + NextEndpoint with empty/non-empty/expired states; merge semantics; ranking; dedup. |
| `reconnect_test.go` (modify) | Supervisor calls fetcher on connect; failed fetch leaves prior state; epIdx cycles base then candidates. Uses an injected fake fetcher. |
| `bootstrap_peers_test.go` (existing, slice 4) | Unchanged. |

## 13. Failure modes

- **`FetchBootstrapPeers` errors during fetch.** Logged at Warn via `cfg.Logger`. Existing candidate set preserved. Plugin continues with operator config.
- **Signed list verification fails (slice 4 logic).** Handled inside `FetchBootstrapPeers`; reroute layer just sees an error, treats it as a fetch failure.
- **Network partition.** Reroute can't help — all endpoints unreachable. Behaves like today.

## 14. Acceptance

- Plugin with `RerouteEnabled=true` and a base set of 2 trackers fetches a bootstrap list on first connect.
- If base[0] drops, supervisor reconnects to base[1]; if base[1] also drops, supervisor tries highest-`health_score` candidate.
- With `RerouteEnabled=false`, behavior is byte-identical to slice 5.

## 15. Future work (out of scope)

- Disk-cached bootstrap list (cross-restart warm start).
- Active probing of candidates to measure RTT (slice 8 lands the data).
- Region-aware ranking.
- Auto-rebind identity if home tracker permanently disappears.
