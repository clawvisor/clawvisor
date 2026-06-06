package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// PendingApprovalStage is the per-hold state in the inline-task-approval
// two-step flow. Empty / StageTool is the standard one-step approval
// (existing behavior). The other stages run the two-step flow:
//
//	StageTool ──user types "task"──► StageAwaitingTaskDefinition
//	                                 │
//	                                 model emits POST /api/control/tasks
//	                                 ▼
//	                                 (new hold) StageAwaitingTaskApproval
//	                                 │
//	                                 user types "approve"
//	                                 ▼
//	                                 create task + release both holds
type PendingApprovalStage string

const (
	// StageTool — the original tool_use hold awaiting yes/no/task.
	StageTool PendingApprovalStage = ""
	// StageAwaitingTaskDefinition — user typed "task". The same hold's
	// ToolUse field still points at the ORIGINAL tool. We're waiting for
	// the model to emit a POST /api/control/tasks tool_use that defines the
	// task that should cover this work.
	StageAwaitingTaskDefinition PendingApprovalStage = "awaiting_task_definition"
	// StageAwaitingTaskApproval — model has emitted the task definition.
	// The hold's ToolUse is the task-creation POST itself; AwaitingTaskFor
	// links back to the original tool hold. We're waiting for the user
	// to yes/no.
	StageAwaitingTaskApproval PendingApprovalStage = "awaiting_task_approval"
)

type PendingLiteApproval struct {
	ID       string
	UserID   string
	AgentID  string
	Provider conversation.Provider
	// ConversationID partitions the hold to a single conversation when
	// multiple conversations share a Clawvisor token (Conductor workspaces,
	// sub-agents, multiple Claude Code sessions on the same install). A
	// bare "y" reply in conversation B can no longer release a hold from
	// conversation A. Empty falls back to the pre-conversation-scoping
	// key shape (user + agent + provider), preserving behavior for older
	// clients that don't surface a conversation identifier on the wire.
	ConversationID string
	ToolUse        conversation.ToolUse

	Inspector   inspector.Verdict
	Fingerprint runtimedecision.DecisionFingerprint

	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time

	// Stage controls the two-step inline-task flow. Empty == StageTool
	// preserves legacy behavior so existing callers don't need to set it.
	Stage PendingApprovalStage

	// AwaitingTaskFor is the ID of the original tool-use hold this task
	// definition will cover. Set ONLY on the inner StageAwaitingTaskApproval
	// hold; empty otherwise. The release path uses this to find the
	// upstream bash/tool hold and release-or-deny it in cascade.
	AwaitingTaskFor string

	// TaskDefinition is the parsed body of the POST /api/control/tasks the
	// model emitted at StageAwaitingTaskDefinition. Used both to render the
	// inline approval prompt and to create the task once the user approves.
	// nil at the other stages.
	TaskDefinition *runtimetasks.TaskCreateRequest

	// PrecomputedRisk is the merged LLM+envelope risk assessment computed
	// at hold time (with RecentUserTurns in scope). The release path
	// passes this to CreateInlineApprovedTaskWithAssessment so the
	// persisted task.RiskDetails matches what the user saw in the inline
	// approval prompt — without it the handler runs a fresh assessor call
	// that lacks conversation context and produces a different, more
	// cautious explanation that disagrees with the inline read.
	// nil at stages other than StageAwaitingTaskApproval, and may be nil
	// at that stage if the assessor wasn't configured or returned unknown.
	PrecomputedRisk *taskrisk.RiskAssessment

	// PendingTaskID is the ID of the store.Task row the inline-task
	// intercept landed at status="pending_approval" BEFORE calling
	// Hold. Set only on StageAwaitingTaskApproval holds; empty
	// otherwise. The chat-resolve path (resolveInlineTaskApproval) uses
	// it to call ApproveInlineTask / DenyInlineTask against the
	// already-created Task row rather than creating a fresh task at
	// resolve time. The dashboard Tasks page renders the same row as a
	// pending task while the cache hold awaits the user's reply.
	PendingTaskID string

	// Additional carries the other tool_uses that share this hold when
	// multiple tool_uses in a single upstream response are coalesced into
	// one approval. Empty for the standard single-tool hold path
	// (preserves legacy behavior — the singular ToolUse + Inspector +
	// Fingerprint + Reason describe the only held use). When non-empty,
	// the singular fields describe the FIRST approval-needing use; the
	// slice carries every other use in the turn (which may itself be
	// approval-needing, auto-allow, or auto-rewrite — captured by Kind).
	// One yes/no reply releases or denies the whole batch.
	Additional []HeldToolUse

	// PrimaryIndex is the position of the primary tool_use (the one
	// mapped to the singular ToolUse/Inspector/Fingerprint/Reason
	// fields) in the original turn order. Used by AllHolds() to
	// reconstruct the full slice in turn order — release-time emission
	// must match the order the model produced so that dependent tool
	// call sequences (e.g. Bash then a WebFetch that consumes its
	// output) execute in the right sequence. Zero (the JSON-omitted
	// default) means "primary is the first held use," which is
	// correct for legacy single-tool holds and for coalesced holds
	// whose first held use happens to be the approval trigger.
	PrimaryIndex int `json:",omitempty"`
}

