#!/usr/bin/env bash
# test-phase4.sh — End-to-end smoke test for Phase 4 (Notifications, Agent Role Update, User Settings)
#
# Prerequisites:
#   - Server running:  go run ./cmd/server
#   - jq installed:    brew install jq
#
# Usage:
#   ./scripts/test-phase4.sh [BASE_URL]
#
# BASE_URL defaults to http://localhost:8080

set -uo pipefail   # -e intentionally omitted: we collect failures, not abort on first

BASE="${1:-http://localhost:8080}"

# ── Colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; RESET='\033[0m'

pass() { echo -e "  ${GREEN}✓${RESET} $*"; }
fail() { echo -e "  ${RED}✗${RESET} $*"; FAILURES=$((FAILURES+1)); }
skip() { echo -e "  ${YELLOW}~${RESET} $*"; }
section() { echo -e "\n${BOLD}${CYAN}── $* ──${RESET}"; }

FAILURES=0

jqf() { echo "$1" | jq -r "$2" 2>/dev/null || true; }

# ── Dependency check ────────────────────────────────────────────────────────
if ! command -v jq &>/dev/null; then
  echo -e "${RED}Error: jq is required. Install with: brew install jq${RESET}"
  exit 1
fi

# ── Wait for server ─────────────────────────────────────────────────────────
section "Server health"
for i in $(seq 1 10); do
  if curl -sf "$BASE/health" &>/dev/null; then break; fi
  if [[ $i -eq 10 ]]; then
    echo -e "${RED}Server not reachable at $BASE — start with: go run ./cmd/server${RESET}"
    exit 1
  fi
  echo "  Waiting for server... ($i/10)"
  sleep 1
done

HEALTH=$(curl -sf "$BASE/health")
[[ "$(jqf "$HEALTH" .status)" == "ok" ]] \
  && pass "GET /health → ok" \
  || fail "GET /health unexpected: $HEALTH"

# ── Register and login ───────────────────────────────────────────────────────
section "Auth setup"

EMAIL="phase4-$$@example.com"
PASSWORD="TestPass4Phase!"

REG=$(curl -sf -X POST "$BASE/api/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
TOKEN=$(jqf "$REG" .access_token)
REFRESH=$(jqf "$REG" .refresh_token)

[[ -n "$TOKEN" && "$TOKEN" != "null" ]] \
  && pass "Registered and got access_token" \
  || fail "Registration failed: $REG"

# ── Roles ────────────────────────────────────────────────────────────────────
section "Roles"

ROLE=$(curl -sf -X POST "$BASE/api/roles" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"engineer","description":"Software engineer role"}')
ROLE_ID=$(jqf "$ROLE" .id)

[[ -n "$ROLE_ID" && "$ROLE_ID" != "null" ]] \
  && pass "POST /api/roles → id=$ROLE_ID" \
  || fail "create role: $ROLE"

# ── Agents ────────────────────────────────────────────────────────────────────
section "Agents"

AGENT=$(curl -sf -X POST "$BASE/api/agents" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"test-agent-phase4"}')
AGENT_ID=$(jqf "$AGENT" .id)
AGENT_TOKEN=$(jqf "$AGENT" .token)

[[ -n "$AGENT_ID" && "$AGENT_ID" != "null" ]] \
  && pass "POST /api/agents → id=$AGENT_ID" \
  || fail "create agent: $AGENT"

# PATCH /api/agents/{id} — assign role
PATCHED=$(curl -sf -X PATCH "$BASE/api/agents/$AGENT_ID" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"role_id\":\"$ROLE_ID\"}")

[[ "$(jqf "$PATCHED" .role_id)" == "$ROLE_ID" ]] \
  && pass "PATCH /api/agents/{id} → role assigned" \
  || fail "agent role update: $PATCHED"

# PATCH /api/agents/{id} — remove role
CLEARED=$(curl -sf -X PATCH "$BASE/api/agents/$AGENT_ID" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"role_id":null}')

[[ "$(jqf "$CLEARED" .role_id)" == "null" ]] \
  && pass "PATCH /api/agents/{id} → role cleared" \
  || fail "agent role clear: $CLEARED"

# ── Notifications ────────────────────────────────────────────────────────────
section "Notifications"

# GET /api/notifications → [] initially
NOTIFS=$(curl -sf "$BASE/api/notifications" -H "Authorization: Bearer $TOKEN")
COUNT=$(echo "$NOTIFS" | jq 'length' 2>/dev/null || echo "0")
[[ "$COUNT" == "0" ]] \
  && pass "GET /api/notifications → empty initially" \
  || fail "expected empty notifications, got: $NOTIFS"

# PUT /api/notifications/telegram
TG=$(curl -sf -X PUT "$BASE/api/notifications/telegram" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"bot_token":"9999:TESTTOKEN","chat_id":"12345"}')

[[ "$(jqf "$TG" .channel)" == "telegram" ]] \
  && pass "PUT /api/notifications/telegram → saved" \
  || fail "save telegram config: $TG"

# GET /api/notifications → now shows 1 entry
NOTIFS2=$(curl -sf "$BASE/api/notifications" -H "Authorization: Bearer $TOKEN")
COUNT2=$(echo "$NOTIFS2" | jq 'length' 2>/dev/null || echo "0")
[[ "$COUNT2" -ge 1 ]] \
  && pass "GET /api/notifications → $COUNT2 config(s) after upsert" \
  || fail "expected 1 notification config, got: $NOTIFS2"

CHANNEL=$(echo "$NOTIFS2" | jq -r '.[0].channel' 2>/dev/null || echo "")
[[ "$CHANNEL" == "telegram" ]] \
  && pass "  config channel=telegram" \
  || fail "  wrong channel: $CHANNEL"

# Missing fields → 400
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X PUT "$BASE/api/notifications/telegram" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"bot_token":"only-token"}')
[[ "$STATUS" == "400" ]] \
  && pass "PUT /api/notifications/telegram missing chat_id → 400" \
  || fail "expected 400, got $STATUS"

