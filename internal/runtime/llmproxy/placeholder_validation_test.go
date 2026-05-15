package llmproxy

import (
	"context"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestValidateRuntimePlaceholderAccessChecksTaskAndGrant(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	task := seedActiveTask(t, st, user, agent, nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	auth := &store.CredentialAuthorization{
		ID:            "grant-active",
		UserID:        user.ID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: "github.release",
		Service:       "github.release",
		Host:          "",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		ExpiresAt:     &expiresAt,
	}
	if err := st.CreateCredentialAuthorization(context.Background(), auth); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	placeholder := &store.RuntimePlaceholder{
		Placeholder:       "autovault_github_release_x",
		UserID:            user.ID,
		AgentID:           agent.ID,
		ServiceID:         "github.release",
		VaultItemID:       "github.release",
		CredentialGrantID: auth.ID,
		TaskID:            task.ID,
		ExpiresAt:         &expiresAt,
	}
	if reason, ok := ValidateRuntimePlaceholderAccess(context.Background(), st, placeholder, user.ID, agent.ID, time.Now().UTC()); !ok {
		t.Fatalf("expected placeholder to validate, got %q", reason)
	}

	if err := st.RevokeTask(context.Background(), task.ID, user.ID); err != nil {
		t.Fatalf("RevokeTask: %v", err)
	}
	if reason, ok := ValidateRuntimePlaceholderAccess(context.Background(), st, placeholder, user.ID, agent.ID, time.Now().UTC()); ok || reason != "task is not active" {
		t.Fatalf("expected revoked task to invalidate placeholder, got ok=%v reason=%q", ok, reason)
	}
}

func TestValidateRuntimePlaceholderAccessRejectsExpiredGrant(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	expiredAt := time.Now().UTC().Add(-time.Minute)
	auth := &store.CredentialAuthorization{
		ID:            "grant-expired",
		UserID:        user.ID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: "github.release",
		Service:       "github.release",
		Host:          "",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		ExpiresAt:     &expiredAt,
	}
	if err := st.CreateCredentialAuthorization(context.Background(), auth); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	placeholder := &store.RuntimePlaceholder{
		Placeholder:       "autovault_github_release_x",
		UserID:            user.ID,
		AgentID:           agent.ID,
		ServiceID:         "github.release",
		VaultItemID:       "github.release",
		CredentialGrantID: auth.ID,
	}
	if reason, ok := ValidateRuntimePlaceholderAccess(context.Background(), st, placeholder, user.ID, agent.ID, time.Now().UTC()); ok || reason != "credential grant has expired" {
		t.Fatalf("expected expired grant rejection, got ok=%v reason=%q", ok, reason)
	}
}

func TestValidateRuntimePlaceholderAccessAllowsUnscopedAgent(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	other, err := st.CreateAgent(context.Background(), user.ID, "other", "other-token")
	if err != nil {
		t.Fatalf("CreateAgent(other): %v", err)
	}
	placeholder := &store.RuntimePlaceholder{
		Placeholder: "autovault_shared_x",
		UserID:      user.ID,
		ServiceID:   "github.shared",
	}
	if reason, ok := ValidateRuntimePlaceholderAccess(context.Background(), st, placeholder, user.ID, other.ID, time.Now().UTC()); !ok {
		t.Fatalf("expected unscoped placeholder to validate for another agent, got %q", reason)
	}
	if reason, ok := ValidateRuntimePlaceholderAccess(context.Background(), st, placeholder, user.ID, agent.ID, time.Now().UTC()); !ok {
		t.Fatalf("expected unscoped placeholder to validate for original agent, got %q", reason)
	}
}
