package handlers

import (
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/haikuproxy"
)

// LLMHandler exposes LLM health status and allows runtime config updates.
type LLMHandler struct {
	health     *llm.Health
	configPath string // path to config.yaml for persistence
}

// NewLLMHandler creates an LLM settings handler.
func NewLLMHandler(health *llm.Health, configPath string) *LLMHandler {
	return &LLMHandler{health: health, configPath: configPath}
}

// LLMStatus is the JSON response for GET /api/llm/status.
type LLMStatus struct {
	Status            string   `json:"status"` // "ok" | "spend_cap_exhausted"
	IsHaikuProxy      bool     `json:"is_haiku_proxy"`
	SpendCapExhausted bool     `json:"spend_cap_exhausted"`
	Provider          string   `json:"provider"`
	Model             string   `json:"model"`
	Usage             *LLMUsage `json:"usage,omitempty"` // only for haiku proxy keys
}

// LLMUsage is the spend information for a haiku proxy key.
type LLMUsage struct {
	SpendCap   float64 `json:"spend_cap"`
	TotalSpent float64 `json:"total_spent"`
	Remaining  float64 `json:"remaining"`
	PctUsed    float64 `json:"pct_used"` // 0-100
}

// Status returns the current LLM health status.
func (h *LLMHandler) Status(w http.ResponseWriter, r *http.Request) {
	cfg := h.health.LLMConfig()
	exhausted := h.health.SpendCapExhausted()
	status := "ok"
	if exhausted {
		status = "spend_cap_exhausted"
	}

	resp := LLMStatus{
		Status:            status,
		IsHaikuProxy:      h.health.IsHaikuProxy(),
		SpendCapExhausted: exhausted,
		Provider:          cfg.Provider,
		Model:             cfg.Model,
	}

	// Fetch live usage for haiku proxy keys (best-effort).
	if h.health.IsHaikuProxy() {
		if usage, err := haikuproxy.GetUsage(cfg.APIKey); err == nil {
			pct := 0.0
			if usage.SpendCap > 0 {
				pct = (usage.TotalSpent / usage.SpendCap) * 100
			}
			resp.Usage = &LLMUsage{
				SpendCap:   usage.SpendCap,
				TotalSpent: usage.TotalSpent,
				Remaining:  usage.Remaining,
				PctUsed:    pct,
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// UpdateRequest is the JSON body for PUT /api/llm.
type UpdateRequest struct {
	Provider string `json:"provider"` // "anthropic" | "openai"
	Endpoint string `json:"endpoint"` // base URL
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
}

// Update replaces the LLM API key and endpoint at runtime.
// It updates the in-memory config and persists to config.yaml.
func (h *LLMHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req UpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "api_key is required")
		return
	}
	if req.Provider == "" {
		req.Provider = "anthropic"
	}
	switch req.Provider {
	case "anthropic":
		if req.Endpoint == "" {
			req.Endpoint = "https://api.anthropic.com/v1"
		}
		if req.Model == "" {
			req.Model = "claude-haiku-4-5-20251001"
		}
	case "vertex":
		// Clear any non-vertex endpoint (e.g. leftover anthropic URL).
		// NewClient auto-constructs the Vertex endpoint from VERTEX_REGION
		// and VERTEX_PROJECT_ID env vars.
		if !strings.Contains(req.Endpoint, "aiplatform.googleapis.com") {
			req.Endpoint = ""
		}
		if req.Model == "" {
			req.Model = "claude-haiku-4-5-20251001"
		}
	default:
		if req.Endpoint == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "endpoint is required for non-anthropic providers")
			return
		}
		if req.Model == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "model is required for non-anthropic providers")
			return
		}
	}

	// Update in-memory config. The health tracker propagates changes to
	// the verifier, assessor, and extractor on their next call.
	cfg := h.health.LLMConfig()
	cfg.Provider = req.Provider
	cfg.Endpoint = req.Endpoint
	cfg.APIKey = req.APIKey
	cfg.Model = req.Model

	// Inherit shared fields into subsections.
	inheritIfEmpty := func(sub *config.LLMProviderConfig) {
		if sub.Provider == "" {
			sub.Provider = cfg.Provider
		}
		if sub.Endpoint == "" {
			sub.Endpoint = cfg.Endpoint
		}
		if sub.APIKey == "" {
			sub.APIKey = cfg.APIKey
		}
		if sub.Model == "" {
			sub.Model = cfg.Model
		}
	}
	inheritIfEmpty(&cfg.Verification.LLMProviderConfig)
	inheritIfEmpty(&cfg.TaskRisk.LLMProviderConfig)
	inheritIfEmpty(&cfg.ChainContext.LLMProviderConfig)

	h.health.UpdateConfig(cfg) // clears spend_cap_exhausted flag

	// Persist to config.yaml (best-effort).
	if h.configPath != "" {
		if err := patchConfigLLM(h.configPath, req); err != nil {
			// Log but don't fail — in-memory update already took effect.
			writeJSON(w, http.StatusOK, map[string]any{
				"status":  "updated",
				"warning": "in-memory config updated but failed to persist: " + err.Error(),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// patchConfigLLM reads the existing config.yaml, updates the llm section, and writes it back.
func patchConfigLLM(path string, req UpdateRequest) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Parse as generic map to preserve structure/comments as much as possible.
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}

	llmSection, _ := doc["llm"].(map[string]any)
	if llmSection == nil {
		llmSection = map[string]any{}
	}
	llmSection["provider"] = req.Provider
	llmSection["endpoint"] = req.Endpoint
	llmSection["api_key"] = req.APIKey
	llmSection["model"] = req.Model
	doc["llm"] = llmSection

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}

	// Preserve original file permissions.
	info, err := os.Stat(path)
	if err != nil {
		return os.WriteFile(path, out, 0600)
	}
	return os.WriteFile(path, out, info.Mode().Perm())
}

// MaskAPIKey returns a masked version of an API key for display.
func MaskAPIKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + "..." + key[len(key)-4:]
}
