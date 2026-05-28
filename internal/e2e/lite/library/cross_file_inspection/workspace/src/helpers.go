package main

import "fmt"

// logHello is used from main.go.
func logHello(who string) {
	fmt.Printf("logging hello to %s\n", who)
}

// logFarewell is unused — not called by any other file in src/.
// This is one of the unused functions the scenario asks the agent to
// identify.
func logFarewell(who string) {
	fmt.Printf("farewell, %s\n", who)
}

// internalShout is unexported, so the scenario's question (about
// EXPORTED functions) doesn't apply. Included to keep the agent
// honest — don't list this one.
func internalShout(who string) string {
	return "HEY " + who
}
