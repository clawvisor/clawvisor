package pipeline_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

func TestAuditEventsMarksOnlyRecordedWinnerWithDuplicateEvaluatorNames(t *testing.T) {
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {Outcome: pipeline.OutcomeAllow, Reason: "winner"},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{
				EvaluatorName: "duplicate",
				ToolUseID:     "toolu_1",
				Verdict:       pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip, Reason: "skip"},
			},
			{
				EvaluatorName: "duplicate",
				ToolUseID:     "toolu_1",
				Verdict:       pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeAllow, Reason: "winner"},
				Winning:       true,
			},
		},
	}

	events := result.AuditEvents([]conversation.ToolUse{{ID: "toolu_1", Name: "Bash"}})
	if len(events) != 2 {
		t.Fatalf("AuditEvents length = %d, want 2", len(events))
	}
	if events[0].Winning {
		t.Fatal("skip evaluation with duplicate evaluator name was marked winning")
	}
	if !events[1].Winning {
		t.Fatal("recorded winning evaluation was not marked winning")
	}
}
