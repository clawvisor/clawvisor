package llmproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/version"
	"github.com/google/uuid"
)

// AuditEmitter wraps store.LogAudit with lite-proxy-shaped helpers. Each
// helper writes one row into audit_log. The shape conforms to the
// existing dashboard surface (Audit.tsx) so lite-proxy events show up
// alongside gateway events without UI changes.
//
// Forensic fields (validator prompt SHA, parser version, clawvisor build
// SHA) are stashed in ParamsSafe so an audit row is self-contained — a
// future incident reconstruction can identify exactly which inspector
// build produced the verdict.
type AuditEmitter struct {
	Store  store.Store
	Logger *slog.Logger

	// ValidatorPromptSHA is recorded on every tool_use audit row so a
	// prompt change is forensically visible. Set by the handler when it
	// knows the active validator's prompt hash.
	ValidatorPromptSHA string
}

// NewAuditEmitter builds an AuditEmitter with sensible defaults. Logger
// nil falls back to slog.Default(); pass an inspector.AnthropicValidator
// (or any type with a PromptSHA() method) to populate forensics.
func NewAuditEmitter(st store.Store, logger *slog.Logger, v interface{ PromptSHA() string }) *AuditEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	e := &AuditEmitter{Store: st, Logger: logger}
	if v != nil {
		e.ValidatorPromptSHA = v.PromptSHA()
	}
	return e
}

// LogEndpointCall records one /v1/* request hitting the lite-proxy LLM
// endpoint. Service is the provider name; Action is the route shape
// ("messages.create", "responses.create", "chat.completions.create").
// outcome is "success" / "error_<status>" / "upstream_key_missing" etc.
func (e *AuditEmitter) LogEndpointCall(ctx context.Context, agent *store.Agent, requestID, provider, action string, statusCode int, decision, outcome, reason string, duration time.Duration, paramsExtra map[string]any) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.endpoint_call",
		"http_status":       statusCode,
		"build_sha":         buildSHA(),
		"validator_prompt":  e.ValidatorPromptSHA,
		"parser_version":    parserVersion(),
		"clawvisor_version": version.Version,
	}
	for k, v := range paramsExtra {
		params[k] = v
	}
	paramsJSON, _ := json.Marshal(params)

	entry := &store.AuditEntry{
		ID:         uuid.NewString(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		Timestamp:  time.Now().UTC(),
		Service:    provider,
		Action:     action,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
		DurationMS: int(duration.Milliseconds()),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil {
		e.Logger.WarnContext(ctx, "lite-proxy: audit log failed",
			"agent_id", agent.ID, "action", action, "err", err.Error())
	}
}

// LogToolUseInspected records one tool_use the inspector touched. Each
// row carries verdict source, decision, target host (when known), the
// tool_use ID, and the placeholder substring (no real credential).
func (e *AuditEmitter) LogToolUseInspected(ctx context.Context, agent *store.Agent, requestID, toolUseID string, verdict inspector.Verdict, decision, outcome, reason string) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.tool_use_inspected",
		"verdict_source":    string(verdict.Source),
		"is_api_call":       verdict.IsAPICall,
		"ambiguous":         verdict.Ambiguous,
		"target_host":       verdict.Host,
		"target_method":     verdict.Method,
		"target_path":       verdict.Path,
		"placeholders":      verdict.Placeholders,
		"build_sha":         buildSHA(),
		"validator_prompt":  e.ValidatorPromptSHA,
		"parser_version":    parserVersion(),
		"clawvisor_version": version.Version,
	}
	if len(verdict.CredentialLocations) > 0 {
		creds := make([]map[string]string, 0, len(verdict.CredentialLocations))
		for _, c := range verdict.CredentialLocations {
			creds = append(creds, map[string]string{
				"kind":   c.Kind,
				"name":   c.Name,
				"scheme": c.Scheme,
			})
		}
		params["credential_locations"] = creds
	}
	paramsJSON, _ := json.Marshal(params)

	service := verdict.Host
	if service == "" {
		service = "lite_proxy"
	}
	tu := toolUseID

	entry := &store.AuditEntry{
		ID:         uuid.NewString(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		ToolUseID:  &tu,
		Timestamp:  time.Now().UTC(),
		Service:    service,
		Action:     "lite_proxy.tool_use." + decision,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil {
		e.Logger.WarnContext(ctx, "lite-proxy: tool_use audit failed",
			"agent_id", agent.ID, "tool_use_id", toolUseID, "err", err.Error())
	}
}

// LogResolverSwap records one credential swap at the resolver. Each row
// links to the placeholder, target host, and upstream status.
func (e *AuditEmitter) LogResolverSwap(ctx context.Context, agent *store.Agent, requestID, placeholder, boundService, targetHost, targetPath, method string, statusCode int, decision, outcome, reason string, duration time.Duration) {
	if e == nil || e.Store == nil || agent == nil {
		return
	}
	params := map[string]any{
		"event":             "lite_proxy.resolver_swap",
		"placeholder":       placeholder,
		"bound_service":     boundService,
		"target_host":       targetHost,
		"target_path":       targetPath,
		"method":            method,
		"http_status":       statusCode,
		"build_sha":         buildSHA(),
		"clawvisor_version": version.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	entry := &store.AuditEntry{
		ID:         uuid.NewString(),
		UserID:     agent.UserID,
		AgentID:    &agent.ID,
		RequestID:  requestID,
		Timestamp:  time.Now().UTC(),
		Service:    boundService,
		Action:     "lite_proxy.resolver." + method,
		ParamsSafe: paramsJSON,
		Decision:   decision,
		Outcome:    outcome,
		Reason:     nilIfEmpty(reason),
		DurationMS: int(duration.Milliseconds()),
	}
	if err := e.Store.LogAudit(ctx, entry); err != nil {
		e.Logger.WarnContext(ctx, "lite-proxy: resolver swap audit failed",
			"agent_id", agent.ID, "target_host", targetHost, "err", err.Error())
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// buildSHA returns the clawvisor build identifier. Stamped at link time
// via -ldflags; falls back to "unknown".
func buildSHA() string {
	return version.Version
}

// parserVersion returns a stable identifier for the deterministic
// parser implementation in this build. Bump when parsing semantics
// change; recorded in audit rows so verdict differences across builds
// are forensically visible.
const parserVersionStr = "lite-proxy-parser/v1"

func parserVersion() string { return parserVersionStr }
