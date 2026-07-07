package proxy

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestRuntimeHostCategory covers the LLM host bucketing, in particular that
// Bedrock regional endpoints (bedrock-runtime.<region>.amazonaws.com) map to
// "llm" and not "other".
func TestRuntimeHostCategory(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"api.anthropic.com", "llm"},
		{"api.openai.com", "llm"},
		{"generativelanguage.googleapis.com", "llm"},
		{"aiplatform.googleapis.com", "llm"},
		// Bedrock regional endpoints — the reported gap.
		{"bedrock-runtime.us-east-1.amazonaws.com", "llm"},
		{"bedrock-runtime.eu-west-1.amazonaws.com", "llm"},
		{"bedrock-runtime.ap-southeast-2.amazonaws.com:443", "llm"},
		{"BEDROCK-RUNTIME.US-EAST-1.AMAZONAWS.COM", "llm"},
		// Non-LLM AWS hosts must not be swept in by the bedrock rule.
		{"s3.us-east-1.amazonaws.com", "other"},
		{"example.com", "other"},
		{"", "other"},
	}
	for _, tc := range cases {
		if got := runtimeHostCategory(tc.host); got != tc.want {
			t.Errorf("runtimeHostCategory(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// TestRuntimeProxyDecision asserts the decision attribute is driven by the
// explicit PolicyDenied marker (a Clawvisor synthetic 403), not by any HTTP
// 403 — so a genuine upstream 403 (bad API key) is not mislabeled as a
// Clawvisor denial.
func TestRuntimeProxyDecision(t *testing.T) {
	cases := []struct {
		name string
		st   *RequestState
		want string
	}{
		{
			name: "policy denied marks denied",
			st:   &RequestState{PolicyDenied: true, Session: &store.RuntimeSession{}},
			want: "denied",
		},
		{
			name: "upstream 403 without marker stays allowed",
			// No PolicyDenied marker: an upstream 403 must not count as a
			// Clawvisor denial.
			st:   &RequestState{PolicyDenied: false, Session: &store.RuntimeSession{}},
			want: "allowed",
		},
		{
			name: "observation mode is observed",
			st:   &RequestState{Session: &store.RuntimeSession{ObservationMode: true}},
			want: "observed",
		},
		{
			name: "policy deny takes precedence over observation mode",
			st:   &RequestState{PolicyDenied: true, Session: &store.RuntimeSession{ObservationMode: true}},
			want: "denied",
		},
	}
	for _, tc := range cases {
		if got := runtimeProxyDecision(tc.st); got != tc.want {
			t.Errorf("%s: runtimeProxyDecision = %q, want %q", tc.name, got, tc.want)
		}
	}
}
