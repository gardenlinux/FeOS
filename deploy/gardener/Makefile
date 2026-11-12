# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# Images, manifests and rollouts of the Gardener shoot's deployment
# (README.md). Run from the repository's root: make -C deploy/gardener
# <target>, with KUBECONFIG at the shoot. The parameters come from
# config.env (config.env.example). Pushing needs `docker login` at
# PLATFORM_REGISTRY, where every image of this deployment lives. ferund's image and FeChat's manifests are deploy/k3s's, built
# and filled in with this directory's parameters; their routes are this
# directory's HTTPRoutes in place of k3s's Ingresses.

REPO_ROOT := $(abspath ../..)
-include config.env
# Where the platform's own images live: GHCR, not the projects' registry
# (config.env.example); pulled with the Secret ghcr-pull.
PLATFORM_REGISTRY ?= ghcr.io/gardenlinux/feos
# How long calls wait for a node that announced it stops: ferund's
# --rollout-grace (config.env.example).
FERUN_ROLLOUT_GRACE ?= 8m
# The zone of public environments' default hosts, served by ferun-ingress
# (config.env.example); set empty, no host is served.
APPS_ZONE ?= $(if $(DOMAIN),apps.$(DOMAIN))
# The tenants that own the apps zone's plain names, ferund's
# --apps-zone-owners (config.env.example); empty, none does.
APPS_ZONE_OWNERS ?=
# The zone of the default hosts of the tenants people make, ferund's
# --tenant-zone (ADR 0031, config.env.example); empty, none.
TENANT_ZONE ?=
TENANT_ZONE_TLS_CERT = $(if $(TENANT_ZONE),/etc/ferun-ingress/tenant-tls/tls.crt)
TENANT_ZONE_TLS_KEY = $(if $(TENANT_ZONE),/etc/ferun-ingress/tenant-tls/tls.key)
EDGE_NAME ?= $(if $(DOMAIN),edge.$(DOMAIN))
DOMAIN_CHALLENGE_LABEL ?= _ferun-challenge
ACME_PROFILE ?= shortlived
# The workers' network, which reaches ferun-ingress's health port and mesh.
NODE_CIDR ?= 10.250.0.0/16
# The Gateway that serves run., chat. and oci.<DOMAIN> (config.env.example).
GATEWAY_NAMESPACE ?= gateway
GATEWAY_NAME ?= platform
# DENY_PREFIXES, what the pods may not reach, has no default: the shoot's
# networks are not every shoot's (config.env.example).
# ferund's --deny-prefix: its built-in set ("default") and DENY_PREFIXES,
# since a set given replaces the built-in one.
comma := ,
FERUN_DENY_PREFIX = $(if $(strip $(DENY_PREFIXES)),default$(comma)$(subst $(eval) ,$(comma),$(strip $(DENY_PREFIXES))))
TAG ?= dev
# ferun-sink's image: a platform image, on GHCR with FeOS's others, not in
# the projects' registry (README.md, The sink).
SINK_IMAGE ?= $(PLATFORM_REGISTRY)/ferun-sink:$(TAG)
# ferun-auth (ADR 0054, README.md, Visitors): its host, its image (ferund's,
# which carries /ferun-auth), the namespace of the Gateway's Envoy pods,
# which reach it, and the private blocks an issuer may be in
# (config.env.example). VISITOR_AUTH_URL is ferund's --visitor-auth-url,
# https://<AUTH_HOST> once ferun-auth runs; empty, ferund serves providers
# of kind tenant alone.
AUTH_HOST ?= $(if $(DOMAIN),auth.$(DOMAIN))
AUTH_IMAGE ?= $(PLATFORM_REGISTRY)/ferund:$(TAG)
GATEWAY_PODS_NAMESPACE ?= envoy-gateway-system
AUTH_ALLOW_PREFIXES ?=
VISITOR_AUTH_URL ?=
export DOMAIN REGISTRY PLATFORM_REGISTRY SINK_IMAGE OIDC_ISSUER WORKER_POOL NODE_NAME_PREFIX FERUN_NODES_IP FERUN_ROLLOUT_GRACE FECHAT_MODEL APPS_ZONE APPS_ZONE_OWNERS TENANT_ZONE TENANT_ZONE_TLS_CERT TENANT_ZONE_TLS_KEY NODE_CIDR DENY_PREFIXES FERUN_DENY_PREFIX GATEWAY_NAMESPACE GATEWAY_NAME EDGE_NAME EDGE_ADDRESSES DOMAIN_CHALLENGE_LABEL ACME_DIRECTORY ACME_EMAIL ACME_PROFILE AUTH_HOST AUTH_IMAGE GATEWAY_PODS_NAMESPACE AUTH_ALLOW_PREFIXES VISITOR_AUTH_URL
DOCKER ?= docker
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
.PHONY: ferund push-ferund deploy-ferund deploy-ferun-ingress ferun-sink push-ferun-sink deploy-ferun-sink deploy-ferun-auth
.PHONY: manifests apply-feos apply-ferun apply-ferun-db-import apply-ferun-ingress apply-ferun-sink apply-ferun-auth apply-quickwit apply-kafka apply-fechat apply-registry FORCE

