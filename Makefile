# Development entry points. The defaults match .github/workflows/ci.yml so a
# local `make ci` reproduces what the CI gate enforces.

# Working tree version carries the next-release number with a -dev suffix; the
# real (suffix-less) number is set only when cutting a tag:
# make release VERSION=v0.8.10. Tag builds stamp the tag name via release.yml.
VERSION ?= 0.8.10
# Commit stamps the exact tree into the binary (velruna_status reports it).
# `?=` + shell fallback: local builds get the live HEAD; CI can pass it explicitly.
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/NemoNobody/velruna/internal/version.Version=$(VERSION) -X github.com/NemoNobody/velruna/internal/version.Commit=$(COMMIT)
BIN     ?= velruna

CONTAINER ?= $(shell if command -v docker >/dev/null 2>&1; then echo "docker compose"; elif command -v podman-compose >/dev/null 2>&1; then echo "podman-compose"; else echo "podman compose"; fi)
OSFILE := docker/opensearch.yml

.PHONY: build test lint fmt vet ci install clean release dev-opensearch dev-opensearch-down dev-opensearch-logs dev-opensearch-dashboards dev-opensearch-cluster dev-opensearch-secure dev-opensearch-secure-dashboards docker-build docker-smoke stack-up stack-down mcpb

# Compose profiles in docker/opensearch.yml. `down` must name them all or
# profiled services survive it.
OS_PROFILES := --profile single --profile dashboards --profile cluster --profile secure --profile secure-dash --profile app

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	go test ./...

# Keyring integration: a disposable session bus + gnome-keyring with a
# throwaway HOME, so the developer's real wallet is never touched. The tests
# are opt-in (VELRUNA_KEYRING_TEST=1) because a plain `go test` must not write
# to a live keychain; CI runs this target on Linux and the equivalent against
# the macOS Keychain (internal/keyring).
test-keyring:
	@command -v dbus-run-session >/dev/null 2>&1 || { echo "test-keyring needs dbus-run-session (dbus) and gnome-keyring-daemon"; exit 1; }
	@command -v gnome-keyring-daemon >/dev/null 2>&1 || { echo "test-keyring needs gnome-keyring-daemon (package: gnome-keyring)"; exit 1; }
	dbus-run-session -- bash -c 'h=$$(mktemp -d); export HOME=$$h; eval "$$(echo -n velruna-ci | gnome-keyring-daemon --unlock --replace --components=secrets)" >/dev/null 2>&1; VELRUNA_KEYRING_TEST=1 go test ./internal/keyringnative/ -v; rc=$$?; rm -rf "$$h"; exit $$rc'

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

ci: fmt-check build vet test lint

fmt-check:
	test -z "$$(gofmt -l .)"

# Install the release binary outside the work tree, which is the layout Zed
# and other long-lived clients should point at: a running server must not
# depend on a binary that `go build` overwrites mid-development.
install:
	mkdir -p $(HOME)/.local/bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(HOME)/.local/bin/$(BIN) .

# Dev/single-machine OpenSearch: single node, localhost-only,
# security plugin off (the dev rule), named data volume, snapshots on the host.
dev-opensearch:
	$(CONTAINER) -f $(OSFILE) --profile single up -d

dev-opensearch-down:
	$(CONTAINER) -f $(OSFILE) $(OS_PROFILES) down

dev-opensearch-logs:
	$(CONTAINER) -f $(OSFILE) logs --tail=100

