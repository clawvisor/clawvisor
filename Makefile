.PHONY: build test run run-sqlite migrate lint clean setup tui openclaw-setup openclaw-down

# ── Build ──────────────────────────────────────────────────────────────────────

build: web/dist
	go build -o bin/clawvisor ./cmd/clawvisor

build-server: web/dist
	go build -o bin/clawvisor-server ./cmd/server

web/dist: $(shell find web/src -type f)
	cd web && npm install && npm run build
	@touch web/dist

# ── Test ───────────────────────────────────────────────────────────────────────

test:
	go test ./...

test-verbose:
	go test -v ./...

# ── Run ────────────────────────────────────────────────────────────────────────

# Run locally (rebuilds frontend if web/src changed, then builds + runs)
run: web/dist
	@go build -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor server

run-sqlite:
	@go build -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor server

# Launch TUI dashboard
tui:
	@go build -o bin/clawvisor ./cmd/clawvisor && bin/clawvisor tui

# ── Docker / Cloud ─────────────────────────────────────────────────────────────

# Start Postgres + app with docker compose
up:
	docker compose -f deploy/docker-compose.yml up --build

# Start only Postgres (run app locally with go run)
db-up:
	docker compose -f deploy/docker-compose.yml up postgres -d

db-down:
	docker compose -f deploy/docker-compose.yml down

# Start Clawvisor + Postgres pre-configured for OpenClaw
openclaw-setup:
	bash scripts/openclaw-setup.sh

openclaw-down:
	docker compose -f docker-compose.openclaw.yml down

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

clean:
	rm -rf bin/ web/dist/
