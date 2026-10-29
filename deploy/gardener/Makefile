# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images, manifests and rollouts of the Gardener shoot's deployment
# (README.md). Run from the repository's root: make -C deploy/gardener
# <target>, with KUBECONFIG at the shoot. The parameters come from
# config.env (config.env.example). Pushing needs `docker login` at
# REGISTRY. ferund's image and FeChat's manifests are deploy/k3s's, built
# and filled in with this directory's parameters; their routes are this
# directory's HTTPRoutes in place of k3s's Ingresses.

REPO_ROOT := $(abspath ../..)
-include config.env
# The images are public on GHCR, beside the code (FeCode's ADR 0012 §2):
# no pull secret. Pushing needs a token that writes the organisation's
# packages; the workflow images.yaml pushes master and tags.
REGISTRY ?= ghcr.io/gardenlinux/feos
# How long calls wait for a node that announced it stops: ferund's
# --rollout-grace (config.env.example).
FERUN_ROLLOUT_GRACE ?= 8m
# The zone of public environments' default hosts, served by ferun-ingress
# (config.env.example); set empty, no host is served.
APPS_ZONE ?= $(if $(DOMAIN),apps.$(DOMAIN))
EDGE_NAME ?= $(if $(DOMAIN),edge.$(DOMAIN))
DOMAIN_CHALLENGE_LABEL ?= _ferun-challenge
# The workers' network, which reaches ferun-ingress's health port and mesh.
NODE_CIDR ?= 10.250.0.0/16
# The Gateway that serves run. and chat.<DOMAIN> (config.env.example).
GATEWAY_NAMESPACE ?= traefik
GATEWAY_NAME ?= platform
# DENY_PREFIXES, what the pods may not reach, has no default: the shoot's
# networks are not every shoot's (config.env.example).
# ferund's --deny-prefix: its built-in set ("default") and DENY_PREFIXES,
# since a set given replaces the built-in one.
comma := ,
FERUN_DENY_PREFIX = $(if $(strip $(DENY_PREFIXES)),default$(comma)$(subst $(eval) ,$(comma),$(strip $(DENY_PREFIXES))))
export DOMAIN REGISTRY OIDC_ISSUER WORKER_POOL NODE_NAME_PREFIX FERUN_NODES_IP FERUN_ROLLOUT_GRACE FECHAT_MODEL APPS_ZONE NODE_CIDR DENY_PREFIXES FERUN_DENY_PREFIX GATEWAY_NAMESPACE GATEWAY_NAME EDGE_NAME EDGE_ADDRESSES DOMAIN_CHALLENGE_LABEL
DOCKER ?= docker
TAG ?= dev
PACKAGES_DIR ?= $(REPO_ROOT)/target/packages
CONTEXT := $(REPO_ROOT)/target/gardener/feos-node
MANIFESTS := $(REPO_ROOT)/target/gardener/manifests

# need(names): stops unless every variable named is set.
need = $(foreach v,$(1),$(if $($(v)),,$(error $(v) is not set: see config.env.example)))

# render(names, command[, optional]): writes $@, the command's output with
# ${NAME} of the variables named filled in; each of names must be set,
# those of optional may be empty, and none may be left.
define render
$(call need,$(1))mkdir -p $(@D)
$(2) | envsubst '$(foreach v,$(1) $(3),$${$(v)})' >$@.tmp
@if grep -n '\$${[A-Z_]*}' $@.tmp >&2; then echo "$@: a parameter is left" >&2; rm -f $@.tmp; exit 1; fi
mv $@.tmp $@
endef

# The files of a directory as one stream of documents, in this order.
cat-yaml = awk 'FNR==1{print "---"}1' $(1)

.PHONY: feos-node push-feos-node deploy-feos feos-k8s push-feos-k8s deploy-feos-k8s
.PHONY: ferund push-ferund deploy-ferund deploy-ferun-ingress
.PHONY: manifests apply-feos apply-ferun apply-ferun-db-import apply-ferun-ingress apply-fechat FORCE

