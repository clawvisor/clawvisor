package main

import "fmt"

type Foo struct {
	Name string
}

func NewFoo(name string) *Foo {
	return &Foo{Name: name}
}

// Greet returns a friendly hello for the receiver's Name. Bug: when
// Name is empty, this returns "hello from " (trailing space, no
// recipient). The fix is to detect the empty case and fall back to a
// safe default — see the scenario prompt.
func (f *Foo) Greet() string {
	return fmt.Sprintf("hello from %s", f.Name)
}
