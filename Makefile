.PHONY: all test lint check build clean

MODULES := plugin shared tracker

all: check build

test:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ]; then \
			echo "=== test: $$m ==="; \
			$(MAKE) -C $$m test || exit 1; \
		fi; \
	done

lint:
	golangci-lint run ./...

build:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ]; then \
			echo "=== build: $$m ==="; \
			$(MAKE) -C $$m build || exit 1; \
		fi; \
	done

check: test lint

clean:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ]; then \
			$(MAKE) -C $$m clean || true; \
		fi; \
	done
