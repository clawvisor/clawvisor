package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// orchTestRequest is a minimal ReadOnlyRequest for orchestrator tests.
// Policies in this package can't import the policies package (cycle);
// each test wires its own minimal RequestPolicy stubs.
type orchTestRequest struct {
	provider conversation.Provider
	body     []byte
	validate func([]byte) error
}

func (r *orchTestRequest) Provider() conversation.Provider { return r.provider }
func (r *orchTestRequest) StreamShape() conversation.StreamShape {
	return conversation.StreamShapeUnknown
}
func (r *orchTestRequest) Turns() []conversation.Turn { return nil }
func (r *orchTestRequest) HTTPRequest() *http.Request { return nil }
func (r *orchTestRequest) RawBody() []byte            { return r.body }
func (r *orchTestRequest) IsFirstTurn() bool          { return true }
func (r *orchTestRequest) ConversationID() string     { return "" }
func (r *orchTestRequest) UserID() string             { return "" }
func (r *orchTestRequest) AgentID() string            { return "" }
func (r *orchTestRequest) ValidateReplacementBody(body []byte) error {
	if r.validate == nil {
		return nil
	}
	return r.validate(body)
}

// allowingPolicy is a no-op RequestPolicy that emits one audit field.
type allowingPolicy struct {
	name  string
	field string
	value any
}

func (p *allowingPolicy) Name() string { return p.name }
func (p *allowingPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			p.field: p.value,
		},
	}, nil
}

// bodyReplacingPolicy queues a ReplaceBody. Used to verify eager apply.
type bodyReplacingPolicy struct {
	name    string
	newBody []byte
}

func (p *bodyReplacingPolicy) Name() string { return p.name }
func (p *bodyReplacingPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if err := mut.ReplaceBody(p.newBody); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome:     pipeline.OutcomeAllow,
		AuditParams: map[string]any{"replaced": p.name},
	}, nil
}

// bodyObservingPolicy records the body it sees during Preprocess.
// Used to verify that earlier mutations are visible to later policies.
type bodyObservingPolicy struct {
	name string
	seen []byte
}

func (p *bodyObservingPolicy) Name() string { return p.name }
func (p *bodyObservingPolicy) Preprocess(_ context.Context, req pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	p.seen = append([]byte(nil), req.RawBody()...)
	return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
}

type bodyMutatingPolicy struct{ name string }

func (p *bodyMutatingPolicy) Name() string { return p.name }
func (p *bodyMutatingPolicy) Preprocess(_ context.Context, req pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	body := req.RawBody()
	if len(body) > 0 {
		body[0] = '['
	}
	return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
}

// denyingPolicy returns OutcomeDeny.
type denyingPolicy struct {
	name string
}

func (p *denyingPolicy) Name() string { return p.name }
func (p *denyingPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeDeny,
		Reason:  "test denied",
		AuditParams: map[string]any{
			"deny_reason": "test_deny",
		},
	}, nil
}

// shortCircuitingPolicy returns OutcomeShortCircuit with a synthetic body.
type shortCircuitingPolicy struct {
	name string
	body []byte
}

func (p *shortCircuitingPolicy) Name() string { return p.name }
func (p *shortCircuitingPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	return pipeline.RequestVerdict{
		Outcome:      pipeline.OutcomeShortCircuit,
		ShortCircuit: &pipeline.SyntheticResponse{Body: p.body, StatusCode: 200},
	}, nil
}

// erroringPolicy returns a Go error (distinct from OutcomeDeny).
type erroringPolicy struct{ name string }

func (p *erroringPolicy) Name() string { return p.name }
func (p *erroringPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	return pipeline.RequestVerdict{}, errors.New("explode")
}

