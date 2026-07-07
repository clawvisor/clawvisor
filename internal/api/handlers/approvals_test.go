package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/clawvisor/clawvisor/internal/observability"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// resolveFailStore forces ResolveApprovalRecord to return an error while
// letting every other store method (including GetApprovalRecord) pass through
// to the embedded store.
type resolveFailStore struct {
	store.Store
	err error
}

func (s *resolveFailStore) ResolveApprovalRecord(context.Context, string, string, string, time.Time) error {
	return s.err
}

// holdsCount sums the clawvisor.approvals.holds counter data points for a
// given resolution attribute from a manual metric reader.
func holdsCount(t *testing.T, reader sdkmetric.Reader, resolution string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != observability.MetricApprovalsHolds {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s not int64 sum: %T", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				res, _ := dp.Attributes.Value(observability.AttrResolution)
				if res.AsString() == resolution {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// seedPendingApproval creates a user, agent, and a pending canonical approval
// record, returning the PendingApproval the resolve helper consumes.
func seedPendingApproval(t *testing.T, st store.Store, dbName string) *store.PendingApproval {
	t.Helper()
	ctx := context.Background()
	user, err := st.CreateUser(ctx, dbName+"@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	requestID := "req-" + dbName
	rec := &store.ApprovalRecord{
		ID:        "rec-" + dbName,
		Kind:      "request_once",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: &requestID,
		Status:    "pending",
		Surface:   "dashboard",
	}
	if err := st.CreateApprovalRecord(ctx, rec); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}
	recID := rec.ID
	return &store.PendingApproval{
		UserID:           user.ID,
		RequestID:        requestID,
		AuditID:          "audit-" + dbName,
		ApprovalRecordID: &recID,
		RequestBlob:      []byte(`{}`),
		Status:           "pending",
		ExpiresAt:        time.Now().UTC().Add(30 * time.Minute),
	}
}

// TestResolveCanonicalApproval_RecordHoldOnlyOnSuccess pins the fix that
// clawvisor.approvals.holds is emitted only after ResolveApprovalRecord
// actually resolves the record — not unconditionally after a failed resolve.
func TestResolveCanonicalApproval_RecordHoldOnlyOnSuccess(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	t.Run("success records hold", func(t *testing.T) {
		ctx := context.Background()
		db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "hold-ok.db"))
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		st := sqlite.NewStore(db)
		pa := seedPendingApproval(t, st, "holdok")

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		inst, err := observability.NewInstruments(mp.Meter("test"))
		if err != nil {
			t.Fatalf("NewInstruments: %v", err)
		}
		h := NewApprovalsHandler(st, nil, nil, nil, config.Config{}, taskrisk.NoopAssessor{}, logger, nil)
		h.SetInstruments(inst)

		h.resolveCanonicalApproval(ctx, pa, "deny", "denied")

		if got := holdsCount(t, reader, "denied"); got != 1 {
			t.Fatalf("holds{denied} = %d, want 1 (resolve succeeded)", got)
		}
	})

	t.Run("resolve failure records no hold", func(t *testing.T) {
		ctx := context.Background()
		db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "hold-fail.db"))
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		base := sqlite.NewStore(db)
		pa := seedPendingApproval(t, base, "holdfail")
		// GetApprovalRecord passes through; ResolveApprovalRecord fails.
		st := &resolveFailStore{Store: base, err: errors.New("forced resolve failure")}

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		inst, err := observability.NewInstruments(mp.Meter("test"))
		if err != nil {
			t.Fatalf("NewInstruments: %v", err)
		}
		h := NewApprovalsHandler(st, nil, nil, nil, config.Config{}, taskrisk.NoopAssessor{}, logger, nil)
		h.SetInstruments(inst)

		h.resolveCanonicalApproval(ctx, pa, "deny", "denied")

		if got := holdsCount(t, reader, "denied"); got != 0 {
			t.Fatalf("holds{denied} = %d, want 0 (resolve failed — no hold transition happened)", got)
		}
	})

	t.Run("not-found records no hold", func(t *testing.T) {
		ctx := context.Background()
		db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "hold-nf.db"))
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		base := sqlite.NewStore(db)
		pa := seedPendingApproval(t, base, "holdnf")
		// ErrNotFound was previously silently tolerated and still recorded a
		// hold; it must not.
		st := &resolveFailStore{Store: base, err: store.ErrNotFound}

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		inst, err := observability.NewInstruments(mp.Meter("test"))
		if err != nil {
			t.Fatalf("NewInstruments: %v", err)
		}
		h := NewApprovalsHandler(st, nil, nil, nil, config.Config{}, taskrisk.NoopAssessor{}, logger, nil)
		h.SetInstruments(inst)

		h.resolveCanonicalApproval(ctx, pa, "deny", "denied")

		if got := holdsCount(t, reader, "denied"); got != 0 {
			t.Fatalf("holds{denied} = %d, want 0 (ErrNotFound — record already resolved)", got)
		}
	})
}

// TestExpireTimedOut_StrandedExecutorPreservesApprovedCanonical guards against
// the "illegal canonical approval transition" page. The stranded-executing
// recovery sweep used to pipe its pending-approval row through the same helper
// as the never-replied path, which unconditionally tried to flip the canonical
// record to deny/expired. For a stranded request the canonical record is
// already in "approved" (the user said yes; only execution lapsed), so the
// transition validator rejected it and logged an Error-level line that fired
// the alert. The user's recorded decision must stand, and the recovery sweep
// must not produce that error.
func TestExpireTimedOut_StrandedExecutorPreservesApprovedCanonical(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "stranded-canonical.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "stranded-canonical@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	requestID := "req-stranded-1"
	rec := &store.ApprovalRecord{
		ID:        "rec-stranded-1",
		Kind:      "request_once",
		UserID:    user.ID,
		AgentID:   &agent.ID,
		RequestID: &requestID,
		Status:    "pending",
		Surface:   "dashboard",
	}
	if err := st.CreateApprovalRecord(ctx, rec); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}

	// Simulate the user clicking Approve: canonical record becomes "approved".
	if err := st.ResolveApprovalRecord(ctx, rec.ID, "allow_once", "approved", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveApprovalRecord: %v", err)
	}

	// Pending approval row in the state the stranded sweep finds it: the user
	// already approved, the executor claimed it, then crashed past the lease.
	// processExpiredApproval expects the CAS DELETE has already happened, so
	// we don't insert a pending_approvals row — we only construct the struct
	// the sweeper would hand us.
	recID := rec.ID
	pa := &store.PendingApproval{
		UserID:           user.ID,
		RequestID:        requestID,
		AuditID:          "audit-stranded-1",
		ApprovalRecordID: &recID,
		RequestBlob:      []byte(`{}`),
		Status:           "executing",
		ExpiresAt:        time.Now().UTC().Add(30 * time.Minute),
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewApprovalsHandler(st, nil, nil, nil, config.Config{}, taskrisk.NoopAssessor{}, logger, nil)

	h.processExpiredApproval(ctx, pa, "stranded", "⏰ <b>Recovered</b> — execution lease expired.")

	if strings.Contains(logBuf.String(), "illegal canonical approval transition") {
		t.Fatalf("stranded recovery sweep must not log illegal-transition error\nlogs:\n%s", logBuf.String())
	}

	got, err := st.GetApprovalRecord(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if got.Status != "approved" {
		t.Fatalf("canonical record status after stranded recovery = %q, want %q (user's approval must stand)", got.Status, "approved")
	}
	if got.Resolution != "allow_once" {
		t.Fatalf("canonical record resolution after stranded recovery = %q, want %q", got.Resolution, "allow_once")
	}
}
