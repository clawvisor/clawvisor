package intent

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestParseExtractionResponse_NewFormat(t *testing.T) {
	raw := `{"facts": [{"fact_type": "email_address", "fact_value": "alice@co.com"}], "patterns": [{"fact_type": "email_address", "regex": "\"email\":\\s*\"([^\"]+)\""}]}`
	facts, patterns := parseExtractionResponse(raw, slog.Default(), "test")
	if len(facts) != 1 || facts[0].FactValue != "alice@co.com" {
		t.Errorf("expected 1 fact, got %v", facts)
	}
	if len(patterns) != 1 || patterns[0].FactType != "email_address" {
		t.Errorf("expected 1 pattern, got %v", patterns)
	}
}

func TestParseExtractionResponse_LegacyArray(t *testing.T) {
	raw := `[{"fact_type": "message_id", "fact_value": "msg_001"}]`
	facts, patterns := parseExtractionResponse(raw, slog.Default(), "test")
	if len(facts) != 1 || facts[0].FactValue != "msg_001" {
		t.Errorf("expected 1 fact, got %v", facts)
	}
	if len(patterns) != 0 {
		t.Errorf("expected no patterns from legacy format, got %v", patterns)
	}
}

func TestParseExtractionResponse_EmptyNew(t *testing.T) {
	raw := `{"facts": [], "patterns": []}`
	facts, patterns := parseExtractionResponse(raw, slog.Default(), "test")
	// Empty facts+patterns is valid new format, but since both are empty
	// it falls through to legacy parse (which also produces empty). Either
	// way, the result should be empty.
	if len(facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(facts))
	}
	if len(patterns) != 0 {
		t.Errorf("expected 0 patterns, got %d", len(patterns))
	}
}

func TestParseExtractionResponse_Invalid(t *testing.T) {
	raw := `not json`
	facts, patterns := parseExtractionResponse(raw, slog.Default(), "test")
	if facts != nil || patterns != nil {
		t.Errorf("expected nil for invalid JSON")
	}
}

func TestRunExtractionPatterns_Basic(t *testing.T) {
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
	}
	fullResult := `[{"id": "aabbccddee112233", "from": "alice"}, {"id": "ffee112233445566", "from": "bob"}]`

	matches := runExtractionPatterns(patterns, fullResult, slog.Default(), "test")
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d: %v", len(matches), matches)
	}
	if matches[0].factValue != "aabbccddee112233" {
		t.Errorf("match[0] = %q, want aabbccddee112233", matches[0].factValue)
	}
	if matches[1].factValue != "ffee112233445566" {
		t.Errorf("match[1] = %q, want ffee112233445566", matches[1].factValue)
	}
}

func TestRunExtractionPatterns_MultiplePatterns(t *testing.T) {
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]+)"`},
		{FactType: "email_address", Regex: `"email":\s*"([^"]+@[^"]+)"`},
	}
	fullResult := `{"id": "abc123", "email": "alice@co.com"}, {"id": "def456", "email": "bob@co.com"}`

	matches := runExtractionPatterns(patterns, fullResult, slog.Default(), "test")
	if len(matches) != 4 {
		t.Fatalf("expected 4 matches, got %d: %v", len(matches), matches)
	}
}

func TestRunExtractionPatterns_InvalidRegex(t *testing.T) {
	patterns := []extractionPattern{
		{FactType: "bad", Regex: `[invalid`},
		{FactType: "good", Regex: `"id":\s*"([^"]+)"`},
	}
	fullResult := `{"id": "abc123"}`

	matches := runExtractionPatterns(patterns, fullResult, slog.Default(), "test")
	// Invalid regex is skipped, good regex still runs.
	if len(matches) != 1 || matches[0].factValue != "abc123" {
		t.Errorf("expected 1 match from valid regex, got %v", matches)
	}
}

func TestRunExtractionPatterns_NoCaptureGroup(t *testing.T) {
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `[a-f0-9]{16}`}, // no capture group
	}
	fullResult := `aabbccddee112233`

	matches := runExtractionPatterns(patterns, fullResult, slog.Default(), "test")
	// No capture group → m[1] doesn't exist → skipped.
	if len(matches) != 0 {
		t.Errorf("expected 0 matches without capture group, got %v", matches)
	}
}

func TestRunExtractionPatterns_EmptyPatterns(t *testing.T) {
	matches := runExtractionPatterns(nil, "anything", slog.Default(), "test")
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for nil patterns, got %v", matches)
	}
}

func TestBuiltinPatterns_Gmail(t *testing.T) {
	patterns := builtinPatterns("google.gmail", "list_messages")
	if len(patterns) == 0 {
		t.Fatal("expected builtin patterns for google.gmail")
	}

	result := `{"messages":[{"id":"19d5fe858c900042","from":{"email":"alice@example.com"},"threadId":"19d5fe858c900040","subject":"Test"}]}`
	matches := runExtractionPatterns(patterns, result, slog.Default(), "test")

	found := make(map[string]bool)
	for _, m := range matches {
		found[m.factType+"|"+m.factValue] = true
	}
	if !found["message_id|19d5fe858c900042"] {
		t.Error("expected message_id 19d5fe858c900042")
	}
	if !found["email_address|alice@example.com"] {
		t.Error("expected email_address alice@example.com")
	}
}

