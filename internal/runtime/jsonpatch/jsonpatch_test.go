package jsonpatch_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/jsonpatch"
)

// TestMarshalNoEscape_PreservesAngleBrackets pins the cache-stability
// fix on the proxy's body-mutation path. encoding/json's default
// HTML-escape flips literal `<system-reminder>` (which appears in the
// harness's tool descriptions and prompts) into `<system-reminder>`.
// Anthropic's prompt cache is keyed on raw bytes; when the proxy
// re-marshals a body fragment, the escaped form mismatches the
// harness's original bytes and busts every cache_control breakpoint
// that includes the mutated region. MarshalNoEscape keeps `<`, `>`,
// and `&` literal.
func TestMarshalNoEscape_PreservesAngleBrackets(t *testing.T) {
	out, err := jsonpatch.MarshalNoEscape(map[string]any{
		"text": "<system-reminder>foo & bar</system-reminder>",
	})
	if err != nil {
		t.Fatalf("MarshalNoEscape: %v", err)
	}
	got := string(out)
	want := `{"text":"<system-reminder>foo & bar</system-reminder>"}`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	for _, banned := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if indexOf(got, banned) >= 0 {
			t.Errorf("output contains banned HTML escape %q: %s", banned, got)
		}
	}
}

// TestMarshalNoEscape_NoTrailingNewline ensures the encoder's default
// trailing newline is stripped — without that, splicing into a body via
// jsonsurgery.SetField would leave whitespace inside the value range.
func TestMarshalNoEscape_NoTrailingNewline(t *testing.T) {
	out, err := jsonpatch.MarshalNoEscape("hello")
	if err != nil {
		t.Fatalf("MarshalNoEscape: %v", err)
	}
	if got := string(out); got != `"hello"` {
		t.Errorf("got %q, want %q", got, `"hello"`)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSetTopLevelField_ReplacesExisting(t *testing.T) {
	in := []byte(`{"index":0,"name":"foo"}`)
	out, err := jsonpatch.SetTopLevelField(in, "index", []byte(`5`))
	if err != nil {
		t.Fatalf("SetTopLevelField: %v", err)
	}
	want := `{"index":5,"name":"foo"}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

func TestSetTopLevelField_PreservesWhitespace(t *testing.T) {
	in := []byte(`{ "index" : 0 , "name" : "foo" }`)
	out, err := jsonpatch.SetTopLevelField(in, "index", []byte(`42`))
	if err != nil {
		t.Fatalf("SetTopLevelField: %v", err)
	}
	want := `{ "index" : 42 , "name" : "foo" }`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

func TestSetTopLevelField_AppendsWhenAbsent(t *testing.T) {
	in := []byte(`{"name":"foo"}`)
	out, err := jsonpatch.SetTopLevelField(in, "index", []byte(`5`))
	if err != nil {
		t.Fatalf("SetTopLevelField: %v", err)
	}
	want := `{"name":"foo","index":5}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

func TestSetTopLevelField_AppendsToEmptyObject(t *testing.T) {
	in := []byte(`{}`)
	out, err := jsonpatch.SetTopLevelField(in, "index", []byte(`5`))
	if err != nil {
		t.Fatalf("SetTopLevelField: %v", err)
	}
	want := `{"index":5}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

func TestSetTopLevelField_ReplacesStringValue(t *testing.T) {
	in := []byte(`{"type":"old","next":1}`)
	out, err := jsonpatch.SetTopLevelField(in, "type", []byte(`"new"`))
	if err != nil {
		t.Fatalf("SetTopLevelField: %v", err)
	}
	want := `{"type":"new","next":1}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

func TestSetTopLevelField_ReplacesObjectValue(t *testing.T) {
	in := []byte(`{"meta":{"a":1},"x":2}`)
	out, err := jsonpatch.SetTopLevelField(in, "meta", []byte(`{"b":2}`))
	if err != nil {
		t.Fatalf("SetTopLevelField: %v", err)
	}
	want := `{"meta":{"b":2},"x":2}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

func TestFlattenObject_PreservesKeyOrderAndValueBytes(t *testing.T) {
	in := []byte(`{"zeta":"first","alpha":1,"mu":{"nested":true}}`)
	fields, ok := jsonpatch.FlattenObject(in)
	if !ok {
		t.Fatal("FlattenObject returned ok=false on valid object")
	}
	if len(fields) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(fields))
	}
	if fields[0].Key != "zeta" || fields[1].Key != "alpha" || fields[2].Key != "mu" {
		t.Fatalf("key order not preserved: %v", []string{fields[0].Key, fields[1].Key, fields[2].Key})
	}
	if string(fields[0].Value) != `"first"` || string(fields[1].Value) != `1` || string(fields[2].Value) != `{"nested":true}` {
		t.Fatalf("value bytes not preserved: %v", fields)
	}
}

func TestFlattenObject_RejectsNonObject(t *testing.T) {
	if _, ok := jsonpatch.FlattenObject([]byte(`[1,2,3]`)); ok {
		t.Error("expected ok=false for array input")
	}
	if _, ok := jsonpatch.FlattenObject([]byte(`"string"`)); ok {
		t.Error("expected ok=false for string input")
	}
	if _, ok := jsonpatch.FlattenObject([]byte(`not json`)); ok {
		t.Error("expected ok=false for garbage input")
	}
}

// TestFlattenObject_RejectsMalformedAndTrailingGarbage locks in the
// strictness contract: any input that isn't exactly one well-formed
// JSON object must fail. The looser variant silently stripped trailing
// bytes or accepted truncated objects, which let rewrite paths operate
// on partial payloads.
func TestFlattenObject_RejectsMalformedAndTrailingGarbage(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"trailing_garbage", `{"a":1}garbage`},
		{"trailing_garbage_multifield", `{"a":1,"b":2}xyz`},
		{"concatenated_objects", `{"a":1}{"b":2}`},
		{"truncated_no_close", `{"a":1`},
		{"truncated_after_comma", `{"a":1,`},
		{"truncated_after_key", `{"a"`},
		{"only_open_brace", `{`},
		{"trailing_value", `{"a":1}null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := jsonpatch.FlattenObject([]byte(tc.input)); ok {
				t.Errorf("FlattenObject(%q) returned ok=true; expected rejection", tc.input)
			}
		})
	}
}

func TestFlattenObject_AcceptsTrailingWhitespace(t *testing.T) {
	cases := []string{
		`{"a":1}`,
		`{"a":1}   `,
		`{"a":1}` + "\n",
		`  {"a":1}  `,
		`{}`,
	}
	for _, c := range cases {
		fields, ok := jsonpatch.FlattenObject([]byte(c))
		if !ok {
			t.Errorf("FlattenObject(%q) returned ok=false; expected success", c)
			continue
		}
		// Empty object should produce zero fields; everything else has the "a":1 pair.
		if c == `{}` {
			if len(fields) != 0 {
				t.Errorf("FlattenObject(%q): expected 0 fields, got %d", c, len(fields))
			}
			continue
		}
		if len(fields) != 1 || fields[0].Key != "a" || string(fields[0].Value) != `1` {
			t.Errorf("FlattenObject(%q): unexpected fields %v", c, fields)
		}
	}
}

func TestMarshalObjectFields_RoundTripPreservesKeyOrder(t *testing.T) {
	in := []byte(`{"zeta":"first","alpha":1,"mu":true}`)
	fields, ok := jsonpatch.FlattenObject(in)
	if !ok {
		t.Fatal("FlattenObject returned ok=false")
	}
	out := jsonpatch.MarshalObjectFields(fields)
	if string(out) != string(in) {
		t.Fatalf("round trip changed bytes.\nin:  %s\nout: %s", in, out)
	}
}

func TestMarshalObjectFields_EmitsEmptyObject(t *testing.T) {
	out := jsonpatch.MarshalObjectFields(nil)
	if string(out) != `{}` {
		t.Errorf("expected `{}`, got %q", out)
	}
}
