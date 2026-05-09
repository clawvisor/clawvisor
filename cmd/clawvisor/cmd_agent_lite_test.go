package main

import (
	"strings"
	"testing"
)

func TestBuildLiteProxyEnvClaude(t *testing.T) {
	env, err := buildLiteProxyEnv("claude", " https://clawvisor.example/ ", "cvis_token")
	if err != nil {
		t.Fatalf("buildLiteProxyEnv: %v", err)
	}
	values := envMap(env)

	if got := values["CLAWVISOR_URL"]; got != "https://clawvisor.example" {
		t.Fatalf("CLAWVISOR_URL = %q", got)
	}
	if got := values["CLAWVISOR_AGENT_TOKEN"]; got != "cvis_token" {
		t.Fatalf("CLAWVISOR_AGENT_TOKEN = %q", got)
	}
	if got := values["CLAWVISOR_PROXY_LITE"]; got != "1" {
		t.Fatalf("CLAWVISOR_PROXY_LITE = %q", got)
	}
	if got := values["CLAWVISOR_PROXY_LITE_PROVIDER"]; got != "claude" {
		t.Fatalf("CLAWVISOR_PROXY_LITE_PROVIDER = %q", got)
	}
	if got := values["ANTHROPIC_BASE_URL"]; got != "https://clawvisor.example" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := values["ANTHROPIC_API_KEY"]; got != "cvis_token" {
		t.Fatalf("ANTHROPIC_API_KEY = %q", got)
	}
	if got := values["OPENAI_BASE_URL"]; got != "" {
		t.Fatalf("OPENAI_BASE_URL should be omitted for claude, got %q", got)
	}
}

func TestBuildLiteProxyEnvCodex(t *testing.T) {
	env, err := buildLiteProxyEnv("codex", "https://clawvisor.example", "cvis_token")
	if err != nil {
		t.Fatalf("buildLiteProxyEnv: %v", err)
	}
	values := envMap(env)

	if got := values["OPENAI_BASE_URL"]; got != "https://clawvisor.example/v1" {
		t.Fatalf("OPENAI_BASE_URL = %q", got)
	}
	if got := values["OPENAI_API_KEY"]; got != "cvis_token" {
		t.Fatalf("OPENAI_API_KEY = %q", got)
	}
	if got := values["ANTHROPIC_BASE_URL"]; got != "" {
		t.Fatalf("ANTHROPIC_BASE_URL should be omitted for codex, got %q", got)
	}
}

func TestBuildLiteProxyEnvCodexAvoidsDuplicateV1(t *testing.T) {
	env, err := buildLiteProxyEnv("codex", "https://clawvisor.example/v1/", "cvis_token")
	if err != nil {
		t.Fatalf("buildLiteProxyEnv: %v", err)
	}
	if got := envMap(env)["OPENAI_BASE_URL"]; got != "https://clawvisor.example/v1" {
		t.Fatalf("OPENAI_BASE_URL = %q", got)
	}
}

func TestBuildLiteProxyEnvRejectsUnknownProvider(t *testing.T) {
	_, err := buildLiteProxyEnv("gemini", "https://clawvisor.example", "cvis_token")
	if err == nil || !strings.Contains(err.Error(), "unsupported proxy-lite provider") {
		t.Fatalf("error = %v, want unsupported provider", err)
	}
}

func TestBuildLiteProxyEnvRequiresToken(t *testing.T) {
	_, err := buildLiteProxyEnv("codex", "https://clawvisor.example", " ")
	if err == nil || !strings.Contains(err.Error(), "agent token is required") {
		t.Fatalf("error = %v, want missing token", err)
	}
}
