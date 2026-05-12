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
		"runtime.captured.github.autovault_github_xyz":  "github",
		"runtime.captured.stripe.autovault_stripe_abc":  "stripe",
		"runtime.captured.gmail.autovault_google_42":    "gmail",
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

func TestBoundServiceHosts_UnknownReturnsEmpty(t *testing.T) {
	if got := BoundServiceHosts("not-a-real-service"); len(got) != 0 {
		t.Errorf("unknown service should return empty slice, got %v", got)
	}
}
