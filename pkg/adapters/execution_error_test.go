package adapters

import (
	"errors"
	"fmt"
	"testing"
)

func TestAsExecutionFailureRejectsTypedNil(t *testing.T) {
	var typedNil *ExecutionFailure
	var err error = typedNil

	failure, ok := AsExecutionFailure(fmt.Errorf("wrapped: %w", err))
	if ok || failure != nil {
		t.Fatalf("typed nil must remain unclassified, got (%#v, %v)", failure, ok)
	}
}

func TestAsExecutionFailureReturnsConcreteFailure(t *testing.T) {
	want := &ExecutionFailure{
		Kind: ExecutionFailureDefinite,
		Err:  errors.New("rejected"),
	}

	failure, ok := AsExecutionFailure(fmt.Errorf("wrapped: %w", want))
	if !ok || failure != want {
		t.Fatalf("got (%#v, %v), want (%#v, true)", failure, ok, want)
	}
}
