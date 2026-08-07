package upstreamflavor

import (
	"bytes"
	"net/http"
	"net/url"
	"testing"
)

func TestAzure_BuildURL(t *testing.T) {
	t.Parallel()
	a, err := NewAzure(AzureConfig{ResourceName: "myrsc", APIVersion: "2026-05-01"})
	if err != nil {
		t.Fatalf("NewAzure: %v", err)
	}
	u, err := a.BuildURL("/v1/messages", "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "myrsc.services.ai.azure.com" {
		t.Errorf("Host=%s", u.Host)
	}
	if u.Path != "/anthropic/v1/messages" {
		t.Errorf("Path=%s", u.Path)
	}
	q, _ := url.ParseQuery(u.RawQuery)
	if q.Get("api-version") != "2026-05-01" {
		t.Errorf("api-version=%s", q.Get("api-version"))
	}
}

func TestAzure_BuildURL_CountTokensSupported(t *testing.T) {
	t.Parallel()
	a, _ := NewAzure(AzureConfig{ResourceName: "r", APIVersion: "v"})
	if _, err := a.BuildURL("/v1/messages/count_tokens", ""); err != nil {
		t.Errorf("count_tokens should be supported: %v", err)
	}
}

func TestAzure_BuildURL_CustomPathPrefix(t *testing.T) {
	t.Parallel()
	a, _ := NewAzure(AzureConfig{ResourceName: "r", APIVersion: "v", PathPrefix: "/custom/anthropic"})
	u, _ := a.BuildURL("/v1/messages", "")
	if u.Path != "/custom/anthropic/v1/messages" {
		t.Errorf("Path=%s", u.Path)
	}
}

func TestAzure_TransformBody_Passthrough(t *testing.T) {
	t.Parallel()
	a, _ := NewAzure(AzureConfig{ResourceName: "r", APIVersion: "v"})
	in := []byte(`{"model":"claude","messages":[]}`)
	out, err := a.TransformBody(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("body should be unchanged. got %s", out)
	}
}

func TestAzure_InjectAuth_VaultMode(t *testing.T) {
	t.Parallel()
	a, _ := NewAzure(AzureConfig{ResourceName: "r", APIVersion: "v"})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	req.Header.Set("Authorization", "Bearer cvis_xxx")
	req.Header.Set("anthropic-version", "2023-06-01")
	if err := a.InjectAuth(req, []byte("azure-key-xyz")); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("api-key"); got != "azure-key-xyz" {
		t.Errorf("api-key=%q", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Authorization should be cleared when api-key is set")
	}
	if req.Header.Get("anthropic-version") != "" {
		t.Error("anthropic-version should be stripped")
	}
}

func TestAzure_InjectAuth_PassthroughAAD(t *testing.T) {
	t.Parallel()
	a, _ := NewAzure(AzureConfig{ResourceName: "r", APIVersion: "v"})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	req.Header.Set("Authorization", "Bearer aad-token")
	if err := a.InjectAuth(req, nil); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer aad-token" {
		t.Errorf("passthrough AAD bearer mangled: %q", got)
	}
}
