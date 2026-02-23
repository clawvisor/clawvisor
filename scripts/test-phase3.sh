#!/usr/bin/env bash
# test-phase3.sh — End-to-end smoke test for Phase 3 (Gateway, Adapters, Approvals)
#
# Prerequisites:
#   - Server running:  go run ./cmd/server
#   - jq installed:    brew install jq
#
# Usage:
#   ./scripts/test-phase3.sh [BASE_URL]
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

# jq_field <json> <filter>  — returns the value or empty string on error
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
    echo -e "${RED}Server not reachable at $BASE${RESET}"
    echo -e "${RED}Start it with:  go run ./cmd/server${RESET}"
    exit 1
  fi
  echo "  Waiting for server... ($i/10)"
  sleep 1
done

HEALTH=$(curl -sf "$BASE/health")
[[ "$(jqf "$HEALTH" .status)" == "ok" ]] \
  && pass "GET /health → ok" \
  || fail "GET /health unexpected: $HEALTH"

READY=$(curl -sf "$BASE/ready" || echo '{"status":"error"}')
[[ "$(jqf "$READY" .status)" == "ok" ]] \
  && pass "GET /ready → ok" \
  || fail "GET /ready: $READY"

# ── Auth ────────────────────────────────────────────────────────────────────
section "Auth"

EMAIL="testuser-$$@example.com"
PASSWORD="Test1234!"

# register → {"user":{...}, "access_token":"...", "refresh_token":"..."}
REG=$(curl -sf -X POST "$BASE/api/auth/register" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
[[ "$(jqf "$REG" .user.email)" == "$EMAIL" ]] \
  && pass "POST /api/auth/register" \
  || fail "register: $REG"

LOGIN=$(curl -sf -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}")
TOKEN=$(jqf "$LOGIN" .access_token)
REFRESH_TOKEN=$(jqf "$LOGIN" .refresh_token)
[[ -n "$TOKEN" && "$TOKEN" != "null" ]] \
  && pass "POST /api/auth/login → got access_token" \
  || fail "login: $LOGIN"

ME=$(curl -sf "$BASE/api/me" -H "Authorization: Bearer $TOKEN")
[[ "$(jqf "$ME" .email)" == "$EMAIL" ]] \
  && pass "GET /api/me → correct email" \
  || fail "me: $ME"

# Refresh token
REFRESHED=$(curl -sf -X POST "$BASE/api/auth/refresh" \
  -H "Content-Type: application/json" \
  -d "{\"refresh_token\":\"$REFRESH_TOKEN\"}")
NEW_TOKEN=$(jqf "$REFRESHED" .access_token)
[[ -n "$NEW_TOKEN" && "$NEW_TOKEN" != "null" ]] \
  && pass "POST /api/auth/refresh → new access_token" \
  || fail "refresh: $REFRESHED"
TOKEN="$NEW_TOKEN"

# Request with no token should 401
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/me")
[[ "$STATUS" == "401" ]] \
  && pass "GET /api/me without token → 401" \
  || fail "expected 401 without token, got $STATUS"

# ── Roles ───────────────────────────────────────────────────────────────────
section "Roles"

# POST /api/roles → single AgentRole object
ROLE=$(curl -sf -X POST "$BASE/api/roles" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"automation","description":"CI agent role"}')
ROLE_ID=$(jqf "$ROLE" .id)
[[ -n "$ROLE_ID" && "$ROLE_ID" != "null" ]] \
  && pass "POST /api/roles → id=$ROLE_ID" \
  || fail "create role: $ROLE"

# GET /api/roles → bare array
ROLES=$(curl -sf "$BASE/api/roles" -H "Authorization: Bearer $TOKEN")
ROLE_COUNT=$(jqf "$ROLES" 'length')
[[ "$ROLE_COUNT" -ge 1 ]] \
  && pass "GET /api/roles → $ROLE_COUNT role(s)" \
  || fail "list roles: $ROLES"

# ── Policies ─────────────────────────────────────────────────────────────────
section "Policies (block)"

POLICY=$(curl -sf -X POST "$BASE/api/policies" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"block-gmail-send\",
    \"description\": \"Block outbound email\",
    \"rules\": [{
      \"id\": \"r1\",
      \"role_id\": \"$ROLE_ID\",
      \"service\": \"google.gmail\",
      \"actions\": [\"send\"],
      \"decision\": \"block\",
      \"reason\": \"Sending email requires human sign-off\"
    }]
  }")
