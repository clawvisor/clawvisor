package lite

import (
	"context"
	"strings"
	"sync"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// recordingInlineCreator wraps the production TasksHandler so the
// harness can observe the shape of each `required_credentials` entry the
// agent submits — without otherwise changing the validation, store, or
// audit behavior of inline task creation. The wrapper also captures the
// minted credential placeholders returned in approved tasks so the mock
// upstream can detect whether the agent used a Clawvisor-minted value
// (good) or fabricated its own (the failure mode this harness targets).
//
// Forwards all three lite-proxy inline-task interfaces so the
// type-asserts in inline_task_intercept.go continue to find them and the
// production code paths run unchanged.
type recordingInlineCreator struct {
	inner        llmproxy.InlineTaskCreator
	counters     *Counters
	knownVaultIDs map[string]struct{}

	mu                  sync.Mutex
	mintedPlaceholders  []string
}

// newRecordingInlineCreator builds the wrapper. knownVaultIDs is the
// set of vault item IDs planted by the scenario — used to decide
// whether a bare service id (e.g. `github`) is unscoped relative to
// what's available (`github:personal`). When the set is empty every
// non-`autovault_` handle classifies as scoped, which matches scenarios
// that don't plant any items.
func newRecordingInlineCreator(inner llmproxy.InlineTaskCreator, counters *Counters, knownVaultIDs []string) *recordingInlineCreator {
	known := make(map[string]struct{}, len(knownVaultIDs))
	for _, id := range knownVaultIDs {
		known[id] = struct{}{}
	}
	return &recordingInlineCreator{
		inner:         inner,
		counters:      counters,
		knownVaultIDs: known,
	}
}

// PlaceholderSnapshot returns the list of placeholder strings minted
// across every successful inline-approval seen by the wrapper. Safe to
// call concurrently with new approvals — returns a defensive copy.
func (r *recordingInlineCreator) PlaceholderSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.mintedPlaceholders))
	copy(out, r.mintedPlaceholders)
	return out
}

func (r *recordingInlineCreator) recordRequiredCredentials(creds []runtimetasks.RequiredCredential) {
	for _, cred := range creds {
		r.counters.Inc(SeriesCredentialClassify(classifyCredential(cred, r.knownVaultIDs)))
	}
}

func (r *recordingInlineCreator) recordApproved(task *llmproxy.InlineApprovedTask) {
	if task == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(task.Lifetime)) {
	case "standing":
		r.counters.Inc(SeriesLifetimeStanding)
	case "sliding":
		r.counters.Inc(SeriesLifetimeSliding)
	case "session", "":
		r.counters.Inc(SeriesLifetimeSession)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range task.Credentials {
		if strings.TrimSpace(p.Placeholder) == "" {
			continue
		}
		r.mintedPlaceholders = append(r.mintedPlaceholders, p.Placeholder)
	}
}

func (r *recordingInlineCreator) CreateInlineApprovedTask(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string) (*llmproxy.InlineApprovedTask, error) {
	if req != nil {
		r.recordRequiredCredentials(req.RequiredCredentials)
	}
	out, err := r.inner.CreateInlineApprovedTask(ctx, agent, req, originalToolUseID)
	if err == nil {
		r.recordApproved(out)
	}
	return out, err
}

func (r *recordingInlineCreator) CreateInlineApprovedTaskWithAssessment(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string, precomputed *taskrisk.RiskAssessment) (*llmproxy.InlineApprovedTask, error) {
	if req != nil {
		r.recordRequiredCredentials(req.RequiredCredentials)
	}
	if wa, ok := r.inner.(llmproxy.InlineTaskCreatorWithAssessment); ok {
		out, err := wa.CreateInlineApprovedTaskWithAssessment(ctx, agent, req, originalToolUseID, precomputed)
		if err == nil {
			r.recordApproved(out)
		}
		return out, err
	}
	// Fall back to the plain path if the inner creator doesn't
	// implement the assessment-aware extension.
	out, err := r.inner.CreateInlineApprovedTask(ctx, agent, req, originalToolUseID)
	if err == nil {
		r.recordApproved(out)
	}
	return out, err
}

func (r *recordingInlineCreator) CreatePendingInlineTask(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string, precomputed *taskrisk.RiskAssessment) (string, error) {
	if req != nil {
		r.recordRequiredCredentials(req.RequiredCredentials)
	}
	pc, ok := r.inner.(llmproxy.InlineTaskPendingCreator)
	if !ok {
		return "", errPendingCreatorNotWired
	}
	return pc.CreatePendingInlineTask(ctx, agent, req, originalToolUseID, precomputed)
}

