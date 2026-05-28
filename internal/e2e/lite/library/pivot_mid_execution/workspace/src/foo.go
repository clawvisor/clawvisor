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

func (f *Foo) Greet() string {
	return fmt.Sprintf("hello from %s", strings.ToLower(f.Name))
}