POLICY_ID=$(jqf "$POLICY" .id)
[[ -n "$POLICY_ID" && "$POLICY_ID" != "null" ]] \
  && pass "POST /api/policies → id=$POLICY_ID" \
  || fail "create policy: $POLICY"

GOT_POLICY=$(curl -sf "$BASE/api/policies/$POLICY_ID" -H "Authorization: Bearer $TOKEN")
[[ "$(jqf "$GOT_POLICY" .id)" == "$POLICY_ID" ]] \
  && pass "GET /api/policies/{id}" \
  || fail "get policy: $GOT_POLICY"

POLICIES=$(curl -sf "$BASE/api/policies" -H "Authorization: Bearer $TOKEN")
[[ "$(jqf "$POLICIES" '.policies | length')" -ge 1 ]] \
  && pass "GET /api/policies → list" \
  || fail "list policies: $POLICIES"

# Validate endpoint
VALIDATE=$(curl -sf -X POST "$BASE/api/policies/validate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"rules":[{"id":"v1","role_id":"anyone","service":"google.gmail","actions":["send"],"decision":"block"}]}')
[[ "$(jqf "$VALIDATE" .valid)" == "true" ]] \
  && pass "POST /api/policies/validate → valid=true" \
  || fail "validate: $VALIDATE"

# ── Agents ───────────────────────────────────────────────────────────────────
section "Agents"

# POST /api/agents → {"id":..., "token":..., ...}
AGENT=$(curl -sf -X POST "$BASE/api/agents" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"test-agent-$$\",\"role_id\":\"$ROLE_ID\"}")
AGENT_ID=$(jqf "$AGENT" .id)
AGENT_TOKEN=$(jqf "$AGENT" .token)
[[ -n "$AGENT_TOKEN" && "$AGENT_TOKEN" != "null" ]] \
  && pass "POST /api/agents → id=$AGENT_ID, token shown once" \
  || fail "create agent: $AGENT"

# GET /api/agents → bare array
AGENTS=$(curl -sf "$BASE/api/agents" -H "Authorization: Bearer $TOKEN")
[[ "$(jqf "$AGENTS" 'length')" -ge 1 ]] \
  && pass "GET /api/agents → list" \
  || fail "list agents: $AGENTS"

# ── Gateway: block path ───────────────────────────────────────────────────────
section "Gateway — block"

GW_BLOCK=$(curl -sf -X POST "$BASE/api/gateway/request" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"service\": \"google.gmail\",
    \"action\": \"send\",
    \"params\": {\"to\":\"bob@example.com\",\"subject\":\"Hi\",\"body\":\"Hello\"},
    \"reason\": \"Notifying user of build completion\",
    \"request_id\": \"req-block-$$\"
  }")
[[ "$(jqf "$GW_BLOCK" .status)" == "blocked" ]] \
  && pass "Gateway → status=blocked" \
  || fail "expected blocked: $GW_BLOCK"
[[ "$(jqf "$GW_BLOCK" .reason)" != "null" ]] \
  && pass "Gateway block → reason present" \
  || fail "missing reason: $GW_BLOCK"

# Gateway with no token should 401
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/api/gateway/request" \
  -H "Content-Type: application/json" \
  -d '{"service":"google.gmail","action":"send","params":{}}')
[[ "$STATUS" == "401" ]] \
  && pass "Gateway without agent token → 401" \
  || fail "expected 401 without token, got $STATUS"

# ── Audit log (block recorded) ────────────────────────────────────────────────
section "Audit log — block recorded"

AUDIT_BLOCKED=$(curl -sf "$BASE/api/audit?outcome=blocked" -H "Authorization: Bearer $TOKEN")
AUDIT_COUNT=$(jqf "$AUDIT_BLOCKED" '.entries | length')
[[ "$AUDIT_COUNT" -ge 1 ]] \
  && pass "GET /api/audit?outcome=blocked → $AUDIT_COUNT entry/entries" \
  || fail "expected audit entries: $AUDIT_BLOCKED"

AUDIT_ID=$(jqf "$AUDIT_BLOCKED" '.entries[0].id')
if [[ -n "$AUDIT_ID" && "$AUDIT_ID" != "null" ]]; then
  AUDIT_ENTRY=$(curl -sf "$BASE/api/audit/$AUDIT_ID" -H "Authorization: Bearer $TOKEN")
  [[ "$(jqf "$AUDIT_ENTRY" .outcome)" == "blocked" ]] \
    && pass "GET /api/audit/{id} → outcome=blocked" \
    || fail "audit entry outcome: $AUDIT_ENTRY"
else
  fail "No audit entry ID to fetch"
fi

# ── Policy: update to require approval ───────────────────────────────────────
section "Policies (approve)"

UPDATED=$(curl -sf -X PUT "$BASE/api/policies/$POLICY_ID" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"approve-gmail-send\",
    \"description\": \"Require approval for outbound email\",
    \"rules\": [{
      \"id\": \"r1\",
      \"role_id\": \"$ROLE_ID\",
      \"service\": \"google.gmail\",
      \"actions\": [\"send\"],
      \"decision\": \"approve\",
      \"reason\": \"Human must review outbound emails\"
    }]
  }")
