package main

import "testing"

func TestFooGreet(t *testing.T) {
	f := NewFoo("world")
	if got := f.Greet(); got != "hello from world" {
		t.Fatalf("unexpected: %q", got)
	}
}
