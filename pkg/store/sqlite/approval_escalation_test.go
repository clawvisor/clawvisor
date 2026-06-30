package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestApprovalEscalationLifecycle(t *testing.T) {
	ctx := context.Background()
	st, userID, approvalID := newEscalationTestStore(t, ctx)
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	req := json.RawMessage(`{"request_id":"req-1","user_id":"user-1"}`)
	chain := json.RawMessage(`[{"channel":"push","delay_seconds":0},{"channel":"telegram","delay_seconds":60}]`)

	if err := st.CreateApprovalEscalation(ctx, &store.ApprovalEscalation{
		ApprovalRecordID: approvalID,
		UserID:           userID,
		TargetType:       "approval",
		TargetID:         "req-1",
		ApprovalRequest:  req,
		EscalationChain:  chain,
		CurrentStep:      0,
		NextEscalateAt:   now,
		Status:           "active",
	}); err != nil {
		t.Fatalf("CreateApprovalEscalation: %v", err)
	}

	due, err := st.ListDueApprovalEscalations(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListDueApprovalEscalations: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due count=%d, want 1", len(due))
	}
	claimed, err := st.ClaimApprovalEscalationStep(ctx, due[0].ID, 0, now)
	if err != nil {
		t.Fatalf("ClaimApprovalEscalationStep: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim to succeed")
	}
	if claimedAgain, err := st.ClaimApprovalEscalationStep(ctx, due[0].ID, 0, now); err != nil {
		t.Fatalf("second ClaimApprovalEscalationStep: %v", err)
	} else if claimedAgain {
		t.Fatal("expected second claim to fail")
	}

	nextAt := now.Add(time.Minute)
	completed, err := st.CompleteApprovalEscalationStep(ctx, due[0].ID, 0, 1, nextAt)
	if err != nil {
		t.Fatalf("CompleteApprovalEscalationStep: %v", err)
	}
	if !completed {
		t.Fatal("expected complete to succeed")
	}
	notDue, err := st.ListDueApprovalEscalations(ctx, now.Add(30*time.Second), 10)
	if err != nil {
		t.Fatalf("ListDueApprovalEscalations not due: %v", err)
	}
	if len(notDue) != 0 {
		t.Fatalf("not due count=%d, want 0", len(notDue))
	}

	due, err = st.ListDueApprovalEscalations(ctx, nextAt, 10)
	if err != nil {
		t.Fatalf("ListDueApprovalEscalations second: %v", err)
	}
	if len(due) != 1 || due[0].CurrentStep != 1 {
		t.Fatalf("due second=%+v, want step 1", due)
	}
	if err := st.ResolveApprovalEscalation(ctx, "approval", "req-1"); err != nil {
		t.Fatalf("ResolveApprovalEscalation: %v", err)
	}
	due, err = st.ListDueApprovalEscalations(ctx, nextAt, 10)
	if err != nil {
		t.Fatalf("ListDueApprovalEscalations after resolve: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("resolved due count=%d, want 0", len(due))
	}
}

func TestApprovalEscalationConcurrentClaimOnlyWinsOnce(t *testing.T) {
	ctx := context.Background()
	st, userID, approvalID := newEscalationTestStore(t, ctx)
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	if err := st.CreateApprovalEscalation(ctx, &store.ApprovalEscalation{
		ApprovalRecordID: approvalID,
		UserID:           userID,
		TargetType:       "approval",
		TargetID:         "req-1",
		ApprovalRequest:  json.RawMessage(`{"request_id":"req-1"}`),
		EscalationChain:  json.RawMessage(`[{"channel":"push","delay_seconds":0}]`),
		CurrentStep:      0,
		NextEscalateAt:   now,
		Status:           "active",
	}); err != nil {
		t.Fatalf("CreateApprovalEscalation: %v", err)
	}
	due, err := st.ListDueApprovalEscalations(ctx, now, 1)
	if err != nil {
		t.Fatalf("ListDueApprovalEscalations: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due count=%d, want 1", len(due))
	}

	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := st.ClaimApprovalEscalationStep(ctx, due[0].ID, 0, now)
			if err != nil {
				t.Errorf("ClaimApprovalEscalationStep: %v", err)
				return
			}
			results <- claimed
		}()
	}
	wg.Wait()
	close(results)
	var wins int
	for claimed := range results {
		if claimed {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("claim wins=%d, want 1", wins)
	}
}

func TestApprovalEscalationNotificationMessagesListAllChannels(t *testing.T) {
	ctx := context.Background()
	st, _, _ := newEscalationTestStore(t, ctx)
	if err := st.SaveNotificationMessage(ctx, "approval", "req-1", "push", "push-msg"); err != nil {
		t.Fatalf("SaveNotificationMessage push: %v", err)
	}
	if err := st.SaveNotificationMessage(ctx, "approval", "req-1", "telegram", "telegram-msg"); err != nil {
		t.Fatalf("SaveNotificationMessage telegram: %v", err)
	}
	messages, err := st.ListNotificationMessages(ctx, "approval", "req-1")
	if err != nil {
		t.Fatalf("ListNotificationMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count=%d, want 2", len(messages))
	}
	got := map[string]string{}
	for _, msg := range messages {
		got[msg.Channel] = msg.MessageID
	}
	if got["push"] != "push-msg" || got["telegram"] != "telegram-msg" {
		t.Fatalf("messages=%v, want push/telegram IDs", got)
	}
}

func newEscalationTestStore(t *testing.T, ctx context.Context) (*Store, string, string) {
	t.Helper()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db)
	user, err := st.CreateUser(ctx, "escalation@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	approval := &store.ApprovalRecord{
		Kind:                "request_once",
		UserID:              user.ID,
		Status:              "pending",
		Surface:             "dashboard",
		SummaryJSON:         json.RawMessage(`{"summary":"request"}`),
		PayloadJSON:         json.RawMessage(`{"request_id":"req-1"}`),
		ResolutionTransport: "execute_pending_request",
	}
	if err := st.CreateApprovalRecord(ctx, approval); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}
	return st, user.ID, approval.ID
}
