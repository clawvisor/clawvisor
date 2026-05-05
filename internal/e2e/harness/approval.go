package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// PendingApprovals returns every pending ApprovalRecord for the user. Used by
// the approver role to drive its decision script.
func (s *Server) PendingApprovals(ctx context.Context, userID string) ([]*store.ApprovalRecord, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("harness: server not started")
	}
	return s.Store.ListPendingApprovalRecords(ctx, userID)
}

// ResolveApproval dispatches to the correct in-process handler for the
// approval's kind. resolution must be one of: allow_once, allow_session,
// allow_always, deny. user is the approver — typically the same user that
// owns the approval. Returns (status, body) so callers can assert.
//
// Kinds:
//   - request_once / credential_review → RuntimeHandler.ResolveApproval
//     (fall-through review one-offs and credential reviews)
//   - task_create                      → TasksHandler.Approve / .Deny
//   - task_expand                      → TasksHandler.ExpandApprove / .ExpandDeny
//
// task_create and task_expand do not support allow_once; the approver should
// pass allow_session or allow_always. Likewise task_expand does not accept
// allow_always — see internal/api/handlers/approval_record_state.go.
func (s *Server) ResolveApproval(ctx context.Context, user *store.User, approvalID, resolution string) (int, []byte, error) {
	if s == nil || s.Handler == nil {
		return 0, nil, fmt.Errorf("harness: server not started")
	}
	rec, err := s.Store.GetApprovalRecord(ctx, approvalID)
	if err != nil {
		return 0, nil, fmt.Errorf("harness: load approval %s: %w", approvalID, err)
	}
	switch rec.Kind {
	case "task_create":
		return s.resolveTaskApproval(ctx, user, rec, resolution, false)
	case "task_expand":
		return s.resolveTaskApproval(ctx, user, rec, resolution, true)
	case "request_once":
		// Two transports share this kind. consume_one_off_retry is the
		// runtime proxy egress-review one-off — resolve via the runtime
		// handler (creates a runtime one-off entry the proxy consumes
		// on retry). execute_pending_request is the gateway path —
		// resolve via the approvals handler (flips the PendingApproval
		// to "approved" so /api/gateway/request/{id}/execute can run).
		if rec.ResolutionTransport == "execute_pending_request" {
			return s.resolveGatewayApproval(ctx, user, rec, resolution)
		}
		return s.resolveRuntimeApproval(ctx, user, approvalID, resolution)
	default:
		return s.resolveRuntimeApproval(ctx, user, approvalID, resolution)
	}
}

// resolveGatewayApproval drives ApprovalsHandler.Approve / .Deny for a
// gateway-created request_once approval (transport=execute_pending_request).
// The handler keys off the PendingApproval's request_id, which is mirrored
// onto the canonical ApprovalRecord.RequestID.
func (s *Server) resolveGatewayApproval(ctx context.Context, user *store.User, recApproval *store.ApprovalRecord, resolution string) (int, []byte, error) {
	if s.API == nil || s.API.approvalsHandler == nil {
		return 0, nil, fmt.Errorf("harness: approvals handler not constructed")
	}
	if recApproval.RequestID == nil || *recApproval.RequestID == "" {
		return 0, nil, fmt.Errorf("harness: gateway approval %s has no request id", recApproval.ID)
	}
	requestID := *recApproval.RequestID
	deny := resolution == "deny"

	body, _ := json.Marshal(map[string]any{"resolution": resolution})
	path := "/api/approvals/" + requestID + "/approve"
	if deny {
		path = "/api/approvals/" + requestID + "/deny"
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("request_id", requestID)
	req = req.WithContext(context.WithValue(ctx, middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	if deny {
		s.API.approvalsHandler.Deny(rec, req)
	} else {
		s.API.approvalsHandler.Approve(rec, req)
	}
	return rec.Code, rec.Body.Bytes(), nil
}

func (s *Server) resolveRuntimeApproval(ctx context.Context, user *store.User, approvalID, resolution string) (int, []byte, error) {
	body, _ := json.Marshal(map[string]any{"resolution": resolution})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/approvals/"+approvalID+"/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", approvalID)
	req = req.WithContext(context.WithValue(ctx, middleware.UserContextKey, user))

	rec := httptest.NewRecorder()
	s.Handler.ResolveApproval(rec, req)
	return rec.Code, rec.Body.Bytes(), nil
}

// resolveTaskApproval routes a task_create or task_expand approval through
// the production TasksHandler endpoints. expand=true picks the expand path.
func (s *Server) resolveTaskApproval(ctx context.Context, user *store.User, recApproval *store.ApprovalRecord, resolution string, expand bool) (int, []byte, error) {
	if s.API == nil || s.API.tasksHandler == nil {
		return 0, nil, fmt.Errorf("harness: tasks handler not constructed")
	}
	if recApproval.TaskID == nil || *recApproval.TaskID == "" {
		return 0, nil, fmt.Errorf("harness: approval %s has no task id", recApproval.ID)
	}
	taskID := *recApproval.TaskID
	deny := resolution == "deny"

	var path string
	switch {
	case expand && deny:
		path = "/api/tasks/" + taskID + "/expand/deny"
	case expand:
		path = "/api/tasks/" + taskID + "/expand/approve"
	case deny:
		path = "/api/tasks/" + taskID + "/deny"
	default:
		path = "/api/tasks/" + taskID + "/approve"
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	req = req.WithContext(context.WithValue(ctx, middleware.UserContextKey, user))
	rec := httptest.NewRecorder()
	switch {
	case expand && deny:
		s.API.tasksHandler.ExpandDeny(rec, req)
	case expand:
		s.API.tasksHandler.ExpandApprove(rec, req)
	case deny:
		s.API.tasksHandler.Deny(rec, req)
	default:
		s.API.tasksHandler.Approve(rec, req)
	}
	return rec.Code, rec.Body.Bytes(), nil
}
