.PHONY: all test lint check build clean run-local docker

all: check build

build:
	go build -o bin/token-bay-tracker ./cmd/token-bay-tracker

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

check: test lint

clean:
	rm -rf bin/ coverage.out .token-bay-local/

run-local: build
	./scripts/dev-run-local.sh

docker:
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
