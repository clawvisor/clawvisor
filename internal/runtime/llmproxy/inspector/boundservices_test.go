package inspector

import "testing"

func TestBoundServiceHosts_KnownService(t *testing.T) {
	hosts := BoundServiceHosts("github")
	if len(hosts) == 0 {
		t.Fatalf("expected github hosts, got empty slice")
	}
}

// Regression: the runtime captured-secret code path stores ServiceID as
// `runtime.captured.<service>.<placeholder>`. BoundServiceHosts must
// normalize the wrapper away before the well-known lookup so captured
// credentials don't fail closed at the boundary check.
func TestBoundServiceHosts_HandlesCapturedPrefix(t *testing.T) {
	cases := map[string]string{
		"runtime.captured.github.autovault_github_xyz": "github",
		"runtime.captured.stripe.autovault_stripe_abc": "stripe",
		"runtime.captured.gmail.autovault_google_42":   "gmail",
	}
	for prefixed, want := range cases {
		got := BoundServiceHosts(prefixed)
		expected := BoundServiceHosts(want)
		if len(got) == 0 || len(got) != len(expected) {
			t.Errorf("prefixed %q produced %d hosts, want same as bare %q (%d)",
				prefixed, len(got), want, len(expected))
		}
	}
}

// Regression: the Shadow Tokens UI stores ServiceID as
// `<service>:<account>` for multi-account installs (e.g.
// `github:ericlevine`). The account suffix scopes ownership only;
// the bound-service host allowlist is the same for every account.
func TestBoundServiceHosts_HandlesAccountSuffix(t *testing.T) {
	cases := map[string]string{
		"github:ericlevine": "github",
		"github:work":       "github",
		"stripe:live":       "stripe",
		"slack:dev":         "slack",
	}
	for scoped, bare := range cases {
		got := BoundServiceHosts(scoped)
		expected := BoundServiceHosts(bare)
		if len(got) == 0 || len(got) != len(expected) {
			t.Errorf("scoped %q produced %d hosts, want same as bare %q (%d)",
				scoped, len(got), bare, len(expected))
		}
	}
}

func TestBoundServiceHosts_HandlesLLMScopedKeys(t *testing.T) {
	cases := map[string]string{
		"agent:agent-123:anthropic":     "anthropic",
		"agent:agent-123:openai":        "openai",
		"llm:anthropic:agent:agent-123": "anthropic",
		"llm:openai:user":               "openai",
	}
	for scoped, bare := range cases {
		got := BoundServiceHosts(scoped)
		expected := BoundServiceHosts(bare)
		if len(got) == 0 || len(got) != len(expected) {
			t.Errorf("scoped %q produced %d hosts, want same as bare %q (%d)",
				scoped, len(got), bare, len(expected))
		}
	}
}

func TestBoundServiceHosts_SplitsGoogleServiceContexts(t *testing.T) {
	gmail := BoundServiceHosts("google.gmail")
	calendar := BoundServiceHosts("google.calendar")
	if !containsHost(gmail, "gmail.googleapis.com") {
		t.Fatalf("gmail hosts should include gmail API, got %v", gmail)
	}
	if containsHost(gmail, "calendar.googleapis.com") {
		t.Fatalf("gmail hosts should not include calendar API, got %v", gmail)
	}
	if !containsHost(calendar, "calendar.googleapis.com") {
		t.Fatalf("calendar hosts should include calendar API, got %v", calendar)
	}
	if containsHost(calendar, "gmail.googleapis.com") {
		t.Fatalf("calendar hosts should not include gmail API, got %v", calendar)
	}
}

func containsHost(hosts []string, want string) bool {
	for _, host := range hosts {
		if host == want {
			return true
		}
	}
	return false
}

// Compose: captured-secret prefix + account suffix simultaneously.
// (We don't see this combination in practice, but the normalizer
// should compose cleanly.)
func TestBoundServiceHosts_HandlesPrefixAndSuffix(t *testing.T) {
	if got := BoundServiceHosts("runtime.captured.github.autovault_github_xxx"); len(got) == 0 {
		t.Errorf("captured wrapper should resolve to github hosts, got empty")
	}
}

func TestBoundServiceHosts_UnknownReturnsEmpty(t *testing.T) {
	if got := BoundServiceHosts("not-a-real-service"); len(got) != 0 {
		t.Errorf("unknown service should return empty slice, got %v", got)
	}
}
