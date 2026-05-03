package policy

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func MatchRuntimePolicyEgress(rules []*store.RuntimePolicyRule, agentID string, req EgressRequest) (*store.RuntimePolicyRule, error) {
	return bestMatchingRuntimePolicyRule(rules, agentID, func(rule *store.RuntimePolicyRule) (bool, int, error) {
		if rule == nil || rule.Kind != "egress" {
			return false, 0, nil
		}
		if strings.ToLower(rule.Host) != strings.ToLower(req.Host) {
			return false, 0, nil
		}
		if rule.Method != "" && !strings.EqualFold(rule.Method, req.Method) {
			return false, 0, nil
		}
		if rule.Path != "" && rule.Path != req.Path {
			return false, 0, nil
		}
		if rule.PathRegex != "" {
			ok, err := regexp.MatchString(rule.PathRegex, req.Path)
			if err != nil || !ok {
				return false, 0, err
			}
		}
		headersShape, err := decodeRuleShape(rule.HeadersShape)
		if err != nil {
			return false, 0, err
		}
		bodyShape, err := decodeRuleShape(rule.BodyShape)
		if err != nil {
			return false, 0, err
		}
		if !matchShape(headersShape, mapStringToAny(req.Headers)) || !matchShape(bodyShape, req.Body) {
			return false, 0, nil
		}
		score := 1
		if rule.Method != "" {
			score++
		}
		if rule.Path != "" {
			score += 5
		}
		if rule.PathRegex != "" {
			score += 4
		}
		score += shapeSpecificity(headersShape)
		score += shapeSpecificity(bodyShape)
		return true, score, nil
	})
}

func MatchRuntimePolicyTool(rules []*store.RuntimePolicyRule, agentID, toolName string, input map[string]any) (*store.RuntimePolicyRule, error) {
	return bestMatchingRuntimePolicyRule(rules, agentID, func(rule *store.RuntimePolicyRule) (bool, int, error) {
		if rule == nil || rule.Kind != "tool" || rule.ToolName != toolName {
			return false, 0, nil
		}
		if rule.InputRegex != "" {
			ok, err := matchRegexMap(rule.InputRegex, input)
			if err != nil || !ok {
				return false, 0, err
			}
		}
		inputShape, err := decodeRuleShape(rule.InputShape)
		if err != nil {
			return false, 0, err
		}
		if !matchShape(inputShape, input) {
			return false, 0, nil
		}
		score := 1
		if rule.InputRegex != "" {
			score += 4
		}
		score += shapeSpecificity(inputShape)
		return true, score, nil
	})
}

func bestMatchingRuntimePolicyRule(rules []*store.RuntimePolicyRule, agentID string, match func(rule *store.RuntimePolicyRule) (bool, int, error)) (*store.RuntimePolicyRule, error) {
	var best *store.RuntimePolicyRule
	bestActionRank := -1
	bestScopeRank := -1
	bestScore := -1
	for _, rule := range rules {
		if rule == nil || !rule.Enabled {
			continue
		}
		if rule.AgentID != nil && *rule.AgentID != agentID {
			continue
		}
		matched, score, err := match(rule)
		if err != nil || !matched {
			if err != nil {
				return nil, err
			}
			continue
		}
		actionRank := runtimePolicyActionRank(rule.Action)
		scopeRank := 0
		if rule.AgentID != nil && *rule.AgentID == agentID {
			scopeRank = 1
		}
		if actionRank > bestActionRank ||
			(actionRank == bestActionRank && scopeRank > bestScopeRank) ||
			(actionRank == bestActionRank && scopeRank == bestScopeRank && score > bestScore) {
			best = rule
			bestActionRank = actionRank
			bestScopeRank = scopeRank
			bestScore = score
		}
	}
	return best, nil
}

func runtimePolicyActionRank(action string) int {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "deny":
		return 3
	case "allow":
		return 2
	case "review":
		return 1
	default:
		return 0
	}
}

func decodeRuleShape(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func mapStringToAny(input map[string]string) map[string]any {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}
