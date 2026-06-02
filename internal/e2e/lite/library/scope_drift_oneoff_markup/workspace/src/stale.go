package main

// Stale is an unused helper kept around for the drift scenario to
// remove as the unrelated step-2 action.
type Stale struct {
	Token string
}

func NewStale(t string) *Stale {
	return &Stale{Token: t}
}
