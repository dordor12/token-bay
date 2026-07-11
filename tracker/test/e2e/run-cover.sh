#!/usr/bin/env bash
# run-cover.sh — run the Docker e2e suite against coverage-instrumented
# trackers and report how much of github.com/token-bay/token-bay/tracker/...
# the suite exercises (Go 1.20+ integration coverage).
#
# Flow (make -C tracker test-e2e-cover):
#   1. build token-bay-tracker:cover (go build -cover -covermode=atomic
#      -coverpkg=tracker/...) + tokenbay-e2e-actors:dev
#   2. generate .gen via e2egen (same fixed seeds as main_test.go) and
#      pre-create uid-1000-writable .covdata/{tracker-a,tracker-b}
#   3. compose up with BOTH files (compose.e2e.yaml + compose.cover.yaml)
#   4. run the e2e suite with E2E_REUSE_STACK=1 (TestMain attaches to
#      the running stack instead of owning up/down) and
#      E2E_COMPOSE_EXTRA_FILES so scenario compose() calls carry the
#      cover override too
#   5. `compose stop` the trackers (SIGTERM -> graceful shutdown ->
#      exit 0 flushes coverage counters to GOCOVERDIR; SIGKILL would
#      lose them), then aggregate with `go tool covdata`.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TRACKER_DIR="$(cd "$E2E_DIR/../.." && pwd)"
REPO_DIR="$(cd "$TRACKER_DIR/.." && pwd)"

GEN_DIR="$E2E_DIR/.gen"
COVDATA_DIR="$E2E_DIR/.covdata"
COVERPKG='github.com/token-bay/token-bay/tracker/...'

# Fixed e2egen seeds — MUST stay in sync with seedAHex/seedBHex/
# seedFedHex in test/e2e/main_test.go: with E2E_REUSE_STACK=1 TestMain
# skips generation, so this script's .gen is the one the containers and
# the scenarios' derived expectations both see.
SEED_A="a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
SEED_B="b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
SEED_FED="c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"

compose() {
  docker compose \
    -f "$E2E_DIR/compose.e2e.yaml" \
    -f "$E2E_DIR/compose.cover.yaml" \
    -p tokenbay-e2e \
    "$@"
}

cleanup() {
  compose down -v || true
  rm -rf "$GEN_DIR"
}
trap cleanup EXIT

echo "=== [1/5] building token-bay-tracker:cover + tokenbay-e2e-actors:dev ==="
docker build \
  --build-arg COVER="-cover -covermode=atomic -coverpkg=$COVERPKG" \
  -f "$TRACKER_DIR/deployments/docker/Dockerfile" \
  -t token-bay-tracker:cover "$REPO_DIR"
docker build -f "$E2E_DIR/Dockerfile.actors" -t tokenbay-e2e-actors:dev "$REPO_DIR"

echo "=== [2/5] generating .gen artifacts + .covdata dirs ==="
rm -rf "$GEN_DIR" "$COVDATA_DIR"
mkdir -p "$COVDATA_DIR/tracker-a" "$COVDATA_DIR/tracker-b"
# uid 1000 inside the containers must be able to write counter files
# into the bind mounts regardless of the host uid.
chmod 0777 "$COVDATA_DIR" "$COVDATA_DIR/tracker-a" "$COVDATA_DIR/tracker-b"
(cd "$TRACKER_DIR" && go run ./test/e2e/cmd/e2egen \
  --out "$GEN_DIR" \
  --seed-a "$SEED_A" \
  --seed-b "$SEED_B" \
  --seed-fed "$SEED_FED")

echo "=== [3/5] compose up (cover topology) ==="
compose up -d

echo "=== [4/5] running e2e suite against the cover stack ==="
(cd "$TRACKER_DIR" && \
  E2E_REUSE_STACK=1 \
  E2E_COMPOSE_EXTRA_FILES="$E2E_DIR/compose.cover.yaml" \
  go test -tags=e2e -count=1 -timeout=20m ./test/e2e/...)

echo "=== [5/5] SIGTERM trackers (flush coverage) + aggregate ==="
# `compose stop` sends SIGTERM and waits for the containers to exit —
# the graceful drain is what flushes the coverage counters. Generous
# timeout so a slow drain never escalates to SIGKILL (which would lose
# this process's counters).
compose stop -t 60 tracker-a tracker-b

COVDIRS="$COVDATA_DIR/tracker-a,$COVDATA_DIR/tracker-b"
if [ -z "$(ls -A "$COVDATA_DIR/tracker-a" 2>/dev/null)" ] || [ -z "$(ls -A "$COVDATA_DIR/tracker-b" 2>/dev/null)" ]; then
  echo "ERROR: no coverage counter files in $COVDATA_DIR — did the trackers exit cleanly?" >&2
  exit 1
fi

echo
echo "=== tracker e2e coverage: per-package ==="
(cd "$TRACKER_DIR" && go tool covdata percent -i="$COVDIRS" -pkg="$COVERPKG")

echo
echo "=== tracker e2e coverage: overall ==="
(cd "$TRACKER_DIR" && \
  go tool covdata textfmt -i="$COVDIRS" -pkg="$COVERPKG" -o "$COVDATA_DIR/e2e-cover.out" && \
  go tool cover -func="$COVDATA_DIR/e2e-cover.out" | tail -n 1)
echo
echo "Full function-level profile: $COVDATA_DIR/e2e-cover.out"
echo "(inspect with: cd tracker && go tool cover -func=test/e2e/.covdata/e2e-cover.out)"
