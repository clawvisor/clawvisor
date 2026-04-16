package handlers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// chainFallbackTestStore is a minimal store for chain fallback tests.
type chainFallbackTestStore struct {
	store.Store // embed nil interface; only override needed methods
	facts       map[string]bool
}

func (s *chainFallbackTestStore) ChainFactValueExists(_ context.Context, taskID, sessionID, value string) (bool, error) {
	return s.facts[taskID+"|"+sessionID+"|"+value], nil
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_002"), loaded, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow after finding value in loaded facts")
	}
}

func TestChainContextFallback_MultipleValuesAllInLoaded(t *testing.T) {
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}, {FactValue: "msg_003"}}

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_001", "msg_003"), loaded, "task-1", task, "")
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_099"), nil, "task-1", task, "")
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_001", "msg_099"), loaded, "task-1", task, "")
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_099"), nil, "task-1", task, "sess-abc")
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_unknown"), loaded, "task-1", task, "")
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_001", "msg_not_extracted"), loaded, "task-1", task, "")
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

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_unknown"), nil, "task-1", task, "")
	if v.Allow {
		t.Error("expected reject with no chain facts at all")
	}
}

func TestChainContextFallback_StandingTaskNoSession(t *testing.T) {
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "standing"}

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_001"), nil, "task-1", task, "")
	if v.Allow {
		t.Error("expected reject for standing task without session_id")
	}
}
