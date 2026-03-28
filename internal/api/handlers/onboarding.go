package handlers

import (
	"fmt"
	"net/http"
	"strings"
)

// OnboardingHandler serves the agent onboarding document at GET /setup.
// The document is self-contained markdown that tells an agent how to register,
// authenticate, set up E2E encryption, and verify the connection.
type OnboardingHandler struct {
	relayHost string // e.g. "relay.clawvisor.com"
	daemonID  string // relay daemon ID
}

func NewOnboardingHandler(relayHost, daemonID string) *OnboardingHandler {
	return &OnboardingHandler{relayHost: relayHost, daemonID: daemonID}
}

// Setup serves the onboarding markdown document.
func (h *OnboardingHandler) Setup(w http.ResponseWriter, r *http.Request) {
	if h.daemonID == "" || h.relayHost == "" {
		http.Error(w, "Daemon not registered with relay. Re-run `clawvisor setup`.", http.StatusServiceUnavailable)
		return
	}

	clawvisorURL := fmt.Sprintf("https://%s/d/%s", h.relayHost, h.daemonID)

	var b strings.Builder
	fmt.Fprintf(&b, "# Clawvisor Agent Setup\n\n")
	fmt.Fprintf(&b, "Welcome! This document will walk you through connecting to a Clawvisor daemon.\n")
	fmt.Fprintf(&b, "Follow each step in order.\n\n")

	fmt.Fprintf(&b, "## 1. Set your endpoint\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "CLAWVISOR_URL=%s\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "All API calls below use this URL.\n\n")

	fmt.Fprintf(&b, "## 2. Register with the daemon\n\n")
	fmt.Fprintf(&b, "Send a connection request. The daemon owner will be notified to approve it.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -s -X POST \"$CLAWVISOR_URL/api/agents/connect\" \\\n")
	fmt.Fprintf(&b, "  -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(&b, "  -d '{\n")
	fmt.Fprintf(&b, "    \"name\": \"<your agent name>\",\n")
	fmt.Fprintf(&b, "    \"description\": \"<brief description of what you do>\"\n")
	fmt.Fprintf(&b, "  }'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Response:\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"connection_id\": \"<id>\",\n")
	fmt.Fprintf(&b, "  \"status\": \"pending\",\n")
	fmt.Fprintf(&b, "  \"poll_url\": \"/api/agents/connect/<id>/status\",\n")
	fmt.Fprintf(&b, "  \"expires_at\": \"...\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 3. Wait for approval\n\n")
	fmt.Fprintf(&b, "Long-poll the status endpoint until the owner approves your request:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -s \"$CLAWVISOR_URL/api/agents/connect/<connection_id>/status\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "This blocks for up to 30 seconds. Repeat until `status` is `approved`.\n")
	fmt.Fprintf(&b, "On approval, the response includes your bearer token:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"status\": \"approved\",\n")
	fmt.Fprintf(&b, "  \"token\": \"<your agent token>\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Save this token — it is shown only once:\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=<your agent token>\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 4. Set up E2E encryption\n\n")
	fmt.Fprintf(&b, "All **API requests** (`/api/...`) through the relay **require** end-to-end encryption.\n")
	fmt.Fprintf(&b, "Static skill files (`/skill/...`) and the key-discovery endpoint (`/.well-known/...`) are served in plaintext — no E2E needed to fetch them.\n\n")
	fmt.Fprintf(&b, "Fetch the daemon's public key (cache this — it rarely changes):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -s \"$CLAWVISOR_URL/.well-known/clawvisor-keys\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Response:\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"daemon_id\": \"...\",\n")
	fmt.Fprintf(&b, "  \"x25519\": \"<base64 public key>\",\n")
	fmt.Fprintf(&b, "  \"algorithm\": \"x25519-ecdh-aes256gcm\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "For each request:\n")
	fmt.Fprintf(&b, "1. Generate an ephemeral X25519 keypair\n")
	fmt.Fprintf(&b, "2. ECDH: your ephemeral private key × daemon's public key = 32-byte shared secret\n")
	fmt.Fprintf(&b, "3. AES-256-GCM encrypt the JSON request body with a random 12-byte nonce (skip for GET requests)\n")
	fmt.Fprintf(&b, "4. Send with headers:\n")
	fmt.Fprintf(&b, "   - `X-Clawvisor-E2E: aes-256-gcm`\n")
	fmt.Fprintf(&b, "   - `X-Clawvisor-Ephemeral-Key: <base64 ephemeral public key>`\n")
	fmt.Fprintf(&b, "5. Body (POST only): `base64(nonce ‖ ciphertext ‖ GCM auth tag)`\n")
	fmt.Fprintf(&b, "6. Decrypt the response the same way (response also has `X-Clawvisor-E2E` header)\n\n")
	fmt.Fprintf(&b, "If a helper is available, use it — the bundled [`e2e.mjs`](%s/skill/e2e.mjs) (Node.js, zero deps) or `cvis-e2e` (Go) handle all of this automatically.\n", clawvisorURL)
	fmt.Fprintf(&b, "The `e2e.mjs` client exposes `request(method, endpoint, body)` for any endpoint and `gatewayRequest(body)` as a shorthand.\n\n")

	fmt.Fprintf(&b, "## 5. Install the Clawvisor skill\n\n")
	fmt.Fprintf(&b, "Fetch **and install** the full skill so you can use it for the rest of this session:\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "GET %s/skill/SKILL.md\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Read the returned document and install it as a skill for future use.\n")
	fmt.Fprintf(&b, "It documents the task-scoped authorization model, all gateway request fields,\n")
	fmt.Fprintf(&b, "response statuses, callbacks, and service catalog.\n\n")
	fmt.Fprintf(&b, "A zip bundle containing both the skill and the E2E helper is also available:\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "GET %s/skill/skill.zip\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 6. Verify the connection\n\n")
	fmt.Fprintf(&b, "Create a test task to confirm everything works end-to-end:\n\n")
	fmt.Fprintf(&b, "1. Fetch your service catalog: `GET $CLAWVISOR_URL/api/skill/catalog`\n")
	fmt.Fprintf(&b, "2. Pick any active read-only action (e.g. `google.gmail` → `list_messages`, or `google.calendar` → `list_events`)\n")
	fmt.Fprintf(&b, "3. Create a task, wait for approval, execute the action, and complete the task\n")
	fmt.Fprintf(&b, "4. If the result comes back with `status: \"executed\"`, you're all set\n\n")
	fmt.Fprintf(&b, "Tell the user what you found — this is their confirmation that the integration is working.\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}
