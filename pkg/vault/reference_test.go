package vault

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

// refTestSchema mirrors the production vault_entries table including the spec-10
// kind discriminator.
const refTestSchema = `
	CREATE TABLE vault_entries (
		id         TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		service_id TEXT NOT NULL,
		encrypted  TEXT NOT NULL,
		iv         TEXT NOT NULL,
		auth_tag   TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		kind       TEXT NOT NULL DEFAULT 'push',
		UNIQUE(user_id, service_id)
	);`

func newRefTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), refTestSchema); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// fakeResolver is an in-process Resolver for the deterministic lane.
type fakeResolver struct {
	val       []byte
	err       error
	calls     int
	failFirst int // return ErrRefThrottled for the first N calls, then val/err
}

func (f *fakeResolver) Resolve(_ context.Context, ref RefEnvelope) ([]byte, error) {
	f.calls++
	if f.failFirst > 0 && f.calls <= f.failFirst {
		return nil, ErrRefThrottled
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.val, nil
}

// memVault is a push backend with NO DB rows — it stands in for the `gcp`
// push backend, where credentials live in Secret Manager rather than the DB,
// to prove references still work under it.
type memVault struct{ m map[string][]byte }

func newMemVault() *memVault { return &memVault{m: map[string][]byte{}} }
func memKey(u, s string) string { return u + "\x00" + s }

func (v *memVault) Set(_ context.Context, u, s string, c []byte) error {
	v.m[memKey(u, s)] = append([]byte(nil), c...)
	return nil
}
func (v *memVault) SetIfAbsent(_ context.Context, u, s string, c []byte) error {
	if _, ok := v.m[memKey(u, s)]; ok {
		return ErrAlreadyExists
	}
	v.m[memKey(u, s)] = append([]byte(nil), c...)
	return nil
}
func (v *memVault) Get(_ context.Context, u, s string) ([]byte, error) {
	c, ok := v.m[memKey(u, s)]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}
func (v *memVault) Delete(_ context.Context, u, s string) error {
	delete(v.m, memKey(u, s))
	return nil
}
func (v *memVault) List(_ context.Context, u string) ([]string, error) {
	var out []string
	for k := range v.m {
		if len(k) > len(u) && k[:len(u)] == u && k[len(u)] == 0 {
			out = append(out, k[len(u)+1:])
		}
	}
	return out, nil
}

func newRefVault(t *testing.T, inner Vault, resolvers map[string]Resolver, allow []string) *ReferenceVault {
	t.Helper()
	db := newRefTestDB(t)
	crypto, err := NewLocalVaultFromKeyWithDB(newKey(t), db, "sqlite")
	if err != nil {
		t.Fatalf("crypto vault: %v", err)
	}
	if inner == nil {
		inner = crypto // local push backend shares the same DB table
	}
	return NewReferenceVault(inner, crypto, resolvers, allow)
}

func TestReferenceVault_EnvelopeRoundTrip(t *testing.T) {
	fake := &fakeResolver{val: []byte("resolved-secret")}
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:aws:"})
	ctx := context.Background()

	env := RefEnvelope{Backend: BackendAWSSM, ID: "arn:aws:secretsmanager:us-east-1:1:secret:x", JSONKey: "api_key"}
	if err := rv.SetReference(ctx, "u1", "anthropic", env); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	got, err := rv.Get(ctx, "u1", "anthropic")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("resolved-secret")) {
		t.Fatalf("Get = %q, want resolved-secret", got)
	}
	// The stored envelope preserves the backend/id/json_key.
	stored, err := rv.store.getEnvelope(ctx, "u1", "anthropic")
	if err != nil {
		t.Fatalf("getEnvelope: %v", err)
	}
	if stored.Backend != env.Backend || stored.ID != env.ID || stored.JSONKey != env.JSONKey || stored.Marker != refEnvelopeMarker {
		t.Fatalf("envelope round-trip mismatch: %+v", stored)
	}
}

