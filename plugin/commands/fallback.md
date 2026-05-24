---
description: Manually trigger Token-Bay consumer fallback for the current session
allowed-tools: ["Bash", "Read"]
---

# Token-Bay Manual Fallback

Force the consumer flow to enter network mode for the current session. This is the "I know I'm rate-limited, just route my next prompt" override (plugin spec §5.3).

1. Read `~/.token-bay/sidecar.url`. If missing, report "Sidecar is not running" and stop.
2. POST a synthetic `StopFailure{rate_limit}` payload to `<sidecar-url>_hooks/StopFailure` via `curl -s`:
   ```
   curl -s -X POST <url>_hooks/StopFailure \
     -H 'Content-Type: application/json' \
     -d '{"hook_event_name":"StopFailure","session_id":"<current-session>","transcript_path":"","cwd":"","error":"rate_limit"}'
   ```
3. Then GET `<url>_status` and report whether the session is now in network mode (`tracker_phase`, `ccproxy_url`).

If the user has not enrolled (no `~/.token-bay/identity.json`), say so and recommend `/token-bay enroll` first.
