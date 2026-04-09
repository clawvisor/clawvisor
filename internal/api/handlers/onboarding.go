package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/relay"
)

var validUserID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

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

	userID := r.URL.Query().Get("user_id")
	if userID != "" && !validUserID.MatchString(userID) {
		userID = ""
	}

	fmt.Fprintf(&b, "## 2. Register with the daemon and wait for approval\n\n")
	fmt.Fprintf(&b, "Send a connection request with `?wait=true`. The daemon owner will be notified,\n")
	fmt.Fprintf(&b, "and the request blocks until they approve (or the timeout elapses).\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -s -X POST \"$CLAWVISOR_URL/api/agents/connect?wait=true&timeout=120\" \\\n")
	fmt.Fprintf(&b, "  -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(&b, "  -d '{\n")
	fmt.Fprintf(&b, "    \"name\": \"<your agent name>\",\n")
	if userID != "" {
		fmt.Fprintf(&b, "    \"user_id\": \"%s\",\n", userID)
	}
	fmt.Fprintf(&b, "    \"description\": \"<brief description of what you do>\"\n")
	fmt.Fprintf(&b, "  }'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If the timeout elapses while still pending, long-poll the status endpoint\n")
	fmt.Fprintf(&b, "until the owner approves:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -s \"$CLAWVISOR_URL/api/agents/connect/<connection_id>/status\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "This blocks for up to 30 seconds. Repeat until `status` is no longer `pending`.\n")
	fmt.Fprintf(&b, "On approval, the response includes your bearer token:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"connection_id\": \"<id>\",\n")
	fmt.Fprintf(&b, "  \"status\": \"approved\",\n")
	fmt.Fprintf(&b, "  \"token\": \"<your agent token>\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Save this token — it is shown only once:\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=<your agent token>\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 3. Set up E2E encryption\n\n")
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

	fmt.Fprintf(&b, "## 4. Install the Clawvisor skill\n\n")
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

	fmt.Fprintf(&b, "## 5. Verify the connection\n\n")
	fmt.Fprintf(&b, "Create a test task to confirm everything works end-to-end:\n\n")
	fmt.Fprintf(&b, "1. Fetch your service catalog: `GET $CLAWVISOR_URL/api/skill/catalog`\n")
	fmt.Fprintf(&b, "2. Pick any active read-only action (e.g. `google.gmail` → `list_messages`, or `google.calendar` → `list_events`)\n")
	fmt.Fprintf(&b, "3. Create a task, wait for approval, execute the action, and complete the task\n")
	fmt.Fprintf(&b, "4. If the result comes back with `status: \"executed\"`, you're all set\n\n")
	fmt.Fprintf(&b, "Tell the user what you found — this is their confirmation that the integration is working.\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}

// resolveURL returns the best Clawvisor URL for the requesting client. When
// the request arrives directly (not via relay), the local origin is used so
// agents talk to the daemon without an extra hop. Relay-routed requests get
// the public relay URL.
func (h *OnboardingHandler) resolveURL(r *http.Request) string {
	if !relay.ViaRelay(r.Context()) {
		// Direct (local) access — use the request's own host.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	if h.daemonID != "" && h.relayHost != "" {
		return fmt.Sprintf("https://%s/d/%s", h.relayHost, h.daemonID)
	}
	return "http://localhost:25297"
}

// ClaudeCodeSetup serves a Claude Code /clawvisor-setup slash command with
// the Clawvisor URL pre-filled. Users curl this into ~/.claude/commands/ to
// install the command, then run /clawvisor-setup in Claude Code.
func (h *OnboardingHandler) ClaudeCodeSetup(w http.ResponseWriter, r *http.Request) {
	clawvisorURL := h.resolveURL(r)
	skillURL := clawvisorURL + "/skill/SKILL.md"
	userID := r.URL.Query().Get("user_id")
	if userID != "" && !validUserID.MatchString(userID) {
		userID = ""
	}

	connectBody := `{"name": "claude-code", "description": "Claude Code agent"}`
	if userID != "" {
		connectBody = fmt.Sprintf(`{"name": "claude-code", "description": "Claude Code agent", "user_id": "%s"}`, userID)
	}

	var b strings.Builder
	b.WriteString("Set up Clawvisor in the current project so Claude Code can make gated API\n")
	b.WriteString("requests (Gmail, Calendar, Drive, GitHub, Slack, etc.) through the Clawvisor\n")
	b.WriteString("gateway with task-scoped authorization and human approval.\n\n")

	b.WriteString("## Steps\n\n")

	b.WriteString("### 1. Connect as an agent\n\n")
	b.WriteString("Register with the daemon and wait for the user to approve:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "curl -s -X POST \"%s/api/agents/connect?wait=true&timeout=120\" -H \"Content-Type: application/json\" -d '%s'\n", clawvisorURL, connectBody)
	b.WriteString("```\n\n")
	b.WriteString("This sends a connection request to the daemon. The user will be notified\n")
	b.WriteString("to approve. The `?wait=true` parameter makes the request block until the\n")
	b.WriteString("user approves (or the timeout elapses).\n\n")
	b.WriteString("Parse the JSON response. If `status` is `approved`, save the `token`\n")
	b.WriteString("value — you will need it below. If the timeout elapses and `status` is\n")
	b.WriteString("still `pending`, tell the user to approve the connection request in the\n")
	b.WriteString("Clawvisor dashboard and long-poll `GET /api/agents/connect/{connection_id}/status`\n")
	b.WriteString("until it resolves.\n\n")

	b.WriteString("### 2. Set environment variables\n\n")
	b.WriteString("Save the agent token and daemon URL to `~/.claude/settings.json` so they\n")
	b.WriteString("persist across all Claude Code sessions and projects.\n\n")
	b.WriteString("Read `~/.claude/settings.json` (create it if it doesn't exist). Merge the\n")
	b.WriteString("following into the `env` object at the top level, preserving all other keys:\n\n")
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"env\": {\n")
	fmt.Fprintf(&b, "    \"CLAWVISOR_URL\": \"%s\",\n", clawvisorURL)
	b.WriteString("    \"CLAWVISOR_AGENT_TOKEN\": \"<token from step 1>\"\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	b.WriteString("```\n\n")
	b.WriteString("Write the updated JSON back to `~/.claude/settings.json`. The variables\n")
	b.WriteString("will be available in every future Claude Code session without any per-project\n")
	b.WriteString("setup.\n\n")

	b.WriteString("### 3. Install the Clawvisor skill\n\n")
	b.WriteString("Download and install the skill globally so it's available in all projects:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.claude/skills/clawvisor && curl -sf \"%s\" -o ~/.claude/skills/clawvisor/SKILL.md\n", skillURL)
	b.WriteString("```\n\n")

	b.WriteString("### 4. Verify\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "curl -sf -H \"Authorization: Bearer $CLAWVISOR_AGENT_TOKEN\" \\\n  %s/api/skill/catalog | head -20\n", clawvisorURL)
	b.WriteString("```\n\n")
	b.WriteString("This should return a JSON service catalog. If it returns 401, the token is\n")
	b.WriteString("wrong. If it fails to connect, the daemon is not running.\n\n")

	b.WriteString("### 5. End-to-end smoke test\n\n")
	b.WriteString("Now that everything is configured, run a quick smoke test to prove the full\n")
	b.WriteString("flow works. Use the Clawvisor skill to:\n\n")
	b.WriteString("1. **Create a test task** — pick any connected service visible in the catalog\n")
	b.WriteString("   (e.g. Gmail, Calendar, GitHub) and create a task with a narrow scope such as\n")
	b.WriteString("   \"read my most recent email subject\" or \"list my GitHub notifications\".\n")
	b.WriteString("   Tell the user to approve the task in the Clawvisor dashboard or mobile app,\n")
	b.WriteString("   then wait for approval before continuing.\n\n")
	b.WriteString("2. **Make an in-scope request** — once approved, make a gateway call that falls\n")
	b.WriteString("   within the task's approved scope. Show the user the successful response.\n\n")
	b.WriteString("3. **Make an out-of-scope request** — make a second gateway call using the same\n")
	b.WriteString("   task that is clearly outside the approved scope (e.g. sending an email when\n")
	b.WriteString("   the task only allows reading). Show the user that this request is rejected,\n")
	b.WriteString("   demonstrating that Clawvisor enforces task boundaries.\n\n")
	b.WriteString("Summarize the results: the in-scope call should have succeeded and the\n")
	b.WriteString("out-of-scope call should have been denied. If either result is unexpected,\n")
	b.WriteString("help the user debug.\n\n")

	b.WriteString("### 6. Done\n\n")
	b.WriteString("Tell the user setup is complete. The Clawvisor skill will be loaded\n")
	b.WriteString("automatically when relevant, or they can invoke it explicitly. Remind them to:\n\n")
	b.WriteString("- Connect services in the Clawvisor dashboard (Services tab) before asking\n")
	b.WriteString("  you to use them\n")
	b.WriteString("- Approve tasks in the dashboard or via mobile when you request them\n\n")

	b.WriteString("### 7. Offer to uninstall /clawvisor-setup (optional)\n\n")
	b.WriteString("Now that setup is complete, ask the user if they'd like to remove the\n")
	b.WriteString("`/clawvisor-setup` slash command since it's no longer needed. If they agree:\n\n")
	b.WriteString("```bash\n")
	b.WriteString("rm ~/.claude/commands/clawvisor-setup.md\n")
	b.WriteString("```\n\n")
	b.WriteString("If they decline, remind them they can delete it later with the same command.\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}
