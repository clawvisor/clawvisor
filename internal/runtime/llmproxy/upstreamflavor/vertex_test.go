package upstreamflavor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestVertex_BuildURL(t *testing.T) {
	t.Parallel()
	v, err := NewVertex(VertexConfig{ProjectID: "my-proj", Region: "us-east5"})
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	u, err := v.BuildURL("/v1/messages", "claude-opus-4-7@20260101")
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-east5/publishers/anthropic/models/claude-opus-4-7@20260101:streamRawPredict"
	if u.String() != want {
		t.Errorf("URL\n got: %s\nwant: %s", u.String(), want)
	}
}

func TestVertex_BuildURL_ModelOverride(t *testing.T) {
	t.Parallel()
	v, err := NewVertex(VertexConfig{ProjectID: "p", Region: "r", ModelOverride: "claude-opus-4-7@20260101"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := v.BuildURL("/v1/messages", "client-sent-this")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.Path, "claude-opus-4-7@20260101") {
		t.Errorf("ModelOverride not applied: %s", u.Path)
	}
	if strings.Contains(u.Path, "client-sent-this") {
		t.Errorf("client model leaked into URL: %s", u.Path)
	}
}

func TestVertex_BuildURL_NoModel(t *testing.T) {
	t.Parallel()
	v, _ := NewVertex(VertexConfig{ProjectID: "p", Region: "r"})
	_, err := v.BuildURL("/v1/messages", "")
	if err == nil {
		t.Fatal("expected error when model missing and no override")
	}
}

func TestVertex_BuildURL_CountTokensUnsupported(t *testing.T) {
	t.Parallel()
	v, _ := NewVertex(VertexConfig{ProjectID: "p", Region: "r"})
	if _, err := v.BuildURL("/v1/messages/count_tokens", "m"); err == nil {
		t.Fatal("expected error for unsupported count_tokens path")
	}
}

func TestVertex_TransformBody_StripsModelInjectsVersion(t *testing.T) {
	t.Parallel()
	v, _ := NewVertex(VertexConfig{ProjectID: "p", Region: "r"})
	in := []byte(`{"model":"claude-opus-4-7","max_tokens":1024,"messages":[]}`)
	out, err := v.TransformBody(in)
	if err != nil {
		t.Fatalf("TransformBody: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if _, present := parsed["model"]; present {
		t.Error("model field should be stripped")
	}
	if _, present := parsed["max_tokens"]; !present {
		t.Error("max_tokens should be preserved")
	}
	if raw, present := parsed["anthropic_version"]; !present {
		t.Error("anthropic_version should be injected")
	} else {
		var s string
		_ = json.Unmarshal(raw, &s)
		if s != AnthropicBodyVersion {
			t.Errorf("anthropic_version=%q want %q", s, AnthropicBodyVersion)
		}
	}
}

func TestVertex_InjectAuth_VaultMode(t *testing.T) {
	t.Parallel()
	v, _ := NewVertex(VertexConfig{ProjectID: "p", Region: "r"})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "should-go")
	if err := v.InjectAuth(req, []byte("ya29.c.token")); err != nil {
		t.Fatalf("InjectAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ya29.c.token" {
		t.Errorf("Authorization=%q", got)
	}
	if req.Header.Get("anthropic-version") != "" {
		t.Error("anthropic-version should be stripped")
	}
	if req.Header.Get("x-api-key") != "" {
		t.Error("x-api-key should be stripped")
	}
}

func TestVertex_InjectAuth_PassthroughKeepsAuthorization(t *testing.T) {
	t.Parallel()
	v, _ := NewVertex(VertexConfig{ProjectID: "p", Region: "r"})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	req.Header.Set("Authorization", "Bearer caller-supplied-token")
	if err := v.InjectAuth(req, nil); err != nil {
		t.Fatalf("InjectAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer caller-supplied-token" {
		t.Errorf("passthrough Authorization mangled: %q", got)
	}
}

func TestVertex_InjectAuth_RejectsControlBytes(t *testing.T) {
	t.Parallel()
	v, _ := NewVertex(VertexConfig{ProjectID: "p", Region: "r"})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	if err := v.InjectAuth(req, []byte("foo\r\nX-Injected: yes")); err == nil {
		t.Fatal("expected error for control bytes in token")
	}
}
