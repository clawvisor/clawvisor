package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// fakeVault is an in-memory vault.Vault whose writes and deletes can be made
// to fail on demand, which is the only way to reach the rollback and
// orphaned-token paths.
type fakeVault struct {
	mu      sync.Mutex
	data    map[string][]byte
	setErr  error
	delErr  error
	deletes int
}

func newFakeVault() *fakeVault { return &fakeVault{data: map[string][]byte{}} }

func (f *fakeVault) key(userID, serviceID string) string { return userID + "\x00" + serviceID }

func (f *fakeVault) Set(_ context.Context, userID, serviceID string, cred []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.data[f.key(userID, serviceID)] = append([]byte(nil), cred...)
	return nil
}

func (f *fakeVault) SetIfAbsent(ctx context.Context, userID, serviceID string, cred []byte) error {
	f.mu.Lock()
	_, ok := f.data[f.key(userID, serviceID)]
	f.mu.Unlock()
	if ok {
		return vault.ErrAlreadyExists
	}
	return f.Set(ctx, userID, serviceID, cred)
}

func (f *fakeVault) Get(_ context.Context, userID, serviceID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.data[f.key(userID, serviceID)]
	if !ok {
		return nil, vault.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeVault) Delete(_ context.Context, userID, serviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.data, f.key(userID, serviceID))
	return nil
}

func (f *fakeVault) List(_ context.Context, _ string) ([]string, error) { return nil, nil }

func (f *fakeVault) token(t *testing.T, userID string) string {
	t.Helper()
	b, err := f.Get(context.Background(), userID, vaultBotTokenKey)
	if errors.Is(err, vault.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	return string(b)
}

// flakyStore wraps the real sqlite store so a single notification_config
// operation can be made to fail while everything else still behaves.
type flakyStore struct {
	store.Store
	upsertErr error
	deleteErr error
}

func (s *flakyStore) UpsertNotificationConfig(ctx context.Context, userID, channel string, cfg json.RawMessage) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	return s.Store.UpsertNotificationConfig(ctx, userID, channel, cfg)
}

func (s *flakyStore) DeleteNotificationConfig(ctx context.Context, userID, channel string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Store.DeleteNotificationConfig(ctx, userID, channel)
}

