#!/usr/bin/env bash
set -euo pipefail

# Local development runner for token-bay-tracker.
#
# Boots the tracker with a throwaway keypair and SQLite DB under
# ./.token-bay-local/. Intended for manual testing against a locally-running
# plugin. The directory is gitignored.

HERE="$(cd "$(dirname "$0")/.." && pwd)"
DATA_DIR="${HERE}/.token-bay-local"
mkdir -p "${DATA_DIR}"

echo "=== token-bay-tracker local dev run ==="
echo "  data dir: ${DATA_DIR}"
echo "  binary:   ${HERE}/bin/token-bay-tracker"
echo ""
echo "TODO: wire this script to the server 'run' subcommand once it lands"
echo "in a subsequent feature plan. For now this script is a placeholder so"
echo "the Makefile 'run-local' target has something to invoke."
echo ""
echo "ERROR: run subcommand not yet implemented — exiting non-zero so" >&2
echo "       'make run-local' does not silently succeed." >&2
exit 1
