package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// fakeLifecycleStore is a minimal store stub that implements just
// the two TaskLifecycleEvent methods recovery uses. Other Store
// methods panic — tests targeting recovery should never touch them.
type fakeLifecycleStore struct {
	store.Store                                            // embed so unused methods compile; nil receiver panics if called
	byApproval     map[string]*store.TaskLifecycleEvent    // most-recent event per approval (Get lookups)
	byApprovalList map[string][]*store.TaskLifecycleEvent  // all events per approval (List lookups)
	listErr        error
	getErr         error
}

func (f *fakeLifecycleStore) GetTaskLifecycleEventByApprovalID(_ context.Context, approvalID string) (*store.TaskLifecycleEvent, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	ev, ok := f.byApproval[approvalID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return ev, nil
}

func (f *fakeLifecycleStore) ListTaskLifecycleEventsByApprovalID(_ context.Context, approvalID string) ([]*store.TaskLifecycleEvent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byApprovalList[approvalID], nil
}

// TestTryRecoverApprovalFromLifecycle_ApprovedExpansion exercises
// the happy path: a *_approved event exists, plus a *_pending event
// carrying the agent's original tool_use. Recovery returns the
// reconstructed OriginalCall.
func TestTryRecoverApprovalFromLifecycle_ApprovedExpansion(t *testing.T) {
	const approvalID = "cv-recover-1"
	const taskID = "task-recover-1"
	const userID = "user-1"
	pending := &store.TaskLifecycleEvent{
		TaskID:        taskID,
		UserID:        userID,
		EventType:     store.TaskLifecycleEventTaskExpandPending,
		ApprovalID:    approvalID,
		ToolUseID:     "toolu_01OriginalExpandPost",
		ToolName:      "Bash",
		ToolInputJSON: json.RawMessage(`{"command":"curl -X POST .../expand?surface=inline ..."}`),
	}
	approved := &store.TaskLifecycleEvent{
		TaskID:     taskID,
		UserID:     userID,
		EventType:  store.TaskLifecycleEventTaskExpandApproved,
		ApprovalID: approvalID,
	}
	st := &fakeLifecycleStore{
		byApproval:     map[string]*store.TaskLifecycleEvent{approvalID: approved},
		byApprovalList: map[string][]*store.TaskLifecycleEvent{approvalID: {pending, approved}},
	}

	got := tryRecoverApprovalFromLifecycle(context.Background(), st, approvalID, "approve")
	if got == nil {
		t.Fatal("expected recovery to succeed for an approved expansion with a pending row")
	}
	if got.TaskID != taskID {
		t.Errorf("TaskID = %q, want %q", got.TaskID, taskID)
	}
	if got.Verb != "approve" {
		t.Errorf("Verb = %q, want approve", got.Verb)
	}
	if got.Kind != InlineApprovalOutcomeKindTaskExpand {
		t.Errorf("Kind = %q, want %q", got.Kind, InlineApprovalOutcomeKindTaskExpand)
	}
	if got.Original == nil {
		t.Fatal("Original must be hydrated from the pending row")
	}
	if got.Original.ToolUseID != pending.ToolUseID {
		t.Errorf("Original.ToolUseID = %q, want %q", got.Original.ToolUseID, pending.ToolUseID)
	}
	if !strings.Contains(string(got.Original.Input), "/expand?surface=inline") {
		t.Errorf("Original.Input lost the verbatim curl body: %s", got.Original.Input)
	}
}

// TestTryRecoverApprovalFromLifecycle_PendingOnlyRefuses confirms
// recovery refuses when the most-recent event is *_pending. The
// in-flight hold is still authoritative there; recovery would
// incorrectly claim the approval landed.
func TestTryRecoverApprovalFromLifecycle_PendingOnlyRefuses(t *testing.T) {
	const approvalID = "cv-recover-2"
	pending := &store.TaskLifecycleEvent{
		TaskID:     "task-recover-2",
		UserID:     "user-1",
		EventType:  store.TaskLifecycleEventTaskExpandPending,
		ApprovalID: approvalID,
		ToolUseID:  "toolu_01x",
		ToolName:   "Bash",
	}
	st := &fakeLifecycleStore{
		byApproval: map[string]*store.TaskLifecycleEvent{approvalID: pending},
	}
	if got := tryRecoverApprovalFromLifecycle(context.Background(), st, approvalID, "approve"); got != nil {
		t.Errorf("recovery should refuse on pending-only history, got %+v", got)
	}
}

// TestTryRecoverApprovalFromLifecycle_VerbMismatchRefuses confirms
// recovery refuses when the body's claimed verb doesn't match the
// event's resolution direction. A body claiming "deny" against an
// *_approved event is more likely a corrupted retry than a
// legitimate restart, and the recovery path is conservative.
func TestTryRecoverApprovalFromLifecycle_VerbMismatchRefuses(t *testing.T) {
	const approvalID = "cv-recover-3"
	approved := &store.TaskLifecycleEvent{
		TaskID:     "task-recover-3",
		UserID:     "user-1",
		EventType:  store.TaskLifecycleEventTaskExpandApproved,
		ApprovalID: approvalID,
	}
	st := &fakeLifecycleStore{
		byApproval: map[string]*store.TaskLifecycleEvent{approvalID: approved},
	}
	if got := tryRecoverApprovalFromLifecycle(context.Background(), st, approvalID, "deny"); got != nil {
		t.Errorf("recovery should refuse on verb mismatch, got %+v", got)
	}
}

// TestTryRecoverApprovalFromLifecycle_NilStoreOrEmptyApprovalID
// pins the early-return cases. A nil store (config without
// lifecycle audit) or empty approvalID (corrupted body) must
// return nil without panicking.
func TestTryRecoverApprovalFromLifecycle_NilStoreOrEmptyApprovalID(t *testing.T) {
	if got := tryRecoverApprovalFromLifecycle(context.Background(), nil, "cv-x", "approve"); got != nil {
		t.Errorf("nil store should return nil; got %+v", got)
	}
	st := &fakeLifecycleStore{}
	if got := tryRecoverApprovalFromLifecycle(context.Background(), st, "", "approve"); got != nil {
		t.Errorf("empty approvalID should return nil; got %+v", got)
	}
	if got := tryRecoverApprovalFromLifecycle(context.Background(), st, "   ", "approve"); got != nil {
		t.Errorf("whitespace-only approvalID should return nil; got %+v", got)
	}
}

// TestTryRecoverApprovalFromLifecycle_ApprovedTaskCreationMissingPendingRefuses
// confirms recovery refuses an approved task creation when the
// matching *_pending row is gone (older row, or sweep) — we can't
// reconstruct without the original tool_use. The deny path is
// allowed to proceed without the original (the rewrite renders a
// denial notice that doesn't need a tool_use to anchor it), but
// approve requires the reconstruction.
func TestTryRecoverApprovalFromLifecycle_ApprovedMissingPendingRefuses(t *testing.T) {
	const approvalID = "cv-recover-4"
	approved := &store.TaskLifecycleEvent{
		TaskID:     "task-recover-4",
		UserID:     "user-1",
		EventType:  store.TaskLifecycleEventTaskCreateApproved,
		ApprovalID: approvalID,
	}
	st := &fakeLifecycleStore{
		byApproval: map[string]*store.TaskLifecycleEvent{approvalID: approved},
		// No byTask entry → ListTaskLifecycleEvents returns empty.
	}
	if got := tryRecoverApprovalFromLifecycle(context.Background(), st, approvalID, "approve"); got != nil {
		t.Errorf("approve recovery must require the pending row for reconstruction; got %+v", got)
	}
}

// TestTryRecoverApprovalFromLifecycle_DeniedWithoutPendingProceeds
// confirms the deny path is more permissive: even without the
// pending row's original tool_use we can render the denial notice
// and let the model see the rejection.
func TestTryRecoverApprovalFromLifecycle_DeniedWithoutPendingProceeds(t *testing.T) {
	const approvalID = "cv-recover-5"
	denied := &store.TaskLifecycleEvent{
		TaskID:     "task-recover-5",
		UserID:     "user-1",
		EventType:  store.TaskLifecycleEventTaskExpandDenied,
		ApprovalID: approvalID,
	}
	st := &fakeLifecycleStore{
		byApproval: map[string]*store.TaskLifecycleEvent{approvalID: denied},
	}
	got := tryRecoverApprovalFromLifecycle(context.Background(), st, approvalID, "deny")
	if got == nil {
		t.Fatal("deny recovery should proceed without the pending row")
	}
	if got.Verb != "deny" || got.Kind != InlineApprovalOutcomeKindTaskExpand {
		t.Errorf("recovered = {Verb:%q Kind:%q}, want {deny, %s}",
			got.Verb, got.Kind, InlineApprovalOutcomeKindTaskExpand)
	}
}

// TestTryRecoverApprovalFromLifecycle_GetErrorReturnsNil pins
// resilience to store failures: a Get error means we don't know
// the approval state, so recovery cannot safely proceed.
func TestTryRecoverApprovalFromLifecycle_GetErrorReturnsNil(t *testing.T) {
	st := &fakeLifecycleStore{getErr: errors.New("db down")}
	if got := tryRecoverApprovalFromLifecycle(context.Background(), st, "cv-x", "approve"); got != nil {
		t.Errorf("store error should return nil (let caller fall through), got %+v", got)
	}
}

// TestRewriteFromRecoveredApproval_RebuildsReconstructedPair drives
// the recovery path through the actual body editor: the harness's
// retry body (AskUserQuestion tool_result for "yes") gets rewritten
// the same way the live proxy would have if its cache hadn't been
// lost. The assistant turn becomes the reconstructed tool_use; the
// user turn's tool_result is paired against the reconstructed id.
//
// This is the integration anchor for the cross-restart contract: a
// retry on a different proxy instance produces the same model-
// visible history as the original would have.
func TestRewriteFromRecoveredApproval_RebuildsReconstructedPair(t *testing.T) {
	const approvalID = "cv-recoverrt0001"
	const taskID = "task-recover-rt-1"
	const askToolUseID = "toolu_clawvisor_ask_rt"
	const originalToolUseID = "toolu_01OriginalExpandPostRT"

	body := []byte(`{"messages":[` +
		`{"role":"user","content":"expand the task"},` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"` + askToolUseID + `","name":"AskUserQuestion","input":{"questions":[{"question":"approve? [clawvisor:approval=` + approvalID + `]","options":[{"label":"yes"},{"label":"no"}]}]}}` +
		`]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + askToolUseID + `","content":"yes"}` +
		`]}` +
		`]}`)

	editor := anthropicApprovalBodyEditor{body: body}
	recovered := &recoveredApproval{
		TaskID: taskID,
		Verb:   "approve",
		Kind:   InlineApprovalOutcomeKindTaskExpand,
		Original: &InlineApprovalOriginalCall{
			ToolUseID: originalToolUseID,
			ToolName:  "Bash",
			Input:     json.RawMessage(`{"command":"curl -X POST /api/control/tasks/` + taskID + `/expand?surface=inline ..."}`),
		},
	}
	req := InlineApprovalRewriteRequest{Body: body}

	out, err := rewriteFromRecoveredApproval(req, editor, "approve", approvalID, recovered)
	if err != nil {
		t.Fatalf("rewriteFromRecoveredApproval: %v", err)
	}
	if !out.Rewritten {
		t.Fatalf("recovery should produce a rewritten body; got: %s", out.Body)
	}
	if out.Outcome != "inline_expansion_approved_recovered" {
		t.Errorf("Outcome = %q, want inline_expansion_approved_recovered", out.Outcome)
	}
	got := string(out.Body)
	if !strings.Contains(got, originalToolUseID) {
		t.Errorf("rewritten body must carry the reconstructed tool_use_id: %s", got)
	}
	if strings.Contains(got, askToolUseID) {
		t.Errorf("rewritten body must not retain the AskUserQuestion tool_use id: %s", got)
	}
	if !strings.Contains(got, `"tool_use_id":"`+originalToolUseID+`"`) {
		t.Errorf("rewritten body must pair the tool_result against the reconstructed id: %s", got)
	}
}
