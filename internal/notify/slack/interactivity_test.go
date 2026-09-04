package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// stubReplayGuard reports a fixed verdict so the handler's behaviour on an
// unavailable guard can be observed.
type stubReplayGuard struct {
	seen bool
	err  error
}

func (g stubReplayGuard) SeenBefore(context.Context, string, time.Duration) (bool, error) {
	return g.seen, g.err
}

// peekTrackingTokenStore signals that the interaction reached the decision path.
// Peek is the first thing processInteraction does with a real action, so it
// is the earliest observable point of "this payload was processed".
type peekTrackingTokenStore struct {
	peeked chan string
}

func newPeekTrackingTokenStore() *peekTrackingTokenStore {
	return &peekTrackingTokenStore{peeked: make(chan string, 4)}
}

func (r *peekTrackingTokenStore) Generate(_, _, _, _, _ string, _ time.Duration) (string, string, error) {
	return "", "", nil
}

func (r *peekTrackingTokenStore) Peek(shortID string) (*callbackEntry, error) {
	select {
	case r.peeked <- shortID:
	default:
	}
	return nil, errTokenNotFound
}

func (r *peekTrackingTokenStore) Consume(string) (*callbackEntry, error) {
	return nil, errTokenNotFound
}

func (r *peekTrackingTokenStore) Cleanup() {}

// processed reports whether the payload reached the decision path. The
// handler acknowledges Slack before doing that work, so this has to wait.
func (r *peekTrackingTokenStore) processed(t *testing.T) bool {
	t.Helper()
	select {
	case <-r.peeked:
		return true
	case <-time.After(500 * time.Millisecond):
		return false
	}
}

func signedInteraction(t *testing.T, payload string) *http.Request {
	t.Helper()
	body := url.Values{"payload": {payload}}.Encode()
	ts, sig := signFor(t, testSecret, time.Now(), []byte(body))
	req := httptest.NewRequest(http.MethodPost, "/slack/interactivity", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	return req
}

// The replay guard is the only thing stopping a captured, still-in-window
// payload from being resubmitted until it lands on a replica that has not
// seen it. If it cannot answer, the interaction must not be processed.
func TestHandleInteraction_ReplayGuard(t *testing.T) {
	const payload = `{"type":"block_actions","user":{"id":"U1"},"actions":[{"action_id":"clawvisor_approve","value":"tok-1"}]}`

	for _, tc := range []struct {
		name          string
		guard         stubReplayGuard
		wantStatus    int
		wantProcessed bool
	}{
		{
			name:          "first delivery is processed",
			guard:         stubReplayGuard{},
			wantStatus:    http.StatusOK,
			wantProcessed: true,
		},
		{
			name:          "replayed payload is dropped",
			guard:         stubReplayGuard{seen: true},
			wantStatus:    http.StatusOK,
			wantProcessed: false,
		},
		{
			name:          "unavailable guard fails closed",
			guard:         stubReplayGuard{err: errors.New("redis down")},
			wantStatus:    http.StatusServiceUnavailable,
			wantProcessed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := testNotifier()
			tokens := newPeekTrackingTokenStore()
			n.cbTokens = tokens
			n.replay = tc.guard

			rec := httptest.NewRecorder()
			n.HandleInteraction(rec, signedInteraction(t, payload))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := tokens.processed(t); got != tc.wantProcessed {
				t.Fatalf("processed = %v, want %v", got, tc.wantProcessed)
			}
		})
	}
}

// Workspace and channel isolation must not be opt-out: a payload that simply
// omits an identifier has to be rejected exactly like one that contradicts
// it, or a crafted interaction could resolve another workspace's request.
func TestIdentityMismatch(t *testing.T) {
	const (
		wrongWorkspace = ":no_entry: This request belongs to a different workspace."
		wrongChannel   = ":no_entry: This request cannot be resolved from this channel."
	)

	for _, tc := range []struct {
		name string
		// teamID and entryCh are what was recorded when the prompt was
		// posted; clickTeam and clickCh are what the incoming click claims.
		teamID    string
		entryCh   string
		clickTeam string
		clickCh   string
		want      string
	}{
		{"matching workspace and channel", "T1", "C1", "T1", "C1", ""},
		{"different workspace", "T1", "C1", "T2", "C1", wrongWorkspace},
		{"missing team on the payload", "T1", "C1", "", "C1", wrongWorkspace},
		{"different channel", "T1", "C1", "T1", "C2", wrongChannel},
		{"missing channel on the payload", "T1", "C1", "T1", "", wrongChannel},
		{"missing both on the payload", "T1", "C1", "", "", wrongWorkspace},
		{"nothing recorded to compare against", "", "", "T9", "C9", ""},
		{"no team recorded, channel still checked", "", "C1", "", "C2", wrongChannel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := notify.SlackConfig{TeamID: tc.teamID}
			entry := &callbackEntry{ChannelID: tc.entryCh}
			var p interactionPayload
			p.Team.ID = tc.clickTeam
			p.Channel.ID = tc.clickCh

			if got := identityMismatch(cfg, entry, p); got != tc.want {
				t.Fatalf("identityMismatch = %q, want %q", got, tc.want)
			}
		})
	}
}

// The resolve path has to read back the key the send path wrote, or the
// resolved message silently loses its attribution and detail thread.
func TestMessageContextKey_MatchesTheSendPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry *callbackEntry
		want  string
	}{
		{
			name:  "task-scoped approval keys on the composite id",
			entry: &callbackEntry{Type: "approval", TargetID: "req-1", TaskID: "task-1"},
			want:  contextKey("approval", approvalNotifyTargetID("req-1", "task-1")),
		},
		{
			name:  "standalone approval keys on the bare request id",
			entry: &callbackEntry{Type: "approval", TargetID: "req-1"},
			want:  contextKey("approval", "req-1"),
		},
		{
			name:  "task approval keys on the task id",
			entry: &callbackEntry{Type: "task", TargetID: "task-1"},
			want:  contextKey("task", "task-1"),
		},
		{
			name:  "scope expansion shares the task namespace",
			entry: &callbackEntry{Type: "scope_expansion", TargetID: "task-1"},
			want:  contextKey("task", "task-1"),
		},
		{
			name:  "connection keys on the connection id",
			entry: &callbackEntry{Type: "connection", TargetID: "conn-1"},
			want:  contextKey("connection", "conn-1"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageContextKey(tc.entry); got != tc.want {
				t.Fatalf("messageContextKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// The concrete regression: SendApprovalRequest records under the composite
// key while the callback token carries only the request ID, so the resolve
// path must rebuild the composite rather than key on TargetID alone.
func TestMessageContextKey_TaskScopedApprovalResolves(t *testing.T) {
	s := newMessageContextStore()
	// What SendApprovalRequest -> recordMessage writes.
	sendKey := contextKey("approval", approvalNotifyTargetID("req-1", "task-1"))
	s.Put(sendKey, messageContext{Summary: "agent · gmail:send"}, time.Minute)

	// What the interaction knows: the bare request ID plus the task ID.
	entry := &callbackEntry{Type: "approval", TargetID: "req-1", TaskID: "task-1"}
	s.SetApprover(messageContextKey(entry), "jane")

	mc, ok := s.TakeForResolve(sendKey)
	if !ok {
		t.Fatal("send-path context not found")
	}
	if mc.Approver != "jane" {
		t.Fatalf("approver = %q, want the clicker's mention — attribution was lost", mc.Approver)
	}
}
