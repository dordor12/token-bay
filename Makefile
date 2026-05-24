MODULES := plugin shared tracker

# Per-module sub-targets are declared phony so `make -jN` can run
# them in parallel. The previous `for m in $(MODULES)` recipe was a
# single shell command and could not be parallelized.
.PHONY: all check clean proto-check
.PHONY: test  $(addprefix test-,$(MODULES))
.PHONY: lint  $(addprefix lint-,$(MODULES))
.PHONY: build $(addprefix build-,$(MODULES))

all: check build

test:  $(addprefix test-,$(MODULES))
lint:  $(addprefix lint-,$(MODULES))
build: $(addprefix build-,$(MODULES))

# Generate explicit per-module rules. Pattern rules (test-%:) cannot
# be used here because .PHONY targets bypass implicit-rule lookup in
# GNU make.
define MODULE_RULES
test-$(1):
	@echo "=== test: $(1) ==="
	@$$(MAKE) -C $(1) test

lint-$(1):
	@echo "=== lint: $(1) ==="
	@$$(MAKE) -C $(1) lint

build-$(1):
	@echo "=== build: $(1) ==="
	@$$(MAKE) -C $(1) build
endef

$(foreach m,$(MODULES),$(eval $(call MODULE_RULES,$(m))))

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
