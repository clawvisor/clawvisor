.PHONY: build build-staging build-local install install-local test run run-sqlite run-staging migrate lint clean setup tui eval-intent release test-e2e-install test-e2e test-e2e-ci

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
ENVIRONMENT ?= production
BUILD_DATE ?= $(shell date -u +%Y-%m-%d)
LDFLAGS := -ldflags="-s -w -X github.com/clawvisor/clawvisor/pkg/version.Version=$(VERSION) -X github.com/clawvisor/clawvisor/pkg/version.Environment=$(ENVIRONMENT) -X github.com/clawvisor/clawvisor/pkg/version.SkillPublishedAt=$(BUILD_DATE)"

# ── Build ──────────────────────────────────────────────────────────────────────

build: web/dist
	go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor

IMESSAGE_HELPER_APP := Clawvisor iMessage Helper.app

build-imessage-helper:
	go build $(LDFLAGS) -o bin/clawvisor-imessage-helper ./cmd/imessage-helper
	mkdir -p "bin/$(IMESSAGE_HELPER_APP)/Contents/MacOS"
	cp bin/clawvisor-imessage-helper "bin/$(IMESSAGE_HELPER_APP)/Contents/MacOS/clawvisor-imessage-helper"
	cp cmd/imessage-helper/Info.plist "bin/$(IMESSAGE_HELPER_APP)/Contents/Info.plist"

build-local:
	go build $(LDFLAGS) -o bin/clawvisor-local ./cmd/clawvisor-local

install-local: build-local
	mkdir -p $(HOME)/.clawvisor/bin
	cp bin/clawvisor-local $(HOME)/.clawvisor/bin/clawvisor-local
	[ "$$(uname)" = "Darwin" ] && codesign -s - $(HOME)/.clawvisor/bin/clawvisor-local 2>/dev/null || true
	@echo "Installed clawvisor-local to ~/.clawvisor/bin/"

build-staging: web/dist
	$(MAKE) build ENVIRONMENT=staging

build-server: web/dist
	go build $(LDFLAGS) -o bin/clawvisor-server ./cmd/server

web/dist: $(shell find web/src -type f)
	cd web && npm install && npm run build
	@touch web/dist

install: build
	mkdir -p $(HOME)/.clawvisor/bin $(HOME)/.clawvisor/logs
	cp bin/clawvisor $(HOME)/.clawvisor/bin/clawvisor
	[ "$$(uname)" = "Darwin" ] && codesign -s - $(HOME)/.clawvisor/bin/clawvisor 2>/dev/null || true
	$(HOME)/.clawvisor/bin/clawvisor install
	@echo ""
	@echo 'Add to your PATH: export PATH="$$HOME/.clawvisor/bin:$$PATH"'

# Install the iMessage helper .app bundle separately. It holds Full Disk Access
# and is codesigned independently so that updating the main binary does not
# invalidate the FDA grant.
install-imessage-helper: build-imessage-helper
	mkdir -p $(HOME)/.clawvisor/bin
	@# Only replace the helper if the binary actually changed, to preserve the
	@# existing FDA grant and codesign.
	@if [ -f "$(HOME)/.clawvisor/bin/$(IMESSAGE_HELPER_APP)/Contents/MacOS/clawvisor-imessage-helper" ] && \
	    cmp -s "bin/$(IMESSAGE_HELPER_APP)/Contents/MacOS/clawvisor-imessage-helper" \
	           "$(HOME)/.clawvisor/bin/$(IMESSAGE_HELPER_APP)/Contents/MacOS/clawvisor-imessage-helper"; then \
		echo "imessage-helper: unchanged, skipping install (FDA preserved)"; \
	else \
		rm -rf "$(HOME)/.clawvisor/bin/$(IMESSAGE_HELPER_APP)"; \
		cp -R "bin/$(IMESSAGE_HELPER_APP)" "$(HOME)/.clawvisor/bin/$(IMESSAGE_HELPER_APP)"; \
		[ "$$(uname)" = "Darwin" ] && codesign -s - "$(HOME)/.clawvisor/bin/$(IMESSAGE_HELPER_APP)/Contents/MacOS/clawvisor-imessage-helper" 2>/dev/null || true; \
		echo "imessage-helper: installed and codesigned"; \
	fi

# ── Test ───────────────────────────────────────────────────────────────────────

test:
	go test ./...

test-verbose:
	go test -v ./...

eval-intent:
	go test -v -run TestEvalIntentVerification -count=1 -timeout=300s ./internal/intent/

test-e2e: build
	CLAWVISOR_BIN=$(CURDIR)/bin/clawvisor go test -v -count=1 -timeout=120s ./e2e/smoke/

test-e2e-ci: build
	CLAWVISOR_BIN=$(CURDIR)/bin/clawvisor go test -v -count=1 -timeout=120s -run '^TestCI' ./e2e/smoke/

test-e2e-install: web/dist
	docker build -f e2e/install/Dockerfile -t clawvisor-e2e-install .
	docker run --rm clawvisor-e2e-install /home/testuser/test_clawvisor_install.sh
	docker run --rm clawvisor-e2e-install /home/testuser/test_curl_install.sh

# ── Run ────────────────────────────────────────────────────────────────────────

# Run locally (rebuilds frontend if web/src changed, then builds + runs)
# Use OPEN=1 to auto-open the magic link in a browser: make run OPEN=1
run: web/dist
	@go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor server $(if $(OPEN),--open,)

run-staging: web/dist
	@$(MAKE) run ENVIRONMENT=staging

run-sqlite:
	@go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor server

# Launch TUI dashboard
tui:
	@go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor tui

# ── Docker / Cloud ─────────────────────────────────────────────────────────────

# Run any clawvisor command in Docker (no local Go/Node needed)
# Usage: make docker-exec CMD="version"
docker-exec:
	@mkdir -p $(HOME)/.clawvisor
	docker compose -f deploy/docker-compose.local.yml run --rm -it --build --entrypoint /clawvisor app $(CMD)

# First-time setup via Docker (no local Go/Node needed)
docker-setup:
	$(MAKE) docker-exec CMD="setup"

# Run clawvisor in Docker with ~/.clawvisor mounted (SQLite, single container)
docker:
	@test -f $(HOME)/.clawvisor/config.yaml || { echo "Error: ~/.clawvisor/config.yaml not found. Run 'make docker-setup' first."; exit 1; }
	docker compose -f deploy/docker-compose.local.yml up --build

# Start Postgres + app with docker compose (production-like)
up:
	docker compose -f deploy/docker-compose.yml up --build

# Start only Postgres (run app locally with go run)
db-up:
	docker compose -f deploy/docker-compose.yml up postgres -d

db-down:
	docker compose -f deploy/docker-compose.yml down

# ── Frontend ───────────────────────────────────────────────────────────────────

web-install:
	cd web && npm install

web-dev: web-install
	cd web && npm run dev -- --port 8080 --host 127.0.0.1

web-build:
	cd web && npm run build

# ── Deploy ─────────────────────────────────────────────────────────────────────

deploy:
	gcloud builds submit --config deploy/cloudbuild.yaml

# ── Misc ───────────────────────────────────────────────────────────────────────

lint:
	go vet ./...

setup: build
	@bin/clawvisor setup

release: web/dist
	scripts/build-release.sh v$(VERSION)

clean:
	rm -rf bin/ dist/
	@# Preserve web/dist/{.gitkeep,placeholder.html} so go:embed still works after clean.
	@if [ -d web/dist ]; then find web/dist -mindepth 1 -not -name '.gitkeep' -not -name 'placeholder.html' -delete; fi
