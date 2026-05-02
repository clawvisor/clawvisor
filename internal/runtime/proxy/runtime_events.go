package proxy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type runtimeEventOptions struct {
	EventType           string
	ActionKind          string
	ApprovalID          *string
	TaskID              *string
	MatchedTaskID       *string
	LeaseID             *string
	ToolUseID           *string
	RequestFingerprint  *string
	ResolutionTransport *string
	Decision            *string
	Outcome             *string
	Reason              *string
	Metadata            map[string]any
}

func emitRuntimeEvent(ctx context.Context, st store.Store, session *store.RuntimeSession, reqState *RequestState, opts runtimeEventOptions) {
	if st == nil || session == nil || opts.EventType == "" {
		return
	}
	var metadataJSON json.RawMessage
	if len(opts.Metadata) > 0 {
		if b, err := json.Marshal(opts.Metadata); err == nil {
			metadataJSON = b
		}
	}
	provider := ""
	if reqState != nil && reqState.Runtime != nil {
		provider = reqState.Runtime.Provider
	}
	_ = st.CreateRuntimeEvent(ctx, &store.RuntimeEvent{
		Timestamp:           time.Now().UTC(),
		SessionID:           session.ID,
		UserID:              session.UserID,
		AgentID:             session.AgentID,
		Provider:            provider,
		EventType:           opts.EventType,
		ActionKind:          opts.ActionKind,
		ApprovalID:          opts.ApprovalID,
		TaskID:              opts.TaskID,
		MatchedTaskID:       opts.MatchedTaskID,
		LeaseID:             opts.LeaseID,
		ToolUseID:           opts.ToolUseID,
		RequestFingerprint:  opts.RequestFingerprint,
		ResolutionTransport: opts.ResolutionTransport,
		Decision:            opts.Decision,
		Outcome:             opts.Outcome,
		Reason:              opts.Reason,
		MetadataJSON:        metadataJSON,
	})
}

func stringPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
