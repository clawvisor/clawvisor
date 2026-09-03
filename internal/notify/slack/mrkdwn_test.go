package slack

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

func TestTelegramHTMLToMrkdwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			// The reported bug: "<b>Approved</b>" rendered literally.
			name: "bold becomes single-asterisk mrkdwn",
			in:   "✅ <b>Approved</b> — task activated.",
			want: "✅ *Approved* — task activated.",
		},
		{
			name: "denial text",
			in:   "❌ <b>Denied</b> — request rejected.",
			want: "❌ *Denied* — request rejected.",
		},
		{
			name: "inline code becomes backticks",
			in:   "Use <code>gmail:send</code> now",
			want: "Use `gmail:send` now",
		},
		{
			// Telegram writes fenced blocks as <pre><code>…</code></pre>; the
			// inner tag must not become a second, inline pair of backticks
			// inside the fence.
			name: "nested pre/code becomes a single fenced block",
			in:   "<pre><code>x</code></pre>",
			want: "```x```",
		},
		{
			name: "bare pre becomes a fenced block",
			in:   "<pre>x</pre>",
			want: "```x```",
		},
		{
			name: "bare code becomes inline backticks",
			in:   "<code>x</code>",
			want: "`x`",
		},
		{
			name: "multiline nested pre/code keeps its body intact",
			in:   "<pre><code>a\nb</code></pre>",
			want: "```a\nb```",
		},
		{
			name: "italic uses underscores, not asterisks",
			in:   "<i>waiting</i>",
			want: "_waiting_",
		},
		{
			name: "strong and em map to the same markers as b and i",
			in:   "<strong>a</strong> and <em>b</em>",
			want: "*a* and _b_",
		},
		{
			// html.EscapeString ran at the Telegram renderer, so entities
			// must be decoded and then re-escaped for Slack rather than
			// reaching the user as literal "&amp;".
			name: "escaped entities round-trip to Slack escaping",
			in:   "<b>Tom &amp; Jerry</b>",
			want: "*Tom &amp; Jerry*",
		},
		{
			name: "angle brackets in content stay escaped for Slack",
			in:   "value is &lt;nil&gt;",
			want: "value is &lt;nil&gt;",
		},
		{
			name: "br becomes a newline",
			in:   "line one<br/>line two",
			want: "line one\nline two",
		},
		{
			// An unknown or unclosed tag must be dropped, never rendered.
			name: "unknown tags are stripped",
			in:   "<span class=\"x\">plain</span> <b>bold",
			want: "plain bold",
		},
		{
			name: "uppercase tags are handled",
			in:   "<B>Approved</B>",
			want: "*Approved*",
		},
		{
			name: "plain text is unchanged",
			in:   "nothing to do here",
			want: "nothing to do here",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramHTMLToMrkdwn(tc.in); got != tc.want {
				t.Fatalf("telegramHTMLToMrkdwn(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// The fallback string shown in push notifications and notification centre
// carries no markup at all.
func TestPlainText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"tags are stripped", "✅ <b>Approved</b> — task activated.", "✅ Approved — task activated."},
		{"entities are decoded, not re-escaped", "<b>Tom &amp; Jerry</b>", "Tom & Jerry"},
		{"br becomes a space, not a newline", "a<br/>b", "a b"},
		{"nested pre/code loses both tags", "<pre><code>x</code></pre>", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := plainText(tc.in); got != tc.want {
				t.Fatalf("plainText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// hasVerificationWarning and verificationLines live in blocks.go; they are
// tested here because this is the package's only test file for renderers.

func TestHasVerificationWarning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scope      string
		coherence  string
		wantWarned bool
	}{
		{"both empty means verification never ran", "", "", false},
		{"both ok is a clean result", "ok", "ok", false},
		// "n/a" means the scope check did not come back clean, so it must be
		// surfaced rather than treated as a pass.
		{"n/a param scope warns", "n/a", "ok", true},
		{"n/a reason coherence warns", "ok", "n/a", true},
		{"violation warns", "violation", "ok", true},
		{"incoherent warns", "ok", "incoherent", true},
		{"insufficient warns", "ok", "insufficient", true},
		// A verdict this renderer has never seen still means "not ok".
		{"unknown verdict warns", "ok", "unreviewed", true},
		{"one axis reported, the other not run", "violation", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := notify.ApprovalRequest{
				VerifyParamScope:      tc.scope,
				VerifyReasonCoherence: tc.coherence,
			}
			if got := hasVerificationWarning(req); got != tc.wantWarned {
				t.Fatalf("hasVerificationWarning(%q, %q) = %v, want %v",
					tc.scope, tc.coherence, got, tc.wantWarned)
			}
		})
	}
}

func TestVerificationLines(t *testing.T) {
	for _, tc := range []struct {
		name      string
		req       notify.ApprovalRequest
		want      string
		wantEmpty bool
	}{
		{
			name: "violation and insufficient",
			req: notify.ApprovalRequest{
				VerifyParamScope:      "violation",
				VerifyReasonCoherence: "insufficient",
			},
			want: "• :x: *param_scope:* violation\n• :warning: *reason:* insufficient",
		},
		{
			name: "incoherent",
			req:  notify.ApprovalRequest{VerifyReasonCoherence: "incoherent"},
			want: "• :x: *reason:* incoherent",
		},
		{
			// Every warning section must name what it is warning about; an
			// unrecognised verdict is still reported, not dropped.
			name: "n/a param scope is named, not dropped",
			req: notify.ApprovalRequest{
				VerifyParamScope:      "n/a",
				VerifyReasonCoherence: "ok",
			},
			want: "• :warning: *param_scope:* n/a\n• :white_check_mark: reason: ok",
		},
		{
			name: "explanation is escaped for Slack",
			req: notify.ApprovalRequest{
				VerifyParamScope:  "violation",
				VerifyExplanation: "to > from & <x>",
			},
			want: "• :x: *param_scope:* violation\n• :speech_balloon: to &gt; from &amp; &lt;x&gt;",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verificationLines(tc.req); got != tc.want {
				t.Fatalf("verificationLines()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// A warning must never render as a header with nothing under it: whatever
// tripped hasVerificationWarning has to be visible to the reviewer.
func TestVerificationWarningIsNeverEmpty(t *testing.T) {
	for _, scope := range []string{"", "ok", "n/a", "violation", "surprise"} {
		for _, coherence := range []string{"", "ok", "n/a", "incoherent", "insufficient", "surprise"} {
			req := notify.ApprovalRequest{VerifyParamScope: scope, VerifyReasonCoherence: coherence}
			if !hasVerificationWarning(req) {
				continue
			}
			if verificationLines(req) == "" {
				t.Fatalf("param_scope=%q reason=%q warns but renders no lines", scope, coherence)
			}
		}
	}
}