// HeldToolUseKind tags how a tool_use was originally classified at hold
// time. The release path re-evaluates each held use against current state,
// but the original classification is the audit-trail truth and the cue
// for whether the use needs re-rewriting at release.
type HeldToolUseKind string

const (
	// HeldKindApproval is a tool_use that needed user approval. The hold
	// exists because of these uses.
	HeldKindApproval HeldToolUseKind = "approval"
	// HeldKindAllow is a tool_use that would have auto-allowed (e.g.
	// read-only bash, pass-through with no credential trigger) but is
	// held alongside an approval-needing sibling because we hold the
	// whole turn.
	HeldKindAllow HeldToolUseKind = "allow"
	// HeldKindRewrite is a tool_use that would have been credential-
	// rewritten and auto-allowed, held alongside an approval-needing
	// sibling. On release we re-run the rewriter to mint a fresh nonce
	// (the one minted at hold time has long since expired).
	HeldKindRewrite HeldToolUseKind = "rewrite"
	// HeldKindDeny is a tool_use that policy would refuse outright.
	// Not actually held (the coalesce decision treats this as a hard
	// block — see Postprocess). The kind exists for classification
	// completeness so downstream code can pattern-match without a
	// default fallthrough.
	HeldKindDeny HeldToolUseKind = "deny"
)

// HeldToolUse is one tool_use carried by a coalesced PendingLiteApproval.
// Each entry remembers the original classification, the inspector
// verdict, and the decision fingerprint so the release path can replay
// the per-use decision in isolation.
type HeldToolUse struct {
	ToolUse     conversation.ToolUse
	Kind        HeldToolUseKind
	Inspector   inspector.Verdict
	Fingerprint runtimedecision.DecisionFingerprint
	Reason      string
}

// AllHolds returns every held tool_use in original turn order. For the
// standard single-tool hold this is a one-element slice constructed
// from the singular fields. For a coalesced hold the singular fields
// describe one entry and Additional carries the others; PrimaryIndex
// is the original position of the singular entry within the turn, so
// the released call sequence matches what the model produced (e.g. a
// preceding Bash whose stdout a later WebFetch consumes).
func (p PendingLiteApproval) AllHolds() []HeldToolUse {
	primary := HeldToolUse{
		ToolUse:     p.ToolUse,
		Kind:        HeldKindApproval,
		Inspector:   p.Inspector,
		Fingerprint: p.Fingerprint,
		Reason:      p.Reason,
	}
	total := 1 + len(p.Additional)
	idx := p.PrimaryIndex
	if idx < 0 || idx >= total {
		// Defensive: stale entries from before PrimaryIndex existed
		// have idx==0, which is also the natural primary-first
		// fallback. Out-of-range values get clamped to 0 to keep
		// release order deterministic rather than panic on a slice
		// bound.
		idx = 0
	}
	out := make([]HeldToolUse, 0, total)
	addIdx := 0
	for i := 0; i < total; i++ {
		if i == idx {
			out = append(out, primary)
			continue
		}
		out = append(out, p.Additional[addIdx])
		addIdx++
	}
	return out
}

