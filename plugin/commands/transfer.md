---
description: Move Token-Bay credits to another region (stub — pending shared/proto schema change)
allowed-tools: ["Bash"]
---

# Token-Bay Transfer

Cross-region credit transfer (federation spec §4). The cmd-layer transfer subcommand is wired (`token-bay-sidecar transfer --to <region> --amount <n>`), but full end-to-end is gated on a shared/proto schema change that adds the consumer-sig field to TransferRequest.

For now, this slash command surfaces the limitation:

1. Print: "not yet wired pending shared/proto schema change (federation §4.2 follow-up). The CLI `token-bay-sidecar transfer` exists but the tracker-side handler currently records intent without applying credits at the destination."
2. If the user really wants to invoke the half-wired path, point them at `token-bay-sidecar transfer --dry-run --to <region> --amount <n> --config ~/.token-bay/config.yaml` to see the request preview.
