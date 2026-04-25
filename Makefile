.PHONY: all test lint check build clean proto-check

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
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ]; then \
			echo "=== lint: $$m ==="; \
			$(MAKE) -C $$m lint || exit 1; \
		fi; \
	done

build:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ]; then \
			echo "=== build: $$m ==="; \
			$(MAKE) -C $$m build || exit 1; \
		fi; \
	done

check: test lint

proto-check:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ] && grep -q '^proto-check:' $$m/Makefile; then \
			echo "=== proto-check: $$m ==="; \
			$(MAKE) -C $$m proto-check || exit 1; \
		fi; \
	done

clean:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ]; then \
			$(MAKE) -C $$m clean || true; \
		fi; \
	done
