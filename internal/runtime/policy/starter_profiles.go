package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type StarterProfile struct {
	ID          string                    `json:"id"`
	DisplayName string                    `json:"display_name"`
	Description string                    `json:"description"`
	CommandKeys []string                  `json:"command_keys"`
	Rules       []StarterProfileRuleDraft `json:"rules"`
}

type StarterProfileRuleDraft struct {
	Kind      string `json:"kind"`
	Action    string `json:"action"`
	Host      string `json:"host,omitempty"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	PathRegex string `json:"path_regex,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type RuntimeRuleCandidate struct {
	Rule         *store.RuntimePolicyRule `json:"rule"`
	ScopeDefault string                   `json:"scope_default"`
}

func StarterProfiles() []StarterProfile {
	return []StarterProfile{
		{
			ID:          "claude_code",
			DisplayName: "Claude Code",
			Description: "Recommended allow rules for Claude Code provider traffic and common background calls.",
			CommandKeys: []string{"claude"},
			Rules: []StarterProfileRuleDraft{
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Method: "POST", Path: "/v1/messages", Reason: "Claude model responses"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Method: "POST", Path: "/v1/messages/count_tokens", Reason: "Claude token counting"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/api/event_logging/v2/batch", Reason: "Claude event logging"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/api/claude_cli/bootstrap", Reason: "Claude CLI bootstrap"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/api/claude_code_penguin_mode", Reason: "Claude code mode configuration"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/api/claude_code_grove", Reason: "Claude code feature configuration"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/api/oauth/account/settings", Reason: "Claude account settings lookup"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/v1/mcp_servers", Reason: "Claude MCP server discovery"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", Path: "/mcp-registry/v0/servers", Reason: "Claude MCP registry discovery"},
				{Kind: "egress", Action: "allow", Host: "api.anthropic.com", PathRegex: `^/api/eval/.*`, Reason: "Claude eval and diagnostics control plane"},
				{Kind: "egress", Action: "allow", Host: "platform.claude.com", Method: "POST", Path: "/v1/oauth/token", Reason: "Claude OAuth token refresh"},
				{Kind: "egress", Action: "allow", Host: "mcp-proxy.anthropic.com", PathRegex: `^/v1/mcp/.*`, Reason: "Claude MCP proxy traffic"},
				{Kind: "egress", Action: "allow", Host: "downloads.claude.ai", Method: "GET", Path: "/claude-code-releases/plugins/claude-plugins-official/latest", Reason: "Claude plugin release check"},
				{Kind: "egress", Action: "allow", Host: "downloads.claude.ai", Method: "GET", PathRegex: `^/claude-code-releases/plugins/claude-plugins-official/.*`, Reason: "Claude plugin artifact download"},
				{Kind: "egress", Action: "allow", Host: "storage.googleapis.com", Method: "GET", PathRegex: `^/claude-code-dist-[^/]+/claude-code-releases/stable$`, Reason: "Claude release channel metadata"},
				{Kind: "egress", Action: "allow", Host: "http-intake.logs.us5.datadoghq.com", Method: "POST", Path: "/api/v2/logs", Reason: "Claude runtime log shipping"},
				{Kind: "egress", Action: "allow", Host: "statsig.anthropic.com", Method: "POST", PathRegex: `^/v1/.*`, Reason: "Claude runtime telemetry"},
			},
		},
		{
			ID:          "codex",
			DisplayName: "Codex",
			Description: "Recommended allow rules for OpenAI and Codex runtime control-plane traffic.",
			CommandKeys: []string{"codex"},
			Rules: []StarterProfileRuleDraft{
				{Kind: "egress", Action: "allow", Host: "api.openai.com", Method: "POST", Path: "/v1/responses", Reason: "OpenAI Responses API"},
				{Kind: "egress", Action: "allow", Host: "api.openai.com", Method: "POST", Path: "/v1/chat/completions", Reason: "OpenAI Chat Completions API"},
				{Kind: "egress", Action: "allow", Host: "chatgpt.com", Method: "POST", PathRegex: `^/backend-api/codex/responses(/.*)?$`, Reason: "Codex backend responses"},
			},
		},
	}
}

func StarterProfileByID(profileID string) (*StarterProfile, bool) {
	for _, profile := range StarterProfiles() {
		if profile.ID == profileID {
			cp := profile
			return &cp, true
		}
	}
	return nil, false
}

func DetectStarterProfile(override string, argv []string) (commandKey string, profileID string) {
	if id := normalizeStarterProfileID(override); id != "" {
		if profile, ok := StarterProfileByID(id); ok {
			commandKey = id
			if len(profile.CommandKeys) > 0 {
				commandKey = profile.CommandKeys[0]
			}
			return commandKey, id
		}
	}
	if len(argv) == 0 {
		return "", ""
	}
	cmd := normalizeCommandKey(argv[0])
	for _, profile := range StarterProfiles() {
		for _, key := range profile.CommandKeys {
			if cmd == normalizeCommandKey(key) {
				return cmd, profile.ID
			}
		}
	}
	return cmd, ""
}

func normalizeStarterProfileID(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "", "none":
		return ""
	default:
		return value
	}
}

func normalizeCommandKey(value string) string {
	value = strings.TrimSpace(strings.ToLower(path.Base(value)))
	value = strings.TrimSuffix(value, ".exe")
	return value
}

func ApplyStarterProfileRules(userID, agentID string, profile StarterProfile) []*store.RuntimePolicyRule {
	nowRules := make([]*store.RuntimePolicyRule, 0, len(profile.Rules))
	for _, draft := range profile.Rules {
		rule := &store.RuntimePolicyRule{
			ID:        uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("starter-profile:%s:%s:%s:%s:%s:%s:%s", userID, agentID, profile.ID, draft.Kind, draft.Action, draft.Host, firstNonEmpty(draft.Path, draft.PathRegex, draft.ToolName)))).String(),
			UserID:    userID,
			Kind:      draft.Kind,
			Action:    draft.Action,
			Host:      draft.Host,
			Method:    draft.Method,
			Path:      draft.Path,
			PathRegex: draft.PathRegex,
			ToolName:  draft.ToolName,
			Reason:    draft.Reason,
			Source:    "starter_profile",
			Enabled:   true,
		}
		if strings.TrimSpace(agentID) != "" {
			rule.AgentID = &agentID
		}
		nowRules = append(nowRules, rule)
	}
	return nowRules
}

func NormalizeRuntimeEventToRuleCandidate(event *store.RuntimeEvent, action string) (*RuntimeRuleCandidate, error) {
	if event == nil {
		return nil, fmt.Errorf("runtime event is required")
	}
	action = strings.TrimSpace(strings.ToLower(action))
	switch action {
	case "allow", "deny", "review":
	default:
		return nil, fmt.Errorf("invalid runtime rule action %q", action)
	}
	metadata := mapStringAny(event.MetadataJSON)
	kind := strings.TrimSpace(strings.ToLower(event.ActionKind))
	switch kind {
	case "egress":
		host := strings.TrimSpace(stringValue(metadata["host"]))
		if host == "" {
			return nil, fmt.Errorf("runtime event does not include an egress host")
		}
		method := strings.ToUpper(strings.TrimSpace(stringValue(metadata["method"])))
		pathValue := strings.TrimSpace(stringValue(metadata["path"]))
		rule := &store.RuntimePolicyRule{
			UserID:  event.UserID,
			Kind:    "egress",
			Action:  action,
			Host:    host,
			Method:  method,
			Reason:  strings.TrimSpace(firstNonEmpty(ptrString(event.Reason), "Created from runtime event")),
			Source:  "user",
			Enabled: true,
		}
		if pathValue != "" && stableRuntimePath(pathValue) {
			rule.Path = pathValue
		}
		return &RuntimeRuleCandidate{Rule: rule, ScopeDefault: "agent"}, nil
	case "tool_use":
		toolName := strings.TrimSpace(stringValue(metadata["tool_name"]))
		if toolName == "" {
			toolName = strings.TrimSpace(stringValue(metadata["name"]))
		}
		if toolName == "" {
			return nil, fmt.Errorf("runtime event does not include a tool name")
		}
		rule := &store.RuntimePolicyRule{
			UserID:   event.UserID,
			Kind:     "tool",
			Action:   action,
			ToolName: toolName,
			Reason:   strings.TrimSpace(firstNonEmpty(ptrString(event.Reason), "Created from runtime event")),
			Source:   "user",
			Enabled:  true,
		}
		return &RuntimeRuleCandidate{Rule: rule, ScopeDefault: "agent"}, nil
	default:
		return nil, fmt.Errorf("runtime event kind %q cannot be promoted to a rule", kind)
	}
}

