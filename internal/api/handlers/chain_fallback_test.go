package handlers

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// chainFallbackTestStore is a minimal store for chain fallback tests.
type chainFallbackTestStore struct {
	store.Store // embed nil interface; only override needed methods
	facts       map[string]bool

	// After `appearAfter` calls to ChainFactValueExists, `lateFact` becomes
	// present. Simulates an async extraction landing mid-poll.
	lateFact    string
	appearAfter int32
	calls       int32
}

func (s *chainFallbackTestStore) ChainFactValueExists(_ context.Context, taskID, sessionID, value string) (bool, error) {
	n := atomic.AddInt32(&s.calls, 1)
	if s.lateFact != "" && value == s.lateFact && n > s.appearAfter {
		return true, nil
	}
	return s.facts[taskID+"|"+sessionID+"|"+value], nil
}

// fakeTracker reports a fixed pending state. `drainAfter` decrements the
// pending count so HasPending eventually returns false; this simulates the
// other server's extraction completing.
type fakeTracker struct {
	pending    int32
	drainAfter int32
	calls      int32
}

func (t *fakeTracker) MarkPending(_ context.Context, _, _, _ string)   {}
func (t *fakeTracker) MarkDone(_ context.Context, _, _, _ string)      {}
func (t *fakeTracker) HasPending(_ context.Context, _, _ string) bool {
	n := atomic.AddInt32(&t.calls, 1)
	if t.drainAfter > 0 && n > t.drainAfter {
		return false
	}
	return atomic.LoadInt32(&t.pending) > 0
}

func makeViolationVerdict(missing ...string) *intent.VerificationVerdict {
	return &intent.VerificationVerdict{
		Allow:              false,
		ParamScope:         "violation",
		ReasonCoherence:    "ok",
		MissingChainValues: missing,
		Explanation:        "Entity not in chain context.",
	}
}

func TestChainContextFallback_FoundInLoadedFacts(t *testing.T) {
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}, {FactValue: "msg_003"}}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_002"), loaded, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow after finding value in loaded facts")
	}
}

func TestChainContextFallback_MultipleValuesAllInLoaded(t *testing.T) {
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}, {FactValue: "msg_003"}}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_001", "msg_003"), loaded, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow after finding all values in loaded facts")
	}
}

func TestChainContextFallback_FoundInDB(t *testing.T) {
	st := &chainFallbackTestStore{
		facts: map[string]bool{"task-1|task-1|msg_099": true},
	}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_099"), nil, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow after finding value in DB")
	}
}

func TestChainContextFallback_MixedLoadedAndDB(t *testing.T) {
	st := &chainFallbackTestStore{
		facts: map[string]bool{"task-1|task-1|msg_099": true},
	}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_001", "msg_099"), loaded, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow with one in loaded, one in DB")
	}
}

func TestChainContextFallback_ExplicitSession(t *testing.T) {
	st := &chainFallbackTestStore{
		facts: map[string]bool{"task-1|sess-abc|msg_099": true},
	}
	logger := slog.Default()
	task := &store.Task{Lifetime: "standing"}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_099"), nil, "task-1", task, "sess-abc")
	if !v.Allow {
		t.Error("expected allow with explicit session")
	}
}

// --- Specific missing value not found: reject even when other facts exist ---

func TestChainContextFallback_MissingValueRejectsDespiteOtherFacts(t *testing.T) {
	// Value not in loaded facts or DB. Other chain facts exist (extraction
	// may have been lossy), but we reject rather than auto-allow — the
	// specific missing value must be found, unrelated facts are not a
	// substitute. Otherwise an agent could run a list_* to populate chain
	// facts and then make out-of-scope writes.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_unknown"), loaded, "task-1", task, "")
	if v.Allow {
		t.Error("expected reject when specific missing value not in facts (even if other facts exist)")
	}
}

func TestChainContextFallback_PartialMatchStillRejects(t *testing.T) {
	// One value found in loaded facts, one not found anywhere — reject,
	// because partial coverage is not sufficient.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_001", "msg_not_extracted"), loaded, "task-1", task, "")
	if v.Allow {
		t.Error("expected reject when not all missing values are found")
	}
}

// --- Genuine violation: no chain facts at all ---

func TestChainContextFallback_GenuineViolation_NoChainFacts(t *testing.T) {
	// No chain facts loaded and value not in DB → genuine violation.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_unknown"), nil, "task-1", task, "")
	if v.Allow {
		t.Error("expected reject with no chain facts at all")
	}
}

func TestChainContextFallback_StandingTaskNoSession(t *testing.T) {
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "standing"}

	v := chainContextFallback(context.Background(), st, nil, logger, makeViolationVerdict("msg_001"), nil, "task-1", task, "")
	if v.Allow {
		t.Error("expected reject for standing task without session_id")
	}
}

// --- Tracker-driven polling behavior ---

func TestChainContextFallback_NoPending_RejectsImmediately(t *testing.T) {
	// Tracker reports no pending extraction → do not poll, reject now.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	tracker := &fakeTracker{pending: 0}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}

	start := time.Now()
	v := chainContextFallback(context.Background(), st, tracker, logger, makeViolationVerdict("msg_x"), nil, "task-1", task, "")
	elapsed := time.Since(start)

	if v.Allow {
		t.Error("expected reject when nothing pending")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected immediate reject, took %v", elapsed)
	}
}

func TestChainContextFallback_Pending_FactLandsMidPoll(t *testing.T) {
	// Tracker reports pending; the fact appears in DB after a couple of
	// polls. Fallback should allow without exhausting the full poll window.
	st := &chainFallbackTestStore{
		facts:       map[string]bool{},
		lateFact:    "msg_late",
		appearAfter: 2, // first two DB queries miss; third hits.
	}
	tracker := &fakeTracker{pending: 1}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}

	v := chainContextFallback(context.Background(), st, tracker, logger, makeViolationVerdict("msg_late"), nil, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow after fact lands mid-poll")
	}
}

func TestChainContextFallback_Pending_PollTimeoutRejects(t *testing.T) {
	// Tracker keeps reporting pending but fact never lands → reject after
	// poll window elapses.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	tracker := &fakeTracker{pending: 1}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}

	start := time.Now()
	v := chainContextFallback(context.Background(), st, tracker, logger, makeViolationVerdict("msg_never"), nil, "task-1", task, "")
	elapsed := time.Since(start)

	if v.Allow {
		t.Error("expected reject after poll timeout")
	}
	if elapsed < extractionPollInterval {
		t.Errorf("expected at least one poll interval to elapse, got %v", elapsed)
	}
	if elapsed > extractionPollMaxWait+extractionPollInterval {
		t.Errorf("elapsed %v exceeded max wait + one interval", elapsed)
	}
}

func TestChainContextFallback_Pending_DrainsBeforeTimeout(t *testing.T) {
	// Tracker reports pending initially, then drains. Fallback does one
	// final DB check after drain, which still misses → reject.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	tracker := &fakeTracker{pending: 1, drainAfter: 2}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}

	start := time.Now()
	v := chainContextFallback(context.Background(), st, tracker, logger, makeViolationVerdict("msg_x"), nil, "task-1", task, "")
	elapsed := time.Since(start)

	if v.Allow {
		t.Error("expected reject when pending drains without fact landing")
	}
	if elapsed >= extractionPollMaxWait {
		t.Errorf("expected early exit after pending drained, took %v", elapsed)
	}
}
