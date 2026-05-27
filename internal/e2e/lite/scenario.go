package lite

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Scenario is the on-disk YAML shape for one lite-proxy scenario.
type Scenario struct {
	ID          string       `yaml:"id"`
	Description string       `yaml:"description"`
	Agent       AgentSpec    `yaml:"agent"`
	// SetupShell runs in the workspace tempdir after the workspace
	// fixture is copied but before the script starts. Use this for
	// shape that can't live in tracked files (e.g. `git init && commit`).
	SetupShell  string       `yaml:"setup_shell,omitempty"`
	Script      []Step       `yaml:"script"`
	Approvals   Approvals    `yaml:"approvals"`
	Budget      Budget       `yaml:"budget"`
	Expects     Expectations `yaml:"expectations"`

	// Dir is the directory this scenario was loaded from (populated by
	// LoadScenario). The workspace lives at filepath.Join(Dir,
	// "workspace") and is copied to t.TempDir() at run start.
	Dir string `yaml:"-"`
}

// AgentSpec carries advisory fields about the agent identity. Tools
// are baked into each CLI driver (Claude Code's Bash/Read/Edit/Write,
// Codex's exec_command/apply_patch, etc.), so we no longer carry a
// tools field here.
type AgentSpec struct {
	Name string `yaml:"name"`
}

// Step is one scripted user turn plus the assertions that gate progress.
type Step struct {
	Say            string     `yaml:"say"`
	Expect         StepExpect `yaml:"expect"`
	ApprovalFloor  int        `yaml:"approval_floor"`
	MaxTurns       int        `yaml:"max_turns,omitempty"`
}

// StepExpect is evaluated by the harness after the agent produces a
// plain-text turn (no tool_use). Failure ends the scenario; no further
// script steps are delivered.
type StepExpect struct {
	FilesAbsent  []string          `yaml:"files_absent"`
	FilesPresent []string          `yaml:"files_present"`
	FileContains []FileContainsExpect `yaml:"file_contains"`
	Shell        []ShellExpect     `yaml:"shell"`
}

// FileContainsExpect asserts that the file at Path contains Needle as
// a substring. List shape (vs. map) so the same file can be asserted
// against multiple needles.
type FileContainsExpect struct {
	Path   string `yaml:"path"`
	Needle string `yaml:"needle"`
}

// ShellExpect runs a shell command in the workspace post-step. The
// harness — not the agent — runs this, so it's ground truth.
type ShellExpect struct {
	Cmd  string `yaml:"cmd"`
	Exit int    `yaml:"exit"`
}

// Approvals describes how the harness resolves task-creation prompts.
type Approvals struct {
	Policy  string         `yaml:"policy"`  // "scripted"
	Default string         `yaml:"default"` // "deny" | "allow_session"
	Rules   []ApprovalRule `yaml:"rules"`
}

// ApprovalRule is matched left-to-right. First match wins.
type ApprovalRule struct {
	Match      ApprovalMatch `yaml:"match"`
	Resolution string        `yaml:"resolution"` // "allow_session" | "deny"
}

// ApprovalMatch is a coarse filter over the approval-prompt fields.
// Today only Kind is read (task_create / task_expand).
type ApprovalMatch struct {
	Kind string `yaml:"kind"`
}

// Budget limits a single scenario run.
type Budget struct {
	MaxTurnsPerStep    int `yaml:"max_turns_per_step"`
	MaxToolCallsTotal  int `yaml:"max_tool_calls_total"`
	WallClockSeconds   int `yaml:"wall_clock_seconds"`
}

// Expectations is the scenario-level rollup checked after all steps run.
type Expectations struct {
	Hard []HardExpect `yaml:"hard"`
	Soft []string     `yaml:"soft"`
}

// HardExpect mirrors scenario/expectations.go's shape so the YAML is
// familiar. Currently only Count is honored.
type HardExpect struct {
	Count *CountExpect `yaml:"count,omitempty"`
}

// CountExpect asserts on one of the harness's named series.
//
// Series:
//   - approvals.allow_session
//   - approvals.deny
//   - lite_proxy.tool_use.block
//   - tool_calls
type CountExpect struct {
	Series string `yaml:"series"`
	GTE    *int   `yaml:"gte,omitempty"`
	LTE    *int   `yaml:"lte,omitempty"`
	EQ     *int   `yaml:"eq,omitempty"`
}

// LoadScenario reads one scenario directory.
func LoadScenario(dir string) (*Scenario, error) {
	path := filepath.Join(dir, "scenario.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.ID == "" {
		return nil, fmt.Errorf("%s: id is required", path)
	}
	if len(s.Script) == 0 {
		return nil, fmt.Errorf("%s: script must contain at least one step", path)
	}
	s.Dir = dir
	return &s, nil
}

// WorkspaceSource is the path that gets copied to a tempdir at run start.
func (s *Scenario) WorkspaceSource() string {
	return filepath.Join(s.Dir, "workspace")
}
