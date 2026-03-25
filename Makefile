.PHONY: build install test run run-sqlite migrate lint clean setup tui eval-intent release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -ldflags="-s -w -X github.com/clawvisor/clawvisor/pkg/version.Version=$(VERSION)"

# ── Build ──────────────────────────────────────────────────────────────────────

build: web/dist
	go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor

build-server: web/dist
	go build $(LDFLAGS) -o bin/clawvisor-server ./cmd/server

web/dist: $(shell find web/src -type f)
	cd web && npm install && npm run build
	@touch web/dist

install: build
	mkdir -p $(HOME)/.clawvisor/bin $(HOME)/.clawvisor/logs
	cp bin/clawvisor $(HOME)/.clawvisor/bin/clawvisor
	@echo "Installed to $(HOME)/.clawvisor/bin/clawvisor"
	@echo 'Add to your PATH: export PATH="$$HOME/.clawvisor/bin:$$PATH"'
	$(HOME)/.clawvisor/bin/clawvisor install

# ── Test ───────────────────────────────────────────────────────────────────────

test:
	go test ./...

test-verbose:
	go test -v ./...

eval-intent:
	go test -v -run TestEvalIntentVerification -count=1 -timeout=300s ./internal/intent/

# ── Run ────────────────────────────────────────────────────────────────────────

# Run locally (rebuilds frontend if web/src changed, then builds + runs)
# Use OPEN=1 to auto-open the magic link in a browser: make run OPEN=1
run: web/dist
	@go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor server $(if $(OPEN),--open,)

run-sqlite:
	@go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor server

# Launch TUI dashboard
tui:
	@go build $(LDFLAGS) -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor tui

# ── Docker / Cloud ─────────────────────────────────────────────────────────────

# Start Postgres + app with docker compose
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

web-dev:
	cd web && npm run dev

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
	rm -rf bin/ web/dist/ dist/
