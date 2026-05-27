package main

import "fmt"

// Run is called from main.go's main(). It uses Foo.Greet but not
// Foo.Whisper or anything in helpers.go beyond logHello.
func Run() {
	f := NewFoo("world")
	fmt.Println(f.Greet())
	logHello("world")
}

func main() {
	Run()
}
