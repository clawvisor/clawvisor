package slack

import "testing"

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
	for _, tc := range []struct{ in, want string }{
		{"✅ <b>Approved</b> — task activated.", "✅ Approved — task activated."},
		{"<b>Tom &amp; Jerry</b>", "Tom & Jerry"},
		{"a<br/>b", "a b"},
	} {
		if got := plainText(tc.in); got != tc.want {
			t.Fatalf("plainText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
