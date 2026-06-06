package jsonsurgery

import (
	"testing"
)

func TestSetFieldReplaceExisting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		key  string
		val  string
		want string
	}{
		{
			name: "preserves preceding key order",
			in:   `{"model":"claude","messages":[],"system":"old"}`,
			key:  "system",
			val:  `"new"`,
			want: `{"model":"claude","messages":[],"system":"new"}`,
		},
		{
			name: "preserves following key order",
			in:   `{"system":"old","model":"claude"}`,
			key:  "system",
			val:  `"new"`,
			want: `{"system":"new","model":"claude"}`,
		},
		{
			name: "replaces array value",
			in:   `{"a":[1,2,3],"b":"x"}`,
			key:  "a",
			val:  `[4,5]`,
			want: `{"a":[4,5],"b":"x"}`,
		},
		{
			name: "preserves whitespace around colon",
			in:   `{"a"  :  1 , "b":2}`,
			key:  "a",
			val:  `42`,
			want: `{"a"  :  42 , "b":2}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SetField([]byte(tc.in), tc.key, []byte(tc.val))
			if err != nil {
				t.Fatalf("SetField err: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("byte mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestSetFieldAppendsMissingKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		key  string
		val  string
		want string
	}{
		{
			name: "appends to non-empty object",
			in:   `{"model":"claude"}`,
			key:  "system",
			val:  `"hello"`,
			want: `{"model":"claude","system":"hello"}`,
		},
		{
			name: "appends to empty object",
			in:   `{}`,
			key:  "system",
			val:  `"hello"`,
			want: `{"system":"hello"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SetField([]byte(tc.in), tc.key, []byte(tc.val))
			if err != nil {
				t.Fatalf("SetField err: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("byte mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestSetFieldIgnoresNestedKey(t *testing.T) {
	t.Parallel()
	// `system` exists nested in `metadata` but not at the top level —
	// should append at top level, not modify the nested value.
	in := `{"model":"claude","metadata":{"system":"nested"}}`
	want := `{"model":"claude","metadata":{"system":"nested"},"system":"new"}`
	got, err := SetField([]byte(in), "system", []byte(`"new"`))
	if err != nil {
		t.Fatalf("SetField err: %v", err)
	}
	if string(got) != want {
		t.Errorf("byte mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestFindJSONFieldValueByteRange(t *testing.T) {
	t.Parallel()
	in := []byte(`{"a":1,"b":[2,3],"c":"x"}`)
	start, end, ok := FindFieldValue(in, "b")
	if !ok {
		t.Fatalf("expected to find b")
	}
	if string(in[start:end]) != `[2,3]` {
		t.Errorf("got %q want [2,3]", string(in[start:end]))
	}
}

func TestDeleteFieldKeyWithEscapedQuote(t *testing.T) {
	t.Parallel()
	// Key `a"b` is JSON-encoded as `"a\"b"`. A naive backward walk for
	// the first `"` would stop at the escaped inner quote and pick
	// the wrong boundary.
	in := `{"a\"b":1,"c":2}`
	got, ok := DeleteField([]byte(in), `a"b`)
	if !ok {
		t.Fatalf("expected key to be found")
	}
	if string(got) != `{"c":2}` {
		t.Errorf("got %q want %q", got, `{"c":2}`)
	}
}

func TestDeleteField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		key  string
		want string
	}{
		{
			name: "removes leading comma when deleting non-first",
			in:   `{"a":1,"b":2,"c":3}`,
			key:  "b",
			want: `{"a":1,"c":3}`,
		},
		{
			name: "removes trailing comma when deleting first",
			in:   `{"a":1,"b":2}`,
			key:  "a",
			want: `{"b":2}`,
		},
		{
			name: "removes only field",
			in:   `{"a":1}`,
			key:  "a",
			want: `{}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DeleteField([]byte(tc.in), tc.key)
			if !ok {
				t.Fatalf("expected key to be found")
			}
			if string(got) != tc.want {
				t.Errorf("byte mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
