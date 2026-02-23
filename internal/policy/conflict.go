package policy

import "fmt"

// DetectConflicts checks incoming compiled rules against existing rules for:
//   - "opposing_decisions": same service+action, no condition differentiation, different decisions
//   - "shadowed_rule": a more-specific rule makes a less-specific rule unreachable
func DetectConflicts(incoming []CompiledRule, existing []CompiledRule) []Conflict {
	var conflicts []Conflict

	all := append(existing, incoming...)

	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			a, b := all[i], all[j]

			// Only check rules from different policies (or same policy's incoming vs existing)
			if a.PolicyID == b.PolicyID && !isIncoming(a, incoming) && !isIncoming(b, incoming) {
				continue
			}

			if !overlapsServiceAction(a, b) {
				continue
			}

			// Opposing decisions with no condition to differentiate them
			if a.Decision != b.Decision && a.Condition == nil && b.Condition == nil {
				conflicts = append(conflicts, Conflict{
					RuleA:   a.ID,
					RuleB:   b.ID,
					Type:    "opposing_decisions",
					Message: fmt.Sprintf("rule %q (%s) and rule %q (%s) cover the same service/action with opposing decisions and no conditions to differentiate them", a.ID, a.Decision, b.ID, b.Decision),
				})
				continue
			}

			// Shadowed rule: higher-priority rule with no condition shadows lower-priority
			// rule with no condition and same or broader scope
			if a.Priority > b.Priority && a.Condition == nil && b.Condition == nil {
				if a.Decision == b.Decision {
					// Same decision — redundant but not harmful; flag as shadowed
					conflicts = append(conflicts, Conflict{
						RuleA:   a.ID,
						RuleB:   b.ID,
						Type:    "shadowed_rule",
						Message: fmt.Sprintf("rule %q shadows rule %q (same decision, higher priority, no differentiating condition)", a.ID, b.ID),
					})
				}
			}
		}
	}

	return conflicts
}

func overlapsServiceAction(a, b CompiledRule) bool {
	if !serviceOverlap(a.Service, b.Service) {
		return false
	}
	return actionOverlap(a.Actions, b.Actions)
}

func serviceOverlap(s1, s2 string) bool {
	return s1 == "*" || s2 == "*" || s1 == s2
}

func actionOverlap(aa, bb []string) bool {
	for _, a := range aa {
		for _, b := range bb {
			if a == "*" || b == "*" || a == b {
				return true
			}
		}
	}
	return false
}

func isIncoming(r CompiledRule, incoming []CompiledRule) bool {
	for _, ir := range incoming {
		if ir.ID == r.ID {
			return true
		}
	}
	return false
}
