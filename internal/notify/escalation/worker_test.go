package escalation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type memoryEscalationStore struct {
	mu          sync.Mutex
	escalations map[string]*store.ApprovalEscalation
	messages    []*store.NotificationMessage
	nextID      int
}

func newMemoryEscalationStore() *memoryEscalationStore {
	return &memoryEscalationStore{escalations: make(map[string]*store.ApprovalEscalation)}
}

func (s *memoryEscalationStore) CreateApprovalEscalation(ctx context.Context, e *store.ApprovalEscalation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	cp := cloneEscalation(e)
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("esc-%d", s.nextID)
	}
	if cp.Status == "" {
		cp.Status = "active"
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = cp.NextEscalateAt
	}
	cp.UpdatedAt = cp.CreatedAt
	s.escalations[cp.ID] = cp
	return nil
}

func (s *memoryEscalationStore) ListDueApprovalEscalations(ctx context.Context, now time.Time, limit int) ([]*store.ApprovalEscalation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.ApprovalEscalation
	for _, esc := range s.escalations {
		if esc.Status == "active" && !esc.NextEscalateAt.After(now) {
			out = append(out, cloneEscalation(esc))
		}
	}
	return out, nil
}

func (s *memoryEscalationStore) ClaimApprovalEscalationStep(ctx context.Context, id string, currentStep int, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	esc := s.escalations[id]
	if esc == nil || esc.Status != "active" || esc.CurrentStep != currentStep || esc.NextEscalateAt.After(now) {
		return false, nil
	}
	esc.Status = "dispatching"
	return true, nil
}

func (s *memoryEscalationStore) CompleteApprovalEscalationStep(ctx context.Context, id string, claimedStep, nextStep int, nextAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	esc := s.escalations[id]
	if esc == nil || esc.Status != "dispatching" || esc.CurrentStep != claimedStep {
		return false, nil
	}
	esc.Status = "active"
	esc.CurrentStep = nextStep
	esc.NextEscalateAt = nextAt
	return true, nil
}

func (s *memoryEscalationStore) ReleaseApprovalEscalationStep(ctx context.Context, id string, claimedStep int, retryAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if esc := s.escalations[id]; esc != nil && esc.Status == "dispatching" && esc.CurrentStep == claimedStep {
		esc.Status = "active"
		esc.NextEscalateAt = retryAt
	}
	return nil
}

func (s *memoryEscalationStore) ResolveApprovalEscalation(ctx context.Context, targetType, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, esc := range s.escalations {
		if esc.TargetType == targetType && esc.TargetID == targetID {
			esc.Status = "resolved"
		}
	}
	return nil
}

func (s *memoryEscalationStore) TimeoutApprovalEscalation(ctx context.Context, id string, claimedStep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if esc := s.escalations[id]; esc != nil && esc.Status == "dispatching" && esc.CurrentStep == claimedStep {
		esc.Status = "timed_out"
	}
	return nil
}

func (s *memoryEscalationStore) SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, &store.NotificationMessage{
		TargetType: targetType,
		TargetID:   targetID,
		Channel:    channel,
		MessageID:  messageID,
		CreatedAt:  time.Now().UTC(),
	})
	return nil
}

func (s *memoryEscalationStore) ListNotificationMessages(ctx context.Context, targetType, targetID string) ([]*store.NotificationMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.NotificationMessage
	for _, msg := range s.messages {
		if msg.TargetType == targetType && msg.TargetID == targetID {
			cp := *msg
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memoryEscalationStore) onlyEscalation(t *testing.T) *store.ApprovalEscalation {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.escalations) != 1 {
		t.Fatalf("escalation count=%d, want 1", len(s.escalations))
	}
	for _, esc := range s.escalations {
		return cloneEscalation(esc)
	}
	return nil
}

func (s *memoryEscalationStore) messageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

func cloneEscalation(e *store.ApprovalEscalation) *store.ApprovalEscalation {
	cp := *e
	cp.ApprovalRequest = append([]byte(nil), e.ApprovalRequest...)
	cp.EscalationChain = append([]byte(nil), e.EscalationChain...)
	return &cp
}

type escalationNotifier struct {
	mu      sync.Mutex
	sent    []string
	updates []string
}

func (n *escalationNotifier) SendApprovalRequestToChannel(ctx context.Context, channel string, req notify.ApprovalRequest) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, channel)
	return channel + "-msg", nil
}

func (n *escalationNotifier) UpdateMessageForChannel(ctx context.Context, channel, userID, messageID, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.updates = append(n.updates, channel+":"+messageID)
	return nil
}

func (n *escalationNotifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	return n.SendApprovalRequestToChannel(ctx, "legacy", req)
}

func (n *escalationNotifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	return nil
}

func (n *escalationNotifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	return "", nil
}

func (n *escalationNotifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	return "", nil
}

func (n *escalationNotifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	return nil
}

