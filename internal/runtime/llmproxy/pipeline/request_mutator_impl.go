package pipeline

import (
	"fmt"
)

// eagerRequestMutator is the real RequestMutator for the pre-phase.
// Mutations apply eagerly: ReplaceBody updates the working body
// immediately so subsequent policies see the edited bytes. This matches
// the handler contract where each preprocess step runs against the
// current body.
//
// Only methods exercised by already-migrated policies are implemented.
// The remainder remain as panics through PanicMutator embedding;
// callers that try to use an un-implemented method get a clear failure
// rather than a silent no-op.
type eagerRequestMutator struct {
	PanicMutator // panics on every method not overridden below

	body     []byte
	replaced bool
	validate func([]byte) error
}

// newEagerRequestMutator constructs a mutator with the initial body.
// The mutator captures audit fields each policy returns so the
// orchestrator can flush them in a single audit row.
func newEagerRequestMutator(initialBody []byte, validate func([]byte) error) *eagerRequestMutator {
	return &eagerRequestMutator{
		body:     append([]byte(nil), initialBody...),
		validate: validate,
	}
}

// Body returns the current (possibly mutated) body bytes. Used by the
// orchestrator between policy calls to thread the working body forward.
func (m *eagerRequestMutator) Body() []byte {
	return append([]byte(nil), m.body...)
}

func (m *eagerRequestMutator) BodyReplaced() bool {
	return m.replaced
}

// ReplaceBody applies eagerly — subsequent policies see the new bytes.
func (m *eagerRequestMutator) ReplaceBody(newBody []byte) error {
	if newBody == nil {
		return fmt.Errorf("eagerRequestMutator: ReplaceBody nil")
	}
	if m.validate != nil {
		if err := m.validate(newBody); err != nil {
			return fmt.Errorf("eagerRequestMutator: ReplaceBody parse validation failed: %w", err)
		}
	}
	m.body = append([]byte(nil), newBody...)
	m.replaced = true
	return nil
}

// Compile-time check: eagerRequestMutator satisfies RequestMutator.
var _ RequestMutator = (*eagerRequestMutator)(nil)