# DELETE /api/notifications/telegram
DEL_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/api/notifications/telegram" \
  -H "Authorization: Bearer $TOKEN")
[[ "$DEL_STATUS" == "204" ]] \
  && pass "DELETE /api/notifications/telegram → 204" \
  || fail "expected 204, got $DEL_STATUS"

# ── User: PUT /api/me (change password) ─────────────────────────────────────
section "User — change password"

NEW_PASSWORD="NewPass4Phase!"
UPDATE=$(curl -sf -X PUT "$BASE/api/me" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"current_password\":\"$PASSWORD\",\"new_password\":\"$NEW_PASSWORD\"}")

[[ "$(jqf "$UPDATE" .email)" == "$EMAIL" ]] \
  && pass "PUT /api/me → password changed, user returned" \
  || fail "change password: $UPDATE"

# Old password should fail
STATUS_OLD=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
[[ "$STATUS_OLD" == "401" ]] \
  && pass "Old password rejected after change → 401" \
  || fail "expected 401 with old password, got $STATUS_OLD"

# New password should work
LOGIN_NEW=$(curl -sf -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$NEW_PASSWORD\"}")
TOKEN2=$(jqf "$LOGIN_NEW" .access_token)
[[ -n "$TOKEN2" && "$TOKEN2" != "null" ]] \
  && pass "New password accepted → got new access_token" \
  || fail "login with new password: $LOGIN_NEW"

# Wrong current password → 401
WRONG=$(curl -s -o /dev/null -w "%{http_code}" -X PUT "$BASE/api/me" \
  -H "Authorization: Bearer $TOKEN2" \
  -H "Content-Type: application/json" \
  -d "{\"current_password\":\"WrongPass!\",\"new_password\":\"AnotherPass!\"}")
[[ "$WRONG" == "401" ]] \
  && pass "Wrong current password → 401" \
  || fail "expected 401 for wrong current password, got $WRONG"

# ── User: DELETE /api/me (account deletion) ──────────────────────────────────
section "User — account deletion"

# Create a separate user to delete so we don't break later tests
DEL_EMAIL="delete-me-$$@example.com"
DEL_REG=$(curl -sf -X POST "$BASE/api/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$DEL_EMAIL\",\"password\":\"DeleteMe123!\"}")
DEL_TOKEN=$(jqf "$DEL_REG" .access_token)

# Wrong password confirmation → 401
STATUS_WRONG=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/api/me" \
  -H "Authorization: Bearer $DEL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"password":"WrongPass!"}')
[[ "$STATUS_WRONG" == "401" ]] \
  && pass "DELETE /api/me wrong password → 401" \
  || fail "expected 401, got $STATUS_WRONG"

# Missing password → 400
STATUS_MISSING=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/api/me" \
  -H "Authorization: Bearer $DEL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{}')
[[ "$STATUS_MISSING" == "400" ]] \
  && pass "DELETE /api/me missing password → 400" \
  || fail "expected 400, got $STATUS_MISSING"

# Correct password → 204
STATUS_DEL=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/api/me" \
  -H "Authorization: Bearer $DEL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"password":"DeleteMe123!"}')
[[ "$STATUS_DEL" == "204" ]] \
  && pass "DELETE /api/me → 204 account deleted" \
  || fail "expected 204, got $STATUS_DEL"

# Login should now fail
STATUS_AFTER=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$DEL_EMAIL\",\"password\":\"DeleteMe123!\"}")
[[ "$STATUS_AFTER" == "401" ]] \
  && pass "Login after deletion → 401" \
  || fail "expected 401 after deletion, got $STATUS_AFTER"

# ── Notifications: auth required ────────────────────────────────────────────
section "Auth enforcement"

STATUS_UNAUTH=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/notifications")
[[ "$STATUS_UNAUTH" == "401" ]] \
  && pass "GET /api/notifications without token → 401" \
  || fail "expected 401, got $STATUS_UNAUTH"

STATUS_PATCH_UNAUTH=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH \
  "$BASE/api/agents/$AGENT_ID" -H "Content-Type: application/json" -d '{"role_id":null}')
[[ "$STATUS_PATCH_UNAUTH" == "401" ]] \
  && pass "PATCH /api/agents/{id} without token → 401" \
  || fail "expected 401, got $STATUS_PATCH_UNAUTH"

# ── Summary ──────────────────────────────────────────────────────────────────
echo ""
if [[ $FAILURES -eq 0 ]]; then
  echo -e "${BOLD}${GREEN}All tests passed.${RESET}"
else
  echo -e "${BOLD}${RED}$FAILURES test(s) failed.${RESET}"
  exit 1
fi
