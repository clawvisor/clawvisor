package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestValidateApprovalRecordTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		record     *store.ApprovalRecord
		resolution string
		status     string
		wantErr    bool
	}{
		{
			name:       "request once allow once approved",
			record:     &store.ApprovalRecord{ID: "r1", Kind: "request_once", Status: "pending"},
			resolution: "allow_once",
			status:     "approved",
		},
		{
			name:       "request once deny expired",
			record:     &store.ApprovalRecord{ID: "r2", Kind: "request_once", Status: "pending"},
			resolution: "deny",
			status:     "expired",
		},
		{
			name:       "task create allow session approved",
			record:     &store.ApprovalRecord{ID: "r3", Kind: "task_create", Status: "pending"},
			resolution: "allow_session",
			status:     "approved",
		},
		{
			name:       "task create allow once rejected",
			record:     &store.ApprovalRecord{ID: "r4", Kind: "task_create", Status: "pending"},
			resolution: "allow_once",
			status:     "approved",
			wantErr:    true,
		},
		{
			name:       "task expand allow always rejected",
			record:     &store.ApprovalRecord{ID: "r5", Kind: "task_expand", Status: "pending"},
			resolution: "allow_always",
			status:     "approved",
			wantErr:    true,
		},
		{
			name:       "credential review allow always approved",
			record:     &store.ApprovalRecord{ID: "r6", Kind: "credential_review", Status: "pending"},
			resolution: "allow_always",
			status:     "approved",
		},
		{
			name:       "non pending transition rejected",
			record:     &store.ApprovalRecord{ID: "r7", Kind: "task_call_review", Status: "approved"},
			resolution: "allow_once",
			status:     "approved",
			wantErr:    true,
		},
		{
			name:       "unknown kind rejected",
			record:     &store.ApprovalRecord{ID: "r8", Kind: "mystery", Status: "pending"},
			resolution: "allow_once",
			status:     "approved",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateApprovalRecordTransition(tt.record, tt.resolution, tt.status)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateApprovalRecordTransition() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
