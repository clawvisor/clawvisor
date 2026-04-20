package judge

import (
	"testing"
)

func TestParseVerdict_AllowVariants(t *testing.T) {
	cases := []struct {
		in   string
		want Decision
	}{
		{"allow\nThe request matches the user's stated task.", DecisionAllow},
		{"  Allow  \nSafe read.", DecisionAllow},
		{"decision: allow\nsafe", DecisionAllow},
		{"- allow\nsafe", DecisionAllow},
		{"ALLOW\nok", DecisionAllow},
		{"block\nhigh risk", DecisionBlock},
		{"flag_for_human_review\nunclear", DecisionFlagForHumanReview},
		{"Flag\nambiguous", DecisionFlagForHumanReview},
	}
	for _, c := range cases {
		got, _ := parseVerdict(c.in)
		if got != c.want {
			t.Errorf("parseVerdict(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseVerdict_Unparseable(t *testing.T) {
	got, _ := parseVerdict("I'm not sure what to do here")
	if got != "" {
		t.Errorf("expected empty decision for unparseable output, got %q", got)
	}
}

func TestCacheKey_StableForSameInput(t *testing.T) {
	r1 := Request{
		BridgeID: "b1", RuleName: "r", ConversationID: "c",
		Method: "post", DestinationHost: "API.example.com", DestinationPath: "/x",
	}
	r2 := Request{
		BridgeID: "b1", RuleName: "r", ConversationID: "c",
		Method: "POST", DestinationHost: "api.example.com", DestinationPath: "/x",
	}
	if CacheKey(r1) != CacheKey(r2) {
		t.Error("cache key should be case-insensitive on method + host")
	}
}

func TestCacheKey_DistinctOnRule(t *testing.T) {
	r1 := Request{BridgeID: "b", RuleName: "A", Method: "GET", DestinationHost: "h", DestinationPath: "/"}
	r2 := Request{BridgeID: "b", RuleName: "B", Method: "GET", DestinationHost: "h", DestinationPath: "/"}
	if CacheKey(r1) == CacheKey(r2) {
		t.Error("cache key should differ on rule name")
	}
}