# Optional OpenSearch Dashboards on top of the dev node (compose profile
# "dashboards"): make dev-opensearch first, then this. UI on
# http://127.0.0.1:5601 (VELRUNA_DASH_PORT to move it). The security plugin
# is off on the dev node, so the UI opens without login. The target waits
# for the UI to answer and then pushes the ready-made bundle (index
# patterns, visualizations, overview dashboard) into it automatically —
# re-runnable (stable object ids). For any other (secured) cluster run
# `velruna dashboards push` by hand (same credentials as the MCP server).
dev-opensearch-dashboards:
	$(CONTAINER) -f $(OSFILE) --profile single --profile dashboards up -d
	@echo "waiting for Dashboards on http://127.0.0.1:$${VELRUNA_DASH_PORT:-5601} ..."
	@ok=""; for i in $$(seq 1 60); do \
	  if curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$${VELRUNA_DASH_PORT:-5601}/api/status"; then ok=1; break; fi; \
	  sleep 3; \
	done; \
	if [ -z "$$ok" ]; then \
	  echo "dashboards did not answer within ~3 minutes"; exit 1; \
	fi; \
	for i in $$(seq 1 10); do \
	  if OPENSEARCH_URL="http://127.0.0.1:$${VELRUNA_OS_PORT:-9200}" OPENSEARCH_USER=dev OPENSEARCH_PASSWORD=dev go run . dashboards push; then ok=1; break; fi; \
	  ok=""; echo "bundle push failed (dashboards may still be initializing), retrying in 5s..."; sleep 5; \
	done; \
	if [ -z "$$ok" ]; then \
	  echo "bundle push still failing; manual retry: OPENSEARCH_URL=http://127.0.0.1:$${VELRUNA_OS_PORT:-9200} velruna dashboards push"; exit 1; \
	fi

# 3-node cluster (topology test): velruna stamps replicas automatically
# (index_replicas=-1: 0 on a lone node, 1 on a real cluster). Endpoints:
# 127.0.0.1:9400/9401/9402. Point a config at it to smoke-test velruna
# against real shard allocation.
dev-opensearch-cluster:
	$(CONTAINER) -f $(OSFILE) --profile cluster up -d

# single node WITH security (auth/https smoke): the image installs the
# demo security config using the password passed on the make command
# line — e.g. make dev-opensearch-secure VELRUNA_SECURE_DEV_PASSWORD=...
# then connect with opensearch_url=https://127.0.0.1:9443,
# opensearch_user=admin and insecure_skip_verify=true (demo certs).
dev-opensearch-secure:
	@test -n "$(VELRUNA_SECURE_DEV_PASSWORD)" || { echo "usage: make dev-opensearch-secure VELRUNA_SECURE_DEV_PASSWORD=<pw>"; exit 1; }
	$(CONTAINER) -f $(OSFILE) --profile secure up -d

# Container image. Release tags are published by .github/workflows/image.yml;
# locally the build stamps the same version/commit as `make build`.
IMAGE  ?= ghcr.io/nemonobody/velruna
DOCKER ?= $(shell if command -v docker >/dev/null 2>&1; then echo docker; else echo podman; fi)

docker-build:
	$(DOCKER) build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  -t $(IMAGE):dev .

# MCP over stdio: an agent client starts the container itself (see docs/mcp.md),
# so there is no port to publish and no long-running service. This smoke only
# proves the image runs and the binary is intact (distroless has no shell, so
# `--help` is the cheapest proof).
docker-smoke: docker-build
	$(DOCKER) run --rm $(IMAGE):dev --help >/dev/null
	@echo "image ok: $(IMAGE):dev"

# OpenSearch up, then one velruna run against it. The service is deliberately
# not left running: a stdio MCP server has nothing to do until a client starts
# it, and an exited container lying around is just confusing.
stack-up:
	$(CONTAINER) -f $(OSFILE) --profile single up -d
	@echo "waiting for OpenSearch on http://127.0.0.1:$${VELRUNA_OS_PORT:-9200} ..."
	@ok=""; for i in $$(seq 1 60); do \
	  if curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$${VELRUNA_OS_PORT:-9200}/_cluster/health"; then ok=1; break; fi; \
	  sleep 3; \
	done; \
	if [ -z "$$ok" ]; then echo "OpenSearch did not answer within ~6 minutes"; exit 1; fi
	$(CONTAINER) -f $(OSFILE) --profile single --profile app run --rm velruna status

stack-down:
	$(CONTAINER) -f $(OSFILE) $(OS_PROFILES) down

