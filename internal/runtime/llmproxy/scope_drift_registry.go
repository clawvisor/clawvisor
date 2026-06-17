package llmproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// ScopeDriftSource records WHY a tool call was blocked. The agent-facing
// menu does not vary by source, but downstream telemetry and any future
// verifier-replay path (see docs/design-scope-drift-continuation.md "Open
// Questions") gate on this.
type ScopeDriftSource string

const (
	// ScopeDriftSourceTaskScope means TaskScopeChecker.Check denied the
	// call because no authorized action covered it.
	ScopeDriftSourceTaskScope ScopeDriftSource = "task_scope"
	// ScopeDriftSourceIntentVerification means TaskScopeChecker.Check
	// passed but the intent verifier returned Allow=false. Reserved —
	// natural-yuzu's intent verifier currently hard-denies and does not
	// mint drifts in v1.
	ScopeDriftSourceIntentVerification ScopeDriftSource = "intent_verification"
)

// ScopeDriftOption identifies which of the three explicit recovery
// options resolved a drift. Empty when no option has been chosen yet
// (the implicit fall-through path never sets this — the drift just
// TTL-expires).
type ScopeDriftOption string

const (
	ScopeDriftOptionExpand  ScopeDriftOption = "expand"
	ScopeDriftOptionNewTask ScopeDriftOption = "new_task"
	ScopeDriftOptionOneOff  ScopeDriftOption = "one_off"
)

// ScopeDriftOutcome records how a chosen option ended. Empty while the
// option is still in flight (e.g. user has not yet approved/denied).
type ScopeDriftOutcome string

const (
	// ScopeDriftOutcomePending — option chosen, waiting for resolution.
	ScopeDriftOutcomePending ScopeDriftOutcome = "pending"
	// ScopeDriftOutcomeSucceeded — chosen option resolved positively:
	// expand/new_task user-approved or one_off user-approved. The drift
	// fingerprint is now pre-cleared and the agent's next attempt of the
	// original tool_use will pass scope+intent verification once.
	ScopeDriftOutcomeSucceeded ScopeDriftOutcome = "succeeded"
	// ScopeDriftOutcomeDenied — terminal denial.
	ScopeDriftOutcomeDenied ScopeDriftOutcome = "denied"
)

// ScopeDrift is the per-block record minted when a tool call drifts out
// of scope. The agent-facing menu carries its ID; expand/new_task
// approval flows and one-off approval reply consume it. Records expire
// after a 10-minute TTL — long enough to absorb human-in-the-loop
// approval turns, short enough that stale drifts can't authorize work
// the agent abandoned.
type ScopeDrift struct {
	ID             string
	UserID         string
	AgentID        string
	ConversationID string
	Provider       conversation.Provider

	ToolUse conversation.ToolUse
	Service string
	Action  string
	Host    string
	Method  string
	Path    string

	TaskID      string
	TaskPurpose string
	ExpectedUse string

	Source     ScopeDriftSource
	ReasonText string

	ChosenOption ScopeDriftOption
	Outcome      ScopeDriftOutcome
	AgentNote    string

	CreatedAt time.Time
	ExpiresAt time.Time
}

// Fingerprint is a stable identifier for the blocked tool call. The
// pre-clear path uses this to recognise the agent's retry of the same
// call after a successful option resolution. The key includes the
// conversation, tool/route shape, AND a hash of the tool_use input so a
// successful drift on one call body cannot bypass scope checks for a
// different request on the same endpoint with different params.
func (d ScopeDrift) Fingerprint() string {
	return scopeDriftFingerprintFromParts(d.AgentID, d.ConversationID, d.Service, d.Action, d.Host, d.Method, d.Path, d.ToolUse.Input)
}

func scopeDriftFingerprintFromParts(agentID, conversationID, service, action, host, method, path string, input []byte) string {
	sum := sha256.Sum256(input)
	inputHash := hex.EncodeToString(sum[:8])
	return strings.Join([]string{agentID, conversationID, service, action, host, method, path, inputHash}, "|")
}

// MenuFields is the subset of drift state the menu prompt renders.
type MenuFields struct {
	DriftID    string
	Service    string
	Action     string
	TaskID     string
	ReasonText string
	Source     ScopeDriftSource
}

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
// on this.
var ErrDriftAlreadyResolved = errors.New("scope drift already resolved")

// ErrDriftNotFound is returned when a drift_id is unknown or has
// expired out of the registry.
var ErrDriftNotFound = errors.New("scope drift not found")

