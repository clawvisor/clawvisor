package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// ScopeDriftSource records WHY a tool call was blocked. The agent-facing
// menu and the /justify endpoint behave the same regardless of source,
// but downstream telemetry and the verifier replay differ — we only
// re-run the verifier when the original block came from it.
type ScopeDriftSource string

const (
	// ScopeDriftSourceTaskScope means TaskScopeChecker.Check denied the
	// call because no authorized action covered it.
	ScopeDriftSourceTaskScope ScopeDriftSource = "task_scope"
	// ScopeDriftSourceIntentVerification means TaskScopeChecker.Check
	// passed but the intent verifier returned Allow=false.
	ScopeDriftSourceIntentVerification ScopeDriftSource = "intent_verification"
)

// ScopeDriftOption identifies which of the four agent-facing options
// resolved a drift. Empty when no option has been chosen yet.
type ScopeDriftOption string

const (
	ScopeDriftOptionExpand  ScopeDriftOption = "expand"     // (a)
	ScopeDriftOptionNewTask ScopeDriftOption = "new_task"   // (b)
	ScopeDriftOptionOneOff  ScopeDriftOption = "one_off"    // (c)
	ScopeDriftOptionJustify ScopeDriftOption = "justify"    // (d)
)

// ScopeDriftOutcome records how a chosen option ended. Empty while the
// option is still in flight (e.g. user has not yet approved/denied).
type ScopeDriftOutcome string

const (
	// ScopeDriftOutcomePending — option chosen, waiting for resolution.
	ScopeDriftOutcomePending ScopeDriftOutcome = "pending"
	// ScopeDriftOutcomeSucceeded — chosen option resolved positively:
	// expand/new_task user-approved, one_off user-approved, or justify
	// verifier-accepted. The drift fingerprint is now pre-cleared and
	// the agent's next attempt of the original tool_use will pass
	// verification.
	ScopeDriftOutcomeSucceeded ScopeDriftOutcome = "succeeded"
	// ScopeDriftOutcomeFellBack — chosen option failed; system created
	// a clean one-off fallback. The drift stays around to track the
	// fallback's resolution.
	ScopeDriftOutcomeFellBack ScopeDriftOutcome = "fell_back_to_oneoff"
	// ScopeDriftOutcomeDenied — terminal denial; no further fallback.
	ScopeDriftOutcomeDenied ScopeDriftOutcome = "denied"
)

// ScopeDrift is the per-block record minted when a tool call drifts out
// of scope. The agent-facing menu carries its ID; the /justify and
// /one-off endpoints consume it. Records expire after a short TTL so a
// stale drift can't authorize work the agent is no longer pursuing.
type ScopeDrift struct {
	ID             string
	UserID         string
	AgentID        string
	ConversationID string
	Provider       conversation.Provider

	// Original blocked tool call.
	ToolUse conversation.ToolUse
	Service string
	Action  string
	Host    string
	Method  string
	Path    string

	// Active task at block time. Empty when no task covered the call.
	TaskID      string
	TaskPurpose string
	ExpectedUse string

	// Why the call was blocked. ReasonText is the human-readable
	// explanation the menu prompt surfaces to the agent.
	Source     ScopeDriftSource
	ReasonText string

	// Lifecycle: which option the agent picked (if any) and how it
	// ended. ChosenOption is set atomically by ClaimOption — the
	// one-shot cap is enforced by refusing to claim a drift that
	// already has a chosen option.
	ChosenOption ScopeDriftOption
	Outcome      ScopeDriftOutcome
	// AgentNote (one-off) or AgentJustification (justify) — captured at
	// claim time so the user prompt and audit trail can surface what
	// the agent said.
	AgentNote          string
	AgentJustification string

	CreatedAt time.Time
	ExpiresAt time.Time
}

// Fingerprint is a stable identifier for the blocked tool call. The
// pre-clear path uses this to recognise the agent's retry of the same
// call after a successful option resolution.
func (d ScopeDrift) Fingerprint() string {
	return strings.Join([]string{d.AgentID, d.Service, d.Action, d.Host, d.Method, d.Path}, "|")
}

// MenuFields is the subset of drift state the menu prompt renders.
// Returned separately so callers don't accidentally mutate the record.
type MenuFields struct {
	DriftID    string
	Service    string
	Action     string
	TaskID     string
	ReasonText string
	Source     ScopeDriftSource
}

// MenuFields returns the rendering inputs for the agent menu prompt.
func (d ScopeDrift) MenuFields() MenuFields {
	return MenuFields{
		DriftID:    d.ID,
		Service:    d.Service,
		Action:     d.Action,
		TaskID:     d.TaskID,
		ReasonText: d.ReasonText,
		Source:     d.Source,
	}
}

