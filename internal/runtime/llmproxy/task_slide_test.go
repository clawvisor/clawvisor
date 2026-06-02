package llmproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type stubExpirySetter struct {
	updatedID  string
	updatedExp time.Time
	calls      int
	err        error
}

func (s *stubExpirySetter) UpdateTaskExpiresAt(_ context.Context, id string, expiresAt time.Time) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.updatedID = id
	s.updatedExp = expiresAt
	return nil
}

func TestSlideSessionTaskExpiry(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	farFuture := now.Add(30 * time.Minute)
	soonExpiring := now.Add(2 * time.Minute)

	tests := []struct {
		name        string
		task        *store.Task
		nilStore    bool
		wantWrite   bool
		wantNewExp  time.Time
		wantNoCalls bool
	}{
		{
			name:        "nil task is a no-op",
			task:        nil,
			wantNoCalls: true,
		},
		{
			name:        "standing lifetime skips slide",
			task:        &store.Task{ID: "t1", Lifetime: "standing", ExpiresAt: &soonExpiring},
			wantNoCalls: true,
		},
		{
			name:        "session task with nil expiry skips slide",
			task:        &store.Task{ID: "t2", Lifetime: "session", ExpiresAt: nil},
			wantNoCalls: true,
		},
		{
			name:        "current expiry already past slide window is a no-op",
			task:        &store.Task{ID: "t3", Lifetime: "session", ExpiresAt: &farFuture},
			wantNoCalls: true,
		},
		{
			name:       "session task within slide window gets bumped",
			task:       &store.Task{ID: "t4", Lifetime: "session", ExpiresAt: &soonExpiring},
			wantWrite:  true,
			wantNewExp: now.Add(SessionTaskSlide),
		},
		{
			name:        "nil store is a no-op",
			task:        &store.Task{ID: "t5", Lifetime: "session", ExpiresAt: &soonExpiring},
			nilStore:    true,
			wantNoCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubExpirySetter{}
			var setter taskExpirySetter = stub
			if tt.nilStore {
				setter = nil
			}
			newExp, slid, err := slideSessionTaskExpiry(context.Background(), setter, tt.task, now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNoCalls {
				if stub.calls != 0 {
					t.Fatalf("expected no store calls, got %d", stub.calls)
				}
				if slid {
					t.Fatalf("expected slid=false")
				}
				return
			}
			if tt.wantWrite {
				if !slid {
					t.Fatalf("expected slid=true")
				}
				if stub.calls != 1 {
					t.Fatalf("expected 1 store call, got %d", stub.calls)
				}
				if stub.updatedID != tt.task.ID {
					t.Fatalf("expected updated id %q, got %q", tt.task.ID, stub.updatedID)
				}
				if !stub.updatedExp.Equal(tt.wantNewExp) {
					t.Fatalf("expected new expiry %v, got %v", tt.wantNewExp, stub.updatedExp)
				}
				if !newExp.Equal(tt.wantNewExp) {
					t.Fatalf("expected returned expiry %v, got %v", tt.wantNewExp, newExp)
				}
			}
		})
	}
}

func TestSlideSessionTaskExpiry_StoreError(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	soonExpiring := now.Add(2 * time.Minute)
	stub := &stubExpirySetter{err: errors.New("boom")}
	task := &store.Task{ID: "t", Lifetime: "session", ExpiresAt: &soonExpiring}

	_, slid, err := slideSessionTaskExpiry(context.Background(), stub, task, now)
	if err == nil {
		t.Fatalf("expected store error to propagate")
	}
	if slid {
		t.Fatalf("slid should be false on error")
	}
}