# feosd, diskd, feosctl, Firecracker and the guest from the packages, for
# the feos-node pod (ADR 0020).
feos-node: ## Build the feos-node image from target/packages.
	$(call need,PLATFORM_REGISTRY)
	@ls $(PACKAGES_DIR)/feosd_*.deb >/dev/null 2>&1 || { echo "no packages; run make packages"; exit 1; }
	rm -rf $(CONTEXT) && mkdir -p $(CONTEXT)
	cp $(PACKAGES_DIR)/*.deb feos/Dockerfile feos/run $(CONTEXT)/
	dpkg-deb -f $$(ls $(PACKAGES_DIR)/feosd_*.deb | head -1) Version > $(CONTEXT)/version
	$(DOCKER) build -t $(PLATFORM_REGISTRY)/feos-node:$(TAG) $(CONTEXT)

push-feos-node: feos-node ## Build and push the feos-node image.
	$(DOCKER) push $(PLATFORM_REGISTRY)/feos-node:$(TAG)

deploy-feos: push-feos-node ## Push and roll the feos-node DaemonSet, a node at a time.
	kubectl -n feos rollout restart daemonset/feos-node
	kubectl -n feos rollout status daemonset/feos-node --timeout=30m

feos-k8s: ## Build the feos-k8s image (ADR 0020).
	$(call need,PLATFORM_REGISTRY)
	$(DOCKER) build -f feos-k8s/Dockerfile -t $(PLATFORM_REGISTRY)/feos-k8s:$(TAG) $(REPO_ROOT)

push-feos-k8s: feos-k8s ## Build and push the feos-k8s image.
	$(DOCKER) push $(PLATFORM_REGISTRY)/feos-k8s:$(TAG)

deploy-feos-k8s: $(MANIFESTS)/feos-k8s.yaml push-feos-k8s ## Push, apply and roll feos-k8s.
	kubectl apply -f $(MANIFESTS)/feos-k8s.yaml
	kubectl -n feos rollout restart deployment/feos-k8s
	kubectl -n feos rollout status deployment/feos-k8s --timeout=5m

ferund push-ferund deploy-ferund: ## ferund's image and rollout, as deploy/k3s's.
	$(MAKE) -C ../k3s $@ REGISTRY='$(PLATFORM_REGISTRY)' TAG='$(TAG)' DOCKER='$(DOCKER)'

# ferun-ingress ships in ferund's image: roll ferund first (README.md, The
# ingress).
deploy-ferun-ingress: push-ferund ## Push ferund's image and roll ferun-ingress, a pod at a time.
	kubectl -n ferun-ingress rollout restart statefulset/ferun-ingress
	kubectl -n ferun-ingress rollout status statefulset/ferun-ingress --timeout=10m

ferun-sink: ## Build ferun-sink's image (SINK_IMAGE).
	$(DOCKER) build -f $(REPO_ROOT)/go/Dockerfile --target ferun-sink -t $(SINK_IMAGE) $(REPO_ROOT)

push-ferun-sink: ferun-sink ## Build and push ferun-sink's image.
	$(DOCKER) push $(SINK_IMAGE)

deploy-ferun-sink: push-ferun-sink ## Push ferun-sink's image and roll it.
	kubectl -n ferun set image deployment/ferun-sink ferun-sink=$(SINK_IMAGE)
	kubectl -n ferun rollout status deployment/ferun-sink --timeout=5m

# ferun-auth ships in ferund's image too (README.md, Visitors).
deploy-ferun-auth: push-ferund ## Push ferund's image and roll ferun-auth.
	kubectl -n ferun set image deployment/ferun-auth ferun-auth=$(AUTH_IMAGE)
	kubectl -n ferun rollout status deployment/ferun-auth --timeout=5m

manifests: $(MANIFESTS)/feos.yaml $(MANIFESTS)/feos-k8s.yaml $(MANIFESTS)/ferun.yaml $(MANIFESTS)/fechat.yaml $(MANIFESTS)/registry.yaml $(MANIFESTS)/ferun-ingress.yaml $(MANIFESTS)/ferun-sink.yaml $(MANIFESTS)/ferun-auth.yaml ## Write every part's manifests, with the parameters, to target/gardener/manifests.

$(MANIFESTS)/feos.yaml: REGISTRY = $(PLATFORM_REGISTRY)
$(MANIFESTS)/feos.yaml: FORCE
	$(call render,DOMAIN REGISTRY WORKER_POOL FERUN_NODES_IP DENY_PREFIXES,cat feos/feos.yaml)

$(MANIFESTS)/feos-k8s.yaml: REGISTRY = $(PLATFORM_REGISTRY)
$(MANIFESTS)/feos-k8s.yaml: FORCE
	$(call render,REGISTRY WORKER_POOL NODE_NAME_PREFIX,cat feos/feos-k8s.yaml)

# The k3s manifests live outside this directory. An empty APPS_ZONE
# switches ferund's hosts off.
$(MANIFESTS)/ferun.yaml: REGISTRY = $(PLATFORM_REGISTRY)
$(MANIFESTS)/ferun.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FERUN_NODES_IP FERUN_ROLLOUT_GRACE FERUN_DENY_PREFIX GATEWAY_NAMESPACE GATEWAY_NAME,kubectl kustomize --load-restrictor LoadRestrictionsNone ferun,APPS_ZONE APPS_ZONE_OWNERS TENANT_ZONE EDGE_NAME EDGE_ADDRESSES DOMAIN_CHALLENGE_LABEL ACME_DIRECTORY ACME_EMAIL ACME_PROFILE VISITOR_AUTH_URL)

# Once, when ferund moves from the StatefulSet postgres to ferun-db
# (README.md): ferun-db created from a copy of postgres-0's database.
$(MANIFESTS)/ferun-db-import.yaml: FORCE
	mkdir -p $(@D)
	kubectl kustomize --load-restrictor LoadRestrictionsNone ferun/import >$@

# FeChat's images are on GHCR too: k3s's pull secret oci-pull is ghcr-pull
# here.
$(MANIFESTS)/fechat.yaml: REGISTRY = $(PLATFORM_REGISTRY)
$(MANIFESTS)/fechat.yaml: FORCE
	$(call render,DOMAIN REGISTRY OIDC_ISSUER FECHAT_MODEL GATEWAY_NAMESPACE GATEWAY_NAME,$(call cat-yaml,$(addprefix ../k3s/fechat/,namespace.yaml config.yaml app.yaml backend.yaml) fechat/route.yaml) | sed 's/name: oci-pull$$/name: ghcr-pull/')

$(MANIFESTS)/registry.yaml: FORCE
	$(call render,DOMAIN GATEWAY_NAMESPACE GATEWAY_NAME,cat registry/registry.yaml)

# Only with an apps zone: without one there is nothing to serve. The
# tenant zone's certificate only with TENANT_ZONE.
$(MANIFESTS)/ferun-ingress.yaml: REGISTRY = $(PLATFORM_REGISTRY)
$(MANIFESTS)/ferun-ingress.yaml: FORCE
	$(call render,DOMAIN REGISTRY FERUN_NODES_IP APPS_ZONE NODE_CIDR,$(call cat-yaml,ferun-ingress/ferun-ingress.yaml $(if $(TENANT_ZONE),ferun-ingress/tenant-zone.yaml)),TENANT_ZONE TENANT_ZONE_TLS_CERT TENANT_ZONE_TLS_KEY)

$(MANIFESTS)/ferun-sink.yaml: FORCE
	$(call render,DOMAIN SINK_IMAGE GATEWAY_NAMESPACE GATEWAY_NAME,cat ferun-sink/ferun-sink.yaml)

# Without DENY_PREFIXES ferun-auth denies the built-in set alone.
$(MANIFESTS)/ferun-auth.yaml: FORCE
	$(call render,AUTH_HOST AUTH_IMAGE OIDC_ISSUER GATEWAY_NAMESPACE GATEWAY_NAME GATEWAY_PODS_NAMESPACE NODE_CIDR,cat ferun-auth/ferun-auth.yaml,FERUN_DENY_PREFIX AUTH_ALLOW_PREFIXES)

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

apply-ferun-ingress: $(MANIFESTS)/ferun-ingress.yaml ## Apply ferun-ingress, its balancer and the zones' certificates.
	kubectl apply -f $<

apply-ferun-sink: $(MANIFESTS)/ferun-sink.yaml ## Apply ferun-sink, its database, Services and route (its secrets first: README.md).
	@for s in ferun-sink-token ferun-sink-bucket ferun-sink-db; do \
	  kubectl -n ferun get secret $$s >/dev/null 2>&1 || { echo "the secret ferun/$$s is missing (README.md, The sink)" >&2; exit 1; }; done
	kubectl apply -f $<

apply-ferun-auth: $(MANIFESTS)/ferun-auth.yaml ## Apply ferun-auth, its Service, NetworkPolicy and route (its secrets first: README.md, Visitors).
	@for s in ferun-auth-token ferun-auth-key oidc-client-ferun-auth; do \
	  kubectl -n ferun get secret $$s >/dev/null 2>&1 || { echo "the secret ferun/$$s is missing (README.md, Visitors)" >&2; exit 1; }; done
	kubectl apply -f $<

apply-kafka: ## Apply Strimzi's operator and the Kafka cluster ferun-log (README.md, The sink).
	helm upgrade --install strimzi oci://quay.io/strimzi-helm/strimzi-kafka-operator --version 1.3.0 \
	  -n strimzi --create-namespace -f kafka/operator-values.yaml --wait --timeout 10m
	kubectl apply -f kafka/kafka.yaml
	kubectl -n ferun wait kafka/ferun-log --for=condition=Ready --timeout=15m

# Quickwit's release reads the bucket's name and endpoint from the
# Secret ferun-sink-bucket, as the sink does.
apply-quickwit: ## Apply Quickwit's database, NetworkPolicy and Helm release (README.md, The sink).
	kubectl apply -f quickwit/quickwit.yaml
	helm repo add quickwit https://helm.quickwit.io >/dev/null && helm repo update quickwit >/dev/null
	SINK_BUCKET=$$(kubectl -n ferun get secret ferun-sink-bucket -o jsonpath='{.data.bucket}' | base64 -d) \
	SINK_BUCKET_ENDPOINT=$$(kubectl -n ferun get secret ferun-sink-bucket -o jsonpath='{.data.endpoint}' | base64 -d) \
	  envsubst '$${SINK_BUCKET} $${SINK_BUCKET_ENDPOINT}' <quickwit/values.yaml | \
	  helm upgrade --install quickwit quickwit/quickwit --version 0.8.17 -n ferun -f - --wait --timeout 10m

apply-fechat: $(MANIFESTS)/fechat.yaml ## Apply FeChat, the k3s manifests.
	kubectl apply -f $<

apply-registry: $(MANIFESTS)/registry.yaml ## Apply the image registry.
	kubectl apply -f $<

FORCE:
