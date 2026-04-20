// Package policy is the Stage 3 M1 policy engine: YAML schema, parser,
// compiler, and fast-rule matcher. A policy lives per-bridge; the
// Clawvisor server keeps the YAML + compiled form and delivers the
// compiled form to the proxy via /api/proxy/config. The proxy evaluates
// rules on every request; block → 403, flag → send to judge (Stage 3 M5).
//
// See docs/design-proxy-stage3.md §§2, 3, 4.
package policy

import (
	"encoding/json"
	"time"
)

// Action is the rule-matcher verdict. Values are wire-stable.
type Action string

const (
	ActionAllow Action = "allow"
	ActionBlock Action = "block"
	ActionFlag  Action = "flag"
)

// YAMLPolicy is the source-of-truth form authored by a user. Compiled
// into CompiledPolicy at load time.
type YAMLPolicy struct {
	Version  int        `yaml:"version"`
	Name     string     `yaml:"name"`
	BridgeID string     `yaml:"bridge_id,omitempty"`
	Rules    YAMLRules  `yaml:"rules"`
	Ban      YAMLBan    `yaml:"ban"`
}

type YAMLRules struct {
	Fast    []YAMLFastRule `yaml:"fast"`
	Judge   YAMLJudge      `yaml:"judge"`
	Default string         `yaml:"default"` // allow | block | flag
}

type YAMLFastRule struct {
	Name    string    `yaml:"name"`
	Action  string    `yaml:"action"` // allow | block | flag
	Match   YAMLMatch `yaml:"match"`
	Message string    `yaml:"message,omitempty"`
}

type YAMLMatch struct {
	Hosts   []string          `yaml:"hosts,omitempty"`
	Methods []string          `yaml:"methods,omitempty"`
	Paths   []string          `yaml:"paths,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Query   map[string]string `yaml:"query,omitempty"`
	// Agents restricts matching to a subset of agent_token_ids. Empty
	// means the rule matches any agent of the bridge.
	Agents []string `yaml:"agents,omitempty"`
}

type YAMLJudge struct {
	Enabled   bool   `yaml:"enabled"`
	Model     string `yaml:"model,omitempty"`
	TimeoutMs int    `yaml:"timeout_ms,omitempty"`
	// OnError is one of allow, block, or "fallback_rule:<name>".
	OnError string `yaml:"on_error,omitempty"`
}

type YAMLBan struct {
	Enabled       bool   `yaml:"enabled"`
	MaxViolations int    `yaml:"max_violations,omitempty"`
	Window        string `yaml:"window,omitempty"`       // "1h", "15m", "24h"
	BanDuration   string `yaml:"ban_duration,omitempty"`
	Scope         string `yaml:"scope,omitempty"`        // "per_rule" (default) | "per_bridge"
}

// CompiledPolicy is the wire + runtime form. Duplicated field-for-field
// between the server (this package) and the proxy's clawvisor/types.go
// so the two repos share only the wire contract, not Go types.
type CompiledPolicy struct {
	BridgeID      string         `json:"bridge_id"`
	Name          string         `json:"name"`
	Version       int            `json:"version"`
	Rules         []CompiledRule `json:"rules"`
	Judge         CompiledJudge  `json:"judge"`
	DefaultAction Action         `json:"default_action"`
	Ban           CompiledBan    `json:"ban"`
}

type CompiledRule struct {
	Name    string   `json:"name"`
	Action  Action   `json:"action"`
	Hosts   []string `json:"hosts,omitempty"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	// Headers is flattened key→value; match is case-insensitive on key,
	// exact on value (empty value means "header present").
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	Agents  []string          `json:"agents,omitempty"`
	Message string            `json:"message,omitempty"`
}

type CompiledJudge struct {
	Enabled bool   `json:"enabled"`
	Model   string `json:"model,omitempty"`
	// Nanosecond resolution would be nice but JSON + cross-lang makes
	// milliseconds the pragmatic wire unit.
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	OnError   string `json:"on_error,omitempty"`
}

type CompiledBan struct {
	Enabled         bool          `json:"enabled"`
	MaxViolations   int           `json:"max_violations,omitempty"`
	WindowSeconds   int           `json:"window_seconds,omitempty"`
	DurationSeconds int           `json:"duration_seconds,omitempty"`
	Scope           string        `json:"scope,omitempty"`
	// Internal-only: parsed durations (not emitted to wire).
	Window   time.Duration `json:"-"`
	Duration time.Duration `json:"-"`
}

// Marshal a compiled policy to JSON suitable for storage and wire
// transport. Window/Duration are re-derived from the *Seconds fields on
// unmarshal, so round-trips are lossless.
func (c *CompiledPolicy) MarshalJSON() ([]byte, error) {
	// Encode via intermediate alias to force the computed *Seconds fields.
	type alias CompiledPolicy
	// Ensure seconds fields reflect the time.Duration values at emit time.
	if c.Ban.Window > 0 {
		c.Ban.WindowSeconds = int(c.Ban.Window / time.Second)
	}
	if c.Ban.Duration > 0 {
		c.Ban.DurationSeconds = int(c.Ban.Duration / time.Second)
	}
	return json.Marshal((*alias)(c))
}

// UnmarshalJSON rehydrates durations from the *Seconds fields.
func (c *CompiledPolicy) UnmarshalJSON(b []byte) error {
	type alias CompiledPolicy
	if err := json.Unmarshal(b, (*alias)(c)); err != nil {
		return err
	}
	if c.Ban.WindowSeconds > 0 {
		c.Ban.Window = time.Duration(c.Ban.WindowSeconds) * time.Second
	}
	if c.Ban.DurationSeconds > 0 {
		c.Ban.Duration = time.Duration(c.Ban.DurationSeconds) * time.Second
	}
	return nil
}
