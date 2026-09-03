package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func TestSendApprovalRequestAddsSlackButtons(t *testing.T) {
	ctx := context.Background()
	st, userID := testStoreWithSlackConfig(t, notify.SlackConfig{
		BotToken:      "xoxb-test",
		ChannelID:     "C123",
		SigningSecret: "signing-secret",
		Mode:          "direct",
	})

	var payload struct {
		Channel string `json:"channel"`
		Blocks  []struct {
			Type     string `json:"type"`
			Elements []struct {
				Type     string `json:"type"`
				ActionID string `json:"action_id"`
				Value    string `json:"value"`
				URL      string `json:"url"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Fatalf("unexpected Slack API call: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Fatalf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C123","ts":"1700000000.000100"}`))
	}))
	defer srv.Close()

	n := New(st, nil)
	n.apiBase = srv.URL

	msgID, err := n.SendApprovalRequest(ctx, notify.ApprovalRequest{
		RequestID: "req-1",
		TaskID:    "task-1",
		UserID:    userID,
		AgentName: "OpenClaw",
		Service:   "github",
		Action:    "create_issue",
		ApproveURL: "http://example.com/approve",
		DenyURL:    "http://example.com/deny",
	})
	if err != nil {
		t.Fatalf("SendApprovalRequest: %v", err)
	}
	if msgID != "C123:1700000000.000100" {
		t.Fatalf("message id = %q", msgID)
	}
	if payload.Channel != "C123" {
		t.Fatalf("channel = %q", payload.Channel)
	}
	if len(payload.Blocks) < 3 || payload.Blocks[2].Type != "actions" || len(payload.Blocks[2].Elements) != 2 {
		t.Fatalf("missing action buttons: %#v", payload.Blocks)
	}
	approve := payload.Blocks[2].Elements[0]
	if approve.ActionID != "approve" || approve.URL != "" {
		t.Fatalf("approve button = %+v", approve)
	}
	var value callbackValue
	if err := json.Unmarshal([]byte(approve.Value), &value); err != nil {
		t.Fatalf("approve value is not JSON: %v", err)
	}
	if value.Token == "" || value.UserID != userID {
		t.Fatalf("approve value = %+v", value)
	}
}

func TestHandleInteractionEmitsDecision(t *testing.T) {
	ctx := context.Background()
	signingSecret := "signing-secret"
	st, userID := testStoreWithSlackConfig(t, notify.SlackConfig{
		BotToken:      "xoxb-test",
		ChannelID:     "C123",
		SigningSecret: signingSecret,
		Mode:          "direct",
	})
	n := New(st, nil)
	approveID, _, err := n.cbTokens.Generate("approval", "req-1", userID, "task-1", "C123", time.Minute)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	body := signedInteractionBody(t, signingSecret, callbackValue{Token: approveID, UserID: userID}, "approve")
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/slack/interactions", strings.NewReader(body.body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", body.ts)
	req.Header.Set("X-Slack-Signature", body.sig)
	rec := httptest.NewRecorder()

	n.HandleInteraction(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HandleInteraction status = %d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case d := <-n.DecisionChannel():
		if d.Type != "approval" || d.Action != "approve" || d.TargetID != "req-1" || d.TaskID != "task-1" || d.UserID != userID {
			t.Fatalf("decision = %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decision")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/notifications/slack/interactions", strings.NewReader(body.body))
	req.Header.Set("X-Slack-Request-Timestamp", body.ts)
	req.Header.Set("X-Slack-Signature", "v0=bad")
	n.HandleInteraction(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d", rec.Code)
	}
}

func testStoreWithSlackConfig(t *testing.T, cfg notify.SlackConfig) (*sqlitestore.Store, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "slack@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, _ := json.Marshal(cfg)
	if err := st.UpsertNotificationConfig(ctx, user.ID, "slack", raw); err != nil {
		t.Fatalf("UpsertNotificationConfig: %v", err)
	}
	return st, user.ID
}

type signedBody struct {
	body string
	ts   string
	sig  string
}

func signedInteractionBody(t *testing.T, secret string, value callbackValue, actionID string) signedBody {
	t.Helper()
	valueJSON, _ := json.Marshal(value)
	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"actions": []map[string]any{{
			"action_id": actionID,
			"value":     string(valueJSON),
		}},
	})
	form := url.Values{"payload": {string(payload)}}.Encode()
	ts := strconvNow()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write([]byte(form))
	return signedBody{
		body: form,
		ts:   ts,
		sig:  "v0=" + hex.EncodeToString(mac.Sum(nil)),
	}
}

func strconvNow() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}
