// Package handlers — welcome.go serves the "What is Clawvisor?" page.
//
// It reports the user's setup state (connected services + agents) and, when
// both are present, asks the LLM to suggest personalized first tasks the user
// could hand their agent. Suggestions are best-effort: when the LLM is not
// configured, exhausted, or errors out, we return an empty list and let the
// frontend fall back to a static explainer.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// WelcomeHandler serves the "what is Clawvisor" explainer page data.
type WelcomeHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	llmHealth  *llm.Health
	logger     *slog.Logger
}

// NewWelcomeHandler builds a WelcomeHandler.
func NewWelcomeHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, h *llm.Health, logger *slog.Logger) *WelcomeHandler {
	return &WelcomeHandler{st: st, vault: v, adapterReg: adapterReg, llmHealth: h, logger: logger}
}

// welcomeAction is one action the connected service supports. Categories and
// sensitivities come from adapter metadata and help the LLM (and the UI) bias
// suggestions toward low-risk reads first.
type welcomeAction struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category,omitempty"`    // "read" | "write" | "delete" | "search"
	Sensitivity string `json:"sensitivity,omitempty"` // "low" | "medium" | "high"
}

// welcomeService summarises one connected service for the LLM prompt and UI.
type welcomeService struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Alias       string          `json:"alias,omitempty"`
	Description string          `json:"description,omitempty"`
	IconURL     string          `json:"icon_url,omitempty"`
	IconSVG     string          `json:"icon_svg,omitempty"`
	Actions     []welcomeAction `json:"actions,omitempty"`
}

// welcomeAgent summarises one registered agent.
type welcomeAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// taskSuggestion is one LLM-generated idea for a task the user could hand
// their agent. Kept deliberately small — the frontend renders a prompt the
// user can copy/paste.
type taskSuggestion struct {
	Title    string   `json:"title"`              // short headline, e.g. "Triage inbox"
	Prompt   string   `json:"prompt"`             // example natural-language prompt to hand the agent
	Agent    string   `json:"agent,omitempty"`    // recommended agent name, if any
	Services []string `json:"services"`           // service IDs the prompt involves
	Risk     string   `json:"risk,omitempty"`     // "low" | "medium" | "high"
}

// walkthroughExample personalises the "here's what a task looks like" flow
// using a coherent pairing of the user's connected services. Frontend falls
// back to a hardcoded Gmail/Linear example when this is absent.
type walkthroughExample struct {
	UserPrompt   string   `json:"user_prompt"`           // what the user would ask, e.g. "Triage my Gmail…"
	AgentTask    string   `json:"agent_task"`            // what the agent declares, e.g. "read Gmail…, create Linear issues"
	PrimaryName  string   `json:"primary_name"`          // the main service name, for the subtitle
	SecondaryName string  `json:"secondary_name"`        // the paired service name, for the subtitle
	Services     []string `json:"services,omitempty"`    // service IDs referenced
}

// welcomeResponse is the payload for GET /api/welcome/suggestions.
type welcomeResponse struct {
	Ready       bool                `json:"ready"`        // true when ≥1 service + ≥1 agent
	Services    []welcomeService    `json:"services"`
	Agents      []welcomeAgent      `json:"agents"`
	Suggestions []taskSuggestion    `json:"suggestions"`  // may be empty
	Walkthrough *walkthroughExample `json:"walkthrough,omitempty"`
	LLMUsed     bool                `json:"llm_used"`     // true if suggestions came from LLM
	LLMStatus   string              `json:"llm_status"`   // "ok" | "unconfigured" | "exhausted" | "error"
}

