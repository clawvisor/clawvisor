package lite

import (
	"log/slog"
	"os"
	"strings"

	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// Env vars the harness reads to enable a real intent verifier inside
// the lite-proxy. Without these, the harness leaves IntentVerifier nil
// and drifts can only originate from the task_scope check — the
// /justify path (option d) needs a live verifier to be reachable.
//
// Gemini is the only provider wired today because:
//   - it supports Application Default Credentials (no token to manage
//     in the env), which keeps test setup ergonomic;
//   - gemini-3.1-flash-lite is fast and cheap enough that
//     leaving the verifier on during a CI scenario run doesn't slow
//     the matrix to a crawl;
//   - the existing verifier prompts/caches already have a Gemini
//     variant (see internal/intent/verifier.go StartGeminiCache),
//     so the production code path is exercised.
//
// Anthropic/OpenAI verifier wiring is a straightforward follow-up if
// someone needs it — same shape with Provider/APIKey/Endpoint instead
// of Project/Region.
const (
	EnvVerifierProvider = "CLAWVISOR_E2E_VERIFIER_PROVIDER" // "gemini" or "" (default: disabled)
	EnvGeminiProject    = "CLAWVISOR_E2E_GEMINI_PROJECT"
	EnvGeminiRegion     = "CLAWVISOR_E2E_GEMINI_REGION" // default "global"
	EnvGeminiModel      = "CLAWVISOR_E2E_GEMINI_MODEL"  // default "gemini-3.1-flash-lite"
)

const (
	defaultGeminiRegion = "global"
	defaultGeminiModel  = "gemini-3.1-flash-lite"
)

// verifierConfigFromEnv returns a configured intent.Verifier and the
// llm.Health it reads from, or (nil, nil) when no verifier env vars
// are set (the harness leaves the verifier disabled in that case).
// Errors are returned only on explicit misconfiguration — e.g.
// provider=gemini with no project. Callers should treat (nil, nil)
// as "verifier disabled" and skip scenarios that require one rather
// than fail.
func verifierConfigFromEnv(logger *slog.Logger) (intent.Verifier, *llm.Health, error) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv(EnvVerifierProvider)))
	if provider == "" {
		return nil, nil, nil
	}
	if provider != "gemini" {
		return nil, nil, &verifierConfigError{
			env:    EnvVerifierProvider,
			value:  provider,
			reason: "only \"gemini\" is supported today; leave unset to disable the verifier",
		}
	}

	project := strings.TrimSpace(os.Getenv(EnvGeminiProject))
	if project == "" {
		return nil, nil, &verifierConfigError{
			env:    EnvGeminiProject,
			value:  "",
			reason: "gemini provider needs a GCP project id (Application Default Credentials must already be set up via gcloud auth application-default login)",
		}
	}
	region := strings.TrimSpace(os.Getenv(EnvGeminiRegion))
	if region == "" {
		region = defaultGeminiRegion
	}
	model := strings.TrimSpace(os.Getenv(EnvGeminiModel))
	if model == "" {
		model = defaultGeminiModel
	}

	llmCfg := config.LLMConfig{
		Provider: "gemini",
		Project:  project,
		Region:   region,
		Model:    model,
		Verification: config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:  true,
				Provider: "gemini",
				Project:  project,
				Region:   region,
				Model:    model,
			},
			// FailClosed=false keeps a transient Gemini outage from
			// turning every drift into a hard refusal during a
			// scenario run. The verifier itself logs the error and
			// degrades to "no verification performed" — same as the
			// production fail-open default.
			FailClosed: false,
			// CacheTTL=0 falls back to the verifier's internal
			// default (60s). Long enough for a scenario step's
			// re-verifications to coalesce; short enough that each
			// scenario starts with a cold cache.
			CacheTTLSeconds: 0,
		},
	}

	health := llm.NewHealth(llmCfg)
	verifier := intent.NewLLMVerifier(health, logger)
	return verifier, health, nil
}

// VerifierConfiguredFromEnv reports whether the env vars needed to
// wire a real verifier into the lite harness are present. Used by
// scenarios that REQUIRE a verifier (e.g. exercising the /justify
// path) to skip cleanly when the env isn't configured. Tests that
// don't care about the verifier just leave it unset.
func VerifierConfiguredFromEnv() bool {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv(EnvVerifierProvider)))
	if provider == "" {
		return false
	}
	if provider == "gemini" {
		return strings.TrimSpace(os.Getenv(EnvGeminiProject)) != ""
	}
	return false
}

// verifierConfigError is returned when env vars name a verifier
// provider but are otherwise unusable (unknown provider, missing
// project, etc.). Distinct error type so callers can choose to skip
// vs. fail the run.
type verifierConfigError struct {
	env    string
	value  string
	reason string
}

func (e *verifierConfigError) Error() string {
	if e.value == "" {
		return "lite verifier config: " + e.env + " unset — " + e.reason
	}
	return "lite verifier config: " + e.env + "=" + e.value + " — " + e.reason
}
