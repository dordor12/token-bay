---
description: Show Token-Bay sidecar status (tracker connection, ccproxy URL, running state)
allowed-tools: ["Bash", "Read"]
---

# Token-Bay Status

Read the discovery file `~/.token-bay/sidecar.url` and GET `/_status` to render the current sidecar state.

1. Run `cat ~/.token-bay/sidecar.url 2>/dev/null || echo MISSING`.
2. If `MISSING`, report: "Sidecar is not running. Start it with `token-bay-sidecar run --config ~/.token-bay/config.yaml`."
3. Otherwise, the file contents are the base URL. GET `<url>_status` via `curl -s` and pretty-print the JSON (`jq .` if available).
4. Highlight `running`, `tracker_phase`, `ccproxy_url`. If `tracker_last_error` is set, surface it as a warning.