// newVaultTestNotifier mirrors newTestNotifier but keeps the bot token in a
// vault (as production does) and hands back the seams the lifecycle tests
// need to fail.
func newVaultTestNotifier(t *testing.T) (*Notifier, *flakyStore, *fakeVault, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "slack.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "slack-lifecycle@example.com", "", "user")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	fs := &flakyStore{Store: st}
	fv := newFakeVault()
	n := New(fs, testSecret, AppCredentials{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.SetVault(fv)
	return n, fs, fv, user.ID
}

// seedConnection writes a complete, working connection through the real save
// path so the tests below start from the state a user actually has.
func seedConnection(t *testing.T, n *Notifier, userID, token, channelID string) {
	t.Helper()
	if err := n.SaveSlackConfig(context.Background(), userID, notify.SlackConfig{
		BotToken: token, TeamID: "T1", TeamName: "Acme",
		ChannelID: channelID, ChannelName: "approvals",
		InstallerSlackUserID: "U_INSTALLER",
	}); err != nil {
		t.Fatalf("seed SaveSlackConfig: %v", err)
	}
}

// A failed UPDATE used to delete the vault key unconditionally, which
// destroyed the token the surviving config row still pointed at: the
// connection stayed listed in the UI while every send failed with "no bot
// token configured", and only a full reconnect could repair it.
func TestSaveSlackConfig_FailedUpdatePreservesPriorToken(t *testing.T) {
	n, fs, fv, userID := newVaultTestNotifier(t)
	seedConnection(t, n, userID, "xoxb-old", "C1")

	fs.upsertErr = errors.New("boom")
	err := n.SaveSlackConfig(context.Background(), userID, notify.SlackConfig{
		BotToken: "xoxb-new", TeamID: "T1", ChannelID: "C2",
	})
	if err == nil {
		t.Fatal("SaveSlackConfig returned nil despite a failing upsert")
	}

	if got := fv.token(t, userID); got != "xoxb-old" {
		t.Fatalf("vault token = %q, want the prior %q restored — the surviving "+
			"config row still references it", got, "xoxb-old")
	}
	// The end-to-end proof: the untouched config must still be usable.
	fs.upsertErr = nil
	cfg, err := n.SlackConfig(context.Background(), userID)
	if err != nil {
		t.Fatalf("prior connection became unusable after a failed update: %v", err)
	}
	if cfg.BotToken != "xoxb-old" || cfg.ChannelID != "C1" {
		t.Fatalf("prior connection changed: token=%q channel=%q", cfg.BotToken, cfg.ChannelID)
	}
}

// The rollback must still clean up when there genuinely was no prior value,
// or a failed first install leaves an unreferenced secret in the vault.
func TestSaveSlackConfig_FailedFirstInstallDeletesToken(t *testing.T) {
	n, fs, fv, userID := newVaultTestNotifier(t)

	fs.upsertErr = errors.New("boom")
	if err := n.SaveSlackConfig(context.Background(), userID, notify.SlackConfig{
		BotToken: "xoxb-new", TeamID: "T1", ChannelID: "C1",
	}); err == nil {
		t.Fatal("SaveSlackConfig returned nil despite a failing upsert")
	}

	if got := fv.token(t, userID); got != "" {
		t.Fatalf("vault still holds %q after a failed first install; nothing references it", got)
	}
}

// A vault failure must not block the disconnect: the config row is what makes
// the token reachable, so removing it is the outcome that matters. The
// failure has to be loud, but the user must end up disconnected.
func TestDeleteSlackConfig_VaultFailureStillDisconnects(t *testing.T) {
	n, _, fv, userID := newVaultTestNotifier(t)
	seedConnection(t, n, userID, "xoxb-old", "C1")

	fv.delErr = errors.New("vault down")
	if err := n.DeleteSlackConfig(context.Background(), userID); err != nil {
		t.Fatalf("DeleteSlackConfig = %v, want nil: the disconnect already took effect", err)
	}
	if fv.deletes == 0 {
		t.Fatal("vault delete was never attempted")
	}
	if _, err := n.SlackConfig(context.Background(), userID); err == nil {
		t.Fatal("config row survived the disconnect")
	}
}

// If the config row cannot be removed the token must survive, so a retry can
// still complete. The old vault-first order deleted the token and then left
// the row behind, producing a connection that looked live but could not send.
func TestDeleteSlackConfig_StoreFailureKeepsTokenForRetry(t *testing.T) {
	n, fs, fv, userID := newVaultTestNotifier(t)
	seedConnection(t, n, userID, "xoxb-old", "C1")

	fs.deleteErr = errors.New("db down")
	if err := n.DeleteSlackConfig(context.Background(), userID); err == nil {
		t.Fatal("DeleteSlackConfig returned nil despite a failing store delete")
	}
	if got := fv.token(t, userID); got != "xoxb-old" {
		t.Fatalf("vault token = %q, want it intact: the config row still needs it", got)
	}

	fs.deleteErr = nil
	if err := n.DeleteSlackConfig(context.Background(), userID); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if got := fv.token(t, userID); got != "" {
		t.Fatalf("vault token = %q, want it removed by the successful retry", got)
	}
}

// PendingChannel is a placeholder for "installed, no channel picked yet". It
// is not a Slack channel, so every send must refuse it rather than posting
// into a channel_not_found.
func TestSendPaths_RefusePendingChannel(t *testing.T) {
	n, _, _, userID := newVaultTestNotifier(t)
	seedConnection(t, n, userID, "xoxb-old", PendingChannel)

	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.1", "channel": PendingChannel})
	}))
	defer srv.Close()
	n.apiBase = srv.URL + "/"

	ctx := context.Background()
	sends := map[string]func() error{
		"SendApprovalRequest": func() error {
			_, err := n.SendApprovalRequest(ctx, notify.ApprovalRequest{
				RequestID: "req-1", UserID: userID, AgentName: "agent",
				Service: "google.gmail", Action: "send",
			})
			return err
		},
		"SendTaskApprovalRequest": func() error {
			_, err := n.SendTaskApprovalRequest(ctx, notify.TaskApprovalRequest{
				TaskID: "task-1", UserID: userID, AgentName: "agent",
			})
			return err
		},
		"SendScopeExpansionRequest": func() error {
			_, err := n.SendScopeExpansionRequest(ctx, notify.ScopeExpansionRequest{
				TaskID: "task-1", UserID: userID, AgentName: "agent",
			})
			return err
		},
		"SendConnectionRequest": func() error {
			_, err := n.SendConnectionRequest(ctx, notify.ConnectionRequest{
				ConnectionID: "conn-1", UserID: userID, AgentName: "agent",
			})
			return err
		},
		"SendActivationRequest": func() error {
			return n.SendActivationRequest(ctx, notify.ActivationRequest{
				UserID: userID, AgentName: "agent", Service: "google.gmail",
			})
		},
		"SendAlert":       func() error { return n.SendAlert(ctx, userID, "hi") },
		"SendTestMessage": func() error { return n.SendTestMessage(ctx, userID) },
		"SendSlackTestMessage": func() error {
			return n.SendSlackTestMessage(ctx, userID)
		},
	}
	for name, send := range sends {
		t.Run(name, func(t *testing.T) {
			err := send()
			if !errors.Is(err, ErrNoChannelConfigured) {
				t.Fatalf("%s err = %v, want ErrNoChannelConfigured", name, err)
			}
		})
	}
	if posted != 0 {
		t.Fatalf("%d message(s) were posted to the pending sentinel", posted)
	}
}

// The guard must not fire once a real channel is chosen.
func TestSendPaths_AllowedOnceChannelChosen(t *testing.T) {
	n, _, _, userID := newVaultTestNotifier(t)
	seedConnection(t, n, userID, "xoxb-old", "C1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.1", "channel": "C1"})
	}))
	defer srv.Close()
	n.apiBase = srv.URL + "/"

	if err := n.SendTestMessage(context.Background(), userID); err != nil {
		t.Fatalf("SendTestMessage with a real channel: %v", err)
	}
}
