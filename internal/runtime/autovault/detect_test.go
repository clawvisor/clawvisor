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

func TestDetectCandidatesDoesNotSplitUUIDsIntoSecretLikeFragments(t *testing.T) {
	candidates := DetectCandidates("email id 775b2d9f-dedc-4e83-994f-94d3b5809eaa")
	for _, candidate := range candidates {
		if candidate.Value == "dedc-4e83-994f-94d3b5809eaa" {
			t.Fatalf("uuid fragment should not be emitted as a candidate: %+v", candidates)
		}
	}
}
