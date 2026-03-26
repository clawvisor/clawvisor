#!/usr/bin/env bash
# E2E test: curl -fsSL https://clawvisor.com/install.sh | sh
#
# Runs inside a fresh Docker container with no prior state. A mock HTTP server
# stands in for GitHub, serving the install script and a tarball of the binary.
# After install.sh downloads and sets up the binary, we run it through the full
# setup wizard and verify the daemon starts.
set -euo pipefail
source "$HOME/assertions.sh"

# Non-interactive setup env vars.
export CLAWVISOR_NON_INTERACTIVE=1
export CLAWVISOR_LLM_PROVIDER=openai
export CLAWVISOR_LLM_ENDPOINT=http://localhost:9999
export CLAWVISOR_LLM_MODEL=gpt-4o-mini
export CLAWVISOR_LLM_API_KEY=sk-fake-e2e-test-key
export CLAWVISOR_TELEMETRY=false
export CLAWVISOR_CHAIN_CONTEXT=true
export CLAWVISOR_RELAY_URL=wss://localhost:9998

MOCK_PORT=18493
E2E_BINARY="$HOME/.e2e-bin/clawvisor"

echo ""
echo "═══ E2E: curl | sh install (fresh machine) ═══"
echo ""

# ── 1. Pre-flight: truly nothing exists ──────────────────────────────────────
echo "── 1. Pre-flight"

if [ ! -d "$HOME/.clawvisor" ]; then
  pass "no ~/.clawvisor dir"
else
  fail "~/.clawvisor already exists"
fi

# clawvisor is NOT on PATH — this is a totally fresh machine.
if ! command -v clawvisor &>/dev/null; then
  pass "clawvisor not on PATH (fresh machine)"
else
  fail "clawvisor already on PATH"
fi

if [ ! -f "$HOME/.bashrc" ]; then
  # Create a blank .bashrc like a real fresh Debian install.
  touch "$HOME/.bashrc"
fi

# ── 2. Start mock GitHub server ──────────────────────────────────────────────
echo ""
echo "── 2. Mock GitHub"

MOCK_LOG="$HOME/.mock-server.log"
python3 "$HOME/mock_github_server.py" "$MOCK_PORT" "$E2E_BINARY" "$HOME/install.sh" \
  >"$MOCK_LOG" 2>&1 &
MOCK_PID=$!

MOCK_READY=false
for i in $(seq 1 60); do
  if [ -f "$HOME/.mock-server-ready" ]; then
    MOCK_READY=true
    break
  fi
  # Check if the process died.
  if ! kill -0 "$MOCK_PID" 2>/dev/null; then
    echo "  Mock server process exited early. Log:"
    cat "$MOCK_LOG"
    fail "mock server crashed"
    exit 1
  fi
  sleep 0.5
done

if ! $MOCK_READY; then
  echo "  Mock server did not write ready marker. Log:"
  cat "$MOCK_LOG"
  fail "mock server did not start"
  exit 1
fi
pass "mock server started"

# ── 3. curl | sh — download and install ──────────────────────────────────────
echo ""
echo "── 3. curl | sh"

# This is the exact command a user runs, with two substitutions:
#   - URLs point at the mock server instead of GitHub
#   - CLAWVISOR_SKIP_START=1 so install.sh exits after installing the binary
#     (the next section runs the binary through the full wizard separately)
curl -fsSL "http://127.0.0.1:${MOCK_PORT}/install.sh" | \
  CLAWVISOR_API_BASE="http://127.0.0.1:${MOCK_PORT}" \
  CLAWVISOR_DOWNLOAD_BASE="http://127.0.0.1:${MOCK_PORT}" \
  CLAWVISOR_SKIP_START=1 \
  SHELL=/bin/bash \
  sh

if [ $? -eq 0 ]; then
  pass "curl | sh exit code 0"
else
  fail "curl | sh failed"
fi

INSTALLED_BIN="$HOME/.clawvisor/bin/clawvisor"

assert_file_exists "$INSTALLED_BIN" "binary installed"
assert_file_executable "$INSTALLED_BIN" "binary executable"

