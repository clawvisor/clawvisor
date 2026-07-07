package llmproxy

import (
	"net/http"
	"testing"
)

func reqWithHeaders(h map[string]string) *http.Request {
	r, _ := http.NewRequest("POST", "http://x/api/v1/messages", nil)
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func TestClassifyUpstreamCredential(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    CredentialClass
	}{
		{"no credential", map[string]string{"X-Clawvisor-Agent-Token": "cvis_abc"}, CredentialNone},
		{"cvis in authorization only", map[string]string{"Authorization": "Bearer cvis_abc"}, CredentialNone},
		{"cvis in x-api-key only", map[string]string{"x-api-key": "cvis_abc"}, CredentialNone},
		{"anthropic api key via x-api-key", map[string]string{"x-api-key": "sk-ant-api03-realkey"}, CredentialOrgAPIKey},
		{"anthropic api key via authorization", map[string]string{"Authorization": "Bearer sk-ant-api03-realkey"}, CredentialOrgAPIKey},
		{"openai api key via authorization", map[string]string{"Authorization": "Bearer sk-proj-openaikey"}, CredentialOrgAPIKey},
		{"subscription access token", map[string]string{"Authorization": "Bearer sk-ant-oat01-subscription"}, CredentialSubscription},
		{"subscription refresh token", map[string]string{"Authorization": "Bearer sk-ant-ort01-refresh"}, CredentialSubscription},
		{"opaque bearer fails closed", map[string]string{"Authorization": "Bearer some-opaque-token"}, CredentialUnrecognized},
		{"jwt bearer fails closed", map[string]string{"Authorization": "Bearer eyJhbGc.eyJzdWI.sig"}, CredentialUnrecognized},
		// F2 reverse: a real API key + stray anthropic-beta oauth header is
		// STILL an org API key — the header alone never forces refusal.
		{"api key with oauth beta header not dosed", map[string]string{
			"Authorization":  "Bearer sk-ant-api03-realkey",
			"anthropic-beta": "oauth-2025-01-01",
		}, CredentialOrgAPIKey},
		// F3: unknown sk-ant- variant that isn't api/oat/ort — fail closed.
		{"unknown sk-ant variant fails closed", map[string]string{"Authorization": "Bearer sk-ant-xyz01-weird"}, CredentialUnrecognized},
		// cvis in x-api-key but a real key in Authorization: classify the bearer.
		{"cvis xapikey + subscription bearer", map[string]string{
			"x-api-key":     "cvis_abc",
			"Authorization": "Bearer sk-ant-oat01-sub",
		}, CredentialSubscription},
		// Case-insensitive scheme: a lowercase `bearer` prefix on a
		// subscription token must NOT slip past to CredentialNone (which would
		// bypass the §4c refusal and get silently vault-injected).
		{"lowercase bearer scheme subscription", map[string]string{"Authorization": "bearer sk-ant-oat01-sub"}, CredentialSubscription},
		{"mixed-case bearer scheme org key", map[string]string{"Authorization": "BeArEr sk-ant-api03-realkey"}, CredentialOrgAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyUpstreamCredential(reqWithHeaders(tc.headers)); got != tc.want {
				t.Fatalf("ClassifyUpstreamCredential=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestHasClientProviderCredential(t *testing.T) {
	if HasClientProviderCredential(reqWithHeaders(map[string]string{"X-Clawvisor-Agent-Token": "cvis_x"})) {
		t.Fatal("cvis-only request must not count as a provider credential")
	}
	if HasClientProviderCredential(reqWithHeaders(map[string]string{"Authorization": "Bearer cvis_x"})) {
		t.Fatal("cvis bearer must not count as a provider credential")
	}
	if !HasClientProviderCredential(reqWithHeaders(map[string]string{"Authorization": "Bearer sk-ant-oat01-sub"})) {
		t.Fatal("subscription bearer is a provider credential")
	}
	if !HasClientProviderCredential(reqWithHeaders(map[string]string{"x-api-key": "sk-ant-api03-k"})) {
		t.Fatal("x-api-key is a provider credential")
	}
}