// ErrDriftAlreadyResolved is returned when an endpoint tries to claim a
// drift that has already had an option chosen. The one-shot cap relies
// on this — see ScopeDriftRegistry.ClaimOption.
var ErrDriftAlreadyResolved = errors.New("scope drift already resolved")

// ErrDriftNotFound is returned when a drift_id is unknown or has expired
// out of the registry. Callers map this to a 404.
var ErrDriftNotFound = errors.New("scope drift not found")

// ScopeDriftRegistry holds ScopeDrift records for the lifetime of one
// drift (TTL ~60s by default). Implementations must be safe for
// concurrent use; the memory implementation below uses a single mutex
// since registries are short-lived and per-process.
type ScopeDriftRegistry interface {
	// Register creates a new drift record, mints an ID, and stores it.
	// The returned record is the freshly stored copy (ID and timestamps
	// populated). Errors only on internal storage failures.
	Register(ctx context.Context, drift ScopeDrift) (ScopeDrift, error)

	// Get returns a copy of the named drift. Returns ErrDriftNotFound
	// for unknown or expired IDs. Callers must NOT depend on the
	// pointer identity of returned records — the registry stores by
	// value and may copy on read.
	Get(ctx context.Context, driftID string) (ScopeDrift, error)

	// ClaimOption atomically asserts that no option has been chosen
	// yet, then sets ChosenOption + Outcome to the supplied values.
	// Returns ErrDriftAlreadyResolved when the drift already has a
	// chosen option (the one-shot cap). Optional fields (agent_note,
	// justification) are persisted alongside the option for the user
	// prompt and audit trail.
	ClaimOption(ctx context.Context, driftID string, option ScopeDriftOption, agentNote, agentJustification string) (ScopeDrift, error)

	// SetOutcome updates the outcome field for an already-claimed
	// drift. Used by the async resolution paths — e.g. (a) expand's
	// user approval flows through this to flip the drift from
	// "pending" to "succeeded" or "fell_back_to_oneoff".
	SetOutcome(ctx context.Context, driftID string, outcome ScopeDriftOutcome) error

	// LookupPreClear checks whether the given (agent, fingerprint)
	// has a succeeded drift whose pre-clear is still usable. Returns
	// (driftID, true) on hit; ("", false) otherwise. The hit is
	// CONSUMED — a single succeeded option authorizes one re-attempt
	// of the original call, matching today's one-off semantics. This
	// is how postprocess.go re-honours an option (a/b/c/d) outcome
	// when the agent re-emits the original tool_use.
	LookupPreClear(ctx context.Context, agentID, fingerprint string) (string, bool)
}

type memoryScopeDriftRegistry struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	drifts  map[string]*ScopeDrift             // by ID
	cleared map[string]string                  // (agent+fingerprint) → driftID (one-shot pre-clear)
}

// NewMemoryScopeDriftRegistry returns an in-memory registry with the
// requested TTL. TTL <= 0 falls back to 60s — short enough that a stale
// drift can't authorize work the agent abandoned, long enough for a
// realistic user-mediated approval round-trip on options (a)/(b)/(c).
func NewMemoryScopeDriftRegistry(ttl time.Duration) ScopeDriftRegistry {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &memoryScopeDriftRegistry{
		ttl:     ttl,
		now:     time.Now,
		drifts:  map[string]*ScopeDrift{},
		cleared: map[string]string{},
	}
}

