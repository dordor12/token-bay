.PHONY: test lint check clean build

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

check: test lint

build:
	@echo "shared is a library; nothing to build"

clean:
	rm -f coverage.out
