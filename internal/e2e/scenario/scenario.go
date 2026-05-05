// Package scenario describes one e2e mission for the LLM-driven harness.
//
// A scenario is a single YAML file under scenario/library/ that declares the
// goal, persona, fixture (httptest upstream mocks + vault seeds + runtime
// policy rules), the approver decision script, and the expectations the
// harness will evaluate after the run.
package scenario

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Scenario is a fully-parsed mission ready to run.
type Scenario struct {
	ID        string `yaml:"id"`
	Goal      string `yaml:"goal"`
	Persona   string `yaml:"persona"`
	AgentName string `yaml:"agent_name"`

	// Mission declares which slice of the system this scenario is meant
	// to exercise (e.g. "review→approve→retry", "deny on disallowed host",
	// "probabilistic mix of allow_once and allow_session"). It is surfaced
	// in test output and lets a coverage report show which paths a run
	// touched without inferring intent from goal text.
	Mission string   `yaml:"mission"`
	Tags    []string `yaml:"tags"`

	Fixture      Fixture      `yaml:"fixture"`
	Approvals    Approvals    `yaml:"approvals"`
	Budget       Budget       `yaml:"budget"`
	Expectations Expectations `yaml:"expectations"`

	// Path is set by Load to the YAML file the scenario was parsed from.
	// Used to resolve relative fixture paths (rules, upstream JSON).
	Path string `yaml:"-"`
}

// Fixture is the pre-run state the harness installs before the first
// LLM turn.
type Fixture struct {
	// Upstreams describes the httptest mocks the harness should bring up
	// and route the proxy to (instead of the real internet).
	Upstreams []Upstream `yaml:"upstreams"`

	// Rules is a list of runtime policy rules to install. Path-relative
	// references in body/headers shape are not supported in v1.
	Rules []PolicyRule `yaml:"rules"`

	// Vault seeds vault entries for the scenario's agent. Values come
	// from os.Getenv(ValueEnv); empty means a synthetic value is used.
	Vault []VaultSeed `yaml:"vault"`
}

// Upstream is one httptest server the harness will spin up. Host is the
// externally-visible name the agent will dial; FixturePath is JSON the
// mock returns for matching requests.
type Upstream struct {
	Host        string `yaml:"host"`
	FixturePath string `yaml:"fixtures"`
}

// PolicyRule mirrors the runtime policy rule fields the scenario YAML
// can declaratively set. The harness fills in UserID/AgentID/Source at
// fixture-apply time.
type PolicyRule struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"`   // "egress" or "tool_use"
	Action  string `yaml:"action"` // "allow", "deny", "review"
	Service string `yaml:"service,omitempty"`
	Host    string `yaml:"host,omitempty"`
	Method  string `yaml:"method,omitempty"`
	Path    string `yaml:"path,omitempty"`
	Reason  string `yaml:"reason,omitempty"`
}

// VaultSeed names a credential the harness should put in the vault.
type VaultSeed struct {
	Service     string `yaml:"service"`
	KeyName     string `yaml:"key_name"`
	Placeholder string `yaml:"placeholder"`
	ValueEnv    string `yaml:"value_env"`
}

// Approvals describes how the approver role handles pending runtime
// approvals.
type Approvals struct {
	// Policy is "scripted" (deterministic) or "probabilistic" (weighted).
	Policy string         `yaml:"policy"`
	Rules  []ApprovalRule `yaml:"rules"`
	// Seed is the RNG seed for probabilistic mode. 0 = time-based.
	Seed int64 `yaml:"seed"`
	// Default is the resolution applied when no rule matches. Empty means
	// "deny" — fail closed so a missing rule never silently approves.
	Default string `yaml:"default"`
}

// ApprovalRule matches a pending approval and emits a resolution.
type ApprovalRule struct {
	Match      ApprovalMatch      `yaml:"match"`
	Resolution string             `yaml:"resolution"`
	Weights    map[string]float64 `yaml:"weights"`
}

// ApprovalMatch is an AND of optional predicates against the approval's
// summary. Any field left empty matches anything.
type ApprovalMatch struct {
	// Kind matches the ApprovalRecord.Kind ("request_once", "task_create",
	// "task_expand", "credential_review"). Required for task_* kinds since
	// their payload has no Host/Method/Path the runtime fields would key on.
	Kind       string `yaml:"kind"`
	Host       string `yaml:"host"`
	PathPrefix string `yaml:"path_prefix"`
	Method     string `yaml:"method"`
}

// Budget caps a scenario's resource use.
type Budget struct {
	MaxTurns         int `yaml:"max_turns"`
	MaxToolCalls     int `yaml:"max_tool_calls"`
	WallClockSeconds int `yaml:"wall_clock_seconds"`
	MaxLLMTokens     int `yaml:"max_llm_tokens"`
}

// Load reads a scenario YAML file from disk.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario: %w", err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse scenario %s: %w", path, err)
	}
	if s.ID == "" {
		return nil, fmt.Errorf("scenario %s: missing id", path)
	}
	if s.Goal == "" {
		return nil, fmt.Errorf("scenario %s: missing goal", path)
	}
	if s.AgentName == "" {
		s.AgentName = "e2e-" + s.ID
	}
	s.Path = path
	return &s, nil
}
