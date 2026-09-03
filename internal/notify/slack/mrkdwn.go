package slack

import (
	"html"
	"regexp"
	"strings"
)

// The resolve path ("✅ <b>Approved</b> — task activated.") is authored for
// Telegram's parse_mode=HTML and shared verbatim across every channel by
// updateNotificationMsg. Slack speaks mrkdwn, not HTML, so that text has to
// be translated on the way in or the tags render literally.
//
// Translating here rather than at the ~14 call sites keeps Telegram's
// formatting untouched and means a future call site is handled without
// having to know Slack exists.
var (
	boldRe   = regexp.MustCompile(`(?is)<b\b[^>]*>(.*?)</b>`)
	strongRe = regexp.MustCompile(`(?is)<strong\b[^>]*>(.*?)</strong>`)
	italicRe = regexp.MustCompile(`(?is)<i\b[^>]*>(.*?)</i>`)
	emRe     = regexp.MustCompile(`(?is)<em\b[^>]*>(.*?)</em>`)
	preRe    = regexp.MustCompile(`(?is)<pre\b[^>]*>(.*?)</pre>`)
	codeRe   = regexp.MustCompile(`(?is)<code\b[^>]*>(.*?)</code>`)
	brRe     = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyTagRe = regexp.MustCompile(`(?s)<[^>]*>`)
)

// telegramHTMLToMrkdwn converts the Telegram-flavoured HTML used in
// notification text into Slack mrkdwn.
//
// Slack's mrkdwn is not Markdown: bold is *single asterisks*, italic is
// _underscores_. Any tag without a mrkdwn equivalent is dropped rather than
// left to render literally.
func telegramHTMLToMrkdwn(s string) string {
	// <pre> before <code> so a <pre><code> block becomes one fenced block
	// rather than a fence wrapped around inline code.
	// ${1} rather than $1: Go parses "$1_" as a capture group *named* "1_",
	// which does not exist and expands to empty. Braces everywhere so the
	// next marker added here cannot reintroduce that.
	s = preRe.ReplaceAllString(s, "```${1}```")
	s = codeRe.ReplaceAllString(s, "`${1}`")
	s = boldRe.ReplaceAllString(s, "*${1}*")
	s = strongRe.ReplaceAllString(s, "*${1}*")
	s = italicRe.ReplaceAllString(s, "_${1}_")
	s = emRe.ReplaceAllString(s, "_${1}_")
	s = brRe.ReplaceAllString(s, "\n")

	// Drop anything left over — an unknown or unclosed tag must not reach
	// the user as literal markup.
	s = anyTagRe.ReplaceAllString(s, "")

	// The source is HTML-escaped (html.EscapeString at the Telegram
	// renderer), so entities have to be decoded before re-escaping for
	// Slack — otherwise "&amp;" reaches the user as literal "&amp;".
	s = html.UnescapeString(s)

	return escapeMrkdwn(s)
}

// escapeMrkdwn escapes the three characters Slack treats as control
// characters, leaving mrkdwn markers (* _ `) intact.
func escapeMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// plainText strips all markup, for the notification/preview string shown
// where blocks cannot render.
func plainText(s string) string {
	s = brRe.ReplaceAllString(s, " ")
	s = anyTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}