func stableRuntimePath(p string) bool {
	if p == "" || p == "/" {
		return true
	}
	if strings.ContainsAny(p, "?*") {
		return false
	}
	variableSeg := regexp.MustCompile(`^(?:\d+|[0-9a-f]{8,}|[0-9a-f-]{16,}|[A-Za-z0-9_-]{24,})$`)
	for _, segment := range strings.Split(strings.Trim(p, "/"), "/") {
		if segment == "" {
			continue
		}
		if variableSeg.MatchString(segment) {
			return false
		}
	}
	return true
}

func mapStringAny(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := jsonUnmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func stringValue(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func ptrString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func jsonUnmarshal(data []byte, dest any) error {
	return json.Unmarshal(data, dest)
}

func StarterProfileIDs() []string {
	ids := make([]string, 0, len(StarterProfiles()))
	for _, profile := range StarterProfiles() {
		ids = append(ids, profile.ID)
	}
	sort.Strings(ids)
	return ids
}

type RuntimeEventTaskEnvelope struct {
	Purpose        string
	ExpectedUse    string
	ExpectedEgress json.RawMessage
	ExpectedTools  json.RawMessage
}

func NormalizeRuntimeEventTaskEnvelope(event *store.RuntimeEvent) *RuntimeEventTaskEnvelope {
	if event == nil {
		return nil
	}
	metadata := mapStringAny(event.MetadataJSON)
	switch strings.TrimSpace(strings.ToLower(event.ActionKind)) {
	case "egress":
		host := strings.TrimSpace(stringValue(metadata["host"]))
		if host == "" {
			return nil
		}
		method := strings.ToUpper(strings.TrimSpace(firstNonEmpty(stringValue(metadata["method"]), "GET")))
		reqPath := strings.TrimSpace(stringValue(metadata["path"]))
		purpose := firstNonEmpty(ptrString(event.Reason), fmt.Sprintf("Runtime egress to %s%s", host, reqPath))
		expectedUse := fmt.Sprintf("%s %s%s", method, host, reqPath)
		payload, _ := json.Marshal([]runtimetasks.ExpectedEgress{{
			Host:   host,
			Method: method,
			Path:   reqPath,
			Why:    firstNonEmpty(ptrString(event.Reason), "Runtime event promotion"),
		}})
		return &RuntimeEventTaskEnvelope{
			Purpose:        purpose,
			ExpectedUse:    expectedUse,
			ExpectedEgress: payload,
		}
	case "tool_use":
		toolName := strings.TrimSpace(firstNonEmpty(stringValue(metadata["tool_name"]), stringValue(metadata["name"])))
		if toolName == "" {
			return nil
		}
		purpose := firstNonEmpty(ptrString(event.Reason), fmt.Sprintf("Runtime tool use: %s", toolName))
		payload, _ := json.Marshal([]runtimetasks.ExpectedTool{{
			ToolName: toolName,
			Why:      firstNonEmpty(ptrString(event.Reason), "Runtime event promotion"),
		}})
		return &RuntimeEventTaskEnvelope{
			Purpose:       purpose,
			ExpectedUse:   fmt.Sprintf("Use runtime tool %s", toolName),
			ExpectedTools: payload,
		}
	default:
		return nil
	}
}
