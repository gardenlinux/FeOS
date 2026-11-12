##@ Go (go/: ferund, ADR 0015; ferun-ingress, ADR 0021; ferun-sink, ADR 0026)

GO_DIR := go
GO_OUT := target/go
# The Makefile defines it after including this file.
LOCALBIN ?= $(shell pwd)/bin
# The toolchain go/go.mod names; go downloads it when it is not there.
GO ?= go
PROTOC_GEN_GO_VERSION ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
GO_MODULE := github.com/gardenlinux/feos/go
# Every proto/feos/<api>/v1/<api>.proto becomes the Go package
# $(GO_MODULE)/gen/feos/<api>/v1, named <api>v1. The protos name no Go
# package of their own, so the mapping is given to protoc here.
GO_PROTOS := $(sort $(wildcard proto/feos/*/v1/*.proto))
GO_PROTO_MAP := $(foreach p,$(GO_PROTOS),M$(patsubst proto/%,%,$(p))=$(GO_MODULE)/gen/$(patsubst proto/%/,%,$(dir $(p)));$(word 2,$(subst /, ,$(patsubst proto/%,%,$(p))))v1)
# OpenTelemetry's protos (ADR 0026, the sink takes OTLP) become
# $(GO_MODULE)/gen/otlp/<path>, named as OpenTelemetry's own Go module
# names them: otlp<signal>v1 for collector/<signal>/v1, else <dir>v1.
OTLP_PROTOS := $(sort $(shell find proto/opentelemetry -name '*.proto'))
OTLP_PROTO_MAP := $(foreach p,$(OTLP_PROTOS),M$(patsubst proto/%,%,$(p))=$(GO_MODULE)/gen/otlp/$(patsubst proto/opentelemetry/proto/%/,%,$(dir $(p)));$(if $(findstring /collector/,$(p)),otlp$(word 5,$(subst /, ,$(p)))v1,$(word 4,$(subst /, ,$(p)))v1))
GO_PROTOS += $(OTLP_PROTOS)
GO_PROTO_MAP += $(OTLP_PROTO_MAP)

.PHONY: go-gen go-build feos-k8s go-fmt-check go-vet go-lint go-test go-test-unit go-test-netns web

$(LOCALBIN)/protoc-gen-go: | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(LOCALBIN)/protoc-gen-go-grpc: | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

go-gen: $(LOCALBIN)/protoc-gen-go $(LOCALBIN)/protoc-gen-go-grpc ## Generate go/gen from proto/.
	rm -rf $(GO_DIR)/gen/feos $(GO_DIR)/gen/otlp
	mkdir -p $(GO_DIR)/gen
	protoc -I proto \
		--plugin=protoc-gen-go=$(LOCALBIN)/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(LOCALBIN)/protoc-gen-go-grpc \
		--go_out=$(GO_DIR)/gen --go_opt=module=$(GO_MODULE)/gen \
		$(foreach m,$(GO_PROTO_MAP),'--go_opt=$(m)') \
		--go-grpc_out=$(GO_DIR)/gen --go-grpc_opt=module=$(GO_MODULE)/gen \
		$(foreach m,$(GO_PROTO_MAP),'--go-grpc_opt=$(m)') \
		$(GO_PROTOS)

go-build: feos-k8s ## Build ferund, ferun-ingress, ferun-sink, ferun-auth, ferun-testnode and feos-k8s, static, into target/go/.
	mkdir -p $(GO_OUT)
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferund ./cmd/ferund
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferun-ingress ./cmd/ferun-ingress
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferun-sink ./cmd/ferun-sink
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferun-auth ./cmd/ferun-auth
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferun-testnode ./cmd/ferun-testnode

feos-k8s: ## Build feos-k8s, the controller of FeOS on Kubernetes (ADR 0020), static, into target/go/.
	mkdir -p $(GO_OUT)
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/feos-k8s ./cmd/feos-k8s

go-fmt-check: ## Check the Go code's formatting.
	@out=$$(cd $(GO_DIR) && gofmt -l $$(find . -name '*.go' -not -path './gen/*')); \
	if [ -n "$$out" ]; then echo "gofmt: $$out"; exit 1; fi

go-vet: ## Vet the Go code.
	cd $(GO_DIR) && $(GO) vet ./...

go-lint: go-fmt-check go-vet ## gofmt and go vet.

go-test-unit: ## The Go tests that need no database: those that do skip.
	cd $(GO_DIR) && $(GO) test ./...

go-test: ## All Go tests, against a PostgreSQL and Pebble (ACME) in docker unless FERUN_TEST_DATABASE_URL and FERUN_TEST_ACME_DIRECTORY name them.
	hack/go/with-postgres hack/go/with-pebble sh -c 'cd $(GO_DIR) && $(GO) test -count=1 ./...'

go-test-netns: ## The WireGuard test of ferun-ingress in network namespaces of its own; needs sudo -n and WireGuard.
	GO=$(GO) hack/go/netns-test

web: ## Build the management UI into go/internal/ui/dist.
	cd web && npm ci && npm run build
