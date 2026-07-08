package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

type testNotifier struct {
	decremented int
	updated     []string
}

func (n *testNotifier) SendApprovalRequest(context.Context, notify.ApprovalRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) SendActivationRequest(context.Context, notify.ActivationRequest) error {
	return nil
}

func (n *testNotifier) SendTaskApprovalRequest(context.Context, notify.TaskApprovalRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) SendScopeExpansionRequest(context.Context, notify.ScopeExpansionRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) UpdateMessage(_ context.Context, _ string, _ string, text string) error {
	n.updated = append(n.updated, text)
	return nil
}

func (n *testNotifier) SendTestMessage(context.Context, string) error {
	return nil
}

func (n *testNotifier) SendConnectionRequest(context.Context, notify.ConnectionRequest) (string, error) {
	return "", nil
}

func (n *testNotifier) SendAlert(context.Context, string, string) error {
	return nil
}

func (n *testNotifier) DecrementPolling(string) {
	n.decremented++
}

func TestConnectionsHandlerApproveUpdatesNotificationState(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	req := &store.ConnectionRequest{
		UserID:    user.ID,
		Name:      "Claude Code",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := st.CreateConnectionRequest(ctx, req); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}
	if err := st.SaveNotificationMessage(ctx, "connection", req.ID, "telegram", "msg-1"); err != nil {
		t.Fatalf("SaveNotificationMessage: %v", err)
	}

	notifier := &testNotifier{}
	h := NewConnectionsHandler(st, notifier, nil, slog.Default(), "http://example.com", false)

	agentID, err := h.ApproveByID(ctx, req.ID, user.ID)
	if err != nil {
		t.Fatalf("ApproveByID: %v", err)
	}
	if agentID == "" {
		t.Fatal("expected agent ID")
	}
	if notifier.decremented != 1 {
		t.Fatalf("expected polling decrement once, got %d", notifier.decremented)
	}
	if len(notifier.updated) != 1 || notifier.updated[0] != "✅ <b>Approved</b> — agent connected." {
		t.Fatalf("unexpected notification updates: %#v", notifier.updated)
	}
}

func TestConnectionsHandlerExpireUpdatesNotificationState(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	req := &store.ConnectionRequest{
		UserID:    user.ID,
		Name:      "Claude Code",
		Status:    "pending",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := st.CreateConnectionRequest(ctx, req); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}
	if err := st.SaveNotificationMessage(ctx, "connection", req.ID, "telegram", "msg-1"); err != nil {
		t.Fatalf("SaveNotificationMessage: %v", err)
	}

	notifier := &testNotifier{}
	h := NewConnectionsHandler(st, notifier, nil, slog.Default(), "http://example.com", false)

	modified, err := h.expireByID(ctx, req.ID, user.ID)
	if err != nil {
		t.Fatalf("expireByID: %v", err)
	}
	if !modified {
		t.Fatalf("expected expireByID to modify the pending row")
	}

	got, err := st.GetConnectionRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetConnectionRequest: %v", err)
	}
	if got.Status != "expired" {
		t.Fatalf("expected expired status, got %q", got.Status)
	}
	if notifier.decremented != 1 {
		t.Fatalf("expected polling decrement once, got %d", notifier.decremented)
	}
	if len(notifier.updated) != 1 || notifier.updated[0] != "⏰ <b>Expired</b> — connection request timed out." {
		t.Fatalf("unexpected notification updates: %#v", notifier.updated)
	}
}