func (r *memoryScopeDriftRegistry) Register(_ context.Context, drift ScopeDrift) (ScopeDrift, error) {
	if r == nil {
		return drift, errors.New("scope drift registry not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	now := r.now().UTC()
	if drift.ID == "" {
		id, err := newDriftID()
		if err != nil {
			return ScopeDrift{}, fmt.Errorf("mint drift id: %w", err)
		}
		drift.ID = id
	}
	if drift.CreatedAt.IsZero() {
		drift.CreatedAt = now
	}
	if drift.ExpiresAt.IsZero() {
		drift.ExpiresAt = now.Add(r.ttl)
	}
	stored := drift
	r.drifts[drift.ID] = &stored
	return stored, nil
}

func (r *memoryScopeDriftRegistry) Get(_ context.Context, driftID string) (ScopeDrift, error) {
	if r == nil {
		return ScopeDrift{}, ErrDriftNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	d, ok := r.drifts[driftID]
	if !ok {
		return ScopeDrift{}, ErrDriftNotFound
	}
	return *d, nil
}

func (r *memoryScopeDriftRegistry) ClaimOption(_ context.Context, driftID string, option ScopeDriftOption, agentNote, agentJustification string) (ScopeDrift, error) {
	if r == nil {
		return ScopeDrift{}, ErrDriftNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	d, ok := r.drifts[driftID]
	if !ok {
		return ScopeDrift{}, ErrDriftNotFound
	}
	if d.ChosenOption != "" {
		// One-shot cap. The agent gets exactly one swing at the
		// menu; subsequent /one-off or /justify calls against the
		// same drift_id are rejected at the registry level so the
		// handlers stay simple.
		return *d, ErrDriftAlreadyResolved
	}
	d.ChosenOption = option
	d.Outcome = ScopeDriftOutcomePending
	d.AgentNote = agentNote
	d.AgentJustification = agentJustification
	return *d, nil
}

func (r *memoryScopeDriftRegistry) SetOutcome(_ context.Context, driftID string, outcome ScopeDriftOutcome) error {
	if r == nil {
		return ErrDriftNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	d, ok := r.drifts[driftID]
	if !ok {
		return ErrDriftNotFound
	}
	d.Outcome = outcome
	if outcome == ScopeDriftOutcomeSucceeded {
		// Pre-clear: the next attempt of the same (agent, fingerprint)
		// is allowed past task scope + intent verification once.
		// LookupPreClear consumes the entry. The drift record itself
		// stays in the cache until TTL so /control/scope-drift/{id}
		// status reads stay coherent.
		r.cleared[preClearKey(d.AgentID, d.Fingerprint())] = d.ID
	}
	return nil
}

func (r *memoryScopeDriftRegistry) LookupPreClear(_ context.Context, agentID, fingerprint string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	key := preClearKey(agentID, fingerprint)
	driftID, ok := r.cleared[key]
	if !ok {
		return "", false
	}
	// One-shot: delete on first hit. A second attempt would land back
	// in the scope/verification path and either pass (if the user
	// approved an option that adjusted task scope) or generate a new
	// drift.
	delete(r.cleared, key)
	return driftID, true
}

func (r *memoryScopeDriftRegistry) pruneLocked() {
	now := r.now().UTC()
	for id, d := range r.drifts {
		if d.ExpiresAt.IsZero() || d.ExpiresAt.After(now) {
			continue
		}
		delete(r.drifts, id)
		delete(r.cleared, preClearKey(d.AgentID, d.Fingerprint()))
	}
}

func preClearKey(agentID, fingerprint string) string {
	return agentID + "|" + fingerprint
}

func newDriftID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "drift-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// MarshalJSON makes ScopeDrift safe to embed in audit payloads. We
// strip the raw ToolUse.Input from the marshaled shape because it can
// carry the agent's full curl body (including arguments the audit
// trail already records separately) — keeping the audit payloads
// trim is worth the extra method.
func (d ScopeDrift) MarshalJSON() ([]byte, error) {
	type alias struct {
		ID                 string            `json:"id"`
		UserID             string            `json:"user_id,omitempty"`
		AgentID            string            `json:"agent_id,omitempty"`
		Service            string            `json:"service,omitempty"`
		Action             string            `json:"action,omitempty"`
		Host               string            `json:"host,omitempty"`
		Method             string            `json:"method,omitempty"`
		Path               string            `json:"path,omitempty"`
		TaskID             string            `json:"task_id,omitempty"`
		Source             ScopeDriftSource  `json:"source,omitempty"`
		ReasonText         string            `json:"reason,omitempty"`
		ChosenOption       ScopeDriftOption  `json:"chosen_option,omitempty"`
		Outcome            ScopeDriftOutcome `json:"outcome,omitempty"`
		AgentNote          string            `json:"agent_note,omitempty"`
		AgentJustification string            `json:"agent_justification,omitempty"`
		CreatedAt          time.Time         `json:"created_at,omitempty"`
		ExpiresAt          time.Time         `json:"expires_at,omitempty"`
	}
	return json.Marshal(alias{
		ID:                 d.ID,
		UserID:             d.UserID,
		AgentID:            d.AgentID,
		Service:            d.Service,
		Action:             d.Action,
		Host:               d.Host,
		Method:             d.Method,
		Path:               d.Path,
		TaskID:             d.TaskID,
		Source:             d.Source,
		ReasonText:         d.ReasonText,
		ChosenOption:       d.ChosenOption,
		Outcome:            d.Outcome,
		AgentNote:          d.AgentNote,
		AgentJustification: d.AgentJustification,
		CreatedAt:          d.CreatedAt,
		ExpiresAt:          d.ExpiresAt,
	})
}
