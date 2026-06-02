package lite

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Scenario is the on-disk YAML shape for one lite-proxy scenario.
type Scenario struct {
	ID          string    `yaml:"id"`
	Description string    `yaml:"description"`
	Agent       AgentSpec `yaml:"agent"`
	// SetupShell runs in the workspace tempdir after the workspace
	// fixture is copied but before the script starts. Use this for
	// shape that can't live in tracked files (e.g. `git init && commit`).
	SetupShell string       `yaml:"setup_shell,omitempty"`
	VaultItems []VaultItem  `yaml:"vault_items,omitempty"`
	// ServiceCatalog opts the scenario into a populated lite-proxy
	// service catalog. Values are built-in service ids the harness
	// knows about (see internal/e2e/lite/service_catalog.go); each
	// listed id adds a minimal yamldef.ServiceDef so the proxy can
	// reverse-resolve credentialed (host, method, path) to
	// (service, action). Unset (or empty) leaves the catalog
	// uninitialized — the scope-drift menu won't fire and behavior
	// matches the harness's pre-catalog state.
	ServiceCatalog []string     `yaml:"service_catalog,omitempty"`
	Script         []Step       `yaml:"script"`
	Approvals      Approvals    `yaml:"approvals"`
	Budget         Budget       `yaml:"budget"`
	Expects        Expectations `yaml:"expectations"`

	// Dir is the directory this scenario was loaded from (populated by
	// LoadScenario). The workspace lives at filepath.Join(Dir,
	// "workspace") and is copied to t.TempDir() at run start.
	Dir string `yaml:"-"`
}

// VaultItem is a non-LLM credential planted in the harness vault before
// the scenario runs. ID is the public vault item id the agent should
// declare in required_credentials (e.g. "github:personal"); Secret is
// the raw value Clawvisor would substitute behind the placeholder. The
// scenario YAML keeps Secret as plain text — these are throwaway fakes,
// not real tokens.
type VaultItem struct {
	ID     string `yaml:"id"`
	Secret string `yaml:"secret"`
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
//   - task_creates.credential_fabricated_autovault — agent declared a
//     `required_credentials` entry whose vault_item_id (or handle) starts
//     with `autovault_` (a placeholder it invented from prior context
//     rather than a real vault item id).
//   - task_creates.credential_unscoped — agent declared a bare service
//     id (e.g. `github`) that doesn't match a planted vault item.
//   - task_creates.credential_scoped — agent declared an id that matches
//     a planted vault item exactly.
//   - downstream.calls_total — total calls received by the harness's
//     mock upstream server.
//   - downstream.placeholder_used — calls whose headers contained one of
//     the placeholders minted by an inline-approved task; proves the
//     agent used the placeholder Clawvisor returned rather than a
//     fabricated string.
//   - control.vault_items_listed — number of times the agent fetched
//     GET /api/control/vault/items. Proves the agent discovered the
//     available vault items via the control plane rather than guessing
//     handle shapes.
//   - task_creates.lifetime_standing — approved tasks whose lifetime
//     came back as `standing` (no expiry, reusable across follow-ups).
//   - task_creates.lifetime_session — approved tasks whose lifetime
//     was `session` (or empty, which defaults to session).
//   - scope_drift.minted — tool_uses blocked by task scope or intent
//     verification that produced a scope-drift menu. Each block mints
//     one record. A non-zero count proves the menu fired in this run.
//   - scope_drift.option_chosen.one_off — agent emitted a
//     <clawvisor:decision option="one-off"> markup block whose drift
//     claim succeeded.
//   - scope_drift.option_chosen.justify — agent emitted a
//     <clawvisor:decision option="justify"> markup block whose drift
//     claim succeeded.
//   - scope_drift.outcome.succeeded — drift resolved positively. For
//     (c) this means user-approved the one-off; for (d) this means
//     the verifier accepted the agent's justification. Triggers
//     pre-clear insertion.
//   - scope_drift.outcome.denied — drift resolved negatively. User
//     denied the one-off or verifier rejected the justification.
//   - scope_drift.outcome.fell_back — drift transitioned to
//     fell-back-to-oneoff (verifier outage on the second pass). A
//     healthy production run should never see this.
//   - scope_drift.pre_clear_consumed — the agent's retry of the
//     original blocked tool_use consumed the one-shot pre-clear and
//     passed scope+verification. A drift outcome of succeeded that
//     produces no pre-clear consumption usually means the agent
//     never retried (gave up or pivoted).
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
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Strict decoding: a typo in the scenario YAML (e.g. `aproval_floor`
	// instead of `approval_floor`) silently used a zero value before
	// this switch and produced scenarios that looked correct but
	// ignored the misspelled field. Fail fast instead.
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
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
