package govlocal

import (
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestPatternMatchSafe_Keyword: case-insensitive substring.
func TestPatternMatchSafe_Keyword(t *testing.T) {
	p := &store.InstanceContentPolicy{PatternKind: "keyword", Pattern: "SeCReT"}
	if !patternMatchSafe(p, "this is a Secret memo") {
		t.Fatal("keyword should match case-insensitively")
	}
	if patternMatchSafe(p, "nothing here") {
		t.Fatal("keyword should not match absent text")
	}
}

// TestPatternMatchSafe_Regex: basic regex + compile-error never matches.
func TestPatternMatchSafe_Regex(t *testing.T) {
	p := &store.InstanceContentPolicy{PatternKind: "regex", Pattern: `\d{3}-\d{2}-\d{4}`}
	if !patternMatchSafe(p, "ssn 123-45-6789 here") {
		t.Fatal("regex should match SSN shape")
	}
	bad := &store.InstanceContentPolicy{PatternKind: "regex", Pattern: `(unclosed`}
	if patternMatchSafe(bad, "(unclosed") {
		t.Fatal("compile error must never match")
	}
}

// TestPatternMatchSafe_LongPatternNeverMatches: a 257-char regex pattern
// never matches (ReDoS guard — patterns > 256 chars are rejected).
func TestPatternMatchSafe_LongPatternNeverMatches(t *testing.T) {
	pattern := strings.Repeat("a", 257)
	p := &store.InstanceContentPolicy{PatternKind: "regex", Pattern: pattern}
	if patternMatchSafe(p, strings.Repeat("a", 300)) {
		t.Fatal("257-char pattern must never match (ReDoS guard)")
	}
	// A 256-char pattern is within bound and can match.
	ok := &store.InstanceContentPolicy{PatternKind: "regex", Pattern: strings.Repeat("a", 256)}
	if !patternMatchSafe(ok, strings.Repeat("a", 256)) {
		t.Fatal("256-char pattern should be allowed and match")
	}
}

// TestPatternMatchSafe_PathologicalRegexBounded: a pathological regex on
// 100KB input returns within the truncation bound (content capped to 64KB
// before MatchString) — the call completes fast rather than hanging.
func TestPatternMatchSafe_PathologicalRegexBounded(t *testing.T) {
	// Classic catastrophic-backtracking shape, kept under 256 chars.
	p := &store.InstanceContentPolicy{PatternKind: "regex", Pattern: `(a+)+$`}
	content := strings.Repeat("a", 100*1024) + "!"
	done := make(chan bool, 1)
	go func() {
		_ = patternMatchSafe(p, content)
		done <- true
	}()
	select {
	case <-done:
		// Returned — Go's RE2 is linear-time anyway, but the truncation
		// bound is what makes this deterministic regardless of engine.
	case <-time.After(5 * time.Second):
		t.Fatal("patternMatchSafe did not return within the truncation bound")
	}
}
