---
description: Tail the Token-Bay audit log
allowed-tools: ["Bash", "Read"]
---

# Token-Bay Logs

Show the most recent activity from the append-only audit log (`~/.token-bay/audit.log`, per plugin spec §8). Each line is one of:

- `consumer` — your fallback requests (success, refusal, exit).
- `seeder`   — requests you served (when you run with role=seeder).
- `transfer` — cross-region credit moves.

1. Run `tail -n 50 ~/.token-bay/audit.log` (or N if the user gave one).
2. Render each line in human-readable form: timestamp, kind, request_id, key fields.
3. If a `consumer` record's `served_locally=true` and `seeder_id` starts with `refuse:`, surface the refusal reason from the `seeder_id` field — it's the spec §11 diagnostic.

The log is append-only — never truncate it. Rotation is per-file (plugin CLAUDE.md rule #4).
