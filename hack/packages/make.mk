NFPM_VERSION = $(shell cat hack/packages/nfpm.version)
NFPM_TARBALL = nfpm_$(NFPM_VERSION)_Linux_x86_64.tar.gz
NFPM = target/nfpm/nfpm
PACKAGES_DIR = target/packages
# ferund's package, apart from the node's: a node image takes every .deb
# of PACKAGES_DIR, and the control plane is not a node's.
FERUND_PACKAGES_DIR = $(PACKAGES_DIR)/ferund
# The guest's version in a package name, which takes no ~.
GUEST_NAME = $(subst ~,-,$(GUEST_VERSION))
# The times of the files in a package: the last commit that changed what
# it holds, so that a rebuild of a published version is the same file,
# which publish-apt requires. Firecracker's packages change with its
# version and their configuration, the others with every commit.
SOURCE_EPOCH = $(shell git log -1 --format=%ct)
FIRECRACKER_EPOCH = $(shell git log -1 --format=%ct -- hack/firecracker \
	hack/packages/feos-firecracker.yaml hack/packages/feos-firecracker-meta.yaml)

.PHONY: packages package-binaries ferund-release ferund-package

packages: firecracker guest package-binaries $(NFPM) ## Build the node's .deb packages into target/packages/ (ADR 0009).
	mkdir -p $(PACKAGES_DIR)
	rm -f $(PACKAGES_DIR)/*.deb
	for package in feosd feosctl diskd feos-firecracker feos-firecracker-meta feos-guest feos-guest-meta; do \
		case $$package in \
			feos-firecracker*) epoch=$(FIRECRACKER_EPOCH) ;; \
			*) epoch=$(SOURCE_EPOCH) ;; \
		esac; \
		SOURCE_DATE_EPOCH=$$epoch \
		FEOS_VERSION=$(FEOS_VERSION) GUEST_VERSION=$(GUEST_VERSION) GUEST_NAME=$(GUEST_NAME) \
		FIRECRACKER_VERSION=$(FIRECRACKER_VERSION) \
		MUSL_RELEASE=target/$(MUSL_TARGET)/release \
		$(NFPM) package --config hack/packages/$$package.yaml \
			--packager deb --target $(PACKAGES_DIR)/ || exit 1; \
	done

# Per package, so that no binary carries another's features.
package-binaries:
	cargo build --release --features git-version --target=$(MUSL_TARGET) -p feos --bin feosd
	cargo build --release --target=$(MUSL_TARGET) -p feos-authn
	cargo build --release --target=$(MUSL_TARGET) -p feosctl
	cargo build --release --target=$(MUSL_TARGET) -p diskd
	cargo build --release --target=$(MUSL_TARGET) -p diskctl

# ferund, static (CGO_ENABLED=0) for the package's architecture whatever
# the build machine's, with the management UI that make web builds
# embedded, and the package's version as its own. No VCS stamp: the
# version says which commit it is, and the stamp would count untracked
# files, so that a rebuild of a published version in another checkout
# differed from it.
ferund-release: web ## Release build of ferund with its UI: target/go/ferund.
	mkdir -p $(GO_OUT)
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false \
		-ldflags "-X main.version=$(FEOS_VERSION)" -o ../$(GO_OUT)/ferund ./cmd/ferund

ferund-package: ferund-release $(NFPM) ## Build ferund's .deb into target/packages/ferund/.
	mkdir -p $(FERUND_PACKAGES_DIR)
	rm -f $(FERUND_PACKAGES_DIR)/*.deb
	SOURCE_DATE_EPOCH=$(SOURCE_EPOCH) FEOS_VERSION=$(FEOS_VERSION) GO_OUT=$(GO_OUT) \
		$(NFPM) package --config hack/packages/ferund.yaml --packager deb --target $(FERUND_PACKAGES_DIR)/

$(NFPM): hack/packages/nfpm.version hack/packages/nfpm.sha256
	mkdir -p $(dir $(NFPM))
	curl -fsSL -o "$(dir $(NFPM))$(NFPM_TARBALL)" \
		"https://github.com/goreleaser/nfpm/releases/download/v$(NFPM_VERSION)/$(NFPM_TARBALL)"
	echo "$$(cat hack/packages/nfpm.sha256)  $(dir $(NFPM))$(NFPM_TARBALL)" | sha256sum -c -
	tar -xzf "$(dir $(NFPM))$(NFPM_TARBALL)" -C $(dir $(NFPM)) nfpm
	rm -f "$(dir $(NFPM))$(NFPM_TARBALL)"
	touch $@

.PHONY: test-packages
test-packages: ## Install the packages on Debian 13 with systemd and check feosd; needs docker.
	hack/packages/test-packages

.PHONY: test-ferund-package
test-ferund-package: ## Install ferund's package on Debian 13 and check its user and files; needs docker.
	hack/packages/test-ferund-package

# A build of a commit as a version below the release it leads to:
# <FeOS version>~dev.<UTC time>.<commit>.
DEV_VERSION := $(FEOS_VERSION)~dev.$(shell date -u +%Y%m%d%H%M).$(shell git rev-parse --short=7 HEAD 2>/dev/null)

.PHONY: dev-packages
dev-packages: ## Build the packages of HEAD as a dev version, for an image or a node.
	$(MAKE) packages FEOS_VERSION=$(DEV_VERSION)

.PHONY: publish-apt
publish-apt: ## Build the packages of HEAD, ferund's too, as a dev version and publish them to apt.fecode.dev.
	@git diff --quiet HEAD -- crates proto hack go web Cargo.toml Cargo.lock || \
		{ echo "publish-apt: commit what the packages hold first"; exit 1; }
	@# The builds take untracked files too: go build compiles every .go file
	@# of a package, and ferund embeds every migration.
	@test -z "$$(git ls-files --others --exclude-standard -- crates proto hack go web Cargo.toml Cargo.lock)" || \
		{ echo "publish-apt: untracked files in the sources; commit or remove them first:"; \
		  git ls-files --others --exclude-standard -- crates proto hack go web Cargo.toml Cargo.lock; exit 1; }
	$(MAKE) packages FEOS_VERSION=$(DEV_VERSION)
	$(MAKE) ferund-package FEOS_VERSION=$(DEV_VERSION)
	hack/packages/publish-apt $(PACKAGES_DIR)/*.deb $(FERUND_PACKAGES_DIR)/*.deb
