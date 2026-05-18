package autovault

import (
	"strings"
	"testing"
)

func TestBuildSecretAdjudicatorPromptRedactsPeerCandidates(t *testing.T) {
	current := "AmbiguousCurrent_8gyXD1ddhvF8iEFwrt9f3ywd"
	peer := "AmbiguousPeer_9hyYE2eeivG9jFGxsu0g4zxe"
	content := "The request mentioned " + current + " and another possible credential " + peer + "."

	prompt := BuildSecretAdjudicatorPrompt("api.example.test", "content", content, Candidate{
		Value:   current,
		Charset: "mixed",
		Entropy: 4.2,
	})

	if strings.Contains(prompt, current) {
		t.Fatalf("current candidate should be redacted before adjudication:\n%s", prompt)
	}
	if strings.Contains(prompt, peer) {
		t.Fatalf("peer candidate should also be redacted before adjudication:\n%s", prompt)
	}
}