[[ "$(jqf "$UPDATED" .name)" == "approve-gmail-send" ]] \
  && pass "PUT /api/policies/{id} → decision updated to approve" \
  || fail "update policy: $UPDATED"

# ── Gateway: approve (queues for human review) ────────────────────────────────
section "Gateway — approve"

REQ_ID="req-approve-$$"
GW_PEND=$(curl -sf -X POST "$BASE/api/gateway/request" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"service\": \"google.gmail\",
    \"action\": \"send\",
    \"params\": {\"to\":\"bob@example.com\",\"subject\":\"Build passed\",\"body\":\"All green!\"},
    \"reason\": \"Notifying stakeholder of successful build\",
    \"request_id\": \"$REQ_ID\"
  }")
[[ "$(jqf "$GW_PEND" .status)" == "pending" ]] \
  && pass "Gateway → status=pending" \
  || fail "expected pending: $GW_PEND"

# ── Approvals: list ───────────────────────────────────────────────────────────
section "Approvals — list"

# GET /api/approvals → {"total":..., "entries":[...]}
APPROVALS=$(curl -sf "$BASE/api/approvals" -H "Authorization: Bearer $TOKEN")
APPROVAL_COUNT=$(jqf "$APPROVALS" '.entries | length')
[[ "$APPROVAL_COUNT" -ge 1 ]] \
  && pass "GET /api/approvals → $APPROVAL_COUNT pending" \
  || fail "expected pending approvals: $APPROVALS"

# ── Approvals: deny ───────────────────────────────────────────────────────────
section "Approvals — deny"

DENY=$(curl -sf -X POST "$BASE/api/approvals/$REQ_ID/deny" \
  -H "Authorization: Bearer $TOKEN")
[[ "$(jqf "$DENY" .status)" == "denied" ]] \
  && pass "POST /approvals/{id}/deny → status=denied" \
  || fail "deny: $DENY"