// TestRunPre_AppliesPoliciesInOrderAndMergesAudit verifies the
// happy path: every policy runs, each contributes its audit field
// to the merged result, and the final body reflects all queued
// ReplaceBody calls applied in declared order.
func TestRunPre_AppliesPoliciesInOrderAndMergesAudit(t *testing.T) {
	original := []byte(`{"original":true}`)
	intermediate := []byte(`{"after_first":true}`)
	final := []byte(`{"after_second":true}`)

	observer := &bodyObservingPolicy{name: "observer"}
	policies := []pipeline.RequestPolicy{
		&bodyReplacingPolicy{name: "first", newBody: intermediate},
		observer, // should see intermediate body
		&bodyReplacingPolicy{name: "second", newBody: final},
		&allowingPolicy{name: "tagger", field: "tagger_ran", value: true},
	}

	req := &orchTestRequest{body: original}
	result, err := pipeline.RunPre(context.Background(), req, policies)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	if string(result.FinalBody) != string(final) {
		t.Errorf("FinalBody = %q, want %q", result.FinalBody, final)
	}
	if string(observer.seen) != string(intermediate) {
		t.Errorf("middle policy saw body %q, want %q (eager apply broken)", observer.seen, intermediate)
	}
	if result.AuditParams["replaced"] != "second" {
		t.Errorf("expected last writer wins on `replaced`; got %v", result.AuditParams["replaced"])
	}
	if result.AuditParams["tagger_ran"] != true {
		t.Errorf("expected tagger_ran:true in merged audit; got %v", result.AuditParams)
	}
	if len(result.Verdicts) != 4 {
		t.Errorf("expected 4 verdicts, got %d", len(result.Verdicts))
	}
}

func TestRunPre_RawBodyIsReadOnlyView(t *testing.T) {
	original := []byte(`{"original":true}`)
	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: original}
	result, err := pipeline.RunPre(context.Background(), req, []pipeline.RequestPolicy{
		&bodyMutatingPolicy{name: "mutator"},
	})
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}
	if string(result.FinalBody) != string(original) {
		t.Fatalf("FinalBody changed through RawBody alias: got %q want %q", result.FinalBody, original)
	}
}

func TestRunPre_ReplaceBodyValidatesProviderRequestShape(t *testing.T) {
	req := &orchTestRequest{
		body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		validate: func([]byte) error {
			return errors.New("parse failed")
		},
	}
	result, err := pipeline.RunPre(context.Background(), req, []pipeline.RequestPolicy{
		&bodyReplacingPolicy{name: "bad", newBody: []byte(`{"messages":`)},
	})
	if err == nil {
		t.Fatal("RunPre error = nil, want malformed replacement body rejected")
	}
	if result == nil {
		t.Fatal("RunPre result = nil")
	}
	if string(result.FinalBody) != string(req.body) {
		t.Fatalf("FinalBody = %s, want original body preserved", result.FinalBody)
	}
}

// TestRunPre_HaltsOnDeny verifies that OutcomeDeny halts the chain
// and the remaining policies don't run.
func TestRunPre_HaltsOnDeny(t *testing.T) {
	observer := &bodyObservingPolicy{name: "should_not_run"}
	policies := []pipeline.RequestPolicy{
		&allowingPolicy{name: "first", field: "first_ran", value: true},
		&denyingPolicy{name: "denier"},
		observer,
	}

	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: []byte(`{}`)}
	result, err := pipeline.RunPre(context.Background(), req, policies)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	if result.DenyReason != "test denied" {
		t.Errorf("DenyReason = %q, want \"test denied\"", result.DenyReason)
	}
	if result.DeniedBy != "denier" {
		t.Errorf("DeniedBy = %q, want denier", result.DeniedBy)
	}
	if observer.seen != nil {
		t.Errorf("policy after Deny ran (saw body %q)", observer.seen)
	}
	if len(result.Verdicts) != 2 {
		t.Errorf("expected 2 verdicts (first + denier), got %d", len(result.Verdicts))
	}
	if result.AuditParams["first_ran"] != true {
		t.Errorf("first policy's audit field lost")
	}
	if result.AuditParams["deny_reason"] != "test_deny" {
		t.Errorf("denier's audit field lost")
	}
}

// TestRunPre_HaltsOnShortCircuit verifies that OutcomeShortCircuit
// populates ShortCircuit and halts the chain.
func TestRunPre_HaltsOnShortCircuit(t *testing.T) {
	observer := &bodyObservingPolicy{name: "should_not_run"}
	syntheticBody := []byte(`{"synthetic":true}`)
	policies := []pipeline.RequestPolicy{
		&allowingPolicy{name: "first", field: "first_ran", value: true},
		&shortCircuitingPolicy{name: "synth", body: syntheticBody},
		observer,
	}

	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: []byte(`{}`)}
	result, err := pipeline.RunPre(context.Background(), req, policies)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	if result.ShortCircuit == nil {
		t.Fatalf("expected ShortCircuit populated")
	}
	if string(result.ShortCircuit.Body) != string(syntheticBody) {
		t.Errorf("ShortCircuit.Body = %q, want %q", result.ShortCircuit.Body, syntheticBody)
	}
	if observer.seen != nil {
		t.Errorf("policy after ShortCircuit ran")
	}
}