func TestConnectionsStoreInstallContextRoundTrip(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	want := &store.InstallContext{
		Harness:        "codex",
		HarnessVersion: "0.31.0",
		InstallMode:    "docker",
		HostOS:         "darwin",
		ContainerID:    "abc123",
		AuthMode:       "passthrough",
		AliasIntent:    "safe",
	}
	req := &store.ConnectionRequest{
		UserID:         user.ID,
		Name:           "codex",
		Status:         "pending",
		ExpiresAt:      time.Now().Add(5 * time.Minute),
		InstallContext: want,
	}
	if err := st.CreateConnectionRequest(ctx, req); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}

	got, err := st.GetConnectionRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetConnectionRequest: %v", err)
	}
	if got.InstallContext == nil {
		t.Fatalf("install context unset after round-trip")
	}
	if !installContextEqual(got.InstallContext, want) {
		t.Fatalf("install context mismatch:\n want: %+v\n got:  %+v", *want, *got.InstallContext)
	}

	// A request created with no install context round-trips as nil so older
	// rows (and callers that don't send it) don't fabricate empty structs.
	bare := &store.ConnectionRequest{
		UserID:    user.ID,
		Name:      "bare",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := st.CreateConnectionRequest(ctx, bare); err != nil {
		t.Fatalf("CreateConnectionRequest bare: %v", err)
	}
	bareGot, err := st.GetConnectionRequest(ctx, bare.ID)
	if err != nil {
		t.Fatalf("GetConnectionRequest bare: %v", err)
	}
	if bareGot.InstallContext != nil {
		t.Fatalf("expected nil install context for bare request, got %+v", *bareGot.InstallContext)
	}

	// List should surface install context alongside pending rows.
	list, err := st.ListPendingConnectionRequests(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}
	var seen *store.InstallContext
	for _, r := range list {
		if r.ID == req.ID {
			seen = r.InstallContext
		}
	}
	if seen == nil || !installContextEqual(seen, want) {
		t.Fatalf("install context not surfaced by List: got %+v", seen)
	}
}