// PendingSubstitutionKey identifies a pending tool_result substitution
// — the next inbound /v1/messages request from this agent on this
// conversation, when it carries a tool_result for this tool_use_id, has
// the menu text spliced in as the result content.
//
// Keyed (agent, conversation, tool_use_id) rather than (drift_id) so the
// inbound rewriter can locate the substitution from the tool_result
// block alone (Anthropic tool_result blocks carry tool_use_id, not the
// drift_id — the drift_id only lives in the rendered menu prose).
type PendingSubstitutionKey struct {
	AgentID        string
	ConversationID string
	ToolUseID      string
}

// ScopeDriftRegistry holds ScopeDrift records for the lifetime of one
// drift (TTL ~10 min by default). Implementations must be safe for
// concurrent use.
type ScopeDriftRegistry interface {
	// Register creates a new drift record, mints an ID, and stores it.
	// The returned record is the freshly stored copy (ID and timestamps
	// populated).
	Register(ctx context.Context, drift ScopeDrift) (ScopeDrift, error)

	// Get returns a copy of the named drift. Returns ErrDriftNotFound
	// for unknown or expired IDs.
	Get(ctx context.Context, driftID string) (ScopeDrift, error)

	// ClaimOption atomically asserts that no option has been chosen yet,
	// then sets ChosenOption + Outcome to the supplied values. Returns
	// ErrDriftAlreadyResolved when the drift already has a chosen option.
	ClaimOption(ctx context.Context, driftID string, option ScopeDriftOption, agentNote string) (ScopeDrift, error)

	// SetOutcome updates the outcome field for an already-claimed drift.
	// On Succeeded a one-shot pre-clear is minted for the drift's
	// (agent, fingerprint).
	SetOutcome(ctx context.Context, driftID string, outcome ScopeDriftOutcome) error

	// LookupPreClear checks whether the given (agent, fingerprint) has a
	// succeeded drift whose pre-clear is still usable. Returns
	// (driftID, true) on hit; ("", false) otherwise. The hit is CONSUMED
	// — a single succeeded option authorizes one re-attempt.
	LookupPreClear(ctx context.Context, agentID, fingerprint string) (string, bool)

	// RegisterPendingSubstitution stores everything the inbound rewriter
	// needs to:
	//   1. Restore the model's original tool_use byte-for-byte in every
	//      future /v1/messages assistant turn that carries the
	//      harness-side placeholder we substituted on the response leg.
	//   2. Replace the harness-supplied tool_result content with the
	//      menu text on the immediate follow-up turn.
	//
	// Conversation-scoped lifetime: substitutions persist much longer
	// than the drift record itself (substitutionTTL, hours). The
	// harness's stored assistant history contains the placeholder for
	// the rest of the conversation; without a long-lived substitution
	// record we'd restore it only once and then the model would see the
	// placeholder forever after.
	RegisterPendingSubstitution(ctx context.Context, key PendingSubstitutionKey, value PendingSubstitution) error

	// LookupPendingSubstitution returns the substitution registered for
	// the key, if any. Does NOT consume the entry — restoration of the
	// assistant turn must work on every future inbound while the
	// substitution is live. Returns (zero, false) on miss.
	LookupPendingSubstitution(ctx context.Context, key PendingSubstitutionKey) (PendingSubstitution, bool)
}

// substitutionTTL is the time-to-live for pending substitution
// records. Sized generously (24h) because the harness's stored history
// keeps the Bash placeholder for the full conversation; the inbound
// rewriter has to be able to restore on every follow-up turn until the
// user closes the session. A bound is still enforced so the in-memory
// registry doesn't grow unbounded across long-running deployments.
const substitutionTTL = 24 * time.Hour

// PendingSubstitution carries every field the inbound rewriter needs
// to splice the model's original tool_use back into the assistant turn
// and replace the harness-supplied tool_result content with the menu.
type PendingSubstitution struct {
	DriftID          string
	MenuText         string
	OriginalToolName string
	OriginalToolInput []byte
}

type pendingSubstitutionEntry struct {
	Substitution PendingSubstitution
	ExpiresAt    time.Time
}

type memoryScopeDriftRegistry struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	drifts  map[string]*ScopeDrift
	cleared map[string]string
	pending map[string]pendingSubstitutionEntry
}

