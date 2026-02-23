package policy

import "fmt"

// Compile converts a slice of Policies into CompiledRules sorted by descending priority.
// userID is stored on each rule for registry keying.
//
// Priority scoring:
//
//	block:   100 + (20 if condition present) + (10 if specific actions, not *)
//	approve: 50  + same modifiers
//	execute: 10  + same modifiers
func Compile(policies []Policy, userID string) []CompiledRule {
	var rules []CompiledRule

	for _, p := range policies {
		for i, r := range p.Rules {
			decision := ruleDecision(r)
			priority := basePriority(decision)

			if r.Condition != nil {
				priority += 20
			}
			if !isWildcard(r.Actions) {
				priority += 10
			}

			cr := CompiledRule{
				ID:              fmt.Sprintf("%s:rule-%d", p.ID, i),
				PolicyID:        p.ID,
				UserID:          userID,
				RoleID:          p.Role,
				Service:         r.Service,
				Actions:         r.Actions,
				Decision:        decision,
				Condition:       r.Condition,
				TimeWindow:      r.TimeWindow,
				Reason:          r.Reason,
				ResponseFilters: r.ResponseFilters,
				Priority:        priority,
			}
			rules = append(rules, cr)
		}
	}

	// Sort by descending priority (stable to keep policy order for equal priorities)
	sortByPriority(rules)
	return rules
}

func ruleDecision(r Rule) Decision {
	if r.RequireApproval {
		if r.Allow != nil && !*r.Allow {
			// block beats require_approval
			return DecisionBlock
		}
		return DecisionApprove
	}
	if r.Allow != nil && !*r.Allow {
		return DecisionBlock
	}
	return DecisionExecute
}

func basePriority(d Decision) int {
	switch d {
	case DecisionBlock:
		return 100
	case DecisionApprove:
		return 50
	default: // execute
		return 10
	}
}

func isWildcard(actions []string) bool {
	return len(actions) == 1 && actions[0] == "*"
}

// sortByPriority sorts rules by descending Priority using insertion sort
// (small n makes this fine; also preserves relative order for equal priorities).
func sortByPriority(rules []CompiledRule) {
	for i := 1; i < len(rules); i++ {
		for j := i; j > 0 && rules[j].Priority > rules[j-1].Priority; j-- {
			rules[j], rules[j-1] = rules[j-1], rules[j]
		}
	}
}
