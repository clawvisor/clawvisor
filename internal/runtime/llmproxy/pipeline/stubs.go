package pipeline

import "encoding/json"

// Phase 1 deliverable: stub mutator implementations. These panic on every
// call. The package compiles and downstream consumers can write code
// against the interface, but no caller actually wires through pipeline
// until Phase 2 ports the first operation (PrependAssistantText via the
// event stream).
//
// Why panic and not no-op: a no-op mutator would silently swallow policy
// intent during a half-migration, producing wrong behavior that's hard
// to trace. A panic surfaces the missing wiring at the call site.

// PanicMutator is the placeholder used wherever a RequestMutator,
// ResponseMutator, or ToolUseMutator is required. It implements all
// three interfaces with panicking methods.
type PanicMutator struct{}

const panicMessage = "pipeline mutator called before Phase 2 wiring landed — see .context/llmproxy-refactor-plan.md"

// --- RequestMutator ---------------------------------------------------

func (PanicMutator) InjectSystemNotice(string) error                                    { panic(panicMessage) }
func (PanicMutator) PrependUserTurn(string) error                                       { panic(panicMessage) }
func (PanicMutator) RewriteHistoricalToolUseArgs(string, json.RawMessage) error         { panic(panicMessage) }
func (PanicMutator) StripTurns(func(StripContext) bool) error                           { panic(panicMessage) }
func (PanicMutator) RewriteMostRecentUserText(string) error                             { panic(panicMessage) }
func (PanicMutator) RedactSpans([]ByteSpan) error                                       { panic(panicMessage) }
func (PanicMutator) AppendContinuationTurn(SyntheticContinuation) error                 { panic(panicMessage) }

// --- ResponseMutator --------------------------------------------------

func (PanicMutator) PrependAssistantText(string) error      { panic(panicMessage) }
func (PanicMutator) SubstituteEntireResponse(string) error  { panic(panicMessage) }

// --- ToolUseMutator ---------------------------------------------------

func (PanicMutator) RewriteArgs(json.RawMessage) error { panic(panicMessage) }
func (PanicMutator) ReplaceWithText(string) error      { panic(panicMessage) }

// Compile-time assertions that PanicMutator satisfies all three mutator
// interfaces. These break the build if a Phase 1 interface drifts without
// the stub being updated to match — surfacing the impedance mismatch
// before any migration has to discover it the hard way.
var (
	_ RequestMutator  = PanicMutator{}
	_ ResponseMutator = PanicMutator{}
	_ ToolUseMutator  = PanicMutator{}
)