// NewMemoryScopeDriftRegistry returns an in-memory registry with the
// requested TTL. TTL <= 0 falls back to 10 minutes — sized to cover
// mint→claim, claim→outcome (user approval round-trip), and
// outcome→pre-clear-consumption (agent retry) phases without letting
// stale records live forever. See docs/design-scope-drift-continuation.md
// "Decisions" §1.
func NewMemoryScopeDriftRegistry(ttl time.Duration) ScopeDriftRegistry {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &memoryScopeDriftRegistry{
		ttl:     ttl,
		now:     time.Now,
		drifts:  map[string]*ScopeDrift{},
		cleared: map[string]string{},
		pending: map[string]pendingSubstitutionEntry{},
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

func (r *memoryScopeDriftRegistry) ClaimOption(_ context.Context, driftID string, option ScopeDriftOption, agentNote string) (ScopeDrift, error) {
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
		return *d, ErrDriftAlreadyResolved
	}
	d.ChosenOption = option
	d.Outcome = ScopeDriftOutcomePending
	d.AgentNote = agentNote
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
	delete(r.cleared, key)
	return driftID, true
}

func (r *memoryScopeDriftRegistry) RegisterPendingSubstitution(_ context.Context, key PendingSubstitutionKey, value PendingSubstitution) error {
	if r == nil {
		return errors.New("scope drift registry not configured")
	}
	if key.AgentID == "" || key.ToolUseID == "" {
		return errors.New("pending substitution requires agent_id and tool_use_id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	r.pending[pendingSubstitutionStorageKey(key)] = pendingSubstitutionEntry{
		Substitution: value,
		ExpiresAt:    r.now().UTC().Add(substitutionTTL),
	}
	return nil
}

// LookupPendingSubstitution returns the substitution for the key but
// does NOT delete it. The harness's stored assistant history carries
// the Bash placeholder for the rest of the conversation; every
// subsequent inbound /v1/messages has to restore that placeholder back
// to the model's original tool_use so the model never sees its own
// past as a fabricated Bash. The entry expires via substitutionTTL.
func (r *memoryScopeDriftRegistry) LookupPendingSubstitution(_ context.Context, key PendingSubstitutionKey) (PendingSubstitution, bool) {
	if r == nil {
		return PendingSubstitution{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	storage := pendingSubstitutionStorageKey(key)
	entry, ok := r.pending[storage]
	if !ok {
		return PendingSubstitution{}, false
	}
	return entry.Substitution, true
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
	for key, entry := range r.pending {
		if entry.ExpiresAt.IsZero() || entry.ExpiresAt.After(now) {
			continue
		}
		delete(r.pending, key)
	}
}

func preClearKey(agentID, fingerprint string) string {
	return agentID + "|" + fingerprint
}

func pendingSubstitutionStorageKey(key PendingSubstitutionKey) string {
	return key.AgentID + "|" + key.ConversationID + "|" + key.ToolUseID
}

func newDriftID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "drift-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// MarshalJSON makes ScopeDrift safe to embed in audit payloads. The raw
// ToolUse.Input is stripped — the audit trail records request arguments
// separately, and the input bytes can carry the agent's full curl body.
func (d ScopeDrift) MarshalJSON() ([]byte, error) {
	type alias struct {
		ID           string            `json:"id"`
		UserID       string            `json:"user_id,omitempty"`
		AgentID      string            `json:"agent_id,omitempty"`
		Service      string            `json:"service,omitempty"`
		Action       string            `json:"action,omitempty"`
		Host         string            `json:"host,omitempty"`
		Method       string            `json:"method,omitempty"`
		Path         string            `json:"path,omitempty"`
		TaskID       string            `json:"task_id,omitempty"`
		Source       ScopeDriftSource  `json:"source,omitempty"`
		ReasonText   string            `json:"reason,omitempty"`
		ChosenOption ScopeDriftOption  `json:"chosen_option,omitempty"`
		Outcome      ScopeDriftOutcome `json:"outcome,omitempty"`
		AgentNote    string            `json:"agent_note,omitempty"`
		CreatedAt    time.Time         `json:"created_at,omitempty"`
		ExpiresAt    time.Time         `json:"expires_at,omitempty"`
	}
	return json.Marshal(alias{
		ID:           d.ID,
		UserID:       d.UserID,
		AgentID:      d.AgentID,
		Service:      d.Service,
		Action:       d.Action,
		Host:         d.Host,
		Method:       d.Method,
		Path:         d.Path,
		TaskID:       d.TaskID,
		Source:       d.Source,
		ReasonText:   d.ReasonText,
		ChosenOption: d.ChosenOption,
		Outcome:      d.Outcome,
		AgentNote:    d.AgentNote,
		CreatedAt:    d.CreatedAt,
		ExpiresAt:    d.ExpiresAt,
	})
}
