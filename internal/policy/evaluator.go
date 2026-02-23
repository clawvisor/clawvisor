package policy

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Evaluate is a pure function — no I/O. It returns the policy decision for the
// given request against the provided compiled rules.
//
// Algorithm:
//  1. Partition rules into global (RoleID == "") and role-specific (RoleID == req.AgentRoleID).
//  2. For each partition, filter to rules matching service + action + condition + time_window.
//  3. Role-specific rules take precedence over global rules on conflict.
//  4. Combined matched rules: if any block → block; if any approve → approve; if any execute → execute.
//  5. No match → default approve.
//
// ResponseFilters are collected from all matched execute/approve rules.
func Evaluate(req EvalRequest, rules []CompiledRule) PolicyDecision {
	now := time.Now()

	var globalMatches, roleMatches []CompiledRule

	for _, r := range rules {
		if !matchesService(r.Service, req.Service) {
			continue
		}
		if !matchesAction(r.Actions, req.Action) {
			continue
		}
		if r.Condition != nil && !matchesCondition(r.Condition, req) {
			continue
		}
		if r.TimeWindow != nil && !matchesTimeWindow(r.TimeWindow, now) {
			continue
		}

		if r.RoleID == "" {
			globalMatches = append(globalMatches, r)
		} else if r.RoleID == req.AgentRoleID {
			roleMatches = append(roleMatches, r)
		}
	}

	// Role-specific rules override global rules; merge with role rules taking priority.
	// If there are any role matches, they shadow global rules for the same service/action.
	matched := mergeRules(globalMatches, roleMatches)

	if len(matched) == 0 {
		return PolicyDecision{Decision: DecisionApprove, Reason: "no matching rule; default approve"}
	}

	return decideFromMatches(matched)
}

// mergeRules combines global and role-specific matches.
// If role matches exist, role rules take precedence: global rules that have a
// conflicting decision with any role rule are excluded.
func mergeRules(global, role []CompiledRule) []CompiledRule {
	if len(role) == 0 {
		return global
	}
	// Role matches exist — include all role matches and global matches that don't conflict.
	// "Conflict" means same service+action overlap: if a role rule covers the same
	// service/action, it wins over the global rule.
	result := make([]CompiledRule, 0, len(global)+len(role))
	result = append(result, role...)

	for _, g := range global {
		shadowed := false
		for _, r := range role {
			if matchesService(r.Service, g.Service) || matchesService(g.Service, r.Service) {
				for _, ga := range g.Actions {
					for _, ra := range r.Actions {
						if ga == ra || ga == "*" || ra == "*" {
							shadowed = true
							break
						}
					}
					if shadowed {
						break
					}
				}
			}
			if shadowed {
				break
			}
		}
		if !shadowed {
			result = append(result, g)
		}
	}
	return result
}

func decideFromMatches(matched []CompiledRule) PolicyDecision {
	var filters []ResponseFilter

	for _, r := range matched {
		if r.Decision == DecisionBlock {
			return PolicyDecision{
				Decision: DecisionBlock,
				PolicyID: r.PolicyID,
				RuleID:   r.ID,
				Reason:   r.Reason,
			}
		}
	}

	for _, r := range matched {
		if r.Decision == DecisionApprove {
			// Collect filters from all non-block matches before returning
			for _, m := range matched {
				if m.Decision != DecisionBlock {
					filters = append(filters, m.ResponseFilters...)
				}
			}
			return PolicyDecision{
				Decision:        DecisionApprove,
				PolicyID:        r.PolicyID,
				RuleID:          r.ID,
				Reason:          r.Reason,
				ResponseFilters: filters,
			}
		}
	}

	// All matched rules are execute
	for _, r := range matched {
		filters = append(filters, r.ResponseFilters...)
	}
	first := matched[0]
	return PolicyDecision{
		Decision:        DecisionExecute,
		PolicyID:        first.PolicyID,
		RuleID:          first.ID,
		Reason:          first.Reason,
		ResponseFilters: filters,
	}
}

// ── Matching helpers ──────────────────────────────────────────────────────────

func matchesService(ruleService, reqService string) bool {
	return ruleService == "*" || ruleService == reqService
}

func matchesAction(ruleActions []string, reqAction string) bool {
	for _, a := range ruleActions {
		if a == "*" || a == reqAction {
			return true
		}
	}
	return false
}

func matchesCondition(c *Condition, req EvalRequest) bool {
	switch c.Type {
	case "param_matches":
		val, ok := paramString(req.Params, c.Param)
		if !ok {
			return false
		}
		matched, err := regexp.MatchString(c.Pattern, val)
		return err == nil && matched

	case "param_not_contains":
		val, ok := paramString(req.Params, c.Param)
		if !ok {
			return true // param absent → condition satisfied (doesn't contain)
		}
		return !strings.Contains(val, c.Value)

	case "max_results_under":
		val, ok := paramNumber(req.Params, c.Param)
		if !ok {
			return false
		}
		return val < float64(c.Max)

	case "recipient_in_contacts":
		resolved, ok := req.ResolvedConditions["recipient_in_contacts"]
		if !ok {
			return false // contacts adapter not activated → condition not met
		}
		return resolved
	}

	return false
}

func matchesTimeWindow(tw *TimeWindow, now time.Time) bool {
	if tw.Timezone != "" {
		loc, err := time.LoadLocation(tw.Timezone)
		if err == nil {
			now = now.In(loc)
		}
	}

	if len(tw.Days) > 0 {
		wd := strings.ToLower(now.Weekday().String()[:3])
		found := false
		for _, d := range tw.Days {
			if strings.ToLower(d) == wd {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if tw.Hours != "" {
		parts := strings.SplitN(tw.Hours, "-", 2)
		if len(parts) == 2 {
			startH, startM := parseHHMM(parts[0])
			endH, endM := parseHHMM(parts[1])
			nowMins := now.Hour()*60 + now.Minute()
			startMins := startH*60 + startM
			endMins := endH*60 + endM
			if nowMins < startMins || nowMins >= endMins {
				return false
			}
		}
	}

	return true
}

func parseHHMM(s string) (int, int) {
	var h, m int
	fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m)
	return h, m
}

func paramString(params map[string]any, key string) (string, bool) {
	v, ok := params[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func paramNumber(params map[string]any, key string) (float64, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
