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
		RequiredCredentials: []runtimetasks.RequiredCredential{{
			Why: "",
		}},
		IntentVerificationMode: "unsafe",
	})

	if len(issues) < 8 {
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
		RequiredCredentials: []runtimetasks.RequiredCredential{{
			VaultItemID: "vault_github_release_bot",
			Why:         "Read GitHub issue metadata for deployment triage.",
		}},
		IntentVerificationMode: "strict",
	})

	if len(issues) != 0 {
		t.Fatalf("expected no validation issues, got %#v", issues)
	}
}

func TestValidateTaskEnvelopeRejectsCredentialWithoutVaultItem(t *testing.T) {
	issues := ValidateTaskEnvelope(runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{
			ToolName: "Bash",
			Why:      "Use the selected credential to call the provider API.",
		}},
		RequiredCredentials: []runtimetasks.RequiredCredential{{
			Why: "Call the provider API for the approved task.",
		}},
	})

	if len(issues) != 1 {
		t.Fatalf("expected one issue, got %#v", issues)
	}
	if issues[0].Field != "required_credentials_json[0].vault_item_id" {
		t.Fatalf("unexpected field %q", issues[0].Field)
	}
}
