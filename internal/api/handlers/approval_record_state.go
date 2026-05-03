package handlers

import (
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type approvalTransitionRule struct {
	resolutions map[string]struct{}
}

var approvalTransitionRules = map[string]approvalTransitionRule{
	"request_once": {
		resolutions: setOf("allow_once", "allow_session", "allow_always", "deny"),
	},
	"task_create": {
		resolutions: setOf("allow_session", "allow_always", "deny"),
	},
	"task_expand": {
		resolutions: setOf("allow_session", "deny"),
	},
	"task_call_review": {
		resolutions: setOf("allow_once", "allow_session", "allow_always", "deny"),
	},
	"credential_review": {
		resolutions: setOf("allow_once", "allow_session", "allow_always", "deny"),
	},
}

func validateApprovalRecordTransition(rec *store.ApprovalRecord, resolution, status string) error {
	if rec == nil {
		return fmt.Errorf("approval record is required")
	}
	if strings.TrimSpace(rec.Kind) == "" {
		return fmt.Errorf("approval %s has empty kind", rec.ID)
	}
	if rec.Status != "pending" {
		return fmt.Errorf("approval %s in kind %s cannot transition from status %s", rec.ID, rec.Kind, rec.Status)
	}
	rule, ok := approvalTransitionRules[rec.Kind]
	if !ok {
		return fmt.Errorf("approval %s uses unsupported kind %s", rec.ID, rec.Kind)
	}
	if _, ok := rule.resolutions[resolution]; !ok {
		return fmt.Errorf("approval %s in kind %s does not allow resolution %s", rec.ID, rec.Kind, resolution)
	}
	switch resolution {
	case "allow_once", "allow_session", "allow_always":
		if status != "approved" {
			return fmt.Errorf("approval %s resolution %s requires approved status, got %s", rec.ID, resolution, status)
		}
	case "deny":
		if status != "denied" && status != "expired" {
			return fmt.Errorf("approval %s deny resolution requires denied or expired status, got %s", rec.ID, status)
		}
	default:
		return fmt.Errorf("approval %s uses unsupported resolution %s", rec.ID, resolution)
	}
	return nil
}

func setOf(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