# Denied entry should no longer appear in pending list
APPROVALS_AFTER=$(curl -sf "$BASE/api/approvals" -H "Authorization: Bearer $TOKEN")
STILL_THERE=$(jqf "$APPROVALS_AFTER" ".entries // [] | map(select(.request_id == \"$REQ_ID\")) | length")
[[ "$STILL_THERE" == "0" ]] \
  && pass "Denied approval removed from pending list" \
  || fail "denied approval still in list (count=$STILL_THERE)"

# ── Approvals: approve (execute) ──────────────────────────────────────────────
section "Approvals — approve"

REQ_ID2="req-approve2-$$"
curl -sf -X POST "$BASE/api/gateway/request" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"service\": \"google.gmail\",
    \"action\": \"send\",
    \"params\": {\"to\":\"carol@example.com\",\"subject\":\"Deploy done\",\"body\":\"Deployed!\"},
    \"reason\": \"Deployment notification\",
    \"request_id\": \"$REQ_ID2\"
  }" >/dev/null

APPROVE=$(curl -sf -X POST "$BASE/api/approvals/$REQ_ID2/approve" \
  -H "Authorization: Bearer $TOKEN")
APPROVE_STATUS=$(jqf "$APPROVE" .status)
case "$APPROVE_STATUS" in
  executed)
    pass "POST /approvals/{id}/approve → executed (Gmail credentials active)" ;;
  error)
    pass "POST /approvals/{id}/approve → error (expected: no Gmail credentials configured)" ;;
  *)
    fail "unexpected approve outcome '$APPROVE_STATUS': $APPROVE" ;;
esac

# ── Audit log: complete picture ───────────────────────────────────────────────
section "Audit log — full picture"

ALL_AUDIT=$(curl -sf "$BASE/api/audit" -H "Authorization: Bearer $TOKEN")
TOTAL=$(jqf "$ALL_AUDIT" .total)
[[ "$TOTAL" -ge 3 ]] \
  && pass "GET /api/audit → $TOTAL total entries" \
  || fail "expected ≥3 audit entries, got $TOTAL"

for OUTCOME in blocked pending denied; do
  COUNT=$(jqf "$ALL_AUDIT" "[.entries[] | select(.outcome == \"$OUTCOME\")] | length")
  [[ "$COUNT" -ge 1 ]] \
    && pass "  outcome=$OUTCOME present ($COUNT entry/entries)" \
    || fail "  no outcome=$OUTCOME entries found"
done

# ── Services catalog ──────────────────────────────────────────────────────────
section "Services"

SERVICES=$(curl -sf "$BASE/api/services" -H "Authorization: Bearer $TOKEN")
SVCCOUNT=$(jqf "$SERVICES" '.services | length')
if [[ "$SVCCOUNT" -ge 1 ]]; then
  pass "GET /api/services → $SVCCOUNT service(s) (GOOGLE_CLIENT_ID is set)"
  echo "$SERVICES" | jq -r '.services[] | "    service=\(.id) status=\(.status) actions=\(.actions)"' 2>/dev/null || true
else
  skip "GET /api/services → 0 services (add google.client_id to config.yaml to enable Gmail)"
fi

# ── Logout ────────────────────────────────────────────────────────────────────
section "Auth — logout"

LOGOUT=$(curl -sf -X POST "$BASE/api/auth/logout" -H "Authorization: Bearer $TOKEN")
[[ "$(jqf "$LOGOUT" .message)" != "" ]] \
  && pass "POST /api/auth/logout" \
  || fail "logout: $LOGOUT"

# Token should be invalid after logout
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/me" -H "Authorization: Bearer $TOKEN")
[[ "$STATUS" == "401" ]] \
  && pass "Token invalidated after logout → 401" \
  || fail "expected 401 after logout, got $STATUS"

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
if [[ $FAILURES -eq 0 ]]; then
  echo -e "${BOLD}${GREEN}All tests passed.${RESET}"
else
  echo -e "${BOLD}${RED}$FAILURES test(s) failed.${RESET}"
  exit 1
fi
