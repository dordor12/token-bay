.PHONY: all test test-integration lint check build clean run-local docker

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

lint:
	golangci-lint run ./...

check: test lint

clean:
	rm -rf bin/ coverage.out .token-bay-local/

run-local: build
	./scripts/dev-run-local.sh

docker:
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
