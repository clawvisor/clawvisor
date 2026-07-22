package upstreamflavor

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestBedrock_BuildURL(t *testing.T) {
	t.Parallel()
	b, err := NewBedrock(BedrockConfig{
		Region:        "us-west-2",
		ModelOverride: "anthropic.claude-opus-4-7-v1:0",
	})
	if err != nil {
		t.Fatalf("NewBedrock: %v", err)
	}
	u, err := b.BuildURL("/v1/messages", "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://bedrock-runtime.us-west-2.amazonaws.com/model/anthropic.claude-opus-4-7-v1:0/invoke-with-response-stream"
	if u.String() != want {
		t.Errorf("URL\n got: %s\nwant: %s", u.String(), want)
	}
}

func TestBedrock_RequiresModelOverride(t *testing.T) {
	t.Parallel()
	if _, err := NewBedrock(BedrockConfig{Region: "us-west-2"}); err == nil {
		t.Fatal("expected error without ModelOverride")
	}
}

func TestBedrock_TransformBody_StripsModelInjectsVersion(t *testing.T) {
	t.Parallel()
	b, _ := NewBedrock(BedrockConfig{Region: "us-west-2", ModelOverride: "anthropic.claude:0"})
	in := []byte(`{"model":"claude","messages":[],"max_tokens":1024}`)
	out, err := b.TransformBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, has := parsed["model"]; has {
		t.Error("model should be stripped")
	}
	if _, has := parsed["anthropic_version"]; !has {
		t.Error("anthropic_version should be injected")
	}
}

func TestBedrock_InjectAuth_NoopSignerNotImplemented(t *testing.T) {
	t.Parallel()
	b, _ := NewBedrock(BedrockConfig{Region: "us-west-2", ModelOverride: "anthropic.claude:0"})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	err := b.InjectAuth(req, []byte("ignored"))
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented, got %v", err)
	}
}

type fakeSigner struct {
	called bool
	body   []byte
}

func (f *fakeSigner) Sign(req *http.Request, body []byte) error {
	f.called = true
	f.body = body
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 fake")
	return nil
}

func TestBedrock_InjectAuth_CallsSigner(t *testing.T) {
	t.Parallel()
	signer := &fakeSigner{}
	b, _ := NewBedrock(BedrockConfig{
		Region:        "us-west-2",
		ModelOverride: "anthropic.claude:0",
		SignerFactory: func() Signer { return signer },
	})
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	if err := b.InjectAuth(req, []byte("body-bytes")); err != nil {
		t.Fatal(err)
	}
	if !signer.called {
		t.Fatal("signer was not invoked")
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization not set by signer: %q", req.Header.Get("Authorization"))
	}
}
