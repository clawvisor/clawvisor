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
	if got := values["CODEX_API_KEY"]; got != "cvis_token" {
		t.Fatalf("CODEX_API_KEY = %q", got)
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

func TestLiteProxyRunPlanInfersClaudeFromCommand(t *testing.T) {
	provider, commandArgs, err := liteProxyRunPlan([]string{"/usr/local/bin/claude", "--model", "sonnet"}, "")
	if err != nil {
		t.Fatalf("liteProxyRunPlan: %v", err)
	}
	if provider != "claude" {
		t.Fatalf("provider = %q", provider)
	}
	if strings.Join(commandArgs, "\x00") != "/usr/local/bin/claude\x00--model\x00sonnet" {
		t.Fatalf("commandArgs = %#v", commandArgs)
	}
}

func TestLiteProxyRunPlanInfersCodexFromCommand(t *testing.T) {
	provider, commandArgs, err := liteProxyRunPlan([]string{"codex", "hello"}, "")
	if err != nil {
		t.Fatalf("liteProxyRunPlan: %v", err)
	}
	if provider != "codex" {
		t.Fatalf("provider = %q", provider)
	}
	if strings.Join(commandArgs, "\x00") != "codex\x00hello" {
		t.Fatalf("commandArgs = %#v", commandArgs)
	}
}

func TestLiteProxyRunPlanUsesExplicitProviderForWrapper(t *testing.T) {
	provider, commandArgs, err := liteProxyRunPlan([]string{"my-codex-wrapper", "--debug"}, "codex")
	if err != nil {
		t.Fatalf("liteProxyRunPlan: %v", err)
	}
	if provider != "codex" {
		t.Fatalf("provider = %q", provider)
	}
	if strings.Join(commandArgs, "\x00") != "my-codex-wrapper\x00--debug" {
		t.Fatalf("commandArgs = %#v", commandArgs)
	}
}

func TestLiteProxyRunPlanDefaultsCommandWithExplicitProvider(t *testing.T) {
	provider, commandArgs, err := liteProxyRunPlan(nil, "claude")
	if err != nil {
		t.Fatalf("liteProxyRunPlan: %v", err)
	}
	if provider != "claude" {
		t.Fatalf("provider = %q", provider)
	}
	if strings.Join(commandArgs, "\x00") != "claude" {
		t.Fatalf("commandArgs = %#v", commandArgs)
	}
}

func TestLiteProxyRunPlanRequiresProviderForUnknownCommand(t *testing.T) {
	_, _, err := liteProxyRunPlan([]string{"my-wrapper"}, "")
	if err == nil || !strings.Contains(err.Error(), "could not infer proxy-lite provider") {
		t.Fatalf("error = %v, want inference failure", err)
	}
}

func TestPrepareLiteProxyCommandArgsInjectsCodexConfig(t *testing.T) {
	opts := &liteProxyOptions{Provider: "codex", BaseURL: "https://clawvisor.example"}
	got := prepareLiteProxyCommandArgs(opts, []string{"codex", "exec", "hello"})
	want := []string{
		"codex",
		"-c", `model_provider="openai"`,
		"-c", `openai_base_url="https://clawvisor.example/v1"`,
		"exec",
		"hello",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command args = %#v, want %#v", got, want)
	}
}

func TestPrepareLiteProxyCommandArgsLeavesNonCodexWrapperAlone(t *testing.T) {
	opts := &liteProxyOptions{Provider: "codex", BaseURL: "https://clawvisor.example"}
	got := prepareLiteProxyCommandArgs(opts, []string{"my-codex-wrapper", "hello"})
	want := []string{"my-codex-wrapper", "hello"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command args = %#v, want %#v", got, want)
	}
}
