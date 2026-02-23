package policy

import "sync"

// Registry is the in-memory compiled rule cache.
// Handlers update it on every policy write; the gateway reads from it per request.
type Registry struct {
	mu    sync.RWMutex
	rules map[string][]CompiledRule // userID → compiled rules
}

func NewRegistry() *Registry {
	return &Registry{rules: make(map[string][]CompiledRule)}
}

// Load replaces all compiled rules for a user from a fresh slice of Policies.
func (r *Registry) Load(userID string, policies []Policy) {
	compiled := Compile(policies, userID)
	r.mu.Lock()
	r.rules[userID] = compiled
	r.mu.Unlock()
}

// Append adds or replaces a single policy's rules for a user.
// If a policy with the same ID already exists in the registry, its rules are removed first.
func (r *Registry) Append(userID string, p Policy) {
	compiled := Compile([]Policy{p}, userID)

	r.mu.Lock()
	defer r.mu.Unlock()

	existing := r.rules[userID]
	// Remove any rules from the same policy
	filtered := existing[:0:0]
	for _, cr := range existing {
		if cr.PolicyID != p.ID {
			filtered = append(filtered, cr)
		}
	}
	filtered = append(filtered, compiled...)
	sortByPriority(filtered)
	r.rules[userID] = filtered
}

// Remove deletes all rules belonging to a policy from the user's rule set.
func (r *Registry) Remove(userID string, policyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing := r.rules[userID]
	filtered := existing[:0:0]
	for _, cr := range existing {
		if cr.PolicyID != policyID {
			filtered = append(filtered, cr)
		}
	}
	r.rules[userID] = filtered
}

// Evaluate returns the policy decision for the given user and request.
func (r *Registry) Evaluate(userID string, req EvalRequest) PolicyDecision {
	r.mu.RLock()
	rules := r.rules[userID]
	r.mu.RUnlock()
	return Evaluate(req, rules)
}

// RulesFor returns a copy of the compiled rules for a user (used by the dry-run endpoint).
func (r *Registry) RulesFor(userID string) []CompiledRule {
	r.mu.RLock()
	rules := r.rules[userID]
	r.mu.RUnlock()
	out := make([]CompiledRule, len(rules))
	copy(out, rules)
	return out
}
