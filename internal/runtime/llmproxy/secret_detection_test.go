package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestScanInboundSecrets_DoesNotRewriteToolSchemaNames(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__codex_apps__github",
				"tools":[
					{"type":"function","name":"_compare_commits"}
				]
			}
		],
		"input":[{"role":"user","content":[{"type":"input_text","text":"inspect the branch"}]}]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if found {
		t.Fatalf("tool schema should not produce findings: %+v", scan.Findings)
	}
	if scan.RedactedBody != nil && bytes.Contains(scan.RedactedBody, []byte("[redacted secret:resend]")) {
		t.Fatalf("tool schema name was redacted: %s", scan.RedactedBody)
	}
}

func TestScanInboundSecrets_StillDetectsKnownPrefixInUserContent(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"tools":[{"type":"function","name":"shell"}],
		"input":[{"role":"user","content":[{"type":"input_text","text":"resend key re_1234567890abcdefABCDEF"}]}]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if !found || len(scan.Findings) != 1 {
		t.Fatalf("expected one finding in user content, found=%v findings=%+v", found, scan.Findings)
	}
	if scan.Findings[0].Service != "resend" || scan.Findings[0].Source != "known_prefix" {
		t.Fatalf("unexpected finding: %+v", scan.Findings[0])
	}
	if !bytes.Contains(scan.RedactedBody, []byte("[redacted secret:resend]")) {
		t.Fatalf("expected redacted user content, got %s", scan.RedactedBody)
	}
}

func TestScanInboundSecrets_KnownPrefixRequiresTokenBoundary(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[{"role":"user","content":[{"type":"input_text","text":"ignore identifiers required_credentials_json re_escalated _re_hidden xre_embedded but catch re_1234567890abcdefABCDEF"}]}]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if !found || len(scan.Findings) != 1 {
		t.Fatalf("expected only the standalone resend token, found=%v findings=%+v", found, scan.Findings)
	}
	if scan.Findings[0].Value != "re_1234567890abcdefABCDEF" {
		t.Fatalf("unexpected finding: %+v", scan.Findings[0])
	}
	redacted := string(scan.RedactedBody)
	for _, kept := range []string{"required_credentials_json", "re_escalated", "_re_hidden", "xre_embedded"} {
		if !strings.Contains(redacted, kept) {
			t.Fatalf("expected %q preserved in %s", kept, redacted)
		}
	}
}

func TestScanInboundSecrets_KnownPrefixMatrix(t *testing.T) {
	for _, spec := range runtimeautovault.KnownPrefixSpecs() {
		spec := spec
		t.Run(spec.Prefix, func(t *testing.T) {
			value := exampleKnownPrefixSecret(spec.Prefix)
			body := openAIResponsesUserBody("Use this credential for the requested service: " + value)

			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: conversation.ProviderOpenAI,
				Body:     body,
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if !found || len(scan.Findings) != 1 {
				t.Fatalf("expected one finding for prefix %q, found=%v findings=%+v", spec.Prefix, found, scan.Findings)
			}
			finding := scan.Findings[0]
			if finding.Value != value || finding.Service != spec.Service || finding.Source != "known_prefix" {
				t.Fatalf("unexpected finding for prefix %q: %+v", spec.Prefix, finding)
			}
			if bytes.Contains(scan.RedactedBody, []byte(value)) {
				t.Fatalf("known-prefix value was not redacted: %s", scan.RedactedBody)
			}
		})
	}
}

func TestScanInboundSecrets_PrefixLikeIdentifiersDoNotTrigger(t *testing.T) {
	cases := []string{
		"pre_loading",
		"required_credentials_json",
		"github_patent_pending",
		"ghp_loading_state",
		"xoxb_pending_review",
		"xoxp_connection_state",
		"sk_live_mode",
		"sk_test_fixture",
		"sk-ant-feature-flag",
		"re_escalated",
	}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			body := openAIResponsesUserBody("This ordinary identifier appeared in logs: " + value)

			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: conversation.ProviderOpenAI,
				Body:     body,
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if found {
				t.Fatalf("identifier %q should not produce findings: %+v", value, scan.Findings)
			}
		})
	}
}

