package leases

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type Service struct {
	Store store.Store
}

func (s Service) Open(ctx context.Context, sessionID, taskID, toolUseID, toolName string, ttl time.Duration) (*store.ToolExecutionLease, error) {
	lease := &store.ToolExecutionLease{
		SessionID: sessionID,
		TaskID:    taskID,
		ToolUseID: toolUseID,
		ToolName:  toolName,
		Status:    "open",
		OpenedAt:  time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	if err := s.Store.CreateToolExecutionLease(ctx, lease); err != nil {
		return nil, err
	}
	return lease, nil
}

func (s Service) Close(ctx context.Context, leaseID string) error {
	return s.Store.CloseToolExecutionLease(ctx, leaseID, time.Now().UTC(), "closed")
}

func (s Service) ListOpen(ctx context.Context, sessionID string) ([]*store.ToolExecutionLease, error) {
	return s.Store.ListOpenToolExecutionLeases(ctx, sessionID)
}
