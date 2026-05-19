package autovault

import (
	"encoding/base64"
	"testing"
)

func TestClawvisorStringsAreNotShadowPlaceholders(t *testing.T) {
	if LooksLikeShadow("clawvisor_x") {
		t.Fatal("clawvisor marker should not be treated as an autovault placeholder")
	}
	if HeaderMaybeContainsShadow("clawvisor-smoke/1.0") {
		t.Fatal("plain clawvisor user-agent should not be treated as an autovault placeholder")
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("clawvisor:secret"))
	if HeaderMaybeContainsShadow(basic) {
		t.Fatal("basic auth username clawvisor should not be treated as an autovault placeholder")
	}
}

func TestReplaceHeaderValueIgnoresPlainClawvisorHeaderValues(t *testing.T) {
	called := false
	got, replaced, err := ReplaceHeaderValue("clawvisor-smoke/1.0", func(string) (string, error) {
		called = true
		return "resolved", nil
	})
	if err != nil {
		t.Fatalf("ReplaceHeaderValue: %v", err)
	}
	if called {
		t.Fatal("resolver should not be called for plain clawvisor header values")
	}
	if got != "clawvisor-smoke/1.0" {
		t.Fatalf("value changed unexpectedly: %q", got)
	}
	if len(replaced) != 0 {
		t.Fatalf("expected no replacements, got %+v", replaced)
	}
}