func TestBuiltinPatterns_Drive(t *testing.T) {
	patterns := builtinPatterns("google.drive", "list_files")
	result := `{"files":[{"id":"file_001_abc","owners":[{"emailAddress":"bob@co.com"}]}]}`
	matches := runExtractionPatterns(patterns, result, slog.Default(), "test")

	found := make(map[string]bool)
	for _, m := range matches {
		found[m.factType+"|"+m.factValue] = true
	}
	if !found["file_id|file_001_abc"] {
		t.Error("expected file_id file_001_abc")
	}
	if !found["email_address|bob@co.com"] {
		t.Error("expected email_address bob@co.com")
	}
}

func TestBuiltinPatterns_InstanceSuffix(t *testing.T) {
	// Service with instance suffix (e.g. "google.gmail:personal") should still match.
	patterns := builtinPatterns("google.gmail:personal", "list_messages")
	if len(patterns) == 0 {
		t.Fatal("expected builtin patterns for google.gmail:personal")
	}
	hasMessageID := false
	for _, p := range patterns {
		if p.FactType == "message_id" {
			hasMessageID = true
		}
	}
	if !hasMessageID {
		t.Error("expected message_id pattern for gmail with instance suffix")
	}
}

func TestBuiltinPatterns_Unknown(t *testing.T) {
	patterns := builtinPatterns("some.unknown.service", "do_thing")
	if len(patterns) == 0 {
		t.Fatal("expected generic builtin patterns for unknown service")
	}
	result := `{"id":"abc123","email":"test@example.com"}`
	matches := runExtractionPatterns(patterns, result, slog.Default(), "test")
	if len(matches) == 0 {
		t.Error("expected at least one match from generic patterns")
	}
}

func TestRunExtractionPatterns_CapsAtMaxMatches(t *testing.T) {
	// Create a result with 300 matches but maxRegexMatches is 200.
	var sb []byte
	for i := 0; i < 300; i++ {
		sb = append(sb, []byte(`"id": "aabbccddee112233" `)...)
	}
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
	}
	matches := runExtractionPatterns(patterns, string(sb), slog.Default(), "test")
	if len(matches) > maxRegexMatches {
		t.Errorf("expected at most %d matches, got %d", maxRegexMatches, len(matches))
	}
}

// --- mergeExtractionResults: post-LLM merge logic ---

// factValues collects fact_values for a given fact_type from a ChainFact slice.
func factValues(facts []*store.ChainFact, factType string) []string {
	var out []string
	for _, f := range facts {
		if f.FactType == factType {
			out = append(out, f.FactValue)
		}
	}
	return out
}

func makeMergeReq(result string) ExtractRequest {
	return ExtractRequest{
		TaskID:    "task-1",
		SessionID: "sess-1",
		AuditID:   "audit-1",
		Service:   "google.gmail",
		Action:    "list_messages",
		Result:    result,
	}
}

func TestMergeExtractionResults_DirectFactsOnly(t *testing.T) {
	// No patterns → only direct facts land. Substring validation against
	// the full result gates entry.
	result := `{"messages":[{"id":"aabbccddee112233"},{"id":"ffee112233445566"}]}`
	direct := []extractedFact{
		{FactType: "message_id", FactValue: "aabbccddee112233"},
		{FactType: "message_id", FactValue: "ffee112233445566"},
	}

	facts, dropped := mergeExtractionResults(direct, nil, makeMergeReq(result), slog.Default())
	if dropped != 0 {
		t.Errorf("expected 0 dropped, got %d", dropped)
	}
	got := factValues(facts, "message_id")
	if len(got) != 2 {
		t.Fatalf("expected 2 message_ids, got %d: %v", len(got), got)
	}
}

func TestMergeExtractionResults_RegexFillsInMissedIDs(t *testing.T) {
	// This is the core fix: the LLM returned only 2 of 3 message_ids in its
	// direct facts, and the result is NOT truncated. Previously regex would
	// be skipped; now it runs and fills in the missing ID.
	result := `{"messages":[` +
		`{"id":"aabbccddee112233"},` +
		`{"id":"ffee112233445566"},` +
		`{"id":"112233445566778a"}` +
		`]}`
	direct := []extractedFact{
		{FactType: "message_id", FactValue: "aabbccddee112233"},
		{FactType: "message_id", FactValue: "ffee112233445566"},
		// "112233445566778a" was silently omitted by the LLM.
	}
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
	}

	facts, _ := mergeExtractionResults(direct, patterns, makeMergeReq(result), slog.Default())
	got := factValues(facts, "message_id")
	if len(got) != 3 {
		t.Fatalf("expected all 3 message_ids after regex fill-in, got %d: %v", len(got), got)
	}
	// Verify the missed ID is there.
	found := false
	for _, v := range got {
		if v == "112233445566778a" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected missed ID 112233445566778a to be rescued by regex, got %v", got)
	}
}

