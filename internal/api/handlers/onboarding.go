package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/pluginbundle"
	"github.com/clawvisor/clawvisor/internal/relay"
)

var validUserID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// OnboardingHandler serves the agent onboarding document at GET /setup.
// The document is self-contained markdown that tells an agent how to register,
// authenticate, set up E2E encryption, and verify the connection.
type OnboardingHandler struct {
	relayHost string // e.g. "relay.clawvisor.com"
	daemonID  string // relay daemon ID
	isLocal   bool   // true when bound to loopback (not a cloud deployment)
}

func NewOnboardingHandler(relayHost, daemonID string, isLocal bool) *OnboardingHandler {
	return &OnboardingHandler{relayHost: relayHost, daemonID: daemonID, isLocal: isLocal}
}

// Setup is the legacy setup handler. It delegates to SetupHermes for
// backwards compatibility with existing dashboard links and pasted setup
// URLs — old callers expected a single all-purpose agent-pairing flow, which
// is what the Hermes handler still implements.
func (h *OnboardingHandler) Setup(w http.ResponseWriter, r *http.Request) {
	h.SetupHermes(w, r)
}

// SetupProxy serves the onboarding markdown for the Clawvisor Network
// Proxy. The agent reads this and walks the user through installing
// the proxy via clawvisor-local, trusting the CA cert, and then
// re-launching itself wrapped by `clawvisor proxy run`.
//
// Query params:
//   bridge_id     — pre-fills the bridge UUID this proxy will serve
//   proxy_token   — pre-fills the cvisproxy_ token (one-time, sensitive)
//   agent_token   — pre-fills the cvis_ agent token to launch under
//   server_url    — overrides the default Clawvisor server URL
//
// All optional. Without them, the doc tells the agent to ask the user
// to grab the values from the dashboard's Proxies → Enable Proxy flow.
func (h *OnboardingHandler) SetupProxy(w http.ResponseWriter, r *http.Request) {
	clawvisorURL := h.resolveURL(r)
	bridgeID := strings.TrimSpace(r.URL.Query().Get("bridge_id"))
	proxyToken := strings.TrimSpace(r.URL.Query().Get("proxy_token"))
	agentToken := strings.TrimSpace(r.URL.Query().Get("agent_token"))
	serverURL := strings.TrimSpace(r.URL.Query().Get("server_url"))
	if serverURL == "" {
		serverURL = clawvisorURL
	}

	var b strings.Builder
	b.WriteString("# Clawvisor Network Proxy Setup\n\n")
	b.WriteString("You're being asked to wire your network traffic through the Clawvisor Network Proxy. ")
	b.WriteString("This gives Clawvisor tamper-proof transcripts of your LLM calls, lets it inject vault credentials so you never see raw API keys, and (when policies are enabled) enforces what hosts you can call.\n\n")
	b.WriteString("**This is per-process scoped.** Only commands you explicitly launch through `clawvisor proxy run` are observed. The user's browser, package manager, git, etc. stay direct.\n\n")

	b.WriteString("## What you'll do\n\n")
	b.WriteString("Five steps, all run from a terminal on the user's machine. Ask permission before each `Bash` invocation that mutates state — particularly the keychain trust step.\n\n")
	b.WriteString("1. Confirm `clawvisor-local` is installed (the local supervisor daemon).\n")
	b.WriteString("2. Confirm a `clawvisor-proxy` (kumo) binary is available.\n")
	b.WriteString("3. Configure + start the proxy under daemon supervision.\n")
	b.WriteString("4. Trust the proxy's TLS CA cert in the user's keychain.\n")
	b.WriteString("5. Re-launch yourself wrapped by `clawvisor proxy run`.\n\n")

	b.WriteString("## 1. Confirm the daemon is running\n\n")
	b.WriteString("```bash\ncurl -s http://127.0.0.1:25299/api/proxy/status\n```\n\n")
	b.WriteString("- 200 with JSON: daemon is up. Continue.\n")
	b.WriteString("- Connection refused: tell the user to install/launch `clawvisor-local`. The dashboard's Settings page has the install command. Wait for the user, then retry.\n\n")

	b.WriteString("## 2. Locate the proxy binary\n\n")
	b.WriteString("```bash\nwhich clawvisor-proxy || which kumo\n```\n\n")
	b.WriteString("If neither exists, ask the user where they have it (or to download it from the Clawvisor releases page). Note the absolute path; you'll pass it to install.\n\n")

	b.WriteString("## 3. Get the credentials\n\n")
	if bridgeID != "" && proxyToken != "" {
		b.WriteString("This setup link came pre-populated with credentials:\n\n")
		fmt.Fprintf(&b, "- Bridge ID: `%s`\n", bridgeID)
		fmt.Fprintf(&b, "- Proxy token: `%s`\n", proxyToken)
		if agentToken != "" {
			fmt.Fprintf(&b, "- Agent token: `%s`\n", agentToken)
		}
		fmt.Fprintf(&b, "- Server URL: `%s`\n\n", serverURL)
		b.WriteString("**Treat the proxy token as a secret.** It's shown only once. If the user closed the dashboard before saving it, they need to rotate it from the Proxies tab.\n\n")
	} else {
		fmt.Fprintf(&b, "Ask the user to open `%s/dashboard/proxies`, click their bridge (or **Add proxy** for a fresh standalone one), then click **Enable Proxy** to mint a one-time `cvisproxy_…` token.\n\n", clawvisorURL)
		b.WriteString("Have them paste back to you:\n")
		b.WriteString("- The bridge ID (UUID at the top of the proxy detail page).\n")
		b.WriteString("- The `cvisproxy_…` token from the orange box.\n\n")
		b.WriteString("Also ask which agent token (`cvis_…`) you should launch under — that determines who Clawvisor attributes the traffic to. They can mint one in the Agents tab if needed.\n\n")
	}

	b.WriteString("## 4. Configure + start the proxy\n\n")
	b.WriteString("Run with the user's permission:\n\n")
	b.WriteString("```bash\nclawvisor proxy install \\\n")
	b.WriteString("  --binary <ABSOLUTE_PATH_FROM_STEP_2> \\\n")
	if serverURL != "" {
		fmt.Fprintf(&b, "  --server-url %s \\\n", serverURL)
	} else {
		b.WriteString("  --server-url http://127.0.0.1:25297 \\\n")
	}
	if proxyToken != "" {
		fmt.Fprintf(&b, "  --proxy-token %s \\\n", proxyToken)
	} else {
		b.WriteString("  --proxy-token cvisproxy_<FROM_STEP_3> \\\n")
	}
	if bridgeID != "" {
		fmt.Fprintf(&b, "  --bridge-id %s\n", bridgeID)
	} else {
		b.WriteString("  --bridge-id <UUID_FROM_STEP_3>\n")
	}
	b.WriteString("```\n\n")
	b.WriteString("This persists the config under `~/.clawvisor/proxy/`, hands the daemon a managed lifecycle for the binary, and starts the process. Verify:\n\n")
	b.WriteString("```bash\nclawvisor proxy status\n```\n\n")
	b.WriteString("Look for `state: running`. If it's `failed`, the JSON's `last_error` field tells you why — most commonly a port conflict or a stale CA cert from a previous install.\n\n")

	b.WriteString("## 5. Trust the CA cert\n\n")
	b.WriteString("The proxy MITM-intercepts TLS using its own cert authority. The user's system needs to trust it.\n\n")
	b.WriteString("```bash\nclawvisor proxy trust-ca\n```\n\n")
	b.WriteString("**This will prompt the user for their keychain password** (macOS) or sudo (Linux). Tell them what's about to happen before invoking it. The cert is added to the user-scoped keychain only — no system-wide install — and `clawvisor proxy uninstall` cleans it up.\n\n")

	b.WriteString("## 6. Re-launch yourself under the proxy\n\n")
	b.WriteString("Each command you launch through `clawvisor proxy run` is scoped — only that process tree gets the env vars (`HTTP_PROXY`, `HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`). Nothing else on the system is affected.\n\n")
	b.WriteString("If you have a way to ask your runtime to relaunch, do so:\n\n")
	b.WriteString("```bash\n")
	if agentToken != "" {
		fmt.Fprintf(&b, "clawvisor proxy run --agent-token %s -- <YOUR_LAUNCH_COMMAND>\n", agentToken)
	} else {
		b.WriteString("clawvisor proxy run --agent-token cvis_<AGENT_TOKEN> -- <YOUR_LAUNCH_COMMAND>\n")
	}
	b.WriteString("```\n\n")
	b.WriteString("If you can't self-relaunch, tell the user: \"Quit me, then re-run me from a shell with this prefix.\" Show them the exact command.\n\n")
	b.WriteString("Once relaunched, your subsequent LLM calls flow through the proxy. Verify:\n\n")
	b.WriteString("```bash\ncurl -sv https://api.anthropic.com/ -o /dev/null -w \"%{http_code}\\n\" 2>&1 | grep -E \"Connected to clawvisor|^[0-9]\"\n```\n\n")
	b.WriteString("You should see `Connected to clawvisor-proxy …` followed by an Anthropic status code. If you connect directly to `api.anthropic.com` instead, the env vars didn't propagate to the new process — the most common cause is the runtime forking differently than expected. Tell the user.\n\n")

	b.WriteString("## What to tell the user when you're done\n\n")
	b.WriteString("\"You're proxied. Open the Proxies tab in the Clawvisor dashboard to see live traffic, set policies, or rotate the token.\"\n\n")
	b.WriteString("If you'd like to scope a credential to vault-injection (so the user's API key stops living on disk in the agent's config), point them at the **Vault Credentials** card on the Agents tab. Once they move the key in, you can remove it from your environment and the proxy will inject it on every matching request.\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}

// SetupHermes serves the onboarding markdown for the Hermes flow (agent
// calls POST /api/agents/connect directly; no separate plugin pairing step).
func (h *OnboardingHandler) SetupHermes(w http.ResponseWriter, r *http.Request) {
	clawvisorURL := h.resolveURL(r)
	viaRelay := relay.ViaRelay(r.Context())

	var b strings.Builder
	stepNum := 1
	fmt.Fprintf(&b, "# Clawvisor Agent Setup\n\n")
	if h.isLocal {
		fmt.Fprintf(&b, "Welcome! This document will walk you through connecting to a Clawvisor daemon.\n")
	} else {
		fmt.Fprintf(&b, "Welcome! This document will walk you through connecting to Clawvisor.\n")
	}
	fmt.Fprintf(&b, "Follow each step in order.\n\n")

	fmt.Fprintf(&b, "## %d. Set your endpoint\n\n", stepNum)
	stepNum++
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "CLAWVISOR_URL=%s\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "All API calls below use this URL.\n\n")

	userID := r.URL.Query().Get("user_id")
	if userID != "" && !validUserID.MatchString(userID) {
		userID = ""
	}

	if h.isLocal {
		fmt.Fprintf(&b, "## %d. Register with the daemon and wait for approval\n\n", stepNum)
	} else {
		fmt.Fprintf(&b, "## %d. Register and wait for approval\n\n", stepNum)
	}
	stepNum++
	if h.isLocal {
		fmt.Fprintf(&b, "Send a connection request with `?wait=true`. The daemon owner will be notified,\n")
		fmt.Fprintf(&b, "and the request blocks until they approve (or the timeout elapses).\n\n")
	} else {
		fmt.Fprintf(&b, "Send a connection request with `?wait=true`. The account owner will be notified,\n")
		fmt.Fprintf(&b, "and the request blocks until they approve (or the timeout elapses).\n\n")
	}
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

	if viaRelay {
		fmt.Fprintf(&b, "## %d. Set up E2E encryption\n\n", stepNum)
		stepNum++
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
	}

	fmt.Fprintf(&b, "## %d. Install the Clawvisor skill\n\n", stepNum)
	stepNum++
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

	fmt.Fprintf(&b, "## %d. Verify the connection\n\n", stepNum)
	stepNum++
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
		if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
			scheme = fp
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
	if h.isLocal {
		b.WriteString("Register with the daemon and wait for the user to approve:\n\n")
	} else {
		b.WriteString("Register with Clawvisor and wait for the user to approve:\n\n")
	}
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "curl -s -X POST \"%s/api/agents/connect?wait=true&timeout=120\" -H \"Content-Type: application/json\" -d '%s'\n", clawvisorURL, connectBody)
	b.WriteString("```\n\n")
	if h.isLocal {
		b.WriteString("This sends a connection request to the daemon. The user will be notified\n")
	} else {
		b.WriteString("This sends a connection request to Clawvisor. The user will be notified\n")
	}
	b.WriteString("to approve. The `?wait=true` parameter makes the request block until the\n")
	b.WriteString("user approves (or the timeout elapses).\n\n")
	b.WriteString("Parse the JSON response. If `status` is `approved`, save the `token`\n")
	b.WriteString("value — you will need it below. If the timeout elapses and `status` is\n")
	b.WriteString("still `pending`, tell the user to approve the connection request in the\n")
	b.WriteString("Clawvisor dashboard and long-poll `GET /api/agents/connect/{connection_id}/status`\n")
	b.WriteString("until it resolves.\n\n")

	b.WriteString("### 2. Set environment variables\n\n")
	b.WriteString("Save the agent token and Clawvisor URL to `~/.claude/settings.json` so they\n")
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

	stepNum := 3
	if !h.isLocal {
		fmt.Fprintf(&b, "### %d. Add auto-approve permission rules\n\n", stepNum)
		b.WriteString("Add a permission rule so Claude Code doesn't prompt for approval on every\n")
		b.WriteString("curl request to Clawvisor. Read `~/.claude/settings.json` and append the\n")
		b.WriteString("following entry to the `permissions.allow` array (create the array if it\n")
		b.WriteString("doesn't exist), preserving all other entries:\n\n")
		b.WriteString("```json\n")
		b.WriteString("{\n")
		fmt.Fprintf(&b, "  \"Bash(curl *%s/*)\": true\n", clawvisorURL)
		b.WriteString("}\n")
		b.WriteString("```\n\n")
		b.WriteString("Write the updated JSON back to `~/.claude/settings.json`.\n\n")
		stepNum++
	}

	fmt.Fprintf(&b, "### %d. Install the Clawvisor skill\n\n", stepNum)
	b.WriteString("Download and install the skill globally so it's available in all projects:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.claude/skills/clawvisor && curl -sf \"%s\" -o ~/.claude/skills/clawvisor/SKILL.md\n", skillURL)
	b.WriteString("```\n\n")

	stepNum++
	fmt.Fprintf(&b, "### %d. Verify\n\n", stepNum)
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "curl -sf -H \"Authorization: Bearer $CLAWVISOR_AGENT_TOKEN\" \\\n  %s/api/skill/catalog | head -20\n", clawvisorURL)
	b.WriteString("```\n\n")
	b.WriteString("This should return a JSON service catalog. If it returns 401, the token is\n")
	if h.isLocal {
		b.WriteString("wrong. If it fails to connect, the daemon is not running.\n\n")
	} else {
		b.WriteString("wrong. If it fails to connect, check the Clawvisor URL.\n\n")
	}

	stepNum++
	fmt.Fprintf(&b, "### %d. End-to-end smoke test\n\n", stepNum)
	b.WriteString("Now that everything is configured, run a quick smoke test to prove the full\n")
	b.WriteString("flow works.\n\n")
	b.WriteString("First, load the Clawvisor skill by calling the Skill tool with skill: \"clawvisor\".\n")
	b.WriteString("Once it is loaded, use it to:\n\n")
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

	stepNum++
	fmt.Fprintf(&b, "### %d. Done\n\n", stepNum)
	b.WriteString("Tell the user setup is complete. The Clawvisor skill will be loaded\n")
	b.WriteString("automatically when relevant, or they can invoke it explicitly. Remind them to:\n\n")
	b.WriteString("- Connect services in the Clawvisor dashboard (Services tab) before asking\n")
	b.WriteString("  you to use them\n")
	b.WriteString("- Approve tasks in the dashboard or via mobile when you request them\n\n")

	stepNum++
	fmt.Fprintf(&b, "### %d. Offer to uninstall /clawvisor-setup (optional)\n\n", stepNum)
	b.WriteString("Now that setup is complete, ask the user if they'd like to remove the\n")
	b.WriteString("`/clawvisor-setup` slash command since it's no longer needed. If they agree:\n\n")
	b.WriteString("```bash\n")
	b.WriteString("rm ~/.claude/commands/clawvisor-setup.md\n")
	b.WriteString("```\n\n")
	b.WriteString("If they decline, remind them they can delete it later with the same command.\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}

// validPairCode matches the `cvpc_` token format (prefix + 18 hex chars).
// Tight regex because the code ends up in a YAML config the agent edits —
// we don't want to encourage pasting shell-special characters into it.
var validPairCode = regexp.MustCompile(`^cvpc_[a-f0-9]{18}$`)

// setupOpenClawPluginInstallStep renders the install step that fetches
// the embedded OpenClaw plugin tarball from this very Clawvisor instance.
// Running a Clawvisor build that matches a specific plugin version is
// what guarantees the pair / ingest / task-create protocol alignment;
// pulling from npm-latest or a public registry would re-introduce the
// version-skew problem we're specifically avoiding.
func setupOpenClawPluginInstallStep(b *strings.Builder, clawvisorURL, pluginVersion, pluginSHA256 string) {
	b.WriteString("## 1. Install the Clawvisor plugin into OpenClaw\n\n")
	if pluginVersion == "" {
		b.WriteString("_(This Clawvisor build does not embed a plugin bundle. The operator of this instance needs to rebuild with `make plugin-bundle`.)_\n\n")
		return
	}
	fmt.Fprintf(b, "This Clawvisor instance serves a plugin tarball matched to its own protocol version: **%s** (sha256 `%s`).\n\n", pluginVersion, pluginSHA256)
	b.WriteString("> **Note for agents:** some agent runtimes block `web_fetch` on private/loopback hostnames (e.g. `localhost`, `host.docker.internal`). If `web_fetch` errors on the URLs below, use `curl` via shell exec instead — it doesn't have the same restriction.\n\n")
	b.WriteString("Install via OpenClaw's plugin installer. The `--dangerously-force-unsafe-install` flag is required because the plugin uses env vars + network (a static analyzer flag the operator is opting past explicitly here):\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(b, "curl -fsSL -o /tmp/clawvisor.tgz %s/skill/openclaw-plugin.tgz\n", clawvisorURL)
	b.WriteString("openclaw plugins install /tmp/clawvisor.tgz --dangerously-force-unsafe-install --force\n")
	b.WriteString("```\n\n")
	b.WriteString("If `openclaw plugins install` isn't available in your environment (non-standard OpenClaw packaging), fall back to manual extraction — the tarball unpacks to a `clawvisor/` directory:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(b, "curl -fsSL %s/skill/openclaw-plugin.tgz | tar xz -C ~/.openclaw/plugins\n", clawvisorURL)
	b.WriteString("```\n\n")
	b.WriteString("Verify the download matches the advertised hash:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(b, "curl -fsSL %s/skill/openclaw-plugin.sha256\n", clawvisorURL)
	b.WriteString("```\n\n")
	b.WriteString("The tarball contains a pre-bundled `index.js` (all Node dependencies inlined), `openclaw.plugin.json`, `package.json` (with the `openclaw.extensions` field set), and a `VERSION` file. No `npm install` needed inside the plugin dir.\n\n")
}

// SetupOpenClaw serves the onboarding markdown for the OpenClaw flow, which
// is structurally different from Hermes: the agent does NOT call
// /api/agents/connect directly. Instead, the OpenClaw-side Clawvisor plugin
// runtime (trusted; not driven by LLM output) pairs itself and receives
// both a bridge token and per-agent tokens in a single dashboard approval.
//
// The agent's only job is to deposit a one-time pair code where the plugin
// can find it, then nudge the user to approve the resulting pairing card.
// user_id never appears in plugin config — the server derives it by
// consuming the code, so even an exposed user_id can't be used by a third
// party to initiate a pair attempt.
func (h *OnboardingHandler) SetupOpenClaw(w http.ResponseWriter, r *http.Request) {
	clawvisorURL := h.resolveURL(r)

	pairCode := r.URL.Query().Get("pair_code")
	if pairCode != "" && !validPairCode.MatchString(pairCode) {
		pairCode = ""
	}

	var b strings.Builder
	b.WriteString("# Clawvisor OpenClaw Plugin Setup\n\n")
	b.WriteString("You are running under OpenClaw with the Clawvisor plugin installed. ")
	b.WriteString("Setup here is a handoff between you, the plugin runtime, and the user. ")
	b.WriteString("Unlike a direct agent connection, **you do not call `/api/agents/connect`** — ")
	b.WriteString("the plugin pairs itself on your behalf and receives tokens directly.\n\n")

	b.WriteString("## How this works\n\n")
	b.WriteString("The Clawvisor plugin is a trusted Node module loaded by OpenClaw. It:\n\n")
	b.WriteString("1. Is installed into OpenClaw's plugin directory from a tarball served by this Clawvisor instance (guarantees a version match with this server).\n")
	b.WriteString("2. Reads a one-time `pairCode` from the OpenClaw plugin config.\n")
	b.WriteString("3. Calls `POST /api/plugin/pair?wait=true` with that code plus a fingerprint for this install, the hostname, and the list of agent IDs configured in OpenClaw.\n")
	b.WriteString("4. The user sees a pairing approval card in the Clawvisor dashboard (same place they approve tasks).\n")
	b.WriteString("5. On approve, the server mints a **bridge token** (held only by the plugin — never exposed to you, the agent) and one **agent token per agent** configured in OpenClaw.\n")
	b.WriteString("6. The plugin stores these in a plugin-owned 0600 secrets file and the `clawvisor_*` tools become available to you.\n\n")
	b.WriteString("Your job is two steps: install the plugin, then deposit the pair code.\n\n")

	// Step 1: install the plugin from this instance.
	pluginVersion := pluginbundle.Version()
	pluginSHA := ""
	if shaBytes, err := pluginbundle.SHA256(); err == nil {
		// The sha256 file format is "<hex>  filename" — take just the hex.
		fields := strings.Fields(string(shaBytes))
		if len(fields) > 0 {
			pluginSHA = fields[0]
		}
	}
	setupOpenClawPluginInstallStep(&b, clawvisorURL, pluginVersion, pluginSHA)

	b.WriteString("## 2. Configure the plugin in OpenClaw's config\n\n")
	if pairCode != "" {
		fmt.Fprintf(&b, "Your one-time pair code is embedded in this setup link:\n\n```\n%s\n```\n\n", pairCode)
		b.WriteString("Open the OpenClaw config (typically `~/.openclaw/config.yaml`, or JSON/TOML equivalent — ask the user if unsure) and add the Clawvisor plugin entry under `plugins.entries.clawvisor`. Note that user-supplied fields (`url`, `pairCode`, `agents`) live **under `.config`**, not directly on the entry — OpenClaw's schema validator rejects unknown keys at the top level of a plugin entry.\n\n")
		b.WriteString("YAML form:\n\n")
		b.WriteString("```yaml\n")
		b.WriteString("plugins:\n")
		b.WriteString("  entries:\n")
		b.WriteString("    clawvisor:\n")
		b.WriteString("      enabled: true\n")
		b.WriteString("      config:\n")
		fmt.Fprintf(&b, "        url: %s\n", clawvisorURL)
		fmt.Fprintf(&b, "        pairCode: %s\n", pairCode)
		b.WriteString("        agents: [\"main\"]   # list the OpenClaw agent ids to mint tokens for\n")
		b.WriteString("```\n\n")
		b.WriteString("JSON form (for configs that use JSON):\n\n")
		b.WriteString("```json\n")
		b.WriteString("\"plugins\": {\n")
		b.WriteString("  \"entries\": {\n")
		b.WriteString("    \"clawvisor\": {\n")
		b.WriteString("      \"enabled\": true,\n")
		b.WriteString("      \"config\": {\n")
		fmt.Fprintf(&b, "        \"url\": \"%s\",\n", clawvisorURL)
		fmt.Fprintf(&b, "        \"pairCode\": \"%s\",\n", pairCode)
		b.WriteString("        \"agents\": [\"main\"]\n")
		b.WriteString("      }\n")
		b.WriteString("    }\n")
		b.WriteString("  }\n")
		b.WriteString("}\n")
		b.WriteString("```\n\n")
		b.WriteString("**Important:** set `agents` in the same edit as `pairCode` — not afterwards. The pair code is single-use, and the server mints exactly the agent tokens listed in `agents` at pair time. If `agents` is empty or missing on first load, the pair succeeds but produces zero agent tokens and the code is burned; recovery requires minting a fresh code, deleting `~/.openclaw/plugins/clawvisor/secrets.json`, and reloading. (The plugin now detects this state and logs a warning telling the user to re-pair, but avoiding it in the first place is simpler.)\n\n")
		b.WriteString("The pair code expires in ~10 minutes, so don't delay.\n\n")
	} else {
		b.WriteString("This setup link is missing (or has a malformed) `pair_code` query parameter. Ask the user to regenerate the link from the Clawvisor dashboard (Agents page → Connect OpenClaw). Codes expire after 10 minutes; regenerating always works.\n\n")
	}

	b.WriteString("## 3. Ask the user to reload the Clawvisor plugin\n\n")
	b.WriteString("Once the config is updated, the plugin needs to pick up the new `pairCode`. Ask the user to either:\n\n")
	b.WriteString("- **Reload the Clawvisor plugin** from OpenClaw's plugin management UI, *or*\n")
	b.WriteString("- **Restart OpenClaw**.\n\n")
	b.WriteString("The plugin will redeem the code and a pairing request will appear in the Clawvisor dashboard.\n\n")

	b.WriteString("## 4. Ask the user to approve the pairing\n\n")
	fmt.Fprintf(&b, "The user should open the Clawvisor dashboard at `%s/dashboard/agents` and approve the pending plugin pairing. ", clawvisorURL)
	b.WriteString("The approval card shows the install fingerprint, hostname, and list of agents that will receive tokens. A checkbox on the card controls whether the plugin is allowed to drive auto-approval from observed conversations — encourage the user to read that option before checking it.\n\n")
	b.WriteString("Once they approve, the plugin receives tokens silently — you'll know it worked when the `clawvisor_*` tools start appearing in your tool list (may require a tool-list refresh depending on your runtime).\n\n")

	b.WriteString("## 5. Verify\n\n")
	b.WriteString("Once the `clawvisor_*` tools are visible, call `clawvisor_fetch_catalog` with no arguments. A successful JSON catalog confirms the bridge + agent tokens are wired up.\n\n")
	b.WriteString("Tell the user setup is complete.\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}