// Suggestions returns the welcome-page payload.
//
// GET /api/welcome/suggestions
// Auth: user JWT
func (h *WelcomeHandler) Suggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	services, err := h.listActivatedServices(ctx, user.ID)
	if err != nil {
		h.logger.Warn("welcome: failed to list services", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list services")
		return
	}

	agents, err := h.st.ListAgents(ctx, user.ID)
	if err != nil {
		h.logger.Warn("welcome: failed to list agents", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list agents")
		return
	}
	agentSummaries := make([]welcomeAgent, 0, len(agents))
	for _, a := range agents {
		agentSummaries = append(agentSummaries, welcomeAgent{ID: a.ID, Name: a.Name})
	}

	resp := welcomeResponse{
		Ready:       len(services) > 0 && len(agents) > 0,
		Services:    services,
		Agents:      agentSummaries,
		Suggestions: []taskSuggestion{},
	}

	if !resp.Ready {
		resp.LLMStatus = "ok"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Decide whether to call the LLM.
	cfg := h.llmHealth.LLMConfig()
	if cfg.APIKey == "" {
		resp.LLMStatus = "unconfigured"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if h.llmHealth.SpendCapExhausted() {
		resp.LLMStatus = "exhausted"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	suggestions, walkthrough, err := h.generateSuggestions(ctx, cfg, services, agentSummaries)
	if err != nil {
		if errors.Is(err, llm.ErrSpendCapExhausted) {
			h.llmHealth.SetSpendCapExhausted()
			resp.LLMStatus = "exhausted"
		} else {
			h.logger.Warn("welcome: suggestion generation failed", "err", err)
			resp.LLMStatus = "error"
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.Suggestions = suggestions
	resp.Walkthrough = walkthrough
	resp.LLMUsed = true
	resp.LLMStatus = "ok"
	writeJSON(w, http.StatusOK, resp)
}

// listActivatedServices returns the user's activated services, with display
// names, descriptions, and action IDs the agent can take. This mirrors the
// activated-branch of ServicesHandler.List but keeps only the fields the
// welcome page (and the LLM prompt) needs.
func (h *WelcomeHandler) listActivatedServices(ctx context.Context, userID string) ([]welcomeService, error) {
	activatedKeys, err := h.vault.List(ctx, userID)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return nil, err
	}
	keySet := make(map[string]bool, len(activatedKeys))
	for _, k := range activatedKeys {
		keySet[k] = true
	}

	metas, err := h.st.ListServiceMetas(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := make([]welcomeService, 0)
	seen := make(map[string]bool)

	buildEntry := func(a adapters.Adapter, alias string) welcomeService {
		name := display.ServiceName(a.ServiceID())
		desc := display.ServiceDescription(a.ServiceID())
		var iconURL, iconSVG string
		actionMetaByID := map[string]adapters.ActionMeta{}
		if mp, ok := a.(adapters.MetadataProvider); ok {
			meta := mp.ServiceMetadata()
			if meta.DisplayName != "" {
				name = meta.DisplayName
			}
			if meta.Description != "" {
				desc = meta.Description
			}
			iconURL = meta.IconURL
			iconSVG = meta.IconSVG
			actionMetaByID = meta.ActionMeta
		}
		actions := make([]welcomeAction, 0, len(a.SupportedActions()))
		for _, actionID := range a.SupportedActions() {
			label := display.ActionName(actionID)
			var category, sensitivity string
			if am, ok := actionMetaByID[actionID]; ok {
				if am.DisplayName != "" {
					label = am.DisplayName
				}
				category = am.Category
				sensitivity = am.Sensitivity
			}
			actions = append(actions, welcomeAction{
				ID:          actionID,
				DisplayName: label,
				Category:    category,
				Sensitivity: sensitivity,
			})
		}
		entry := welcomeService{
			ID:          a.ServiceID(),
			Name:        name,
			Description: desc,
			IconURL:     iconURL,
			IconSVG:     iconSVG,
			Actions:     actions,
		}
		if alias != "" && alias != "default" {
			entry.Alias = alias
		}
		return entry
	}

	for _, a := range h.adapterReg.All() {
		if ac, ok := a.(adapters.AvailabilityChecker); ok && !ac.Available() {
			continue
		}
		credentialFree := a.ValidateCredential(nil) == nil

		if credentialFree {
			for _, m := range metas {
				if m.ServiceID != a.ServiceID() {
					continue
				}
				key := a.ServiceID() + ":" + m.Alias
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, buildEntry(a, m.Alias))
			}
			continue
		}

		for _, m := range metas {
			if m.ServiceID != a.ServiceID() {
				continue
			}
			vKey := h.adapterReg.VaultKeyWithAlias(a.ServiceID(), m.Alias)
			if !keySet[vKey] {
				continue
			}
			key := a.ServiceID() + ":" + m.Alias
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, buildEntry(a, m.Alias))
		}

		baseKey := h.adapterReg.VaultKey(a.ServiceID())
		usesSharedKey := baseKey != a.ServiceID()
		if !usesSharedKey && keySet[baseKey] {
			key := a.ServiceID() + ":default"
			if !seen[key] {
				seen[key] = true
				out = append(out, buildEntry(a, ""))
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Alias < out[j].Alias
	})
	return out, nil
}

// generateSuggestions asks the LLM for 3-5 task ideas and a walkthrough example
// tailored to the user's connected services and agent names. Returns an error
// if the LLM call fails.
func (h *WelcomeHandler) generateSuggestions(ctx context.Context, llmCfg config.LLMConfig, services []welcomeService, agents []welcomeAgent) ([]taskSuggestion, *walkthroughExample, error) {
	providerCfg := config.LLMProviderConfig{
		Provider:       llmCfg.Provider,
		Endpoint:       llmCfg.Endpoint,
		APIKey:         llmCfg.APIKey,
		Model:          llmCfg.Model,
		TimeoutSeconds: llmCfg.TimeoutSeconds,
	}

	client := llm.NewClient(providerCfg).WithMaxTokens(1400)

	userMsg := buildSuggestionUserMessage(services, agents)

	// Run with a bounded timeout on top of the client's own.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	messages := []llm.ChatMessage{
		{Role: "system", Content: suggestionSystemPrompt},
		{Role: "user", Content: userMsg},
	}

	var raw string
	var lastErr error
	for attempt := range 2 {
		r, err := client.Complete(ctx, messages)
		if err != nil {
			lastErr = err
			if errors.Is(err, llm.ErrSpendCapExhausted) {
				return nil, nil, err
			}
			if attempt == 0 {
				continue
			}
			return nil, nil, err
		}
		raw = r
		break
	}
	if raw == "" {
		return nil, nil, lastErr
	}

	return parseSuggestionResponse(raw)
}

const suggestionSystemPrompt = `You help Clawvisor users discover useful first tasks to hand to their AI agents.

Clawvisor is a gateway between AI agents and external APIs. Users connect services (Gmail, GitHub, Linear, Slack, etc.) and register agents that act through Clawvisor. Agents declare tasks; the user approves scopes once; individual destructive actions can still require per-request approval.

You will be given (a) each connected service with its supported actions tagged with category (read/write/delete/search) and sensitivity (low/medium/high), and (b) the names of registered agents.

Produce 3-5 concrete, copy-pasteable task prompts tailored to this user's exact setup.

Rules:
- Pick the most appropriate agent per suggestion and put its name in the "agent" field.
- The "prompt" field is the direct instruction the user would paste into the agent chat. Write it as a direct imperative addressed to the agent ("Search my Gmail for …", "Find the most recent …"). DO NOT prefix it with "Ask <agent> to …" — the UI renders that framing separately from the agent field.
- Lead with read-mostly workflows before destructive ones. At most one suggestion should involve high-sensitivity writes.
- Favor combinations across services when natural (e.g. "read Gmail + create Linear issues for anything actionable").
- Be specific: reference concrete fields, recent time windows, or named topics. No vague "summarize stuff".
- Each prompt should be 1-2 sentences the user could literally paste into a chat with the agent.

Also produce ONE walkthrough example illustrating the task → approval → enforcement flow using a coherent pairing of the user's services (e.g. an inbox/messaging service as the source and a task-tracker or notes service as the destination). Pick services that pair naturally; never pair an inbox with a calendar as the "destination". If no coherent pair is possible, use the single best service.

Return ONLY a JSON object — no prose, no markdown fences:

{
  "suggestions": [
    {
      "title": "Short headline (<=50 chars)",
      "prompt": "Full example prompt the user can paste",
      "agent": "agent name from the input, or empty",
      "services": ["service.id.1", "service.id.2"],
      "risk": "low" | "medium" | "high"
    }
  ],
  "walkthrough": {
    "user_prompt": "The quoted one-line natural-language ask, e.g. \"Triage my Gmail and add anything actionable to Linear.\"",
    "agent_task": "One sentence describing what the agent declares, e.g. \"read Gmail messages received in the last 72 hours, create items in Linear.\"",
    "primary_name": "Gmail",
    "secondary_name": "Linear",
    "services": ["service.id.primary", "service.id.secondary"]
  }
}

Use service IDs and agent names EXACTLY as given in the input. Return between 3 and 5 suggestions. Always include the walkthrough object.`

// buildSuggestionUserMessage renders the context for the LLM.
func buildSuggestionUserMessage(services []welcomeService, agents []welcomeAgent) string {
	var b strings.Builder

	b.WriteString("# Connected services\n\n")
	for _, s := range services {
		name := s.Name
		if s.Alias != "" {
			name = fmt.Sprintf("%s (%s)", s.Name, s.Alias)
		}
		fmt.Fprintf(&b, "- **%s** (id: `%s`)", name, s.ID)
		if s.Description != "" {
			fmt.Fprintf(&b, " — %s", s.Description)
		}
		b.WriteString("\n")
		if len(s.Actions) > 0 {
			actions := s.Actions
			if len(actions) > 12 {
				actions = actions[:12]
			}
			labels := make([]string, 0, len(actions))
			for _, act := range actions {
				l := act.DisplayName
				if act.Category != "" || act.Sensitivity != "" {
					tag := strings.TrimSpace(act.Category + " " + act.Sensitivity)
					if tag != "" {
						l = fmt.Sprintf("%s [%s]", l, tag)
					}
				}
				labels = append(labels, l)
			}
			fmt.Fprintf(&b, "  Supported actions: %s\n", strings.Join(labels, ", "))
		}
	}

	b.WriteString("\n# Registered agents\n\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "- `%s`\n", a.Name)
	}

	b.WriteString("\nSuggest 3-5 first tasks. Respond with JSON only.")
	return b.String()
}

// stripAskPrefix removes a leading "Ask <agent> to " (case-insensitive) from a
// prompt, along with any surrounding quote marks, and re-capitalizes the first
// letter. The UI renders the "Ask <agent>:" framing separately.
func stripAskPrefix(s string) string {
	trimmed := strings.TrimSpace(s)
	// Strip outer quotes if the whole thing is quoted.
	if len(trimmed) >= 2 {
		first, last := trimmed[0], trimmed[len(trimmed)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			trimmed = trimmed[1 : len(trimmed)-1]
			trimmed = strings.TrimSpace(trimmed)
		}
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "ask ") {
		return trimmed
	}
	rest := trimmed[4:]
	// Find " to " after the agent name.
	idx := strings.Index(strings.ToLower(rest), " to ")
	if idx < 0 {
		return trimmed
	}
	remainder := strings.TrimSpace(rest[idx+4:])
	if remainder == "" {
		return trimmed
	}
	// Capitalize first letter.
	r := []rune(remainder)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// parseSuggestionResponse extracts the suggestions array and the walkthrough
// example from the LLM output. Tolerates markdown code fences around the JSON.
// The walkthrough is optional — returned as nil if malformed.
func parseSuggestionResponse(raw string) ([]taskSuggestion, *walkthroughExample, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out struct {
		Suggestions []taskSuggestion    `json:"suggestions"`
		Walkthrough *walkthroughExample `json:"walkthrough"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, nil, fmt.Errorf("parse suggestion response: %w", err)
	}

	// Filter out malformed entries and strip any leaked "Ask <agent> to …"
	// prefix — the UI renders that framing from the agent field.
	cleaned := make([]taskSuggestion, 0, len(out.Suggestions))
	for _, s := range out.Suggestions {
		if strings.TrimSpace(s.Title) == "" || strings.TrimSpace(s.Prompt) == "" {
			continue
		}
		s.Prompt = stripAskPrefix(s.Prompt)
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		return nil, nil, fmt.Errorf("no valid suggestions in response")
	}

	walkthrough := out.Walkthrough
	if walkthrough != nil {
		if strings.TrimSpace(walkthrough.UserPrompt) == "" ||
			strings.TrimSpace(walkthrough.AgentTask) == "" ||
			strings.TrimSpace(walkthrough.PrimaryName) == "" ||
			strings.TrimSpace(walkthrough.SecondaryName) == "" {
			walkthrough = nil
		}
	}

	return cleaned, walkthrough, nil
}