// IsCoalesced reports whether the hold covers more than one tool_use.
// Single-tool holds (Additional empty) keep today's 3-option (yes/no/task)
// prompt; coalesced holds are strictly binary.
func (p PendingLiteApproval) IsCoalesced() bool {
	return len(p.Additional) > 0
}

type ResolveRequest struct {
	UserID     string
	AgentID    string
	Provider   conversation.Provider
	// ConversationID scopes the lookup to the requesting conversation's
	// bucket so bare/no-ID resolves and Drops can't cross conversation
	// boundaries. Empty matches the pre-conversation-scoping bucket so
	// older clients keep working without any wire-level changes.
	ConversationID string
	ApprovalID     string
	// Stage, when non-empty, restricts Peek/Resolve/Drop to holds at
	// the named stage. Used by the inline-task path to target its
	// StageAwaitingTaskApproval hold specifically even when older,
	// unresolved tool-stage holds for the same (user, agent, provider)
	// scope sit ahead of it in the cache. Empty matches any stage,
	// preserving existing behavior for callers that don't need to
	// disambiguate.
	Stage PendingApprovalStage
}

type HoldResult struct {
	Pending PendingLiteApproval
	Evicted *PendingLiteApproval
}

type PendingApprovalCache interface {
	Hold(ctx context.Context, pending PendingLiteApproval) (HoldResult, error)
	Peek(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Resolve(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Drop(ctx context.Context, req ResolveRequest) error
}

type MemoryPendingApprovalCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	pending map[pendingApprovalKey][]PendingLiteApproval
	now     func() time.Time
}

type pendingApprovalKey struct {
	userID         string
	agentID        string
	provider       conversation.Provider
	conversationID string
}

var liteApprovalRandRead = rand.Read

func NewMemoryPendingApprovalCache(ttl time.Duration) *MemoryPendingApprovalCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &MemoryPendingApprovalCache{
		ttl:     ttl,
		max:     10,
		pending: map[pendingApprovalKey][]PendingLiteApproval{},
		now:     time.Now,
	}
}

func (c *MemoryPendingApprovalCache) Hold(_ context.Context, pending PendingLiteApproval) (HoldResult, error) {
	if c == nil {
		return HoldResult{Pending: pending}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[pendingApprovalKey][]PendingLiteApproval{}
	}
	now := c.now().UTC()
	if pending.ID == "" {
		id, err := NewLiteApprovalID()
		if err != nil {
			return HoldResult{}, err
		}
		pending.ID = id
	}
	if pending.CreatedAt.IsZero() {
		pending.CreatedAt = now
	}
	if pending.ExpiresAt.IsZero() {
		pending.ExpiresAt = now.Add(c.ttl)
	}
	key := pending.key()
	var evicted *PendingLiteApproval
	items := c.pruneExpiredLocked(key, now)
	if c.max <= 0 {
		c.max = 10
	}
	for len(items) >= c.max {
		existingCopy := items[0]
		evicted = &existingCopy
		items = items[1:]
	}
	items = append(items, pending)
	c.pending[key] = items
	return HoldResult{Pending: pending, Evicted: evicted}, nil
}

func (c *MemoryPendingApprovalCache) Resolve(_ context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, index, items := c.findLocked(req)
	if pending == nil {
		return nil, nil
	}
	key := resolveRequestKey(req)
	items = append(items[:index], items[index+1:]...)
	if len(items) == 0 {
		delete(c.pending, key)
	} else {
		c.pending[key] = items
	}
	return pending, nil
}

func (c *MemoryPendingApprovalCache) Peek(_ context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, _, _ := c.findLocked(req)
	return pending, nil
}