func TestReferenceVault_KindEnvelopeMismatchErrors(t *testing.T) {
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: &fakeResolver{}}, []string{"arn:"})
	ctx := context.Background()
	// Write a push value, then flip the row's kind to 'ref' out of band. The
	// decrypted bytes are NOT a valid envelope, so getEnvelope must reject it
	// rather than misinterpret a credential as a reference.
	if err := rv.Set(ctx, "u1", "svc", []byte("plain-value")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := rv.store.crypto.db.ExecContext(ctx,
		`UPDATE vault_entries SET kind='ref' WHERE user_id='u1' AND service_id='svc'`); err != nil {
		t.Fatalf("flip kind: %v", err)
	}
	if _, err := rv.Get(ctx, "u1", "svc"); !errors.Is(err, ErrRefMalformed) {
		t.Fatalf("expected ErrRefMalformed, got %v", err)
	}
}

func TestReferenceVault_GetDispatchesPushVsRef(t *testing.T) {
	fake := &fakeResolver{val: []byte("from-backend")}
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:"})
	ctx := context.Background()

	if err := rv.Set(ctx, "u1", "push-svc", []byte("pushed")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := rv.SetReference(ctx, "u1", "ref-svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); err != nil {
		t.Fatalf("SetReference: %v", err)
	}

	if got, _ := rv.Get(ctx, "u1", "push-svc"); !bytes.Equal(got, []byte("pushed")) {
		t.Fatalf("push Get = %q, want pushed", got)
	}
	if got, _ := rv.Get(ctx, "u1", "ref-svc"); !bytes.Equal(got, []byte("from-backend")) {
		t.Fatalf("ref Get = %q, want from-backend", got)
	}
	if fake.calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (push Get must not resolve)", fake.calls)
	}
}

