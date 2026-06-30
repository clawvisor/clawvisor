package controltool

import (
	"strings"
	"testing"
)

// TestControlNoticeCarriesMarkerPreservationDirective pins the
// always-on summarizer preservation instruction. Compaction-tolerant
// per-conversation isolation depends on this directive surviving in
// the system prompt of every turn — including the summarizer LLM
// call that runs during /compact-style summarization — so the
// summarizer is told to preserve cv-conv- markers verbatim in its
// output instead of dropping them as opaque noise. Without this,
// even with the marker minted on turn 1 and echoed back through
// every subsequent normal turn, the first compaction would strip
// the marker and orphan the conversation's task-checkout and
// approval state.
func TestControlNoticeCarriesMarkerPreservationDirective(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{"Bash", "Read", "Edit", "Write"})
	for _, want := range []string{
		"CONVERSATION MARKER PRESERVATION",
		"[clawvisor:conversation=cv-conv-<token>]",
		"preserve every such marker verbatim",
		"summarize, compact, condense, or otherwise rewrite",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing required preservation-directive substring %q\nnotice:\n%s", want, notice)
		}
	}
}
