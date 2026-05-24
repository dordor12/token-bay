---
description: Show the user's current Token-Bay credit balance
allowed-tools: ["Bash", "Read"]
---

# Token-Bay Balance

Read the most recent balance from the audit log (the source of truth on the local machine — Token-Bay's authoritative balance lives at the tracker but the audit log mirrors it).

1. Resolve audit log path: `~/.token-bay/audit.log` (default per plugin spec §9).
2. Run `tail -n 200 ~/.token-bay/audit.log` and parse the last `consumer` records.
3. Sum `cost_credits` (negative entries are starter grants / credits received; positive entries are spends).
4. Show the running total alongside the most recent 5 entries (request_id, served_locally, cost_credits, timestamp).

If the file does not exist, suggest `/token-bay enroll` (the enroll flow seeds the starter grant).