func TestScanInboundSecrets_ProtocolAndArtifactIdentifiersDoNotTrigger(t *testing.T) {
	cases := []string{
		"cv-nonce-pr7vwci4umcgs6rsj3lika4vve",
		"toolu_01SpEwDzqeBxzbMBZ3gq8ECs",
		"msg_01HX9N7G3F4T5K6R8S9A0B1C2D",
		"req_8gyXD1ddhvF8iEFwrt9f3ywd",
		"chatcmpl_AbCdEf123456789xyz",
		"asst_abcDEF1234567890xyz",
		"thread_abcDEF1234567890xyz",
		"run_abcDEF1234567890xyz",
		"step_abcDEF1234567890xyz",
		"call_abcDEF1234567890xyz",
		"clear_thinking_01HX9N7G3F4T5K6R8S9A0B1C2D",
		"775b2d9f-dedc-4e83-994f-94d3b5809eaa",
		"NODE_TLS_REJECT_UNAUTHORIZED",
		"account-snapshot-fields-B44aMyxd",
		"assets/index-hC1UZmyq.js",
		"wp-block-button__width-25",
		"rbawri@example.com",
	}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			body := openAIResponsesUserBody("This non-secret artifact appeared in the transcript: " + value)

			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: conversation.ProviderOpenAI,
				Body:     body,
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if found {
				t.Fatalf("artifact %q should not produce findings: %+v", value, scan.Findings)
			}
		})
	}
}

func TestScanInboundSecrets_DetectsSecretsAcrossProviderTranscriptShapes(t *testing.T) {
	cases := []struct {
		name     string
		provider conversation.Provider
		body     []byte
		service  string
	}{
		{
			name:     "openai responses input array",
			provider: conversation.ProviderOpenAI,
			body:     openAIResponsesUserBody("Use this GitHub token: ghp_1234567890ABCDEFabcdef1234567890ABCDEF"),
			service:  "github",
		},
		{
			name:     "openai chat messages",
			provider: conversation.ProviderOpenAI,
			body: mustJSON(map[string]any{
				"model": "gpt-5.4",
				"messages": []map[string]any{{
					"role":    "user",
					"content": "Use this Slack token: " + exampleKnownPrefixSecret("xoxb-"),
				}},
			}),
			service: "slack",
		},
		{
			name:     "anthropic string content",
			provider: conversation.ProviderAnthropic,
			body: mustJSON(map[string]any{
				"model": "claude-sonnet-4",
				"messages": []map[string]any{{
					"role":    "user",
					"content": "Use this Anthropic key: sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
				}},
			}),
			service: "anthropic",
		},
		{
			name:     "anthropic content blocks",
			provider: conversation.ProviderAnthropic,
			body: mustJSON(map[string]any{
				"model": "claude-sonnet-4",
				"messages": []map[string]any{{
					"role": "user",
					"content": []map[string]any{{
						"type": "text",
						"text": "Use this Stripe key: " + exampleKnownPrefixSecret("sk_live_"),
					}},
				}},
			}),
			service: "stripe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: tc.provider,
				Body:     tc.body,
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if !found || len(scan.Findings) != 1 {
				t.Fatalf("expected one finding, found=%v findings=%+v", found, scan.Findings)
			}
			if scan.Findings[0].Service != tc.service {
				t.Fatalf("service=%q, want %q; finding=%+v", scan.Findings[0].Service, tc.service, scan.Findings[0])
			}
		})
	}
}

func TestScanInboundSecrets_PasswordRevealAndContextHeuristics(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		source  string
		service string
	}{
		{
			name:    "password reveal",
			text:    "The Stripe API key is: ExampleStripe_8gyXD1ddhvF8iEFwrt9f3ywd",
			source:  "password_reveal",
			service: "stripe",
		},
		{
			name:    "bearer context",
			text:    "Set Authorization: Bearer ExampleService_8gyXD1ddhvF8iEFwrt9f3ywd for this request",
			source:  "heuristic_swap",
			service: "captured",
		},
		{
			name:    "api token context",
			text:    "Here is the API token ExampleService_9hyYE2eeivG9jFGxsu0g4zxe for the service.",
			source:  "heuristic_swap",
			service: "captured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: conversation.ProviderOpenAI,
				Body:     openAIResponsesUserBody(tc.text),
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if !found || len(scan.Findings) != 1 {
				t.Fatalf("expected one finding, found=%v findings=%+v", found, scan.Findings)
			}
			finding := scan.Findings[0]
			if finding.Source != tc.source || finding.Service != tc.service {
				t.Fatalf("unexpected finding: %+v, want source=%q service=%q", finding, tc.source, tc.service)
			}
		})
	}
}