func TestReferenceVault_RefsWorkUnderGCPPushBackend(t *testing.T) {
	// Inner is a memVault (no DB rows) standing in for the gcp push backend.
	fake := &fakeResolver{val: []byte("resolved")}
	rv := newRefVault(t, newMemVault(), map[string]Resolver{BackendGCPSM: fake}, []string{"projects/"})
	ctx := context.Background()

	// A pushed value goes to the inner (SM) store, not the DB.
	if err := rv.Set(ctx, "u1", "pushed", []byte("in-sm")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A reference lives in the DB regardless of push backend.
	if err := rv.SetReference(ctx, "u1", "reffed", RefEnvelope{Backend: BackendGCPSM, ID: "projects/p/secrets/s"}); err != nil {
		t.Fatalf("SetReference: %v", err)
	}

	if got, _ := rv.Get(ctx, "u1", "pushed"); !bytes.Equal(got, []byte("in-sm")) {
		t.Fatalf("push under gcp = %q", got)
	}
	if got, _ := rv.Get(ctx, "u1", "reffed"); !bytes.Equal(got, []byte("resolved")) {
		t.Fatalf("ref under gcp = %q", got)
	}
	// List returns BOTH kinds even though inner (SM) only knows the push one.
	svcs, err := rv.List(ctx, "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("List = %v, want both pushed and reffed", svcs)
	}
}

func TestReferenceVault_ResolvedPlaintextNeverPersisted(t *testing.T) {
	fake := &fakeResolver{val: []byte("super-secret-plaintext")}
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:"})
	ctx := context.Background()

	if err := rv.SetReference(ctx, "u1", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	before := snapshotRow(t, rv.store.crypto.db, "u1", "svc")
	if _, err := rv.Get(ctx, "u1", "svc"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	after := snapshotRow(t, rv.store.crypto.db, "u1", "svc")
	if before != after {
		t.Fatalf("row changed after Get:\nbefore=%s\nafter =%s", before, after)
	}
	// Belt-and-suspenders: the resolved plaintext must not appear anywhere in
	// the stored row.
	if bytes.Contains([]byte(after), fake.val) {
		t.Fatalf("resolved plaintext leaked into the DB row")
	}
}

func snapshotRow(t *testing.T, db *sql.DB, u, s string) string {
	t.Helper()
	var enc, iv, tag, kind string
	err := db.QueryRowContext(context.Background(),
		`SELECT encrypted, iv, auth_tag, kind FROM vault_entries WHERE user_id=? AND service_id=?`, u, s).
		Scan(&enc, &iv, &tag, &kind)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return enc + "|" + iv + "|" + tag + "|" + kind
}

func TestReferenceVault_AADBindingRejectsRowSwappedRefs(t *testing.T) {
	fake := &fakeResolver{val: []byte("x")}
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:"})
	ctx := context.Background()

	if err := rv.SetReference(ctx, "alice", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:alice"}); err != nil {
		t.Fatalf("SetReference alice: %v", err)
	}
	if err := rv.SetReference(ctx, "bob", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:bob"}); err != nil {
		t.Fatalf("SetReference bob: %v", err)
	}
	// Copy alice's envelope ciphertext into bob's row.
	if _, err := rv.store.crypto.db.ExecContext(ctx, `
		UPDATE vault_entries
		   SET encrypted=(SELECT encrypted FROM vault_entries WHERE user_id='alice' AND service_id='svc'),
		       iv       =(SELECT iv        FROM vault_entries WHERE user_id='alice' AND service_id='svc'),
		       auth_tag =(SELECT auth_tag  FROM vault_entries WHERE user_id='alice' AND service_id='svc')
		 WHERE user_id='bob' AND service_id='svc'`); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if _, err := rv.store.getEnvelope(ctx, "bob", "svc"); err == nil {
		t.Fatalf("expected AAD binding to reject row-swapped ref envelope")
	}
}

func TestReferenceVault_TypedErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"not-found", ErrRefNotFound},
		{"access-denied", ErrRefAccessDenied},
		{"throttled", ErrRefThrottled},
		{"key-missing", ErrRefKeyMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeResolver{err: tc.err}
			rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:"})
			ctx := context.Background()
			if err := rv.SetReference(ctx, "u1", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); err != nil {
				t.Fatalf("SetReference: %v", err)
			}
			_, err := rv.Get(ctx, "u1", "svc")
			if !errors.Is(err, tc.err) {
				t.Fatalf("Get error = %v, want wrapping %v", err, tc.err)
			}
		})
	}
}

func TestReferenceVault_BackoffOnlyOnThrottled(t *testing.T) {
	ctx := context.Background()

	// Throttled then success: resolver retried up to 3 times.
	retry := &fakeResolver{val: []byte("eventually"), failFirst: 2}
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: retry}, []string{"arn:"})
	if err := rv.SetReference(ctx, "u1", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	got, err := rv.Get(ctx, "u1", "svc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("eventually")) || retry.calls != 3 {
		t.Fatalf("throttle-retry: got %q after %d calls, want eventually after 3", got, retry.calls)
	}

	// Non-throttle error is NOT retried.
	deny := &fakeResolver{err: ErrRefAccessDenied}
	rv2 := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: deny}, []string{"arn:"})
	if err := rv2.SetReference(ctx, "u1", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	if _, err := rv2.Get(ctx, "u1", "svc"); !errors.Is(err, ErrRefAccessDenied) {
		t.Fatalf("Get error = %v, want ErrRefAccessDenied", err)
	}
	if deny.calls != 1 {
		t.Fatalf("non-throttle error retried %d times, want 1", deny.calls)
	}
}

