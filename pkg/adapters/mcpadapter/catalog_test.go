package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSchemaParams_OrderPreserved is the load-bearing test for the catalog
// spec: the agent gets misleading docs if we reorder or rename properties.
// We rely on json.Decoder.Token() preserving declaration order — which it
// does for JSON object literals, the format MCP servers emit.
func TestSchemaParams_OrderPreserved(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string"},
			"query_type": {"type": "string"},
			"filters": {"type": "object"},
			"page_size": {"type": "integer"}
		},
		"required": ["query"]
	}`)
	got := SchemaParams(schema)
	wantNames := []string{"query", "query_type", "filters", "page_size"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d params, want %d", len(got), len(wantNames))
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Errorf("param[%d].Name = %q, want %q (order broken)", i, got[i].Name, name)
		}
	}
	if !got[0].Required {
		t.Errorf("query should be required")
	}
	for i := 1; i < len(got); i++ {
		if got[i].Required {
			t.Errorf("param[%d] %q should be optional", i, got[i].Name)
		}
	}
}

func TestSchemaParams_EdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   int
	}{
		{"empty schema returns nothing", `{}`, 0},
		{"no properties block", `{"type":"object","required":["x"]}`, 0},
		{"null schema", `null`, 0},
		{"malformed schema", `{"properties":`, 0},
		{"empty properties", `{"properties":{}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SchemaParams(json.RawMessage(tc.schema))
			if len(got) != tc.want {
				t.Errorf("got %d params, want %d", len(got), tc.want)
			}
		})
	}
}

// TestOneLineSummary covers the cleanup rules required by the spec.
// "No top-level entry contains fenced code, XML/HTML tag pairs, or content
// past a ## header" is the acceptance criterion these tests lock in.
func TestOneLineSummary(t *testing.T) {
	cases := []struct {
		name      string
		desc      string
		want      string
		maxChars  int
	}{
		{
			name:     "single sentence passes through",
			desc:     "Search Notion.",
			want:     "Search Notion.",
			maxChars: 150,
		},
		{
			name:     "first sentence only",
			desc:     "Update a page. Returns the updated page record with all properties.",
			want:     "Update a page.",
			maxChars: 150,
		},
		{
			name:     "strips fenced code blocks before extracting sentence",
			desc:     "Update a page.\n```json\n{\"foo\":\"bar\"}\n```\nReturns the page.",
			want:     "Update a page.",
			maxChars: 150,
		},
		{
			name:     "strips simple html block",
			desc:     "Render <details>some useful info</details> here.",
			want:     "Render here.",
			maxChars: 150,
		},
		{
			name:     "strips self-closing tags",
			desc:     "Use <br/> a tag <hr/> here.",
			want:     "Use a tag here.",
			maxChars: 150,
		},
		{
			name:     "cuts at first h2 before extracting sentence",
			desc:     "Search the workspace.\n## Parameters\nquery: required",
			want:     "Search the workspace.",
			maxChars: 150,
		},
		{
			// Regression: Notion's notion-create-pages / notion-update-page
			// start their description with `## Overview\n...`. The trailing-
			// H2 cut doesn't match (no preceding newline), so the heading
			// used to leak through. The leading-heading strip catches it.
			name:     "strips leading h2 heading line",
			desc:     "## Overview\nCreates one or more Notion pages.",
			want:     "Creates one or more Notion pages.",
			maxChars: 150,
		},
		{
			name:     "strips stacked leading headings",
			desc:     "## Overview\n## Details\nUpdate a page.",
			want:     "Update a page.",
			maxChars: 150,
		},
		{
			name:     "leading heading with no body collapses to empty",
			desc:     "## Overview",
			want:     "",
			maxChars: 150,
		},
		{
			name:     "no period falls back to full text",
			desc:     "Identity for the connected workspace",
			want:     "Identity for the connected workspace",
			maxChars: 150,
		},
		{
			name:     "decimal numbers don't end the sentence",
			desc:     "Charge $3.50 to the customer. The transaction posts immediately.",
			want:     "Charge $3.50 to the customer.",
			maxChars: 150,
		},
		{
			name:     "question mark ends the sentence",
			desc:     "Did the user accept? Returns true/false.",
			want:     "Did the user accept?",
			maxChars: 150,
		},
		{
			name:     "truncates over maxChars with ellipsis",
			desc:     strings.Repeat("x", 200),
			want:     strings.Repeat("x", 40) + "…",
			maxChars: 40,
		},
		{
			name:     "collapses whitespace",
			desc:     "Multiple    spaces\n\nand\tnewlines",
			want:     "Multiple spaces and newlines",
			maxChars: 150,
		},
		{
			name:     "no cap when maxChars is 0",
			desc:     strings.Repeat("x", 200),
			want:     strings.Repeat("x", 200),
			maxChars: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := OneLineSummary(tc.desc, tc.maxChars)
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestFitToBudget proves the binary-search shrinks an over-budget section
// down to fit, leaves under-budget sections at the initial cap, and stops
// at the SummaryFloorChars floor when a server is pathologically chatty.
func TestFitToBudget(t *testing.T) {
	// Each rendered "tool" line is `- **tool<maxChars chars>\n` — 9 bytes
	// of structure plus the variable-length summary.
	const numTools = 20
	const linePrefix = 9 // len("- **tool") + len("\n")
	renderSection := func(maxChars int) string {
		stub := strings.Repeat("x", maxChars)
		var b strings.Builder
		for i := 0; i < numTools; i++ {
			b.WriteString("- **tool")
			b.WriteString(stub)
			b.WriteString("\n")
		}
		return b.String()
	}

	// 1) Plenty of headroom: returns the initialCap render verbatim.
	bigBudget := 1_000_000
	out := FitToBudget(bigBudget, 150, renderSection)
	if len(out) > bigBudget {
		t.Errorf("under-budget render exceeded budget: %d", len(out))
	}
	if !strings.Contains(out, strings.Repeat("x", 150)) {
		t.Errorf("under-budget should use full 150 chars")
	}

	// 2) Achievable-at-floor budget: shrinks to fit.
	//   At floor (40): numTools * (9 + 40) = 980 bytes
	//   At cap (150):  numTools * (9 + 150) = 3180 bytes
	// Budget of 2000 forces a shrink but is achievable.
	tightBudget := 2000
	out = FitToBudget(tightBudget, 150, renderSection)
	if len(out) > tightBudget {
		t.Errorf("over-budget shrink failed: got %d bytes, budget %d", len(out), tightBudget)
	}
	// And it should have shrunk to the largest fitting cap, not collapsed all
	// the way to the floor.
	maxCapThatFits := (tightBudget / numTools) - linePrefix
	if !strings.Contains(out, strings.Repeat("x", SummaryFloorChars+1)) && maxCapThatFits > SummaryFloorChars {
		t.Errorf("shrunk further than necessary; max-fitting cap = %d", maxCapThatFits)
	}

	// 3) Unachievable budget: returns the floor render. The spec says
	// "floor at ~40 chars" — we stop binary-searching there, not "guarantee
	// fit at any cost." A pathologically chatty server renders larger than
	// the budget rather than disappear entirely.
	tinyBudget := 50
	out = FitToBudget(tinyBudget, 150, renderSection)
	if !strings.Contains(out, strings.Repeat("x", SummaryFloorChars)) {
		t.Errorf("floor not respected — should render with floor cap, got %q", out[:80])
	}
}
