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

// --- Extraction incomplete: value not found anywhere, but chain facts exist ---

func TestChainContextFallback_ExtractionIncomplete(t *testing.T) {
	// Value not in loaded facts or DB, but loaded facts exist →
	// extraction was lossy, should allow rather than hard-reject.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}}

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_unknown"), loaded, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow when extraction is incomplete (chain facts exist but value missing)")
	}
	if v.ParamScope != "ok" {
		t.Errorf("expected param_scope=ok, got %q", v.ParamScope)
	}
}

func TestChainContextFallback_ExtractionIncomplete_MultipleOneMissing(t *testing.T) {
	// One value found in loaded facts, one not found anywhere — but chain
	// facts exist so extraction was lossy.
	st := &chainFallbackTestStore{facts: map[string]bool{}}
	logger := slog.Default()
	task := &store.Task{Lifetime: "session"}
	loaded := []store.ChainFact{{FactValue: "msg_001"}, {FactValue: "msg_002"}}

	v := chainContextFallback(context.Background(), st, logger, makeViolationVerdict("msg_001", "msg_not_extracted"), loaded, "task-1", task, "")
	if !v.Allow {
		t.Error("expected allow when one value found and extraction incomplete")
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