func TestReferenceVault_AllowlistFailClosed(t *testing.T) {
	fake := &fakeResolver{val: []byte("x")}
	ctx := context.Background()

	// Empty allowlist: references are disabled entirely.
	closed := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, nil)
	if err := closed.SetReference(ctx, "u1", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); !errors.Is(err, ErrRefTargetNotAllowed) {
		t.Fatalf("empty allowlist: got %v, want ErrRefTargetNotAllowed", err)
	}

	// Prefix allowlist: only matching ids permitted.
	open := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:aws:secretsmanager:us-east-1:1:secret:prod/"})
	if err := open.SetReference(ctx, "u1", "ok", RefEnvelope{Backend: BackendAWSSM, ID: "arn:aws:secretsmanager:us-east-1:1:secret:prod/anthropic"}); err != nil {
		t.Fatalf("allowed prefix rejected: %v", err)
	}
	if err := open.SetReference(ctx, "u1", "bad", RefEnvelope{Backend: BackendAWSSM, ID: "arn:aws:secretsmanager:us-east-1:1:secret:other/db-password"}); !errors.Is(err, ErrRefTargetNotAllowed) {
		t.Fatalf("off-allowlist target: got %v, want ErrRefTargetNotAllowed", err)
	}
}

func TestReferenceVault_UnknownBackendRejected(t *testing.T) {
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: &fakeResolver{}}, []string{"arn:"})
	err := rv.SetReference(context.Background(), "u1", "svc", RefEnvelope{Backend: "made-up", ID: "arn:x"})
	if !errors.Is(err, ErrRefBackendUnknown) {
		t.Fatalf("got %v, want ErrRefBackendUnknown", err)
	}
}

func TestReferenceVault_SetPushResetsRefKind(t *testing.T) {
	fake := &fakeResolver{val: []byte("resolved")}
	rv := newRefVault(t, nil, map[string]Resolver{BackendAWSSM: fake}, []string{"arn:"})
	ctx := context.Background()

	if err := rv.SetReference(ctx, "u1", "svc", RefEnvelope{Backend: BackendAWSSM, ID: "arn:x"}); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	// Overwrite with a pushed value; the row must become kind='push' so it is
	// never reinterpreted as an envelope.
	if err := rv.Set(ctx, "u1", "svc", []byte("now-a-value")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	kind, err := rv.EntryKind(ctx, "u1", "svc")
	if err != nil {
		t.Fatalf("EntryKind: %v", err)
	}
	if kind != KindPush {
		t.Fatalf("kind = %q after push overwrite, want push", kind)
	}
	got, err := rv.Get(ctx, "u1", "svc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("now-a-value")) {
		t.Fatalf("Get = %q, want now-a-value", got)
	}
}

func TestLooksLikeRefEnvelope(t *testing.T) {
	if !LooksLikeRefEnvelope([]byte(`{"$clawvisor_ref":1,"backend":"aws-sm"}`)) {
		t.Fatalf("should detect a reference envelope masquerading as a value")
	}
	if LooksLikeRefEnvelope([]byte("sk-ant-plain-key")) {
		t.Fatalf("plain value must not be flagged")
	}
}

func TestExtractJSONKey(t *testing.T) {
	// Empty key returns raw bytes.
	if got, _ := ExtractJSONKey([]byte("raw-secret"), ""); !bytes.Equal(got, []byte("raw-secret")) {
		t.Fatalf("empty key = %q, want raw-secret", got)
	}
	// String field is unquoted.
	if got, _ := ExtractJSONKey([]byte(`{"api_key":"sk-123","other":"y"}`), "api_key"); !bytes.Equal(got, []byte("sk-123")) {
		t.Fatalf("api_key = %q, want sk-123", got)
	}
	// Missing key -> ErrRefKeyMissing, and the error text must not name the
	// key or the available keys (structure-disclosure guard).
	_, err := ExtractJSONKey([]byte(`{"present":"v"}`), "absent")
	if !errors.Is(err, ErrRefKeyMissing) {
		t.Fatalf("missing key error = %v, want ErrRefKeyMissing", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("absent")) || bytes.Contains([]byte(err.Error()), []byte("present")) {
		t.Fatalf("ErrRefKeyMissing message leaked key names: %q", err.Error())
	}
	// Non-JSON payload with a requested key -> ErrRefKeyMissing.
	if _, err := ExtractJSONKey([]byte("not-json"), "api_key"); !errors.Is(err, ErrRefKeyMissing) {
		t.Fatalf("non-JSON payload: got %v, want ErrRefKeyMissing", err)
	}
}