func (r *recordingInlineCreator) ApproveInlineTask(ctx context.Context, taskID, userID string) (*llmproxy.InlineApprovedTask, error) {
	pc, ok := r.inner.(llmproxy.InlineTaskPendingCreator)
	if !ok {
		return nil, errPendingCreatorNotWired
	}
	out, err := pc.ApproveInlineTask(ctx, taskID, userID)
	if err == nil {
		r.recordApproved(out)
	}
	return out, err
}

func (r *recordingInlineCreator) DenyInlineTask(ctx context.Context, taskID, userID string) error {
	pc, ok := r.inner.(llmproxy.InlineTaskPendingCreator)
	if !ok {
		return errPendingCreatorNotWired
	}
	return pc.DenyInlineTask(ctx, taskID, userID)
}

func (r *recordingInlineCreator) ExpireInlineTask(ctx context.Context, taskID, userID string) error {
	pc, ok := r.inner.(llmproxy.InlineTaskPendingCreator)
	if !ok {
		return errPendingCreatorNotWired
	}
	return pc.ExpireInlineTask(ctx, taskID, userID)
}

// credentialClass is the bucket a single required_credentials entry
// lands in. Used as a counter-series suffix.
type credentialClass int

const (
	credentialClassScoped credentialClass = iota
	credentialClassUnscoped
	credentialClassFabricatedAutovault
)

// classifyCredential decides the bucket for one entry. Order matters:
// an `autovault_` prefix is always fabricated (no real vault id starts
// with that string). Otherwise, an exact match against a planted id is
// scoped; a bare prefix (no `:` or `.` account separator) is unscoped
// when the planted set has account-aliased ids sharing the prefix; and
// any remaining shape is treated as scoped (e.g. scenarios that don't
// plant items at all).
func classifyCredential(cred runtimetasks.RequiredCredential, known map[string]struct{}) credentialClass {
	id := strings.TrimSpace(cred.VaultItemID)
	if id == "" {
		id = strings.TrimSpace(cred.VaultItemHandle)
	}
	if strings.HasPrefix(id, "autovault_") {
		return credentialClassFabricatedAutovault
	}
	if _, ok := known[id]; ok {
		return credentialClassScoped
	}
	if !strings.ContainsAny(id, ":.") && hasKnownPrefix(known, id) {
		return credentialClassUnscoped
	}
	return credentialClassScoped
}

func hasKnownPrefix(known map[string]struct{}, bare string) bool {
	if bare == "" {
		return false
	}
	for id := range known {
		if strings.HasPrefix(id, bare+":") || strings.HasPrefix(id, bare+".") {
			return true
		}
	}
	return false
}

// SeriesCredentialClassify maps a classification bucket to its counter
// series name. Centralized so scenarios and the harness agree on the
// exact strings.
func SeriesCredentialClassify(c credentialClass) string {
	switch c {
	case credentialClassFabricatedAutovault:
		return SeriesCredentialFabricatedAutovault
	case credentialClassUnscoped:
		return SeriesCredentialUnscoped
	default:
		return SeriesCredentialScoped
	}
}

const (
	SeriesCredentialFabricatedAutovault = "task_creates.credential_fabricated_autovault"
	SeriesCredentialUnscoped            = "task_creates.credential_unscoped"
	SeriesCredentialScoped              = "task_creates.credential_scoped"
	SeriesDownstreamCallsTotal          = "downstream.calls_total"
	SeriesDownstreamPlaceholderUsed     = "downstream.placeholder_used"
	SeriesVaultItemsListed              = "control.vault_items_listed"
	SeriesLifetimeStanding              = "task_creates.lifetime_standing"
	SeriesLifetimeSliding               = "task_creates.lifetime_sliding"
	SeriesLifetimeSession               = "task_creates.lifetime_session"
)

// errPendingCreatorNotWired is returned by the pending-creator pass-through
// methods when the wrapped inner creator does not also implement
// InlineTaskPendingCreator. In the harness this never fires (we always
// wrap a real *TasksHandler), but the assertions keep the wrapper
// honest if someone swaps the inner for a stub.
var errPendingCreatorNotWired = pendingCreatorNotWiredError{}

type pendingCreatorNotWiredError struct{}

func (pendingCreatorNotWiredError) Error() string {
	return "lite harness: inner InlineTaskCreator does not implement InlineTaskPendingCreator"
}
