#!/usr/bin/env bash
# Shared assertion helpers for E2E installer tests.
# Source this file — do not execute directly.

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  FAIL: $1"; }

assert_file_exists() {
  if [ -f "$1" ]; then pass "$2"; else fail "$2 — $1 not found"; fi
}

assert_dir_exists() {
  if [ -d "$1" ]; then pass "$2"; else fail "$2 — $1 not found"; fi
}

assert_file_contains() {
  if grep -qF "$2" "$1" 2>/dev/null; then pass "$3"; else fail "$3 — '$2' not in $1"; fi
}

assert_file_not_empty() {
  if [ -s "$1" ]; then pass "$2"; else fail "$2 — $1 is empty or missing"; fi
}

assert_file_mode() {
  local mode
  mode=$(stat -c '%a' "$1" 2>/dev/null || echo "???")
  if [ "$mode" = "$2" ]; then pass "$3"; else fail "$3 — expected mode $2, got $mode"; fi
}

assert_file_executable() {
  if [ -x "$1" ]; then pass "$2"; else fail "$2 — $1 not executable"; fi
}

# extract_dashboard_url pulls the magic-link URL from daemon startup output.
# The server banner prints it as: http://localhost:25297/magic-link?token=...
extract_dashboard_url() {
  grep -oE 'http://[^ ]*magic-link\?token=[^ ]*' "$1" | head -1
}

# assert_dashboard_auth_works takes a file containing daemon startup output,
# extracts the magic-link URL the server printed, and launches a headless
# Chromium instance to navigate to it. The frontend exchanges the magic token
# for a JWT and redirects to /dashboard. If the token is invalid, the user
# would land on the login page instead.
assert_dashboard_auth_works() {
  local daemon_log="$1"
  local dashboard_url
  dashboard_url=$(extract_dashboard_url "$daemon_log")

  if [ -z "$dashboard_url" ]; then
    echo "  Daemon output:"
    cat "$daemon_log"
    fail "dashboard: no magic-link URL in daemon output"
    return
  fi
  pass "dashboard URL found in daemon output"

  # Launch a headless browser to navigate to the URL and verify it lands
  # on the dashboard (not the login page). Use if-else to avoid set -e aborting
  # the script when node exits non-zero.
  local browser_output rc
  if browser_output=$(node "$HOME/verify_dashboard.mjs" "$dashboard_url" 2>&1); then
    rc=0
  else
    rc=$?
  fi

  if [ $rc -eq 0 ]; then
    pass "headless browser landed on dashboard"
  else
    echo "  Browser output: $browser_output"
    fail "headless browser did not reach dashboard"
  fi
}

print_results() {
  echo ""
  echo "═══════════════════════════════════"
  echo "  Results: $PASS passed, $FAIL failed"
  echo "═══════════════════════════════════"
  echo ""
  if [ "$FAIL" -gt 0 ]; then
    exit 1
  fi
}
