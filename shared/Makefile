.PHONY: test lint check clean build proto-gen proto-check

# Source-relative outputs land .pb.go next to the .proto file.
PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto proto/ledger.proto proto/rpc.proto admission/admission.proto federation/federation.proto

test:
	go test -race -coverprofile=coverage.out.raw ./...
	@grep -v '\.pb\.go:' coverage.out.raw > coverage.out
	@rm -f coverage.out.raw

lint:
	golangci-lint run ./...

check: test lint

build:
	@echo "shared is a library; nothing to build"

clean:
	rm -f coverage.out coverage.out.raw

# Regenerate .pb.go files from .proto schemas. Requires protoc + protoc-gen-go on PATH.
proto-gen:
	@command -v protoc >/dev/null 2>&1 || { echo "protoc not found — see plan Task 1 step 1"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "protoc-gen-go not found — see plan Task 1 step 1"; exit 1; }
	protoc -I. \
		--go_out=. --go_opt=paths=source_relative \
		$(PROTO_FILES)

# CI tripwire: regenerate and fail if there is a diff. Catches schemas
# committed without re-running proto-gen.
proto-check: proto-gen
	@if ! git diff --quiet -- $(addsuffix .pb.go,$(basename $(PROTO_FILES))); then \
		echo "ERROR: generated .pb.go files differ from committed; run 'make proto-gen' and commit"; \
		git diff --stat -- $(addsuffix .pb.go,$(basename $(PROTO_FILES))); \
		exit 1; \
	fi
