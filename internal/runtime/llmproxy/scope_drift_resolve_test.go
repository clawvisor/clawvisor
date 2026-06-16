package llmproxy

import (
	"strings"
	"testing"
)

func TestParseScopeDriftDecisions_HappyPath(t *testing.T) {
	t.Parallel()
	body := []byte(`Hello <clawvisor:decision drift="drift-abc" option="one-off">need this once</clawvisor:decision> done.`)
	got := parseScopeDriftDecisions(body)
	if len(got) != 1 {
		t.Fatalf("want 1 decision, got %d", len(got))
	}
	if got[0].DriftID != "drift-abc" || got[0].Option != "one-off" {
		t.Fatalf("parsed wrong attrs: %+v", got[0])
	}
	if got[0].Body != "need this once" {
		t.Fatalf("parsed wrong body: %q", got[0].Body)
	}
}

func TestParseScopeDriftDecisions_JSONEscapedAttrs(t *testing.T) {
	t.Parallel()
	// The markup inside a JSON string field has its `"` escaped to `\"`.
	body := []byte(`<clawvisor:decision drift=\"drift-1\" option=\"one-off\">rationale</clawvisor:decision>`)
	got := parseScopeDriftDecisions(body)
	if len(got) != 1 {
		t.Fatalf("want 1 decision, got %d", len(got))
	}
	if got[0].DriftID != "drift-1" || got[0].Option != "one-off" {
		t.Fatalf("escaped-attr parse: %+v", got[0])
	}
}

func TestParseScopeDriftDecisions_MalformedDropped(t *testing.T) {
	t.Parallel()
	// Missing option attribute → dropped silently.
	body := []byte(`<clawvisor:decision drift="d1">body</clawvisor:decision>`)
	if got := parseScopeDriftDecisions(body); len(got) != 0 {
		t.Fatalf("missing option must drop the decision, got %+v", got)
	}
	// Missing drift attribute → dropped.
	body = []byte(`<clawvisor:decision option="one-off">body</clawvisor:decision>`)
	if got := parseScopeDriftDecisions(body); len(got) != 0 {
		t.Fatalf("missing drift must drop the decision, got %+v", got)
	}
}

func TestParseScopeDriftDecisions_CodeFenceSuppresses(t *testing.T) {
	t.Parallel()
	// Inline backtick — within a single line, an odd backtick before
	// the markup means the markup is treated as decorative code.
	body := []byte("text `pretend <clawvisor:decision drift=\"d1\" option=\"one-off\">a</clawvisor:decision>` end")
	if got := parseScopeDriftDecisions(body); len(got) != 0 {
		t.Fatalf("inline code-span markup must be suppressed, got %+v", got)
	}
	// Triple-fence block.
	body = []byte("```\n<clawvisor:decision drift=\"d1\" option=\"one-off\">a</clawvisor:decision>\n```")
	if got := parseScopeDriftDecisions(body); len(got) != 0 {
		t.Fatalf("triple-fenced markup must be suppressed, got %+v", got)
	}
}

func TestParseScopeDriftDecisions_Multiple(t *testing.T) {
	t.Parallel()
	body := []byte(`<clawvisor:decision drift="a" option="one-off">one</clawvisor:decision>` +
		` <clawvisor:decision drift="b" option="one-off">two</clawvisor:decision>`)
	got := parseScopeDriftDecisions(body)
	if len(got) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(got))
	}
	if got[0].DriftID != "a" || got[1].DriftID != "b" {
		t.Fatalf("ordering wrong: %+v", got)
	}
}

func TestSpliceBytes(t *testing.T) {
	t.Parallel()
	in := []byte("hello world")
	out := spliceBytes(in, 6, 11, []byte("there"))
	if string(out) != "hello there" {
		t.Fatalf("spliceBytes: got %q", out)
	}
	// Original unchanged.
	if string(in) != "hello world" {
		t.Fatalf("spliceBytes mutated input: %q", in)
	}
}

func TestSanitizeStatusValue_StripsControlAndQuote(t *testing.T) {
	t.Parallel()
	got := sanitizeStatusValue("ab\"c\\d\ne")
	if strings.ContainsAny(got, "\"\\\n") {
		t.Fatalf("sanitizeStatusValue did not strip: %q", got)
	}
}
