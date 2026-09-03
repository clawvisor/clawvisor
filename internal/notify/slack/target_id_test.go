package slack

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// recordingTokenStore captures what the send path asks to mint.
type recordingTokenStore struct {
	CallbackTokenStorer
	gotTargetID string
	gotTaskID   string
}

func (r *recordingTokenStore) Generate(entryType, targetID, userID, taskID, channelID string, ttl time.Duration) (string, string, error) {
	r.gotTargetID = targetID
	r.gotTaskID = taskID
	return "approve-tok", "deny-tok", nil
}

func newTestNotifier(t *testing.T) (*Notifier, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "slack.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "slack-test@example.com", "", "user")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cfg, _ := json.Marshal(slackCfgJSON{
		BotToken: "xoxb-test", TeamID: "T1", TeamName: "Acme",
		ChannelID: "C1", ChannelName: "approvals",
		InstallerSlackUserID: "U_INSTALLER",
	})
	if err := st.UpsertNotificationConfig(ctx, user.ID, notifyChannel, cfg); err != nil {
		t.Fatalf("UpsertNotificationConfig: %v", err)
	}

	n := New(st, testSecret, AppCredentials{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return n, user.ID
}

// The callback token's TargetID is handed straight to ApproveByRequestID,
// which looks up by request_id. Sending the composite notification key
// ("reqID|taskID") instead made every task-scoped approval a silent no-op:
// the lookup missed, the click was dropped, and the token was already burned
// so a retry reported "already resolved".
func TestSendApprovalRequest_TokenCarriesBareRequestID(t *testing.T) {
	n, userID := newTestNotifier(t)

	rec := &recordingTokenStore{}
	n.cbTokens = rec

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1700000000.1", "channel": "C1"})
	}))
	defer srv.Close()
	n.apiBase = srv.URL + "/"

	if _, err := n.SendApprovalRequest(context.Background(), notify.ApprovalRequest{
		RequestID: "req-1",
		TaskID:    "task-1",
		UserID:    userID,
		AgentName: "agent",
		Service:   "google.gmail",
		Action:    "send",
	}); err != nil {
		t.Fatalf("SendApprovalRequest: %v", err)
	}

	if rec.gotTargetID != "req-1" {
		t.Fatalf("token TargetID = %q, want the bare request ID %q — the composite "+
			"notification key would make ApproveByRequestID miss", rec.gotTargetID, "req-1")
	}
	if rec.gotTaskID != "task-1" {
		t.Fatalf("token TaskID = %q, want %q — it disambiguates sibling approvals", rec.gotTaskID, "task-1")
	}
}

// The composite key is still correct for addressing the message row, which is
// what makes these two easy to conflate.
func TestApprovalNotifyTargetID_IsCompositeForMessageRows(t *testing.T) {
	if got := approvalNotifyTargetID("req-1", "task-1"); got != "req-1|task-1" {
		t.Fatalf("got %q, want the composite notification key", got)
	}
	if got := approvalNotifyTargetID("req-1", ""); got != "req-1" {
		t.Fatalf("got %q, want the bare request ID when there is no task", got)
	}
}
