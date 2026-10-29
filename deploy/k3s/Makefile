# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images, manifests and rollouts of the k3s deployment (README.md). Run
# from the repository's root: make -C deploy/k3s <target>, with KUBECONFIG
# at the cluster. The parameters come from config.env (config.env.example).
# Pushing needs `docker login` at REGISTRY.

REPO_ROOT := $(abspath ../..)
-include config.env
# The images are public on GHCR, beside the code (FeCode's ADR 0012 §2):
# no pull secret. Pushing needs a token that writes the organisation's
# packages; the workflow images.yaml pushes master and tags.
REGISTRY ?= ghcr.io/gardenlinux/feos
FERUN_NODES_IP ?= 10.43.70.70
export DOMAIN REGISTRY OIDC_ISSUER FERUN_NODES_IP FECHAT_MODEL
DOCKER ?= docker
TAG ?= dev
PACKAGES_DIR ?= $(REPO_ROOT)/target/packages
CONTEXT := $(REPO_ROOT)/target/k3s/feos-node
MANIFESTS := $(REPO_ROOT)/target/k3s/manifests

# need(names): stops unless every variable named is set.
need = $(foreach v,$(1),$(if $($(v)),,$(error $(v) is not set: see config.env.example)))

# render(names, command): writes $@, the command's output with ${NAME} of
# the variables named filled in; each must be set, and none may be left.
define render
$(call need,$(1))mkdir -p $(@D)
$(2) | envsubst '$(foreach v,$(1),$${$(v)})' >$@.tmp
@if grep -n '\$${[A-Z_]*}' $@.tmp >&2; then echo "$@: a parameter is left" >&2; rm -f $@.tmp; exit 1; fi
mv $@.tmp $@
endef

# The files of a directory as one stream of documents, in this order.
cat-yaml = awk 'FNR==1{print "---"}1' $(1)

.PHONY: feos-node push-feos-node deploy-feos ferund push-ferund deploy-ferund
.PHONY: manifests apply-feos apply-ferun apply-fechat FORCE

# The packages are make packages' output; their version is feosd's.
feos-node: ## Build the feos DaemonSet's image from target/packages.
	$(call need,REGISTRY)
	@ls $(PACKAGES_DIR)/feosd_*.deb >/dev/null 2>&1 || { echo "no packages; run make packages"; exit 1; }
	rm -rf $(CONTEXT) && mkdir -p $(CONTEXT)
	cp $(PACKAGES_DIR)/*.deb feos/Dockerfile feos/install $(CONTEXT)/
	dpkg-deb -f $$(ls $(PACKAGES_DIR)/feosd_*.deb | head -1) Version > $(CONTEXT)/version
	$(DOCKER) build -t $(REGISTRY)/feos-node:$(TAG) $(CONTEXT)

push-feos-node: feos-node ## Build and push the feos DaemonSet's image.
	$(DOCKER) push $(REGISTRY)/feos-node:$(TAG)

deploy-feos: push-feos-node ## Push and roll the feos DaemonSet, a node at a time.
	kubectl -n feos rollout restart daemonset/feos-node
	kubectl -n feos rollout status daemonset/feos-node --timeout=15m

# ferund's image builds from the repository's root (go/Dockerfile).
ferund: ## Build ferund's image.
	$(call need,REGISTRY)
	$(DOCKER) build -f $(REPO_ROOT)/go/Dockerfile -t $(REGISTRY)/ferund:$(TAG) $(REPO_ROOT)

push-ferund: ferund ## Build and push ferund's image.
	$(DOCKER) push $(REGISTRY)/ferund:$(TAG)

deploy-ferund: push-ferund ## Push and roll ferund, an instance at a time.
	kubectl -n ferun rollout restart deploy/ferund
	kubectl -n ferun rollout status deploy/ferund --timeout=10m

manifests: $(MANIFESTS)/feos.yaml $(MANIFESTS)/ferun.yaml $(MANIFESTS)/fechat.yaml ## Write every part's manifests, with the parameters, to target/k3s/manifests.

$(MANIFESTS)/feos.yaml: FORCE
	$(call render,DOMAIN REGISTRY FERUN_NODES_IP,cat feos/feos.yaml)

$(MANIFESTS)/ferun.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FERUN_NODES_IP,$(call cat-yaml,ferun/namespace.yaml ferun/postgres.yaml ferun/ferund.yaml ferun/ingress.yaml))

$(MANIFESTS)/fechat.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FECHAT_MODEL,$(call cat-yaml,fechat/namespace.yaml fechat/config.yaml fechat/app.yaml fechat/backend.yaml fechat/ingress.yaml))

apply-feos: $(MANIFESTS)/feos.yaml ## Apply the feos DaemonSet and its configuration.
	kubectl apply -f $<

apply-ferun: $(MANIFESTS)/ferun.yaml ## Apply ferund and its PostgreSQL.
	kubectl apply -f $<

apply-fechat: $(MANIFESTS)/fechat.yaml ## Apply FeChat.
	kubectl apply -f $<

FORCE:
