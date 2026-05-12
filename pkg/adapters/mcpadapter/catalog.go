package mcpadapter

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// ── Render helpers exposed to the catalog handler ───────────────────────────

// DefaultSummaryMaxChars caps an MCP tool's one-line summary in the catalog.
// 150 chars is about one display line; combined with the first-sentence
// heuristic in OneLineSummary, most descriptions land in 50–120 chars and
// only the verbose stragglers hit the cap. The 2 KB-per-server budget
// squeezes this lower for outliers.
const DefaultSummaryMaxChars = 150

// SummaryFloorChars is the lower bound the budget enforcer will collapse to.
// Below this the agent loses meaningful information and may as well call the
// detail endpoint.
const SummaryFloorChars = 40

// SchemaParams parses a JSON Schema's properties + required fields into
// the typed []adapters.ParamMeta the catalog renderer expects, preserving
// the schema's declaration order of property keys. Returns an empty slice
// if the schema is empty, malformed, or has no properties.
func SchemaParams(rawSchema json.RawMessage) []adapters.ParamMeta {
	if len(rawSchema) == 0 || string(rawSchema) == "null" {
		return nil
	}
	// First pass: pull out the two relevant fields via standard decode.
	var top struct {
		Properties *json.RawMessage `json:"properties"`
		Required   *json.RawMessage `json:"required"`
	}
	if err := json.Unmarshal(rawSchema, &top); err != nil {
		return nil
	}

	// Required set.
	required := map[string]bool{}
	if top.Required != nil {
		var arr []string
		if err := json.Unmarshal(*top.Required, &arr); err == nil {
			for _, n := range arr {
				required[n] = true
			}
		}
	}

	// Second pass: walk properties via Decoder.Token() so we preserve the
	// declaration order the server (and presumably the docs author) chose.
	if top.Properties == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(*top.Properties))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var out []adapters.ParamMeta
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := keyTok.(string)
		// Peek the value's `type` field if it's an object (for ParamMeta.Type).
		var subSchema struct {
			Type any `json:"type"`
		}
		if err := dec.Decode(&subSchema); err != nil {
			break
		}
		typeStr, _ := subSchema.Type.(string)
		out = append(out, adapters.ParamMeta{
			Name:     key,
			Type:     typeStr,
			Required: required[key],
		})
	}
	return out
}

// OneLineSummary reduces a tool's free-form Markdown description to a single
// short sentence, capped at maxChars. The transformations:
//
//   - Strip fenced code blocks, HTML/XML pairs, self-closing tags.
//   - Truncate at the first H2-or-deeper heading.
//   - Collapse whitespace.
//   - Take the first sentence (text up to and including the first ".", "!",
//     or "?" followed by whitespace or end-of-string).
//   - Cap at maxChars runes with an ellipsis if it overflows.
//
// maxChars ≤ 0 disables the cap. The first-sentence heuristic is a
// best-effort guess; the param signature already tells the agent what the
// tool does, so a slightly clipped summary is fine.
func OneLineSummary(desc string, maxChars int) string {
	desc = codeFenceRe.ReplaceAllString(desc, " ")
	desc = htmlBlockRe.ReplaceAllString(desc, " ")
	desc = selfClosingTagRe.ReplaceAllString(desc, " ")
	// Strip any heading lines at the very start of the description (e.g.
	// "## Overview\nFoo..."). The h2OrDeeperRe cut below only fires for
	// headings preceded by a newline, so without this pass Notion's
	// `## Overview` lead-ins leak through verbatim. Loop to handle multiple
	// stacked leading headings — anchored ^ only matches once per call.
	for leadingHeadingRe.MatchString(desc) {
		desc = leadingHeadingRe.ReplaceAllString(desc, "")
	}
	if loc := h2OrDeeperRe.FindStringIndex(desc); loc != nil {
		desc = desc[:loc[0]]
	}
	desc = whitespaceRe.ReplaceAllString(desc, " ")
	desc = strings.TrimSpace(desc)
	desc = firstSentence(desc)
	if maxChars > 0 {
		// Count runes, not bytes, so multi-byte chars don't get sliced.
		if runes := []rune(desc); len(runes) > maxChars {
			desc = strings.TrimRight(string(runes[:maxChars]), " ") + "…"
		}
	}
	return desc
}

// firstSentence returns text up to and including the first sentence-final
// punctuation (`.`, `!`, `?`) followed by whitespace or end-of-string.
// Returns the original string if no such boundary is found.
func firstSentence(s string) string {
	runes := []rune(s)
	for i, r := range runes {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// End-of-sentence iff followed by whitespace or end-of-string. This
		// avoids breaking on decimals ("3.14") or initialisms ("U.S.A."),
		// where the period is followed by a digit or another letter.
		if i+1 >= len(runes) || isSentenceTerminator(runes[i+1]) {
			return strings.TrimSpace(string(runes[:i+1]))
		}
	}
	return s
}

func isSentenceTerminator(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// (?s) = dotall: . matches newlines.
var (
	codeFenceRe      = regexp.MustCompile("(?s)```.*?```")
	htmlBlockRe      = regexp.MustCompile(`(?s)<[a-zA-Z][^>]*>.*?</[a-zA-Z][^>]*>`)
	selfClosingTagRe = regexp.MustCompile(`<[a-zA-Z][^>]*/>`)
	h2OrDeeperRe     = regexp.MustCompile(`\n##+\s`)
	leadingHeadingRe = regexp.MustCompile(`^##+\s.*?(\n|$)`)
	whitespaceRe     = regexp.MustCompile(`\s+`)
)

// FitToBudget returns the largest maxChars in [SummaryFloorChars, initialCap]
// for which writeSection (called for each tool) renders within byteBudget
// for the given section. Binary-search; tries the initial value first
// (the common case is "already fits, don't shrink at all").
//
// renderSection is parameterized on maxChars so the caller can keep the
// rendering logic in one place (no need to duplicate per-tool formatting
// in this helper).
func FitToBudget(byteBudget int, initialCap int, renderSection func(maxChars int) string) string {
	if byteBudget <= 0 {
		return renderSection(initialCap)
	}
	candidate := renderSection(initialCap)
	if len(candidate) <= byteBudget {
		return candidate
	}
	// Binary search downward from initialCap to SummaryFloorChars.
	lo, hi := SummaryFloorChars, initialCap
	best := renderSection(lo)
	for lo <= hi {
		mid := (lo + hi) / 2
		s := renderSection(mid)
		if len(s) <= byteBudget {
			best = s
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// PerServerByteBudget is the cap per MCP server's top-level catalog section.
// Servers under budget render verbose; outliers get squeezed by FitToBudget.
const PerServerByteBudget = 2048