func TestScanInboundSecrets_AdjudicatorControlsAmbiguousCandidates(t *testing.T) {
	cases := []struct {
		name    string
		verdict runtimeautovault.SecretAdjudicationVerdict
		want    bool
	}{
		{
			name: "negative verdict suppresses ambiguous candidate",
			verdict: runtimeautovault.SecretAdjudicationVerdict{
				Credential: false,
				Confidence: 0.9,
				Service:    "not_secret",
			},
			want: false,
		},
		{
			name: "positive verdict captures ambiguous candidate",
			verdict: runtimeautovault.SecretAdjudicationVerdict{
				Credential: true,
				Confidence: 0.91,
				Service:    "custom_service",
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: conversation.ProviderOpenAI,
				Body:     openAIResponsesUserBody("The opaque value appeared in output: AmbiguousValue_8gyXD1ddhvF8iEFwrt9f3ywd"),
				Adjudicator: staticSecretAdjudicator{
					verdict: tc.verdict,
				},
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if found != tc.want {
				t.Fatalf("found=%v, want %v, findings=%+v", found, tc.want, scan.Findings)
			}
			if found {
				if len(scan.Findings) != 1 || scan.Findings[0].Source != "heuristic_adjudicated" || scan.Findings[0].Service != "custom_service" {
					t.Fatalf("unexpected adjudicated finding: %+v", scan.Findings)
				}
			}
		})
	}
}

func TestScanInboundSecrets_IgnoresEncryptedReasoningContent(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"reasoning","encrypted_content":"gAAAAABqB_echVKYkwxeJ3VeLEelJHCYJGOugJoTI9x-6ZUoP44yoPB-QU1-CFNvNm5mJJukWH0d09iOl_vZzPQ_f2isMHDl8Bh_4yVHx-NhQngtaoxrLfWWIFMpHeQv3-fbWElcMG_Who1OUo4YeW9bEVJAVnIXl6oWXDXeuzvF1U-vFnNFSHAXLXwckN5CQrdSjvf_NOjnfFytB_z7fKf9TUZtpNse1ZyML9gRaOTGd_K0EYYutKAjnZ3kjH4yc04E6Aq7VQNYSXdXYM0zXW8zv2X1RvcVJjPk2nQLZ6LGikYHYxsday59Wtc3GqgAKR8w9DQAHY8zFZxN60hNv"},
			{"role":"user","content":[{"type":"input_text","text":"continue checking resend emails"}]}
		]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if found {
		t.Fatalf("encrypted reasoning content should not produce findings: %+v", scan.Findings)
	}
}

func TestScanInboundSecrets_IgnoresClawvisorGeneratedBlocks(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"assistant","content":[{"type":"output_text","text":"Clawvisor wants to create a task to cover this work:\n\nPurpose\n  Use the provided Resend API key autovault_resend_abc123 to inspect emails.\n\nTools requested\n  • exec_command — Call curl using the provided autovault autovault_resend_abc123.\n\n[clawvisor:approval=cv-ly5jdelgmj46hiwfcxw5e3sfjy]"}]},
			{"role":"assistant","content":[{"type":"output_text","text":"[Clawvisor: inline task was created and approved by the user inline. Credential placeholders granted for this task: resend=autovault_resend_xyz789; use these exact placeholder values in Authorization headers or curl arguments.]"}]},
			{"role":"assistant","content":[{"type":"output_text","text":"Clawvisor detected a possible raw secret in the last message.\n\nSuggested vault name: ` + "`resend`" + `\nDetection source: known_prefix\n\n[clawvisor:secret=cv-secret-rawlog]"}]},
			{"role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if found {
		t.Fatalf("Clawvisor-generated assistant blocks should not produce findings: %+v", scan.Findings)
	}
}

func TestScanInboundSecrets_IgnoresClawvisorNonceInPriorToolArguments(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"Check the recent emails in Resend using the vaulted credential placeholder."}]},
			{"type":"function_call","name":"exec_command","arguments":"curl -sS -H 'X-Clawvisor-Target-Host: api.resend.com' -H 'X-Clawvisor-Caller: Bearer cv-nonce-pr7vwci4umcgs6rsj3lika4vve' http://localhost:25297/proxy/v1/emails -H 'Authorization: Bearer autovault_resend_1_example'","call_id":"call_abc123"},
			{"type":"function_call_output","call_id":"call_abc123","output":"{\"statusCode\":400,\"message\":\"API key is invalid\",\"name\":\"validation_error\"}"},
			{"role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if found {
		t.Fatalf("Clawvisor nonce in prior tool arguments should not produce findings: %+v", scan.Findings)
	}
}

