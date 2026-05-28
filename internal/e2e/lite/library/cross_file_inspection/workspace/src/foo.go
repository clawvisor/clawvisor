package main

import "fmt"

type Foo struct {
	Name string
}

func NewFoo(name string) *Foo {
	return &Foo{Name: name}
}

// Greet is called from main.go.
func (f *Foo) Greet() string {
	return fmt.Sprintf("hello from %s", f.Name)
}

// Whisper is unused — no other file in src/ calls it. This is what
// the cross_file_inspection scenario asks the agent to identify.
func (f *Foo) Whisper() string {
	return fmt.Sprintf("(hello from %s)", f.Name)
}