func (n *escalationNotifier) SendTestMessage(ctx context.Context, userID string) error { return nil }

func (n *escalationNotifier) SendConnectionRequest(ctx context.Context, req notify.ConnectionRequest) (string, error) {
	return "", nil
}

func (n *escalationNotifier) SendAlert(ctx context.Context, userID, text string) error { return nil }

func (n *escalationNotifier) sentChannels() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

func (n *escalationNotifier) updateMessages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.updates...)
}

func TestStartApprovalDispatchesImmediateFirstStepAndDelaysNext(t *testing.T) {
	ctx := context.Background()
	st := newMemoryEscalationStore()
	notifier := &escalationNotifier{}
	base := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	m := &Manager{
		store:    st,
		notifier: notifier,
		chain: []Step{
			{Channel: "push", DelaySeconds: 0},
			{Channel: "telegram", DelaySeconds: 60},
		},
		now:    func() time.Time { return base },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := m.StartApproval(ctx, "approval-1", "approval", "req-1", notify.ApprovalRequest{RequestID: "req-1", UserID: "user-1"}); err != nil {
		t.Fatalf("StartApproval: %v", err)
	}
	if got := notifier.sentChannels(); len(got) != 1 || got[0] != "push" {
		t.Fatalf("sent=%v, want [push]", got)
	}
	esc := st.onlyEscalation(t)
	if esc.Status != "active" || esc.CurrentStep != 1 || !esc.NextEscalateAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("escalation status=%s step=%d next=%s", esc.Status, esc.CurrentStep, esc.NextEscalateAt)
	}

	m.now = func() time.Time { return base.Add(30 * time.Second) }
	if err := m.ProcessDueOnce(ctx); err != nil {
		t.Fatalf("ProcessDueOnce before due: %v", err)
	}
	if got := notifier.sentChannels(); len(got) != 1 {
		t.Fatalf("sent before second due=%v, want one send", got)
	}

	m.now = func() time.Time { return base.Add(time.Minute) }
	if err := m.ProcessDueOnce(ctx); err != nil {
		t.Fatalf("ProcessDueOnce second due: %v", err)
	}
	if got := notifier.sentChannels(); len(got) != 2 || got[1] != "telegram" {
		t.Fatalf("sent=%v, want [push telegram]", got)
	}
	if esc := st.onlyEscalation(t); esc.Status != "timed_out" {
		t.Fatalf("status=%s, want timed_out", esc.Status)
	}
	if got := st.messageCount(); got != 2 {
		t.Fatalf("message count=%d, want 2", got)
	}
}

func TestResolvedEscalationSkipsLaterDispatchAndUpdatesMessages(t *testing.T) {
	ctx := context.Background()
	st := newMemoryEscalationStore()
	notifier := &escalationNotifier{}
	base := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	m := &Manager{
		store:    st,
		notifier: notifier,
		chain: []Step{
			{Channel: "push", DelaySeconds: 0},
			{Channel: "telegram", DelaySeconds: 60},
		},
		now:    func() time.Time { return base },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := m.StartApproval(ctx, "approval-1", "approval", "req-1", notify.ApprovalRequest{RequestID: "req-1", UserID: "user-1"}); err != nil {
		t.Fatalf("StartApproval: %v", err)
	}
	m.Resolve(ctx, "approval", "req-1", "user-1", "resolved")
	if got := notifier.updateMessages(); len(got) != 1 || got[0] != "push:push-msg" {
		t.Fatalf("updates=%v, want [push:push-msg]", got)
	}
	m.now = func() time.Time { return base.Add(time.Minute) }
	if err := m.ProcessDueOnce(ctx); err != nil {
		t.Fatalf("ProcessDueOnce: %v", err)
	}
	if got := notifier.sentChannels(); len(got) != 1 {
		t.Fatalf("sent after resolve=%v, want only first send", got)
	}
	if esc := st.onlyEscalation(t); esc.Status != "resolved" {
		t.Fatalf("status=%s, want resolved", esc.Status)
	}
}

func TestConcurrentDueProcessingClaimsStepOnce(t *testing.T) {
	ctx := context.Background()
	st := newMemoryEscalationStore()
	notifier := &escalationNotifier{}
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	req, _ := json.Marshal(notify.ApprovalRequest{RequestID: "req-1", UserID: "user-1"})
	chain, _ := json.Marshal([]Step{{Channel: "push", DelaySeconds: 0}})
	if err := st.CreateApprovalEscalation(ctx, &store.ApprovalEscalation{
		ApprovalRecordID: "approval-1",
		UserID:           "user-1",
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
	m := &Manager{
		store:    st,
		notifier: notifier,
		now:      func() time.Time { return now },
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.ProcessDueOnce(ctx); err != nil {
				t.Errorf("ProcessDueOnce: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := notifier.sentChannels(); len(got) != 1 || got[0] != "push" {
		t.Fatalf("sent=%v, want single push dispatch", got)
	}
}
