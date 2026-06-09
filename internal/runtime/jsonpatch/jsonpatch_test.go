package jsonpatch_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/jsonpatch"
)

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
