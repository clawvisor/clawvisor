package adapters

import "errors"

// ExecutionFailureKind describes what is known about a failed adapter
// execution. In particular, Ambiguous means a write may have reached the
// provider and callers must not retry it under a new request ID without first
// reconciling provider state.
type ExecutionFailureKind string

const (
	ExecutionFailureDefinite     ExecutionFailureKind = "definite_failure"
	ExecutionFailureStaleVersion ExecutionFailureKind = "stale_version"
	ExecutionFailureAmbiguous    ExecutionFailureKind = "ambiguous"
)

// ExecutionFailure carries the safety-relevant disposition of an adapter
// error through gateway wrapping. TimedOut is meaningful only for ambiguous
// failures and lets clients distinguish a deadline/timeout from another
// indeterminate transport failure.
type ExecutionFailure struct {
	Kind     ExecutionFailureKind
	TimedOut bool
	Err      error
}

func (e *ExecutionFailure) Error() string {
	if e == nil || e.Err == nil {
		return "adapter execution failed"
	}
	return e.Err.Error()
}

func (e *ExecutionFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AsExecutionFailure returns the first classified adapter failure in err's
// unwrap chain.
func AsExecutionFailure(err error) (*ExecutionFailure, bool) {
	var failure *ExecutionFailure
	if !errors.As(err, &failure) {
		return nil, false
	}
	return failure, true
}
