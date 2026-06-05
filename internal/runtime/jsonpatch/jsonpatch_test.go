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
