.PHONY: all test test-integration test-e2e test-e2e-cover test-perf lint check build clean run-local docker

all: check build

build:
	go build -o bin/token-bay-tracker ./cmd/token-bay-tracker

test:
	go test -race -coverprofile=coverage.out ./...

# test-integration runs the build-tagged integration tier under
# tracker/test/integration. Real QUIC, real mTLS, real UDP on
# 127.0.0.1. Excluded from `make test` to keep the default loop fast.
test-integration:
	go test -race -tags=integration ./test/integration/...

# test-e2e builds the tracker + actor Docker images and runs the //go:build e2e
# suite, which brings up the full compose topology (2 trackers + consumer +
# seeder + fedactor), generates keys/configs via e2egen in TestMain, and drives
# all 20 scenarios against the live stack. Requires Docker + `docker compose`.
# Runs ONLY here (never in `make test`/`go test ./...`) via the `e2e` build tag.
test-e2e:
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
	docker build -f test/e2e/Dockerfile.actors -t tokenbay-e2e-actors:dev ../
	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=e2e -count=1 -timeout=20m ./test/e2e/...

# test-e2e-cover runs the same e2e suite against coverage-instrumented
# trackers (go build -cover + GOCOVERDIR, Go integration coverage) and
# prints the tracker's e2e coverage baseline (overall + per-package).
# Additive to test-e2e: separate image tag (token-bay-tracker:cover),
# compose override (test/e2e/compose.cover.yaml), and env-gated stack
# reuse in TestMain. See test/e2e/run-cover.sh.
test-e2e-cover:
	./test/e2e/run-cover.sh

# test-perf builds the tracker image and runs the //go:build perf soak
# harness (test/perf): 10k simulated consumers + 10k simulated seeders
# (defaults) against a tracker fleet that keeps growing during the run.
# Parameters are PERF_* env vars (PERF_DURATION default 1h — see
# docs/superpowers/specs/tracker/2026-07-12-tracker-perf-ci-design.md §7).
# PERF_TEST_TIMEOUT must comfortably exceed PERF_DURATION (ramp + drain
# + fleet setup/teardown ride on top). No -race: the race detector
# cannot carry 20k-goroutine load, and this tier measures throughput,
# not data races (make test already covers the concurrent modules).
PERF_TEST_TIMEOUT ?= 90m
test-perf:
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=perf -count=1 -v -timeout=$(PERF_TEST_TIMEOUT) ./test/perf/...

lint:
	golangci-lint run ./...

check: test lint

clean:
	rm -rf bin/ coverage.out .token-bay-local/

run-local: build
	./scripts/dev-run-local.sh

docker:
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
