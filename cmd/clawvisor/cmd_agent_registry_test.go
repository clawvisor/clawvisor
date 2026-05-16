package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	in := &agentRegistry{
		Agents: map[string]registeredAgent{
			"alpha": {
				Alias:     "alpha",
				AgentID:   "agent-123",
				AgentName: "alpha",
				ServerURL: "http://127.0.0.1:25297",
				Token:     "cvis_alpha",
			},
		},
	}

	if err := saveAgentRegistry(path, in); err != nil {
		t.Fatalf("saveAgentRegistry: %v", err)
	}
	out, err := loadAgentRegistry(path)
	if err != nil {
		t.Fatalf("loadAgentRegistry: %v", err)
	}
	got := out.Agents["alpha"]
	if got.AgentID != "agent-123" || got.Token != "cvis_alpha" || got.ServerURL != "http://127.0.0.1:25297" {
		t.Fatalf("unexpected registry entry: %+v", got)
	}
}

func TestResolveAgentCredentialsFromRegisteredAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := agentRegistryPath()
	if err != nil {
		t.Fatalf("agentRegistryPath: %v", err)
	}
	if err := saveAgentRegistry(path, &agentRegistry{
		Agents: map[string]registeredAgent{
			"alpha": {
				Alias:     "alpha",
				AgentID:   "agent-123",
				AgentName: "alpha",
				ServerURL: "http://127.0.0.1:25297",
				Token:     "cvis_alpha",
			},
		},
	}); err != nil {
		t.Fatalf("saveAgentRegistry: %v", err)
	}

	creds, err := resolveAgentCredentials("alpha", "", "")
	if err != nil {
		t.Fatalf("resolveAgentCredentials: %v", err)
	}
	if creds.Alias != "alpha" {
		t.Fatalf("unexpected alias %q", creds.Alias)
	}
	if creds.AgentToken != "cvis_alpha" {
		t.Fatalf("unexpected token %q", creds.AgentToken)
	}
	if creds.BaseURL != "http://127.0.0.1:25297" {
		t.Fatalf("unexpected base url %q", creds.BaseURL)
	}
}

func TestResolveAgentCredentialsRegisteredAgentAllowsURLOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := agentRegistryPath()
	if err != nil {
		t.Fatalf("agentRegistryPath: %v", err)
	}
	if err := saveAgentRegistry(path, &agentRegistry{
		Agents: map[string]registeredAgent{
			"alpha": {
				Alias:     "alpha",
				AgentID:   "agent-123",
				AgentName: "alpha",
				ServerURL: "http://127.0.0.1:25297",
				Token:     "cvis_alpha",
			},
		},
	}); err != nil {
		t.Fatalf("saveAgentRegistry: %v", err)
	}

	creds, err := resolveAgentCredentials("alpha", "", "https://clawvisor.example.com")
	if err != nil {
		t.Fatalf("resolveAgentCredentials: %v", err)
	}
	if creds.BaseURL != "https://clawvisor.example.com" {
		t.Fatalf("unexpected base url %q", creds.BaseURL)
	}
}

func TestResolveAgentCredentialsMissingRegisteredAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := resolveAgentCredentials("missing", "", ""); err == nil {
		t.Fatal("expected missing registered agent error")
	}
}

func TestResolveAgentCredentialsRejectsMutuallyExclusiveInputs(t *testing.T) {
	if _, err := resolveAgentCredentials("alpha", "cvis_token", ""); err == nil {
		t.Fatal("expected mutually exclusive error")
	}
}

func TestRuntimeBootstrapOptionsFromFlagsUsesRegisteredAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := agentRegistryPath()
	if err != nil {
		t.Fatalf("agentRegistryPath: %v", err)
	}
	if err := saveAgentRegistry(path, &agentRegistry{
		Agents: map[string]registeredAgent{
			"alpha": {
				Alias:     "alpha",
				AgentID:   "agent-123",
				AgentName: "alpha",
				ServerURL: "http://127.0.0.1:25297",
				Token:     "cvis_alpha",
			},
		},
	}); err != nil {
		t.Fatalf("saveAgentRegistry: %v", err)
	}

	prevName, prevToken, prevURL, prevMode, prevTTL, prevObserve, prevProfile := runtimeAgentName, runtimeAgentToken, runtimeServerURL, runtimeMode, runtimeTTLSeconds, runtimeObserve, runtimeProfileOverride
	t.Cleanup(func() {
		runtimeAgentName = prevName
		runtimeAgentToken = prevToken
		runtimeServerURL = prevURL
		runtimeMode = prevMode
		runtimeTTLSeconds = prevTTL
		runtimeObserve = prevObserve
		runtimeProfileOverride = prevProfile
	})

	runtimeAgentName = "alpha"
	runtimeAgentToken = ""
	runtimeServerURL = ""
	runtimeMode = "proxy"
	runtimeTTLSeconds = 123
	runtimeObserve = true
	runtimeProfileOverride = ""

	opts, err := runtimeBootstrapOptionsFromFlags(nil)
	if err != nil {
		t.Fatalf("runtimeBootstrapOptionsFromFlags: %v", err)
	}
	if opts.AgentToken != "cvis_alpha" {
		t.Fatalf("unexpected token %q", opts.AgentToken)
	}
	if opts.BaseURL != "http://127.0.0.1:25297" {
		t.Fatalf("unexpected base url %q", opts.BaseURL)
	}
}