func TestMergeExtractionResults_DedupOverlap(t *testing.T) {
	// LLM direct facts and regex matches overlap on the same IDs. Dedup
	// keeps exactly one of each — no double-count.
	result := `{"messages":[{"id":"aabbccddee112233"},{"id":"ffee112233445566"}]}`
	direct := []extractedFact{
		{FactType: "message_id", FactValue: "aabbccddee112233"},
		{FactType: "message_id", FactValue: "ffee112233445566"},
	}
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
	}

	facts, _ := mergeExtractionResults(direct, patterns, makeMergeReq(result), slog.Default())
	got := factValues(facts, "message_id")
	if len(got) != 2 {
		t.Fatalf("expected 2 unique message_ids after dedup, got %d: %v", len(got), got)
	}
}

func TestMergeExtractionResults_SubstringValidationDrops(t *testing.T) {
	// LLM hallucinated a value that doesn't appear in the raw result —
	// drop it. This is a security property: we never record facts the LLM
	// invented from thin air.
	result := `{"messages":[{"id":"aabbccddee112233"}]}`
	direct := []extractedFact{
		{FactType: "message_id", FactValue: "aabbccddee112233"},  // legit
		{FactType: "message_id", FactValue: "deadbeefdeadbeef"},  // hallucinated
	}

	facts, dropped := mergeExtractionResults(direct, nil, makeMergeReq(result), slog.Default())
	if dropped != 1 {
		t.Errorf("expected 1 dropped (hallucinated value), got %d", dropped)
	}
	got := factValues(facts, "message_id")
	if len(got) != 1 || got[0] != "aabbccddee112233" {
		t.Errorf("expected only the legit ID, got %v", got)
	}
}

func TestMergeExtractionResults_CapsAtMaxExtractedFacts(t *testing.T) {
	// Generate a result with 60 distinct message IDs — more than the 50
	// fact cap. The final list is truncated to maxExtractedFacts.
	var result []byte
	result = append(result, []byte(`{"messages":[`)...)
	for i := 0; i < 60; i++ {
		if i > 0 {
			result = append(result, ',')
		}
		// 16-hex-char IDs, distinct.
		result = append(result, []byte(fmt.Sprintf(`{"id":"%016x"}`, 0xaabbcc000000+i))...)
	}
	result = append(result, []byte(`]}`)...)

	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
	}

	facts, _ := mergeExtractionResults(nil, patterns, makeMergeReq(string(result)), slog.Default())
	if len(facts) > maxExtractedFacts {
		t.Errorf("expected at most %d facts, got %d", maxExtractedFacts, len(facts))
	}
	if len(facts) != maxExtractedFacts {
		t.Errorf("expected exactly %d facts at the cap, got %d", maxExtractedFacts, len(facts))
	}
}

func TestMergeExtractionResults_SkipsEmptyFields(t *testing.T) {
	// Direct facts with empty type or value are dropped rather than stored.
	direct := []extractedFact{
		{FactType: "", FactValue: "something"},
		{FactType: "message_id", FactValue: ""},
		{FactType: "message_id", FactValue: "aabbccddee112233"},
	}
	result := `{"id":"aabbccddee112233"}`

	facts, dropped := mergeExtractionResults(direct, nil, makeMergeReq(result), slog.Default())
	if dropped != 2 {
		t.Errorf("expected 2 dropped (empty fields), got %d", dropped)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 surviving fact, got %d", len(facts))
	}
}

func TestMergeExtractionResults_PatternsOnly(t *testing.T) {
	// No direct facts (LLM failed or returned nothing), patterns run
	// against full result.
	result := `{"messages":[{"id":"aabbccddee112233"},{"id":"ffee112233445566"}]}`
	patterns := []extractionPattern{
		{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
	}

	facts, dropped := mergeExtractionResults(nil, patterns, makeMergeReq(result), slog.Default())
	if dropped != 0 {
		t.Errorf("expected 0 dropped, got %d", dropped)
	}
	got := factValues(facts, "message_id")
	if len(got) != 2 {
		t.Errorf("expected 2 message_ids from patterns, got %v", got)
	}
}

func TestMergeExtractionResults_NoPatternsNoDirect(t *testing.T) {
	// Empty inputs → empty output, no panic.
	facts, dropped := mergeExtractionResults(nil, nil, makeMergeReq("irrelevant"), slog.Default())
	if dropped != 0 {
		t.Errorf("expected 0 dropped, got %d", dropped)
	}
	if len(facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(facts))
	}
}
