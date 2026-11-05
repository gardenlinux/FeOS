MUSL_TARGET := x86_64-unknown-linux-musl
# FeOS's version: the workspace's.
FEOS_VERSION ?= $(shell sed -n 's/^version = "\(.*\)"/\1/p' Cargo.toml | head -1)

##@ Development

.PHONY: all clippy fmt-check release release-guest run clean cli test test-unit

# feos builds in two profiles: the host profile (the default features) for
# bare-metal hypervisors, and the guest profile for PID 1 in a microVM.
GUEST_FEATURES := --no-default-features --features guest

clippy: ## Lint the workspace in the host profile and feos in the guest profile.
	cargo clippy --workspace --all-targets -- -D warnings
	cargo clippy -p feos $(GUEST_FEATURES) --all-targets -- -D warnings
	cargo clippy -p feos --all-features --all-targets -- -D warnings

fmt-check: ## Check formatting.
	cargo fmt --check

all: clippy ## Debug build of the workspace for musl.
	cargo build --target=$(MUSL_TARGET) --workspace

# Release artifacts are built per package with -p, never with --workspace:
# that would unify the features of all crates into every binary.
release: clippy ## Release build of feosd, feos-authn, diskd and feos-host-init.
	cargo build --release --features git-version --target=$(MUSL_TARGET) -p feos --bin feosd
	cargo build --release --target=$(MUSL_TARGET) -p feos-authn
	cargo build --release --target=$(MUSL_TARGET) -p diskd
	cargo build --release --features git-version --target=$(MUSL_TARGET) -p feos-host-init

release-guest: clippy ## Release build of feos-guest.
	cargo build --release $(GUEST_FEATURES) --features git-version --target=$(MUSL_TARGET) -p feos --bin feos-guest

run: all ## Run the debug build as root.
	sudo ./target/$(MUSL_TARGET)/debug/feosd

clean:
	rm -rf target

test: clippy ## All tests. The integration tests need root.
	cargo test --workspace

test-unit: ## Unit tests only; they run without root.
	cargo test --workspace --lib --bins

cli: ## Release build of feosctl.
	cargo build --package feosctl --release

.PHONY: add-license
add-license: addlicense ## Add license headers to all Rust files.
	find . -name '*.rs' -exec "$(ADDLICENSE)" -f hack/license-header.txt {} +

.PHONY: check-license
check-license: addlicense ## Check license headers in all Rust files.
	find . -name '*.rs' -exec "$(ADDLICENSE)" -check -c 'IronCore authors' {} +

include hack/hack.mk

##@ Tools

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ADDLICENSE ?= $(LOCALBIN)/addlicense
ADDLICENSE_VERSION ?= v1.1.1

.PHONY: addlicense
addlicense: $(ADDLICENSE) ## Download addlicense locally if necessary.
$(ADDLICENSE): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install github.com/google/addlicense@$(ADDLICENSE_VERSION)
