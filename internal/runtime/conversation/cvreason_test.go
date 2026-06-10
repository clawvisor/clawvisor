package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractCvReason(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantReason string
		wantOK     bool
		wantInput  string
	}{
		{
			name:       "extracts and strips top-level cvreason",
			input:      `{"path":"src/auth.go","cvreason":"Reading to locate handler"}`,
			wantReason: "Reading to locate handler",
			wantOK:     true,
			wantInput:  `{"path":"src/auth.go"}`,
		},
		{
			name:       "absent field is no-op",
			input:      `{"path":"src/auth.go"}`,
			wantReason: "",
			wantOK:     false,
			wantInput:  `{"path":"src/auth.go"}`,
		},
		{
			name:       "non-object input is no-op",
			input:      `"just a string"`,
			wantReason: "",
			wantOK:     false,
			wantInput:  `"just a string"`,
		},
		{
			name:       "empty input is no-op",
			input:      ``,
			wantReason: "",
			wantOK:     false,
			wantInput:  ``,
		},
		{
			name:       "non-string cvreason falls back to raw text",
			input:      `{"cvreason":42,"x":"y"}`,
			wantReason: "42",
			wantOK:     true,
			wantInput:  `{"x":"y"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, stripped, ok := ExtractCvReason(json.RawMessage(tc.input))
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if reason != tc.wantReason {
				t.Errorf("reason=%q, want %q", reason, tc.wantReason)
			}
			gotInput := string(stripped)
			// json.Marshal of map can reorder keys; compare via re-decode
			if tc.wantOK && tc.wantInput != "" {
				if !sameJSONObject(t, gotInput, tc.wantInput) {
					t.Errorf("stripped input=%s, want %s", gotInput, tc.wantInput)
				}
			} else if gotInput != tc.wantInput {
				t.Errorf("stripped input=%q, want %q", gotInput, tc.wantInput)
			}
		})
	}
}

func TestNewToolUseFromInputExtractsCvReason(t *testing.T) {
	tu := NewToolUseFromInput("tu_1", 0, "Read", json.RawMessage(`{"path":"foo.go","cvreason":"checking imports"}`))
	if tu.CvReason != "checking imports" {
		t.Errorf("CvReason = %q, want %q", tu.CvReason, "checking imports")
	}
	if strings.Contains(string(tu.Input), "cvreason") {
		t.Errorf("Input retained cvreason: %s", tu.Input)
	}
	if tu.ID != "tu_1" || tu.Index != 0 || tu.Name != "Read" {
		t.Errorf("metadata fields lost: %+v", tu)
	}
}

func TestNewToolUseFromInputNoOpWhenAbsent(t *testing.T) {
	original := json.RawMessage(`{"path":"foo.go"}`)
	tu := NewToolUseFromInput("tu_1", 0, "Read", original)
	if tu.CvReason != "" {
		t.Errorf("CvReason = %q, want empty", tu.CvReason)
	}
	if string(tu.Input) != string(original) {
		t.Errorf("Input mutated when no cvreason present: got %s, want %s", tu.Input, original)
	}
}

func TestPopulateCvReasonPopulatesField(t *testing.T) {
	tu := ToolUse{
		ID:    "tu_1",
		Index: 0,
		Name:  "Read",
		Input: json.RawMessage(`{"path":"foo.go","cvreason":"checking imports"}`),
	}
	got := PopulateCvReason(tu)
	if got.CvReason != "checking imports" {
		t.Errorf("CvReason=%q, want %q", got.CvReason, "checking imports")
	}
	if strings.Contains(string(got.Input), "cvreason") {
		t.Errorf("Input still contains cvreason: %s", got.Input)
	}
	if !strings.Contains(string(got.Input), "foo.go") {
		t.Errorf("Input lost real params: %s", got.Input)
	}
}

func sameJSONObject(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv map[string]any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	if len(av) != len(bv) {
		return false
	}
	for k, v := range av {
		if bv[k] != v {
			return false
		}
	}
	return true
}
