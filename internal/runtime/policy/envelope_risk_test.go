package policy

import (
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
)

func TestAssessTaskEnvelopeFlagsBroadMutatingScope(t *testing.T) {
	assessment := AssessTaskEnvelope("sync external billing state", runtimetasks.Envelope{
		ExpectedEgress: []runtimetasks.ExpectedEgress{{
			Host:   "*.stripe.com",
			Method: "POST",
			Why:    "Apply updates as needed.",
		}},
		IntentVerificationMode: "off",
	})

	if assessment.RiskLevel != "high" {
		t.Fatalf("expected high risk, got %q", assessment.RiskLevel)
	}
	if len(assessment.Factors) == 0 {
		t.Fatal("expected risk factors for broad mutating scope")
	}
	if len(assessment.Conflicts) == 0 {
		t.Fatal("expected quality conflicts for vague rationale")
	}
}

func TestAssessTaskEnvelopeLowRiskReadOnlyScope(t *testing.T) {
	assessment := AssessTaskEnvelope("review release issues", runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{
			ToolName: "github.search_issues",
			Why:      "Look up open issues related to the release candidate.",
		}},
		ExpectedEgress: []runtimetasks.ExpectedEgress{{
			Host:   "api.github.com",
			Method: "GET",
			Path:   "/search/issues",
			Why:    "Read matching issue metadata from GitHub.",
		}},
		IntentVerificationMode: "strict",
	})

	if assessment.RiskLevel != "low" {
		t.Fatalf("expected low risk, got %q", assessment.RiskLevel)
	}
}

func TestAssessTaskEnvelopeRaisesRiskForCredentialAccess(t *testing.T) {
	assessment := AssessTaskEnvelope("create release issues", runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{
			ToolName: "Bash",
			Why:      "Call the GitHub API to create release issues.",
		}},
		RequiredCredentials: []runtimetasks.RequiredCredential{{
			VaultItemID: "vault_github_release_bot",
			Why:         "Use the GitHub release bot credential to create issues in owner/repo.",
		}},
	})

	if assessment.RiskLevel != "medium" {
		t.Fatalf("expected medium risk, got %q", assessment.RiskLevel)
	}
	if len(assessment.Factors) == 0 || assessment.Factors[0] != `task requests credential access to "vault_github_release_bot"` {
		t.Fatalf("expected credential risk factor, got %#v", assessment.Factors)
	}
}