// TestEnrollUnpinnedInviteRejectsExistingAccount proves an unpinned invite
// cannot claim a pre-existing account by naming its email: the caller-supplied
// email is not a possession proof, so reusing that account (and minting an
// agent token under it) would let an unauthenticated caller act as the victim.
// The enroll must refuse with 409 INVITE_ACCOUNT_EXISTS and leave the invite
// unburned and no token minted.
func TestEnrollUnpinnedInviteRejectsExistingAccount(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)

	// A pre-existing verified member account owned by the victim.
	victim, err := st.CreateUser(ctx, "victim@test.example", "hash", store.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// An unpinned (any-email) member invite: Email == "".
	token := "cvinv_unpinnedtoken"
	inv := &store.UserInvite{
		TokenHash: auth.HashToken(token),
		Email:     "",
		Role:      store.RoleMember,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.CreateUserInvite(ctx, inv); err != nil {
		t.Fatalf("CreateUserInvite: %v", err)
	}

	h := NewConnectionsHandler(st, &testNotifier{}, nil, slog.Default(), "http://example.com", false)
	h.ConfigureInviteEnroll(true, 0)

	body, _ := json.Marshal(map[string]any{
		"invite_token": token,
		"email":        "victim@test.example",
		"name":         "agent-1",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/enroll", bytes.NewReader(body))
	h.EnrollWithInvite(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["code"] != "INVITE_ACCOUNT_EXISTS" {
		t.Fatalf("expected code INVITE_ACCOUNT_EXISTS, got %v", resp["code"])
	}
	if _, ok := resp["token"]; ok {
		t.Fatalf("no agent token must be minted, got body %s", rec.Body.String())
	}

	// The invite must not have been burned — the operator can still reissue.
	got, err := st.GetUserInviteByHash(ctx, auth.HashToken(token))
	if err != nil {
		t.Fatalf("GetUserInviteByHash: %v", err)
	}
	if got.UsedAt != nil {
		t.Fatalf("invite must remain unused after a rejected claim")
	}

	// No agent should exist under the victim's account.
	agents, err := st.ListAgents(ctx, victim.ID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected no agents under victim, got %d", len(agents))
	}
}

// TestEnrollPinnedInviteReusesExistingAccount is the companion to the unpinned
// rejection: when the invite IS pinned to the email, reusing a pre-existing
// account is legitimate (the pin is the binding) and enrollment succeeds.
func TestEnrollPinnedInviteReusesExistingAccount(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)

	if _, err := st.CreateUser(ctx, "alice@test.example", "hash", store.RoleMember); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token := "cvinv_pinnedtoken"
	inv := &store.UserInvite{
		TokenHash: auth.HashToken(token),
		Email:     "alice@test.example",
		Role:      store.RoleMember,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.CreateUserInvite(ctx, inv); err != nil {
		t.Fatalf("CreateUserInvite: %v", err)
	}

	h := NewConnectionsHandler(st, &testNotifier{}, nil, slog.Default(), "http://example.com", false)
	h.ConfigureInviteEnroll(true, 0)

	body, _ := json.Marshal(map[string]any{
		"invite_token": token,
		"name":         "agent-1",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/enroll", bytes.NewReader(body))
	h.EnrollWithInvite(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a pinned reuse, got %d (body %s)", rec.Code, rec.Body.String())
	}
}

// TestResolveInviteTokenSurfacesBackendError proves a store lookup failure is
// returned raw (mapped to 500 downstream), not masked as errInviteNotFound
// which would falsely tell the caller their invite is invalid.
func TestResolveInviteTokenSurfacesBackendError(t *testing.T) {
	boom := errors.New("db exploded")
	_, err := resolveInviteToken(context.Background(), errInviteStore{err: boom}, "cvinv_x", "")
	if !errors.Is(err, boom) {
		t.Fatalf("expected raw backend error, got %v", err)
	}
	if errors.Is(err, errInviteNotFound) {
		t.Fatalf("a backend failure must not collapse into errInviteNotFound")
	}
}

// TestResolveInviteTokenAbsenceIsNotFound proves genuine absence still maps to
// the invalid-invite outcome (403), preserving existing behavior.
func TestResolveInviteTokenAbsenceIsNotFound(t *testing.T) {
	_, err := resolveInviteToken(context.Background(), errInviteStore{err: store.ErrNotFound}, "cvinv_x", "")
	if !errors.Is(err, errInviteNotFound) {
		t.Fatalf("absence should map to errInviteNotFound, got %v", err)
	}
}

// TestWriteInviteErrorMapsBackendFailureTo500 proves the HTTP mapping: a raw
// backend error becomes a 500, while a not-found stays a 403 INVITE_INVALID.
func TestWriteInviteErrorMapsBackendFailureTo500(t *testing.T) {
	rec := httptest.NewRecorder()
	writeInviteError(rec, errors.New("db exploded"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("backend failure should map to 500, got %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	writeInviteError(rec2, errInviteNotFound)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("not-found should map to 403, got %d", rec2.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["code"] != "INVITE_INVALID" {
		t.Fatalf("expected INVITE_INVALID, got %v", resp["code"])
	}
}

// errInviteStore is a minimal Store whose invite lookup fails with a fixed
// error; every other method is unused by resolveInviteToken.
type errInviteStore struct {
	store.Store
	err error
}

func (s errInviteStore) GetUserInviteByHash(context.Context, string) (*store.UserInvite, error) {
	return nil, s.err
}

// installContextEqual compares two install contexts by typed fields and by
// Extra-map shape; the map field made the struct non-comparable with `==`.
func installContextEqual(a, b *store.InstallContext) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Harness != b.Harness ||
		a.HarnessVersion != b.HarnessVersion ||
		a.InstallMode != b.InstallMode ||
		a.HostOS != b.HostOS ||
		a.ContainerID != b.ContainerID ||
		a.AuthMode != b.AuthMode ||
		a.AliasIntent != b.AliasIntent {
		return false
	}
	if len(a.Extra) != len(b.Extra) {
		return false
	}
	for k, v := range a.Extra {
		if !reflect.DeepEqual(v, b.Extra[k]) {
			return false
		}
	}
	return true
}