# feosd, diskd, feosctl, Firecracker and the guest from the packages, for
# the feos-node pod (ADR 0020).
feos-node: ## Build the feos-node image from target/packages.
	$(call need,REGISTRY)
	@ls $(PACKAGES_DIR)/feosd_*.deb >/dev/null 2>&1 || { echo "no packages; run make packages"; exit 1; }
	rm -rf $(CONTEXT) && mkdir -p $(CONTEXT)
	cp $(PACKAGES_DIR)/*.deb feos/Dockerfile feos/run $(CONTEXT)/
	dpkg-deb -f $$(ls $(PACKAGES_DIR)/feosd_*.deb | head -1) Version > $(CONTEXT)/version
	$(DOCKER) build -t $(REGISTRY)/feos-node:$(TAG) $(CONTEXT)

push-feos-node: feos-node ## Build and push the feos-node image.
	$(DOCKER) push $(REGISTRY)/feos-node:$(TAG)

deploy-feos: push-feos-node ## Push and roll the feos-node DaemonSet, a node at a time.
	kubectl -n feos rollout restart daemonset/feos-node
	kubectl -n feos rollout status daemonset/feos-node --timeout=30m

feos-k8s: ## Build the feos-k8s image (ADR 0020).
	$(call need,REGISTRY)
	$(DOCKER) build -f feos-k8s/Dockerfile -t $(REGISTRY)/feos-k8s:$(TAG) $(REPO_ROOT)

push-feos-k8s: feos-k8s ## Build and push the feos-k8s image.
	$(DOCKER) push $(REGISTRY)/feos-k8s:$(TAG)

deploy-feos-k8s: $(MANIFESTS)/feos-k8s.yaml push-feos-k8s ## Push, apply and roll feos-k8s.
	kubectl apply -f $(MANIFESTS)/feos-k8s.yaml
	kubectl -n feos rollout restart deployment/feos-k8s
	kubectl -n feos rollout status deployment/feos-k8s --timeout=5m

ferund push-ferund deploy-ferund: ## ferund's image and rollout, as deploy/k3s's.
	$(MAKE) -C ../k3s $@ REGISTRY='$(REGISTRY)' TAG='$(TAG)' DOCKER='$(DOCKER)'

# ferun-ingress ships in ferund's image: roll ferund first (README.md, The
# ingress).
deploy-ferun-ingress: push-ferund ## Push ferund's image and roll ferun-ingress, a pod at a time.
	kubectl -n ferun-ingress rollout restart statefulset/ferun-ingress
	kubectl -n ferun-ingress rollout status statefulset/ferun-ingress --timeout=10m

manifests: $(MANIFESTS)/feos.yaml $(MANIFESTS)/feos-k8s.yaml $(MANIFESTS)/ferun.yaml $(MANIFESTS)/fechat.yaml $(MANIFESTS)/ferun-ingress.yaml ## Write every part's manifests, with the parameters, to target/gardener/manifests.

$(MANIFESTS)/feos.yaml: FORCE
	$(call render,DOMAIN REGISTRY WORKER_POOL FERUN_NODES_IP DENY_PREFIXES,cat feos/feos.yaml)

$(MANIFESTS)/feos-k8s.yaml: FORCE
	$(call render,REGISTRY WORKER_POOL NODE_NAME_PREFIX,cat feos/feos-k8s.yaml)

# The k3s manifests live outside this directory. An empty APPS_ZONE
# switches ferund's hosts off.
$(MANIFESTS)/ferun.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FERUN_NODES_IP FERUN_ROLLOUT_GRACE FERUN_DENY_PREFIX GATEWAY_NAMESPACE GATEWAY_NAME,kubectl kustomize --load-restrictor LoadRestrictionsNone ferun,APPS_ZONE EDGE_NAME EDGE_ADDRESSES DOMAIN_CHALLENGE_LABEL)

# Once, when ferund moves from the StatefulSet postgres to ferun-db
# (README.md): ferun-db created from a copy of postgres-0's database.
$(MANIFESTS)/ferun-db-import.yaml: FORCE
	mkdir -p $(@D)
	kubectl kustomize --load-restrictor LoadRestrictionsNone ferun/import >$@

$(MANIFESTS)/fechat.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FECHAT_MODEL GATEWAY_NAMESPACE GATEWAY_NAME,$(call cat-yaml,$(addprefix ../k3s/fechat/,namespace.yaml config.yaml app.yaml backend.yaml) fechat/route.yaml))

# Only with an apps zone: without one there is nothing to serve.
$(MANIFESTS)/ferun-ingress.yaml: FORCE
	$(call render,DOMAIN REGISTRY FERUN_NODES_IP APPS_ZONE NODE_CIDR,cat ferun-ingress/ferun-ingress.yaml)

# A network of the shoot that DENY_PREFIXES leaves out is open to the
# microVMs, which leave through the feos-node pod on the host's network:
# apply-feos stops on one, of the IPv4 networks the shoot states in the
# ConfigMap kube-system/shoot-info, which Gardener writes.
apply-feos: $(MANIFESTS)/feos.yaml ## Apply the feos-node DaemonSet and its configuration.
	@nets=$$(kubectl -n kube-system get configmap shoot-info \
	    -o jsonpath='{.data.nodeNetwork} {.data.podNetwork} {.data.serviceNetwork}') || \
	  echo "no kube-system/shoot-info: check by hand that DENY_PREFIXES covers the shoot's networks" >&2; \
	awk -v nets="$$nets" -v deny="$$DENY_PREFIXES" ' \
	  function v4(c) { return c ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\/[0-9]+$$/ } \
	  function ip(s, a) { split(s, a, "."); return ((a[1] * 256 + a[2]) * 256 + a[3]) * 256 + a[4] } \
	  function size(c, p, s, k) { split(c, p, "/"); s = 1; for (k = p[2]; k < 32; k++) s *= 2; return s } \
	  function lo(c, p) { split(c, p, "/"); return int(ip(p[1]) / size(c)) * size(c) } \
	  function hi(c) { return lo(c) + size(c) - 1 } \
	  BEGIN { \
	    n = split(nets, N, /[ ,]+/); m = split(deny, D, /[ ]+/); \
	    for (i = 1; i <= n; i++) if (v4(N[i])) { \
	      ok = 0; \
	      for (j = 1; j <= m; j++) if (v4(D[j]) && lo(D[j]) <= lo(N[i]) && hi(N[i]) <= hi(D[j])) ok = 1; \
	      if (!ok) { print "DENY_PREFIXES leaves the shoot network " N[i] " open (kube-system/shoot-info; config.env.example)" > "/dev/stderr"; bad = 1 } \
	    } \
	    exit bad \
	  }'
	kubectl apply -f $<

apply-ferun: $(MANIFESTS)/ferun.yaml ## Apply ferund (three instances) and ferun-db, the shoot's PostgreSQL.
	@if kubectl -n ferun get statefulset postgres >/dev/null 2>&1 && \
	    ! kubectl -n ferun get clusters.postgresql.cnpg.io ferun-db >/dev/null 2>&1; then \
	  echo "ferund's data is in the StatefulSet postgres: move it first (README.md, Moving the database: make apply-ferun-db-import)" >&2; exit 1; fi
	kubectl apply -f $<

apply-ferun-db-import: $(MANIFESTS)/ferun-db-import.yaml ## Once: create ferun-db from the StatefulSet's database (README.md).
	kubectl apply -f $<

apply-ferun-ingress: $(MANIFESTS)/ferun-ingress.yaml ## Apply ferun-ingress, its balancer and the apps zone's certificate.
	kubectl apply -f $<

apply-fechat: $(MANIFESTS)/fechat.yaml ## Apply FeChat, the k3s manifests.
	kubectl apply -f $<

FORCE:
