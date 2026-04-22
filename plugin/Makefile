.PHONY: all test lint check build conformance clean install

all: check build

build:
	go build -o bin/token-bay-sidecar ./cmd/token-bay-sidecar

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

conformance:
	@echo "Running bridge conformance suite..."
	go test -tags=conformance ./test/conformance/...

check: test lint

clean:
	rm -rf bin/ coverage.out

install: build
	@echo "TODO: install as Claude Code plugin (pending installer scripting in feature plan)"
