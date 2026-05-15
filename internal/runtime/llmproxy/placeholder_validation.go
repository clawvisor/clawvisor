package llmproxy

import (
	"context"
	"errors"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func ValidateRuntimePlaceholderAccess(ctx context.Context, st store.Store, ph *store.RuntimePlaceholder, userID, agentID string, now time.Time) (string, bool) {
	if ph == nil {
		return "placeholder missing", false
	}
	if ph.UserID != userID {
		return "placeholder owned by another user", false
	}
	if ph.AgentID != "" && ph.AgentID != agentID {
		return "placeholder owned by another agent", false
	}
	if ph.RevokedAt != nil {
		return "placeholder has been revoked", false
	}
	if ph.ExpiresAt != nil && !ph.ExpiresAt.After(now) {
		return "placeholder has expired", false
	}
	if ph.CredentialGrantID != "" {
		if reason, ok := validatePlaceholderGrant(ctx, st, ph, userID, agentID, now); !ok {
			return reason, false
		}
	}
	if ph.TaskID != "" {
		if reason, ok := validatePlaceholderTask(ctx, st, ph, userID, agentID, now); !ok {
			return reason, false
		}
	}
	return "", true
}

func validatePlaceholderGrant(ctx context.Context, st store.Store, ph *store.RuntimePlaceholder, userID, agentID string, now time.Time) (string, bool) {
	auth, err := st.GetCredentialAuthorization(ctx, ph.CredentialGrantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "credential grant not found", false
		}
		return "credential grant lookup failed", false
	}
	if auth.Status != "active" {
		return "credential grant is not active", false
	}
	if auth.UserID != userID {
		return "credential grant owned by another user", false
	}
	if auth.AgentID != "" && auth.AgentID != agentID {
		return "credential grant owned by another agent", false
	}
	if auth.ExpiresAt != nil && !auth.ExpiresAt.After(now) {
		return "credential grant has expired", false
	}
	if ph.VaultItemID != "" && auth.CredentialRef != "" && auth.CredentialRef != ph.VaultItemID {
		return "credential grant does not match placeholder vault item", false
	}
	return "", true
}

func validatePlaceholderTask(ctx context.Context, st store.Store, ph *store.RuntimePlaceholder, userID, agentID string, now time.Time) (string, bool) {
	task, err := st.GetTask(ctx, ph.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "task not found", false
		}
		return "task lookup failed", false
	}
	if task.UserID != userID {
		return "task owned by another user", false
	}
	if task.AgentID != agentID {
		return "task owned by another agent", false
	}
	if task.Status != "active" {
		return "task is not active", false
	}
	if task.ExpiresAt != nil && !task.ExpiresAt.After(now) {
		return "task has expired", false
	}
	return "", true
}
