package handlers

import (
	"context"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func matchServicePolicyRule(ctx context.Context, st store.Store, userID, service, action string) (*store.RuntimePolicyRule, error) {
	rules, err := st.ListRuntimePolicyRules(ctx, userID, store.RuntimePolicyRuleFilter{
		Kind:    "service",
		Enabled: boolPtr(true),
	})
	if err != nil {
		return nil, err
	}
	var wildcard *store.RuntimePolicyRule
	for _, rule := range rules {
		if rule == nil || rule.Kind != "service" || rule.Service != service {
			continue
		}
		if rule.ServiceAction == action {
			return rule, nil
		}
		if rule.ServiceAction == "*" {
			wildcard = rule
		}
	}
	return wildcard, nil
}

func boolPtr(v bool) *bool { return &v }