if "$INSTALLED_BIN" --help >/dev/null 2>&1; then
  pass "binary runs --help"
else
  fail "binary failed to run"
fi

assert_dir_exists "$HOME/.clawvisor" "data dir"
assert_dir_exists "$HOME/.clawvisor/logs" "logs dir"

assert_file_contains "$HOME/.bashrc" ".clawvisor/bin" "PATH in .bashrc"
assert_file_contains "$HOME/.bashrc" "# Added by Clawvisor installer" "PATH comment"

# ── 4. Idempotency — run install.sh again ────────────────────────────────────
echo ""
echo "── 4. Idempotency"

curl -fsSL "http://127.0.0.1:${MOCK_PORT}/install.sh" | \
  CLAWVISOR_API_BASE="http://127.0.0.1:${MOCK_PORT}" \
  CLAWVISOR_DOWNLOAD_BASE="http://127.0.0.1:${MOCK_PORT}" \
  CLAWVISOR_SKIP_START=1 \
  SHELL=/bin/bash \
  sh

LINE_COUNT=$(grep -c '.clawvisor/bin' "$HOME/.bashrc")
if [ "$LINE_COUNT" -eq 1 ]; then
  pass "PATH not duplicated"
else
  fail "PATH added $LINE_COUNT times"
fi

# Done with mock server.
kill "$MOCK_PID" 2>/dev/null || true
wait "$MOCK_PID" 2>/dev/null || true

# ── 5. Full wizard via installed binary ──────────────────────────────────────
echo ""
echo "── 5. Full wizard + daemon (installed binary)"

# This is what install.sh would have done next: exec clawvisor start.
# The wizard generates config, keys, starts the server — the whole nine yards.
DAEMON_LOG="$HOME/.daemon-output.log"
"$INSTALLED_BIN" start --foreground >"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

HEALTHY=false
for i in $(seq 1 30); do
  if curl -sf http://127.0.0.1:25297/health >/dev/null 2>&1; then
    HEALTHY=true
    break
  fi
  sleep 0.5
done

if $HEALTHY; then
  pass "daemon healthy"
else
  fail "daemon not healthy within 15s"
fi

if $HEALTHY; then
  BODY=$(curl -sf http://127.0.0.1:25297/health 2>/dev/null || echo "{}")
  if echo "$BODY" | grep -q '"status"'; then
    pass "health JSON"
  else
    fail "unexpected health body: $BODY"
  fi
fi

# Verify the wizard ran and generated everything from scratch.
assert_file_exists "$HOME/.clawvisor/config.yaml" "config.yaml generated"
assert_file_exists "$HOME/.clawvisor/vault.key" "vault key generated"
assert_file_exists "$HOME/.clawvisor/daemon-ed25519.key" "Ed25519 key generated"
assert_file_exists "$HOME/.clawvisor/daemon-x25519.key" "X25519 key generated"
assert_file_exists "$HOME/.clawvisor/.local-session" "local session created"
assert_file_exists "$HOME/.clawvisor/clawvisor.db" "database created"

# ── 6. Local session token opens dashboard (not login page) ──────────────────
echo ""
echo "── 6. Dashboard auth"

if $HEALTHY; then
  assert_dashboard_auth_works "$DAEMON_LOG"
else
  fail "skipped dashboard auth test (daemon not healthy)"
fi

kill "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true
pass "daemon stopped"

# ── 7. install.sh script validation ──────────────────────────────────────────
echo ""
echo "── 7. install.sh validation"

bash -n "$HOME/install.sh"
pass "syntax check"

assert_file_contains "$HOME/install.sh" 'set -eu' "strict mode"
assert_file_contains "$HOME/install.sh" 'uname -s' "OS detection"
assert_file_contains "$HOME/install.sh" 'uname -m' "arch detection"
assert_file_contains "$HOME/install.sh" 'chmod +x' "chmod +x"
assert_file_contains "$HOME/install.sh" '.zshrc' "zsh PATH"
assert_file_contains "$HOME/install.sh" '.bash_profile' "bash PATH"

print_results
