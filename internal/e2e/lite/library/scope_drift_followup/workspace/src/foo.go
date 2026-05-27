package main

import "fmt"

type Foo struct {
	Name string
}

func NewFoo(name string) *Foo {
	return &Foo{Name: name}
}

func (f *Foo) Greet() string {
	return fmt.Sprintf("hello from %s", f.Name)
}
