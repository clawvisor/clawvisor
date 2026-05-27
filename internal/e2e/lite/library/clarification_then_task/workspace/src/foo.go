package main

import (
	"fmt"
	"strings"
)

type Foo struct {
	Name string
}

func NewFoo(name string) *Foo {
	return &Foo{Name: name}
}

// Greet lowercases the recipient's name. The user will eventually tell
// the agent that this is the bug — the lowercasing is unwanted — but
// the first user turn is intentionally vague and the agent should ask
// for clarification rather than start guessing.
func (f *Foo) Greet() string {
	return fmt.Sprintf("hello from %s", strings.ToLower(f.Name))
}
