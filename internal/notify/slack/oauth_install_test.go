package slack

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func installTestNotifier(t *testing.T, body map[string]any) *Notifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	n := New(nil, testSecret, AppCredentials{
		ClientID: "cid", ClientSecret: "csec", RedirectURL: "https://example/cb",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.apiBase = srv.URL + "/"
	return n
}

// Slack can answer ok:true with identity fields missing. Accepting that used
// to store an empty InstallerSlackUserID, and SlackConfig.CanApprove admits
// only the installer plus the allowlist — so the person who ran the install
// could no longer approve anything, with their clicks rejected and no error
// anywhere pointing at the install. Failing here makes the exchange the thing
// that reports the problem.
func TestCompleteSlackInstall_RejectsMissingIdentityFields(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{
			name: "missing authed_user.id",
			body: map[string]any{
				"ok": true, "access_token": "xoxb-1",
				"team": map[string]any{"id": "T1", "name": "Acme"},
			},
			wantErr: "authed_user.id",
		},
		{
			name: "empty authed_user.id",
			body: map[string]any{
				"ok": true, "access_token": "xoxb-1",
				"team":        map[string]any{"id": "T1", "name": "Acme"},
				"authed_user": map[string]any{"id": ""},
			},
			wantErr: "authed_user.id",
		},
		{
			name: "missing team.id",
			body: map[string]any{
				"ok": true, "access_token": "xoxb-1",
				"authed_user": map[string]any{"id": "U1"},
			},
			wantErr: "team.id",
		},
		{
			name:    "missing access_token",
			body:    map[string]any{"ok": true, "authed_user": map[string]any{"id": "U1"}},
			wantErr: "bot token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := installTestNotifier(t, tc.body)
			_, err := n.CompleteSlackInstall(context.Background(), "code")
			if err == nil {
				t.Fatalf("CompleteSlackInstall accepted a malformed exchange (%v)", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompleteSlackInstall_AcceptsCompleteExchange(t *testing.T) {
	n := installTestNotifier(t, map[string]any{
		"ok": true, "access_token": "xoxb-1",
		"team":        map[string]any{"id": "T1", "name": "Acme"},
		"authed_user": map[string]any{"id": "U1"},
	})
	got, err := n.CompleteSlackInstall(context.Background(), "code")
	if err != nil {
		t.Fatalf("CompleteSlackInstall: %v", err)
	}
	if got.BotToken != "xoxb-1" || got.TeamID != "T1" || got.TeamName != "Acme" || got.InstallerUserID != "U1" {
		t.Fatalf("install = %+v", got)
	}
}