func TestScanInboundSecrets_IgnoresToolResultIdentifiers(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"Use the vaulted Resend placeholder to list email records."}]},
			{"type":"function_call","name":"exec_command","arguments":"curl -sS https://api.resend.com/emails -H 'Authorization: Bearer autovault_resend_example'","call_id":"call_abc123"},
			{"type":"function_call_output","call_id":"call_abc123","output":"{\"object\":\"list\",\"data\":[{\"id\":\"775b2d9f-dedc8f9a1b2c3d4e8a58\",\"subject\":\"Clawvisor: confirm your email address\",\"last_event\":\"delivered\",\"reset_token\":\"mailtok_abcd1234efgh5678ijkl9012\"}]}"},
			{"role":"user","content":[{"type":"input_text","text":"summarize what came back"}]}
		]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if found {
		t.Fatalf("tool result identifiers should not produce findings: %+v", scan.Findings)
	}
}

func TestScanInboundSecrets_IgnoresProviderToolResultSubtrees(t *testing.T) {
	cases := []struct {
		name     string
		provider conversation.Provider
		body     []byte
	}{
		{
			name:     "openai function_call_output",
			provider: conversation.ProviderOpenAI,
			body: mustJSON(map[string]any{
				"model": "gpt-5.4",
				"input": []map[string]any{
					{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "summarize tool output"}}},
					{"type": "function_call_output", "call_id": "call_abc123", "output": `{"access_token":"ToolOutput_8gyXD1ddhvF8iEFwrt9f3ywd","id":"775b2d9f-dedc-4e83-994f-94d3b5809eaa"}`},
				},
			}),
		},
		{
			name:     "openai role tool",
			provider: conversation.ProviderOpenAI,
			body: mustJSON(map[string]any{
				"model": "gpt-5.4",
				"messages": []map[string]any{
					{"role": "user", "content": "summarize tool output"},
					{"role": "tool", "content": `{"api_key":"ToolOutput_8gyXD1ddhvF8iEFwrt9f3ywd"}`},
				},
			}),
		},
		{
			name:     "anthropic tool_result",
			provider: conversation.ProviderAnthropic,
			body: mustJSON(map[string]any{
				"model": "claude-sonnet-4",
				"messages": []map[string]any{
					{
						"role": "user",
						"content": []map[string]any{{
							"type":        "tool_result",
							"tool_use_id": "toolu_abc123",
							"content":     "api token ToolOutput_8gyXD1ddhvF8iEFwrt9f3ywd",
						}},
					},
				},
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
				Provider: tc.provider,
				Body:     tc.body,
			})
			if err != nil {
				t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
			}
			if found {
				t.Fatalf("tool result subtree should not produce findings: %+v", scan.Findings)
			}
		})
	}
}

