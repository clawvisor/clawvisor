// Package main provides the Foo type used by the demo binary.
//
// Foo wraps a name and exposes a Greet method that produces a
// per-instance greeting.
package main

import "fmt"

type Foo struct {
	Name string
}

func NewFoo(name string) *Foo {
	return &Foo{Name: name}
}

// Greet returns a friendly greeing for the receiver's Name.
func (f *Foo) Greet() string {
	return fmt.Sprintf("hello from %s", f.Name)
}