func (c *MemoryPendingApprovalCache) findLocked(req ResolveRequest) (*PendingLiteApproval, int, []PendingLiteApproval) {
	key := resolveRequestKey(req)
	items := c.pruneExpiredLocked(key, c.now().UTC())
	if len(items) == 0 {
		return nil, -1, items
	}
	// Explicit ApprovalID wins outright — that's the unambiguous form
	// the user typed (e.g. "approve cv-xyz").
	if req.ApprovalID != "" {
		for i, pending := range items {
			if pending.ID != req.ApprovalID {
				continue
			}
			if req.Stage != "" && pending.Stage != req.Stage {
				return nil, -1, items
			}
			return &pending, i, items
		}
		return nil, -1, items
	}
	// Bare reply (no explicit ApprovalID): only the absolute most
	// recent hold qualifies. The user typing "approve" / "deny" /
	// "task" is responding to the harness's LAST rendered prompt —
	// not to anything older. If the newest hold's stage doesn't
	// match a Stage filter the caller passed, the bare reply
	// doesn't apply and we return no match rather than walking
	// past the newest to find an older same-stage hold. Walking
	// would let a stale older same-stage hold steal the user's
	// response away from a newer different-stage prompt the user
	// actually saw last — the opposite of "direct response to the
	// last message."
	idx := len(items) - 1
	pending := items[idx]
	if req.Stage != "" && pending.Stage != req.Stage {
		return nil, -1, items
	}
	return &pending, idx, items
}

func (c *MemoryPendingApprovalCache) Drop(_ context.Context, req ResolveRequest) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := resolveRequestKey(req)
	if req.ApprovalID == "" {
		delete(c.pending, key)
		return nil
	}
	items := c.pending[key]
	for i, pending := range items {
		if pending.ID == req.ApprovalID {
			items = append(items[:i], items[i+1:]...)
			if len(items) == 0 {
				delete(c.pending, key)
			} else {
				c.pending[key] = items
			}
			return nil
		}
	}
	return nil
}

func (p PendingLiteApproval) key() pendingApprovalKey {
	return pendingApprovalKey{
		userID:         p.UserID,
		agentID:        p.AgentID,
		provider:       p.Provider,
		conversationID: p.ConversationID,
	}
}

func resolveRequestKey(req ResolveRequest) pendingApprovalKey {
	return pendingApprovalKey{
		userID:         req.UserID,
		agentID:        req.AgentID,
		provider:       req.Provider,
		conversationID: req.ConversationID,
	}
}

// snapshotHoldsForTest returns the current holds for one
// (user, agent, provider) tuple in insertion order. Test-only — used
// by coalescence tests to assert how many holds were created and what
// they contain without poking the private storage map.
// SetMaxForTest overrides the per-key capacity to force eviction in
// coalescence/eviction tests. Test-only — the cache normally derives
// max from NewMemoryPendingApprovalCacheWithMax.
func (c *MemoryPendingApprovalCache) SetMaxForTest(max int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.max = max
}

// SnapshotHoldsForTest is the test-only entry point for inspecting
// the cache's per-key hold buffer. Exported so the postproc package's
// coalescence tests can assert hold counts + contents without poking
// the private storage map.
func (c *MemoryPendingApprovalCache) SnapshotHoldsForTest(userID, agentID string, provider conversation.Provider) []PendingLiteApproval {
	return c.snapshotHoldsForTest(userID, agentID, provider)
}

func (c *MemoryPendingApprovalCache) snapshotHoldsForTest(userID, agentID string, provider conversation.Provider) []PendingLiteApproval {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := pendingApprovalKey{userID: userID, agentID: agentID, provider: provider}
	items := c.pending[key]
	out := make([]PendingLiteApproval, len(items))
	copy(out, items)
	return out
}

func (c *MemoryPendingApprovalCache) pruneExpiredLocked(key pendingApprovalKey, now time.Time) []PendingLiteApproval {
	items := c.pending[key]
	if len(items) == 0 {
		return nil
	}
	kept := items[:0]
	for _, pending := range items {
		if pending.ExpiresAt.IsZero() || pending.ExpiresAt.After(now) {
			kept = append(kept, pending)
		}
	}
	if len(kept) == 0 {
		delete(c.pending, key)
		return nil
	}
	c.pending[key] = kept
	return kept
}

// NewLiteApprovalID mints a per-hold ID for the inline-approval flow.
// Used by the postproc package's coalesce machinery to mint IDs ahead
// of replay to the underlying cache.
func NewLiteApprovalID() (string, error) {
	var b [16]byte
	if _, err := liteApprovalRandRead(b[:]); err != nil {
		return "", fmt.Errorf("generate approval id: %w", err)
	}
	return "cv-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}
