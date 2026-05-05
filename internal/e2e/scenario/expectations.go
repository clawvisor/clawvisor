package scenario

// Expectations is the assertion DSL the harness evaluates after a run.
//
// Hard expectations are checked programmatically against runtime_events,
// approval records, and upstream-hit counters. Soft expectations are evaluated
// by the LLM judge in a rubric pass over the transcript.
type Expectations struct {
	Hard []HardExpect `yaml:"hard"`
	Soft []string     `yaml:"soft"`
}

// HardExpect is one programmatic assertion. Exactly one of the
// expectation fields should be populated. The evaluator switches on
// which field is non-zero.
type HardExpect struct {
	// Count: assert that the named series satisfies a numeric op.
	// Series names: events.total, events.allow, events.deny,
	// events.review, approvals.resolved, approvals.allow_once,
	// approvals.allow_session, approvals.allow_always, approvals.deny,
	// approvals.pending, tool_calls, upstream.<host>.hits.
	Count *CountExpect `yaml:"count,omitempty"`

	// EventHas: assert at least one runtime_event matches the
	// predicate.
	EventHas *EventExpect `yaml:"event_has,omitempty"`

	// FinalAssistantContains: substring (case-insensitive) check on the
	// responder's last text message.
	FinalAssistantContains string `yaml:"final_assistant_contains,omitempty"`
}

// CountExpect is a numeric assertion against a named series.
type CountExpect struct {
	Series string `yaml:"series"`
	GTE    *int   `yaml:"gte,omitempty"`
	LTE    *int   `yaml:"lte,omitempty"`
	EQ     *int   `yaml:"eq,omitempty"`
}

// EventExpect matches at least one runtime_event by event_type, decision,
// and/or outcome. Empty fields are wildcards.
type EventExpect struct {
	EventType string `yaml:"event_type"`
	Decision  string `yaml:"decision"`
	Outcome   string `yaml:"outcome"`
}