# One MCPB bundle with every platform build, exactly what release.yml attaches
# to a tag: manifest + launcher + 5 binaries. Useful to check the bundle by hand
# (a desktop client can install it from the file).
DIST ?= dist
mcpb:
	@set -e; rm -rf $(DIST); mkdir -p $(DIST); \
	for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; out=$(DIST)/velruna-$$os-$$arch; \
	  if [ "$$os" = windows ]; then out=$$out.exe; fi; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out .; \
	done
	scripts/build-mcpb.sh $(VERSION) $(DIST) $(DIST)/velruna-$(VERSION).mcpb

# Secured node + Dashboards with the security plugin ON: the server-shaped
# recipe (login required, tenant switching real, https). Demo certificates, so
# verification is off inside the stack. UI on http://127.0.0.1:5643
# (VELRUNA_DASH_SECURE_PORT to move it); login admin / the password you pass.
dev-opensearch-secure-dashboards:
	@test -n "$(VELRUNA_SECURE_DEV_PASSWORD)" || { echo "usage: make dev-opensearch-secure-dashboards VELRUNA_SECURE_DEV_PASSWORD=<pw>"; exit 1; }
	$(CONTAINER) -f $(OSFILE) --profile secure --profile secure-dash up -d
	@echo "waiting for secured Dashboards on http://127.0.0.1:$${VELRUNA_DASH_SECURE_PORT:-5643} ..."
	@ok=""; for i in $$(seq 1 80); do \
	  code=$$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$${VELRUNA_DASH_SECURE_PORT:-5643}/api/status"); \
	  if [ "$$code" = "200" ] || [ "$$code" = "401" ]; then ok=1; break; fi; \
	  sleep 3; \
	done; \
	if [ -z "$$ok" ]; then echo "secured Dashboards did not answer within ~4 minutes"; exit 1; fi
	@echo "UI: http://127.0.0.1:$${VELRUNA_DASH_SECURE_PORT:-5643}  (login admin / your VELRUNA_SECURE_DEV_PASSWORD)"

# obscura: one-time installer for the opt-in render fallback (fetch_render=
# obscura). Third-party binary, ~75MB, linux x86_64 glibc 2.35+; the sha256
# is pinned to the audited 0.2.6 stealth build. obscura and obscura-worker
# must stay in one directory (upstream layout); the default
# fetch_obscura_path is "obscura" resolved through PATH, so ~/.local/bin
# must be on PATH. Enable with fetch_render=obscura in the config.
OBSCURA_VERSION ?= 0.2.6
OBSCURA_SHA256 ?= ac8057374ec1b5cabb78ea2d08b2ed890d21d156a42b1ac23357fd049edc08c5
obscura:
	@set -e; tmp=$$(mktemp /tmp/obscura-stealth-XXXXXX.tar.gz); \
	trap 'rm -f "$$tmp"' EXIT; \
	mkdir -p $(HOME)/.local/bin; \
	curl -fsSL --max-time 600 -o "$$tmp" \
		https://github.com/h4ckf0r0day/obscura/releases/download/v$(OBSCURA_VERSION)/obscura-x86_64-linux-stealth.tar.gz; \
	echo "$(OBSCURA_SHA256)  $$tmp" | sha256sum -c -; \
	tar -xzf "$$tmp" -C $(HOME)/.local/bin obscura obscura-worker; \
	chmod +x $(HOME)/.local/bin/obscura $(HOME)/.local/bin/obscura-worker
	@echo "obscura $(OBSCURA_VERSION) installed into $(HOME)/.local/bin; set fetch_render=obscura (env VELRUNA_FETCH_RENDER=obscura) to enable the render fallback"

clean:
	rm -f $(BIN)

# release: cut a versioned release: tag HEAD and push; .github/workflows/
# release.yml builds the binaries and publishes the GitHub release.
# Requires a clean tree; usage: make release VERSION=v0.8.10
release: test
	@printf '%s' "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$' || { echo "usage: make release VERSION=v0.8.10"; exit 2; }
	git diff --exit-code
	git diff --cached --exit-code
	git tag -a $(VERSION) -m "release $(VERSION)"
	git push origin HEAD --follow-tags
