package autovault

import "testing"

func TestDetectCandidatesSkipsLowercaseIdentifiers(t *testing.T) {
	candidates := DetectCandidates("project_onboarding_hitlist project_power_user_gbrain")
	if len(candidates) != 0 {
		t.Fatalf("expected no candidates for lowercase identifiers, got %+v", candidates)
	}
}

func TestDetectCandidatesKeepsTokenLikePrefixedValues(t *testing.T) {
	candidates := DetectCandidates("SystemNoise_8gyXD1ddhvF8iEFwrt9f3ywd")
	if len(candidates) == 0 {
		t.Fatal("expected token-like mixed-case candidate to remain detectable")
	}
}