func TestDockerProxyOptionsFromFlagsUsesRegisteredAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := agentRegistryPath()
	if err != nil {
		t.Fatalf("agentRegistryPath: %v", err)
	}
	if err := saveAgentRegistry(path, &agentRegistry{
		Agents: map[string]registeredAgent{
			"alpha": {
				Alias:     "alpha",
				AgentID:   "agent-123",
				AgentName: "alpha",
				ServerURL: "http://127.0.0.1:25297",
				Token:     "cvis_alpha",
			},
		},
	}); err != nil {
		t.Fatalf("saveAgentRegistry: %v", err)
	}

	prevName, prevToken, prevURL := runtimeAgentName, runtimeAgentToken, runtimeServerURL
	prevContainerURL, prevProxyHost, prevProxyPort, prevCAInside, prevCAHost := dockerContainerURL, dockerProxyHost, dockerProxyPort, dockerCAInside, dockerCAHost
	t.Cleanup(func() {
		runtimeAgentName = prevName
		runtimeAgentToken = prevToken
		runtimeServerURL = prevURL
		dockerContainerURL = prevContainerURL
		dockerProxyHost = prevProxyHost
		dockerProxyPort = prevProxyPort
		dockerCAInside = prevCAInside
		dockerCAHost = prevCAHost
	})

	runtimeAgentName = "alpha"
	runtimeAgentToken = ""
	runtimeServerURL = ""
	dockerContainerURL = ""
	dockerProxyHost = "host.docker.internal"
	dockerProxyPort = 25290
	dockerCAInside = "/clawvisor/ca.pem"
	dockerCAHost = "/host/ca.pem"

	opts, err := dockerProxyOptionsFromFlags()
	if err != nil {
		t.Fatalf("dockerProxyOptionsFromFlags: %v", err)
	}
	if opts.AgentToken != "cvis_alpha" {
		t.Fatalf("unexpected token %q", opts.AgentToken)
	}
	if opts.BaseURL != "http://127.0.0.1:25297" {
		t.Fatalf("unexpected base url %q", opts.BaseURL)
	}
	if opts.ContainerURL != "http://host.docker.internal:25297" {
		t.Fatalf("unexpected container url %q", opts.ContainerURL)
	}
}

func TestAgentRegistryPathUsesHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := agentRegistryPath()
	if err != nil {
		t.Fatalf("agentRegistryPath: %v", err)
	}
	if filepath.Base(path) != "agents.json" {
		t.Fatalf("unexpected registry path %q", path)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected registry dir to be created lazily, stat err=%v", err)
	}
}

func TestNormalizeAgentRegisterLLMProvider(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"anthropic", "anthropic", false},
		{"Claude", "anthropic", false},
		{"claude-code", "anthropic", false},
		{"openai", "openai", false},
		{"Codex", "openai", false},
		{"gemini", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeAgentRegisterLLMProvider(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("normalizeAgentRegisterLLMProvider(%q): expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeAgentRegisterLLMProvider(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("normalizeAgentRegisterLLMProvider(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPrintAgentRegisterNextStepsForStoredProvider(t *testing.T) {
	var out bytes.Buffer
	printAgentRegisterNextSteps(&out, registeredAgent{Alias: "dev"}, &agentRegisterLLMSetup{
		Provider: "openai",
		Stored:   true,
	})
	got := out.String()
	for _, want := range []string{
		"Stored openai upstream API key for this agent.",
		"clawvisor agent codex --agent dev -- exec \"say hi\"",
		"clawvisor agent lite-env codex --agent dev",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("next steps missing %q:\n%s", want, got)
		}
	}
}

func TestPrintAgentRegisterNextStepsWhenCredentialSkipped(t *testing.T) {
	var out bytes.Buffer
	printAgentRegisterNextSteps(&out, registeredAgent{Alias: "dev"}, &agentRegisterLLMSetup{Skipped: true})
	got := out.String()
	for _, want := range []string{
		"Connect through proxy-lite after storing an upstream key:",
		"clawvisor agent claude --agent dev -- --print \"what is 2+2\"",
		"clawvisor agent codex --agent dev -- exec \"say hi\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("next steps missing %q:\n%s", want, got)
		}
	}
}
