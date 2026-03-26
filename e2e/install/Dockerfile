# E2E installer test — simulates a completely fresh Linux machine.
# Build context must be the repo root so we can COPY the full source tree.
#
# Each test runs in its own `docker run` — a truly fresh container every time.
#
# Usage:
#   docker build -f e2e/install/Dockerfile -t clawvisor-e2e-install .
#   docker run --rm clawvisor-e2e-install /home/testuser/test_clawvisor_install.sh
#   docker run --rm clawvisor-e2e-install /home/testuser/test_curl_install.sh

# ── Stage 1: build the binary ────────────────────────────────────────────────
FROM node:20-bookworm AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/web/dist web/dist
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/clawvisor/clawvisor/pkg/version.Version=0.0.0-e2e" \
    -o /clawvisor ./cmd/clawvisor

# ── Stage 2: fresh machine ───────────────────────────────────────────────────
FROM debian:bookworm-slim

# Minimal packages a real user would have.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl ca-certificates procps python3 \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 20 from the NodeSource setup (Debian bookworm ships v18
# which lacks stable top-level await in .mjs files).
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install Playwright and its Chromium browser + OS deps (must be root).
RUN npm install -g playwright@1.52.0 \
    && npx playwright install --with-deps chromium

# Create a non-root user to match real-world usage.
RUN useradd -m -s /bin/bash testuser
USER testuser
WORKDIR /home/testuser

# Install Playwright in testuser's home and download Chromium into testuser's cache.
RUN npm init -y --silent >/dev/null 2>&1 \
    && npm install playwright@1.52.0 --silent 2>&1 | tail -1 \
    && npx playwright install chromium

# Stash the binary where test scripts can find it. NOT on PATH and NOT in
# ~/.clawvisor — the test decides how the binary reaches the user.
COPY --chown=testuser:testuser --from=builder /clawvisor /home/testuser/.e2e-bin/clawvisor

# Copy test scripts and supporting files.
COPY --chown=testuser:testuser e2e/install/test_clawvisor_install.sh /home/testuser/test_clawvisor_install.sh
COPY --chown=testuser:testuser e2e/install/test_curl_install.sh /home/testuser/test_curl_install.sh
COPY --chown=testuser:testuser e2e/install/assertions.sh /home/testuser/assertions.sh
COPY --chown=testuser:testuser e2e/install/mock_github_server.py /home/testuser/mock_github_server.py
COPY --chown=testuser:testuser e2e/install/verify_dashboard.mjs /home/testuser/verify_dashboard.mjs
COPY --chown=testuser:testuser scripts/install.sh /home/testuser/install.sh

RUN chmod +x /home/testuser/test_clawvisor_install.sh \
             /home/testuser/test_curl_install.sh \
             /home/testuser/assertions.sh