// TestRunPre_ObserveDowngradesShortCircuit verifies that under Observe mode a
// non-exempt policy's OutcomeShortCircuit is downgraded to a recorded
// observation: ShortCircuit stays nil, the chain continues, the Observed trail
// captures it, AND AuditParams["observed"] is stamped true (parity with the
// deny and body-rewrite downgrade paths, so the endpoint audit row can render
// the observed badge).
func TestRunPre_ObserveDowngradesShortCircuit(t *testing.T) {
	after := &bodyObservingPolicy{name: "runs_after"}
	policies := []pipeline.RequestPolicy{
		&allowingPolicy{name: "first", field: "first_ran", value: true},
		// "secret_hold" is an enforcing (non-exempt) policy whose
		// short-circuit is a would-be approval hold.
		&shortCircuitingPolicy{name: "secret_hold", body: []byte(`{"synthetic":true}`)},
		after,
	}

	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: []byte(`{}`)}
	result, err := pipeline.RunPre(pipeline.WithObserveMode(context.Background()), req, policies)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}

	if result.ShortCircuit != nil {
		t.Fatalf("observe mode must not apply the short-circuit; got %q", result.ShortCircuit.Body)
	}
	if after.seen == nil {
		t.Fatalf("observe mode must continue the chain past a downgraded short-circuit")
	}
	if len(result.Observed) != 1 || result.Observed[0].Policy != "secret_hold" {
		t.Fatalf("expected the short-circuit recorded in Observed, got %+v", result.Observed)
	}
	if result.AuditParams["observed"] != true {
		t.Fatalf("expected AuditParams[observed]=true on a downgraded short-circuit, got %v", result.AuditParams["observed"])
	}
}

// TestRunPre_PropagatesPolicyError verifies that a policy returning a
// Go error halts the chain with the wrapped error.
func TestRunPre_PropagatesPolicyError(t *testing.T) {
	policies := []pipeline.RequestPolicy{
		&allowingPolicy{name: "first", field: "first_ran", value: true},
		&erroringPolicy{name: "exploder"},
		&allowingPolicy{name: "third", field: "third_ran", value: true},
	}

	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: []byte(`{}`)}
	result, err := pipeline.RunPre(context.Background(), req, policies)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if result == nil {
		t.Fatalf("expected partial result on policy error")
	}
	if result.AuditParams["first_ran"] != true {
		t.Fatalf("prior audit params were dropped on policy error: %+v", result.AuditParams)
	}
	if _, ok := result.AuditParams["third_ran"]; ok {
		t.Fatalf("policy after error ran unexpectedly: %+v", result.AuditParams)
	}
}

// TestRunPre_NilRequestErrors guards against the obvious misuse.
func TestRunPre_NilRequestErrors(t *testing.T) {
	_, err := pipeline.RunPre(context.Background(), nil, []pipeline.RequestPolicy{})
	if err == nil {
		t.Fatalf("expected error for nil request, got nil")
	}
}

// stripsTurnsPolicy queues a StripTurns mutation — used to verify that
// an un-overridden RequestMutator method panics, matching the Phase 1
// contract.
type stripsTurnsPolicy struct{ name string }

func (p *stripsTurnsPolicy) Name() string { return p.name }
func (p *stripsTurnsPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	_ = mut.StripTurns(func(pipeline.StripContext) bool { return false })
	return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
}

// TestRunPre_UnimplementedMutatorMethodPanics verifies that calling
// an un-overridden RequestMutator method (e.g., StripTurns, which no
// migrated policy uses yet) panics — the explicit signal that the
// migration hasn't reached this method yet.
func TestRunPre_UnimplementedMutatorMethodPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic from un-implemented mutator method")
		}
	}()
	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: []byte(`{}`)}
	_, _ = pipeline.RunPre(context.Background(), req, []pipeline.RequestPolicy{
		&stripsTurnsPolicy{name: "strip"},
	})
}

// suppress unused import in some test build configurations
var _ = json.RawMessage(nil)
