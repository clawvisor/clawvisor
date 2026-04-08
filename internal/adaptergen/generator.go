// Package adaptergen generates YAML adapter definitions from API source material
// (MCP tool schemas, OpenAPI specs, or raw API docs) using an adversarial LLM.
// The requesting agent never touches the generated definition — Clawvisor's own
// LLM produces the adapter YAML and independently classifies risk for each action.
package adaptergen

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// Generator orchestrates LLM-powered adapter creation, risk classification,
// validation, and installation.
type Generator struct {
	genClient  *llm.Client // high max_tokens for YAML generation
	riskClient *llm.Client // lower max_tokens for risk classification JSON
	registry   *adapters.Registry
	adaptersDir string // ~/.clawvisor/adapters/
	logger      *slog.Logger
}

// GenerateResult contains the output of a generation attempt.
type GenerateResult struct {
	ServiceID   string         `json:"service_id"`
	DisplayName string         `json:"display_name"`
	Description string         `json:"description,omitempty"`
	BaseURL     string         `json:"base_url"`
	AuthType    string         `json:"auth_type"`
	YAML        string         `json:"yaml"`
	Actions     []ActionPreview `json:"actions"`
	Warnings    []string       `json:"warnings,omitempty"`
	Installed   bool           `json:"installed"`
}

// ActionPreview is a user-friendly summary of a generated action.
type ActionPreview struct {
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name"`
	Method      string        `json:"method,omitempty"`
	Path        string        `json:"path,omitempty"`
	Category    string        `json:"category"`
	Sensitivity string        `json:"sensitivity"`
	Params      []ParamPreview `json:"params,omitempty"`
}

// ParamPreview is a user-friendly summary of an action parameter.
type ParamPreview struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// New creates a Generator.
func New(cfg config.AdapterGenConfig, registry *adapters.Registry, adaptersDir string, logger *slog.Logger) *Generator {
	if logger == nil {
		logger = slog.Default()
	}
	base := llm.NewClient(cfg.LLMProviderConfig)
	return &Generator{
		genClient:   base.WithMaxTokens(16384), // YAML definitions can be very large for big specs
		riskClient:  base.WithMaxTokens(4096),  // risk JSON scales with action count
		registry:    registry,
		adaptersDir: adaptersDir,
		logger:      logger,
	}
}

// Generate takes source material, generates a YAML adapter, classifies risk
// adversarially, validates the result, and installs it into the registry.
func (g *Generator) Generate(ctx context.Context, src Source) (*GenerateResult, error) {
	// Step 1: Build the generation prompt.
	userMsg, err := buildGenerationPrompt(src)
	if err != nil {
		return nil, fmt.Errorf("building prompt: %w", err)
	}

	g.logger.Info("generating adapter definition",
		"source_type", src.Type,
		"service_id_hint", src.ServiceID,
	)

	// Step 2: Call the LLM to generate the YAML (with UNCLASSIFIED risk placeholders).
	rawYAML, err := g.genClient.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: generationSystemPrompt},
		{Role: "user", Content: userMsg},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM generation failed: %w", err)
	}

	// Strip markdown code fences if the LLM included them despite instructions.
	rawYAML = stripCodeFences(rawYAML)

	// Step 3: Classify risk independently.
	classifiedYAML, err := g.classifyRisk(ctx, rawYAML)
	if err != nil {
		return nil, fmt.Errorf("risk classification failed: %w", err)
	}

	// Step 4: Parse and validate (drops incomplete actions with warnings).
	def, validationErrs, warnings, err := parseAndValidate([]byte(classifiedYAML))
	if err != nil {
		return nil, fmt.Errorf("generated YAML is malformed: %w", err)
	}
	if len(validationErrs) > 0 {
		return nil, fmt.Errorf("generated adapter failed validation: %s", strings.Join(validationErrs, "; "))
	}

	// Step 5: Return the result for preview (not yet installed).
	result := buildResult(def, classifiedYAML, false)
	result.Warnings = append(result.Warnings, warnings...)

	g.logger.Info("adapter generated (pending install)",
		"service_id", def.Service.ID,
		"actions", len(def.Actions),
	)
	return result, nil
}

// Install takes previously generated YAML, validates it, writes it to disk,
// and hot-loads it into the registry.
func (g *Generator) Install(yamlContent string) (*GenerateResult, error) {
	def, validationErrs, _, err := parseAndValidate([]byte(yamlContent))
	if err != nil {
		return nil, fmt.Errorf("YAML is malformed: %w", err)
	}
	if len(validationErrs) > 0 {
		return nil, fmt.Errorf("adapter failed validation: %s", strings.Join(validationErrs, "; "))
	}

	if err := g.install(def, yamlContent); err != nil {
		return nil, fmt.Errorf("installation failed: %w", err)
	}

	result := buildResult(def, yamlContent, true)

	g.logger.Info("adapter installed",
		"service_id", def.Service.ID,
		"actions", len(def.Actions),
	)
	return result, nil
}

// Update regenerates an existing adapter from new source material.
// The old adapter is replaced in-place.
func (g *Generator) Update(ctx context.Context, serviceID string, src Source) (*GenerateResult, error) {
	if _, ok := g.registry.Get(serviceID); !ok {
		return nil, fmt.Errorf("adapter %q not found in registry", serviceID)
	}

	// Force the service ID to match the existing adapter.
	src.ServiceID = serviceID
	return g.Generate(ctx, src)
}

// Remove deletes an adapter from the registry and disk.
func (g *Generator) Remove(serviceID string) error {
	if _, ok := g.registry.Get(serviceID); !ok {
		return fmt.Errorf("adapter %q not found in registry", serviceID)
	}

	// Remove from disk (path-traversal safe).
	path, err := safeFilename(g.adaptersDir, serviceID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing adapter file: %w", err)
	}

	g.registry.Remove(serviceID)
	g.logger.Info("adapter removed", "service_id", serviceID)

	return nil
}

// classifyRisk makes an independent LLM call to classify risk for each action,
// then patches the YAML with the classifications.
func (g *Generator) classifyRisk(ctx context.Context, rawYAML string) (string, error) {
	if !hasUnclassifiedRisk(rawYAML) {
		// LLM didn't follow instructions and included risk classifications.
		// Re-classify anyway for safety.
	}

	riskPrompt := buildRiskPrompt(rawYAML)
	riskJSON, err := g.riskClient.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: riskClassificationSystemPrompt},
		{Role: "user", Content: riskPrompt},
	})
	if err != nil {
		return "", fmt.Errorf("risk classification LLM call failed: %w", err)
	}

	// Strip markdown code fences.
	riskJSON = stripCodeFences(riskJSON)

	// Parse the risk classifications.
	var risks map[string]struct {
		Category    string `json:"category"`
		Sensitivity string `json:"sensitivity"`
	}
	if err := json.Unmarshal([]byte(riskJSON), &risks); err != nil {
		return "", fmt.Errorf("parsing risk classifications: %w (raw: %s)", err, riskJSON)
	}

	// Parse the original YAML, apply risk classifications, and re-serialize.
	var def yamldef.ServiceDef
	if err := yaml.Unmarshal([]byte(rawYAML), &def); err != nil {
		return "", fmt.Errorf("parsing generated YAML for risk patching: %w", err)
	}

	for actionName, action := range def.Actions {
		if risk, ok := risks[actionName]; ok {
			action.Risk.Category = risk.Category
			action.Risk.Sensitivity = risk.Sensitivity
		} else {
			// Action not in risk response — default to high sensitivity write.
			action.Risk.Category = "write"
			action.Risk.Sensitivity = "high"
		}
		def.Actions[actionName] = action
	}

	out, err := yaml.Marshal(&def)
	if err != nil {
		return "", fmt.Errorf("re-serializing YAML with risk: %w", err)
	}
	return string(out), nil
}

// install writes the YAML to disk and hot-loads the adapter into the registry.
func (g *Generator) install(def yamldef.ServiceDef, yamlContent string) error {
	// Ensure the adapters directory exists.
	if err := os.MkdirAll(g.adaptersDir, 0o755); err != nil {
		return fmt.Errorf("creating adapters directory: %w", err)
	}

	// Write the YAML file (path-traversal safe).
	path, err := safeFilename(g.adaptersDir, def.Service.ID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		return fmt.Errorf("writing adapter file: %w", err)
	}

	// Build and register the adapter at runtime (hot-load).
	adapter, err := yamlruntime.New(def, nil)
	if err != nil {
		return fmt.Errorf("building adapter from definition: %w", err)
	}
	g.registry.Replace(adapter)

	return nil
}

// safeFilename converts a service ID to a filename and verifies it contains
// no path traversal characters. This prevents writes outside ~/.clawvisor/adapters/
// via LLM-generated service IDs.
func safeFilename(adaptersDir, serviceID string) (string, error) {
	// Reject any service ID containing path separators or traversal sequences.
	if strings.ContainsAny(serviceID, "/\\") {
		return "", fmt.Errorf("service_id %q contains path separators", serviceID)
	}
	if strings.Contains(serviceID, "..") {
		return "", fmt.Errorf("service_id %q contains path traversal sequence", serviceID)
	}

	filename := strings.ReplaceAll(serviceID, ".", "_") + ".yaml"

	// Belt-and-suspenders: verify the resolved path is under adaptersDir.
	absDir, err := filepath.Abs(adaptersDir)
	if err != nil {
		return "", fmt.Errorf("resolving adapters directory: %w", err)
	}
	absPath, err := filepath.Abs(filepath.Join(adaptersDir, filename))
	if err != nil {
		return "", fmt.Errorf("resolving adapter file path: %w", err)
	}
	if !strings.HasPrefix(absPath, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("service_id %q resolves to a path outside the adapters directory", serviceID)
	}
	return absPath, nil
}

// buildResult constructs a GenerateResult with structured action previews from a parsed def.
func buildResult(def yamldef.ServiceDef, yamlContent string, installed bool) *GenerateResult {
	result := &GenerateResult{
		ServiceID:   def.Service.ID,
		DisplayName: def.Service.DisplayName,
		Description: def.Service.Description,
		BaseURL:     def.API.BaseURL,
		AuthType:    def.Auth.Type,
		YAML:        yamlContent,
		Installed:   installed,
	}
	for name, action := range def.Actions {
		ap := ActionPreview{
			Name:        name,
			DisplayName: action.DisplayName,
			Method:      action.Method,
			Path:        action.Path,
			Category:    action.Risk.Category,
			Sensitivity: action.Risk.Sensitivity,
		}
		for pName, param := range action.Params {
			ap.Params = append(ap.Params, ParamPreview{
				Name:     pName,
				Type:     param.Type,
				Required: param.Required,
			})
		}
		result.Actions = append(result.Actions, ap)
	}
	return result
}

// stripCodeFences removes markdown code fences (```yaml ... ``` or ```json ... ```)
// that LLMs sometimes include despite instructions.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	re := regexp.MustCompile("(?s)^```(?:yaml|json|yml)?\\s*\n?(.*?)\\s*```$")
	if m := re.FindStringSubmatch(s); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return s
}
