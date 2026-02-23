package policy

import "time"

// ── YAML / API-facing types ──────────────────────────────────────────────────

// Policy is the top-level structure stored as YAML in the database.
type Policy struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Role        string `yaml:"role" json:"role"` // empty = global (all agents)
	Rules       []Rule `yaml:"rules" json:"rules"`
}

type Rule struct {
	Service         string           `yaml:"service" json:"service"`
	Actions         []string         `yaml:"actions" json:"actions"`
	Allow           *bool            `yaml:"allow" json:"allow"`
	RequireApproval bool             `yaml:"require_approval" json:"require_approval"`
	Condition       *Condition       `yaml:"condition" json:"condition"`
	TimeWindow      *TimeWindow      `yaml:"time_window" json:"time_window"`
	Reason          string           `yaml:"reason" json:"reason"`
	ResponseFilters []ResponseFilter `yaml:"response_filters" json:"response_filters"`
}

type ResponseFilter struct {
	// Exactly one filter type is set per entry.
	Redact        string `yaml:"redact,omitempty" json:"redact,omitempty"`
	RedactRegex   string `yaml:"redact_regex,omitempty" json:"redact_regex,omitempty"`
	RemoveField   string `yaml:"remove_field,omitempty" json:"remove_field,omitempty"`
	TruncateField string `yaml:"truncate_field,omitempty" json:"truncate_field,omitempty"`
	MaxChars      int    `yaml:"max_chars,omitempty" json:"max_chars,omitempty"`
	Semantic      string `yaml:"semantic,omitempty" json:"semantic,omitempty"`
}

func (f ResponseFilter) IsStructural() bool { return f.Semantic == "" }
func (f ResponseFilter) IsSemantic() bool   { return f.Semantic != "" }

type Condition struct {
	Type    string `yaml:"type" json:"type"`
	// Shared across param_matches, param_not_contains, max_results_under
	Param   string `yaml:"param,omitempty" json:"param,omitempty"`
	// param_matches
	Pattern string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	// param_not_contains
	Value   string `yaml:"value,omitempty" json:"value,omitempty"`
	// max_results_under
	Max     int    `yaml:"max,omitempty" json:"max,omitempty"`
}

type TimeWindow struct {
	Days     []string `yaml:"days" json:"days"`
	Hours    string   `yaml:"hours" json:"hours"`       // "08:00-22:00"
	Timezone string   `yaml:"timezone" json:"timezone"` // IANA tz name
}

// ── Compiled / internal types ─────────────────────────────────────────────────

// CompiledRule is a pre-processed rule ready for fast evaluation.
type CompiledRule struct {
	ID              string           // "{policyID}:rule-{index}"
	PolicyID        string
	UserID          string
	RoleID          string // empty = global
	Service         string
	Actions         []string
	Decision        Decision
	Condition       *Condition
	TimeWindow      *TimeWindow
	Reason          string
	ResponseFilters []ResponseFilter
	Priority        int
}

// Decision is the outcome of evaluating a request against policy rules.
type Decision string

const (
	DecisionExecute Decision = "execute"
	DecisionApprove Decision = "approve"
	DecisionBlock   Decision = "block"
)

// PolicyDecision is returned by Evaluate.
type PolicyDecision struct {
	Decision        Decision         `json:"decision"`
	PolicyID        string           `json:"policy_id,omitempty"`
	RuleID          string           `json:"rule_id,omitempty"`
	Reason          string           `json:"reason,omitempty"`
	ResponseFilters []ResponseFilter `json:"response_filters,omitempty"`
}

// EvalRequest is passed to Evaluate describing the agent's requested action.
type EvalRequest struct {
	Service            string
	Action             string
	Params             map[string]any
	AgentRoleID        string          // empty if agent has no role
	ResolvedConditions map[string]bool // pre-resolved async conditions keyed by condition type
	// Time is injected by the evaluator (time.Now()) for time_window checks.
}

// ── Validation types ─────────────────────────────────────────────────────────

type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// ── Conflict types ───────────────────────────────────────────────────────────

type Conflict struct {
	RuleA   string `json:"rule_a"`
	RuleB   string `json:"rule_b"`
	Type    string `json:"type"`    // "opposing_decisions" | "shadowed_rule"
	Message string `json:"message"`
}

// ── Day constants ─────────────────────────────────────────────────────────────

var dayMap = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}
