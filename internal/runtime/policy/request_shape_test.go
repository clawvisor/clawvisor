package policy

import (
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
)

func TestValidateTaskEnvelopeRejectsInvalidItems(t *testing.T) {
	issues := ValidateTaskEnvelope(runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{
			ToolName:   "",
			Why:        "",
			InputRegex: "(",
		}},
		ExpectedEgress: []runtimetasks.ExpectedEgress{{
			Host:      "https://api.example.com/v1",
			Why:       "",
			Method:    "FETCH",
			Path:      "/v1",
			PathRegex: "(",
		}},
		IntentVerificationMode: "unsafe",
	})

	if len(issues) < 6 {
		t.Fatalf("expected multiple validation issues, got %d: %#v", len(issues), issues)
	}
}

func TestValidateTaskEnvelopeAcceptsValidV2Envelope(t *testing.T) {
	issues := ValidateTaskEnvelope(runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{
			ToolName: "github.search",
			Why:      "Search repository issues for the deployment incident.",
		}},
		ExpectedEgress: []runtimetasks.ExpectedEgress{{
			Host:   "api.github.com",
			Method: "GET",
			Path:   "/search/issues",
			Why:    "Fetch matching issues from GitHub search.",
		}},
		IntentVerificationMode: "strict",
	})

	if len(issues) != 0 {
		t.Fatalf("expected no validation issues, got %#v", issues)
	}
}