func TestScanInboundSecrets_DetectsSameTokenInUserMessage(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"Here is an api token: mailtok_abcd1234efgh5678ijkl9012"}]}
		]
	}`)

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if !found || len(scan.Findings) != 1 {
		t.Fatalf("expected user-provided token to produce one finding, found=%v findings=%+v", found, scan.Findings)
	}
	if scan.Findings[0].Source != "heuristic_swap" {
		t.Fatalf("unexpected finding source: %+v", scan.Findings[0])
	}
}

type staticSecretAdjudicator struct {
	verdict runtimeautovault.SecretAdjudicationVerdict
}

func (a staticSecretAdjudicator) AdjudicateSecret(context.Context, runtimeautovault.SecretAdjudicationRequest) (runtimeautovault.SecretAdjudicationResult, error) {
	return runtimeautovault.SecretAdjudicationResult{Verdict: a.verdict}, nil
}

func openAIResponsesUserBody(text string) []byte {
	return mustJSON(map[string]any{
		"model": "gpt-5.4",
		"input": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": text,
			}},
		}},
	})
}

func mustJSON(value any) []byte {
	out, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return out
}

func exampleKnownPrefixSecret(prefix string) string {
	switch prefix {
	case "sk-ant-":
		return "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
	case "ghp_":
		return "ghp_1234567890ABCDEFabcdef1234567890ABCDEF"
	case "github_pat_":
		return "github_pat_11AABBCCDDEEFF0011223344556677889900"
	case "sk-":
		return "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz1234567890"
	case "re_":
		return "re_1234567890abcdefABCDEF"
	case "xoxb-":
		return prefix + "123456789012-123456789012-AbCdEfGhIjKlMnOpQrStUvWx"
	case "xoxp-":
		return prefix + "123456789012-123456789012-AbCdEfGhIjKlMnOpQrStUvWx"
	case "sk_live_":
		return prefix + "51NabcDEFghijkLMNOPqrstuvWXYZ1234567890"
	case "sk_test_":
		return prefix + "51NabcDEFghijkLMNOPqrstuvWXYZ1234567890"
	default:
		return prefix + "AbCdEfGhIjKlMnOpQrStUvWxYz1234567890"
	}
}

func TestScanInboundSecrets_RawLogShapedVaultReplayDoesNotReprompt(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"developer","content":[{"type":"input_text","text":"Use required_credentials_json when credentials are needed. Avoid re_escalated false positives."}]},
			{"role":"user","content":[{"type":"input_text","text":"Can you use this API key to check the emails in resend? autovault_resend_stable"}]},
			{"role":"assistant","content":[{"type":"output_text","text":"Clawvisor detected a possible raw secret in the last message.\n\nSuggested vault name: ` + "`resend`" + `\nDetection source: password_reveal\n\n[clawvisor:secret=cv-secret-rawlog]"}]},
			{"role":"user","content":[{"type":"input_text","text":"vault resend"}]},
			{"type":"reasoning","encrypted_content":"gAAAAABqB_echVKYkwxeJ3VeLEelJHCYJGOugJoTI9x-6ZUoP44yoPB-QU1-CFNvNm5mJJukWH0d09iOl_vZzPQ_f2isMHDl8Bh_4yVHx-NhQngtaoxrLfWWIFMpHeQv3-fbWElcMG_Who1OUo4YeW9bEVJAVnIXl6oWXDXeuzvF1U-vFnNFSHAXLXwckN5CQrdSjvf_NOjnfFytB_z7fKf9TUZtpNse1ZyML9gRaOTGd_K0EYYutKAjnZ3kjH4yc04E6Aq7VQNYSXdXYM0zXW8zv2X1RvcVJjPk2nQLZ6LGikYHYxsday59Wtc3GqgAKR8w9DQAHY8zFZxN60hNv"},
			{"role":"assistant","content":[{"type":"output_text","text":"I will call the Resend API using the provided vault placeholder."}]},
			{"role":"user","content":[{"type":"input_text","text":"Tool output mentioned re_commits and re_escalated; continue."}]}
		]
	}`)

	stripped, err := StripSecretDecisionHistory(SecretDecisionHistoryStripRequest{
		Provider: conversation.ProviderOpenAI,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("StripSecretDecisionHistory: %v", err)
	}
	if !stripped.Modified {
		t.Fatalf("expected raw-log-shaped decision history to be stripped")
	}
	strippedText := string(stripped.Body)
	for _, forbidden := range []string{"Clawvisor detected a possible raw secret", "vault resend", "[clawvisor:secret="} {
		if strings.Contains(strippedText, forbidden) {
			t.Fatalf("expected %q stripped from %s", forbidden, strippedText)
		}
	}

	scan, found, err := ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider: conversation.ProviderOpenAI,
		Body:     stripped.Body,
	})
	if err != nil {
		t.Fatalf("ScanInboundSecretsWithOptions: %v", err)
	}
	if found {
		t.Fatalf("remembered vault replay should not produce fresh secret findings: %+v", scan.Findings)
	}
}

func TestMemoryPendingSecretDecisionCachePreservesMultiplePendingForSameScope(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingSecretDecisionCache(0)

	first, err := cache.HoldSecret(ctx, PendingSecretDecision{
		UserID:       "user-1",
		AgentID:      "agent-1",
		Provider:     conversation.ProviderAnthropic,
		OriginalBody: []byte(`{"first":true}`),
	})
	if err != nil {
		t.Fatalf("HoldSecret(first): %v", err)
	}
	second, err := cache.HoldSecret(ctx, PendingSecretDecision{
		UserID:       "user-1",
		AgentID:      "agent-1",
		Provider:     conversation.ProviderAnthropic,
		OriginalBody: []byte(`{"second":true}`),
	})
	if err != nil {
		t.Fatalf("HoldSecret(second): %v", err)
	}
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("expected distinct generated decision IDs, first=%q second=%q", first.ID, second.ID)
	}

	resolvedA, err := cache.ResolveSecret(ctx, "user-1", "agent-1", conversation.ProviderAnthropic)
	if err != nil {
		t.Fatalf("ResolveSecret(first resolution): %v", err)
	}
	resolvedB, err := cache.ResolveSecret(ctx, "user-1", "agent-1", conversation.ProviderAnthropic)
	if err != nil {
		t.Fatalf("ResolveSecret(second resolution): %v", err)
	}
	if resolvedA == nil || resolvedB == nil {
		t.Fatalf("multiple pending decisions for the same scope must both remain resolvable; got first=%+v second=%+v", resolvedA, resolvedB)
	}
	if resolvedA.ID == resolvedB.ID {
		t.Fatalf("expected two different decisions to resolve, got %+v and %+v", resolvedA, resolvedB)
	}
}
