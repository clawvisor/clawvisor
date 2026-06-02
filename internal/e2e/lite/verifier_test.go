package lite

import (
	"log/slog"
	"strings"
	"testing"
)

func TestVerifierConfigFromEnv_DisabledWhenUnset(t *testing.T) {
	t.Setenv(EnvVerifierProvider, "")
	t.Setenv(EnvGeminiProject, "")
	v, h, err := verifierConfigFromEnv(slog.Default())
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if v != nil || h != nil {
		t.Errorf("verifier should be nil when provider env var is empty; got v=%v h=%v", v, h)
	}
	if VerifierConfiguredFromEnv() {
		t.Errorf("VerifierConfiguredFromEnv reported true with no env vars set")
	}
}

func TestVerifierConfigFromEnv_GeminiHappyPath(t *testing.T) {
	t.Setenv(EnvVerifierProvider, "gemini")
	t.Setenv(EnvGeminiProject, "my-project")
	t.Setenv(EnvGeminiRegion, "us-central1")
	t.Setenv(EnvGeminiModel, "gemini-3.1-flash-lite")

	if !VerifierConfiguredFromEnv() {
		t.Fatal("VerifierConfiguredFromEnv should be true when provider+project are set")
	}
	v, health, err := verifierConfigFromEnv(slog.Default())
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if v == nil {
		t.Fatal("verifier was nil")
	}
	if health == nil {
		t.Fatal("health was nil")
	}
	got := health.VerificationConfig()
	if !got.Enabled {
		t.Errorf("verification disabled")
	}
	if got.Provider != "gemini" {
		t.Errorf("provider=%q, want gemini", got.Provider)
	}
	if got.Project != "my-project" || got.Region != "us-central1" || got.Model != "gemini-3.1-flash-lite" {
		t.Errorf("gemini fields wrong: %+v", got)
	}
}

func TestVerifierConfigFromEnv_GeminiDefaultsRegionAndModel(t *testing.T) {
	t.Setenv(EnvVerifierProvider, "gemini")
	t.Setenv(EnvGeminiProject, "p")
	t.Setenv(EnvGeminiRegion, "")
	t.Setenv(EnvGeminiModel, "")

	_, health, err := verifierConfigFromEnv(slog.Default())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	got := health.VerificationConfig()
	if got.Region != defaultGeminiRegion {
		t.Errorf("region=%q, want default %q", got.Region, defaultGeminiRegion)
	}
	if got.Model != defaultGeminiModel {
		t.Errorf("model=%q, want default %q", got.Model, defaultGeminiModel)
	}
}

func TestVerifierConfigFromEnv_GeminiNeedsProject(t *testing.T) {
	t.Setenv(EnvVerifierProvider, "gemini")
	t.Setenv(EnvGeminiProject, "")

	_, _, err := verifierConfigFromEnv(slog.Default())
	if err == nil {
		t.Fatal("expected error when project is missing")
	}
	if !strings.Contains(err.Error(), EnvGeminiProject) {
		t.Errorf("error should name the missing env var: %v", err)
	}
}

func TestVerifierConfigFromEnv_UnsupportedProviderRejected(t *testing.T) {
	t.Setenv(EnvVerifierProvider, "anthropic")
	_, _, err := verifierConfigFromEnv(slog.Default())
	if err == nil {
		t.Fatal("expected error for non-gemini provider")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error should name the unsupported provider: %v", err)
	}
}
