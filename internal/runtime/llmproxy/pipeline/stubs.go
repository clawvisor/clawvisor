package pipeline

import "encoding/json"

// PanicMutator is the fail-fast base for partially implemented mutators.
// Concrete mutators embed it and override the operations they support;
// any unexpected operation panics instead of being silently ignored.
//
// Why panic and not no-op: a no-op mutator would silently swallow policy
// intent during a half-migration, producing wrong behavior that's hard
// to trace. A panic surfaces the missing wiring at the call site.

// PanicMutator is the placeholder used wherever a RequestMutator,
// ResponseMutator, or ToolUseMutator is required. It implements all
// three interfaces with panicking methods.
type PanicMutator struct{}

const panicMessage = "pipeline mutator method is not implemented for this path"

// --- RequestMutator ---------------------------------------------------

func (PanicMutator) ReplaceBody([]byte) error                                   { panic(panicMessage) }
func (PanicMutator) InjectSystemNotice(string) error                            { panic(panicMessage) }
func (PanicMutator) PrependUserTurn(string) error                               { panic(panicMessage) }
func (PanicMutator) RewriteHistoricalToolUseArgs(string, json.RawMessage) error { panic(panicMessage) }
func (PanicMutator) StripTurns(func(StripContext) bool) error                   { panic(panicMessage) }
func (PanicMutator) RewriteMostRecentUserText(string) error                     { panic(panicMessage) }
func (PanicMutator) RedactSpans([]ByteSpan) error                               { panic(panicMessage) }

// --- ResponseMutator --------------------------------------------------

func (PanicMutator) PrependAssistantText(string) error     { panic(panicMessage) }
func (PanicMutator) SubstituteEntireResponse(string) error { panic(panicMessage) }
func (PanicMutator) Commit() error                         { panic(panicMessage) }

// --- ToolUseMutator ---------------------------------------------------

func (PanicMutator) RewriteArgs(json.RawMessage) error { panic(panicMessage) }
func (PanicMutator) ReplaceWithText(string) error      { panic(panicMessage) }

// Compile-time assertions that PanicMutator satisfies all three mutator
// interfaces. These break the build if an interface drifts without the
// stub being updated to match.
var (
	_ RequestMutator  = PanicMutator{}
	_ ResponseMutator = PanicMutator{}
	_ ToolUseMutator  = PanicMutator{}
)
