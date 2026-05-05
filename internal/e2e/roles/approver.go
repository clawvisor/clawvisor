package roles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ApprovalDecider decides how to resolve one pending ApprovalRecord. It is
// the contract scenarios implement (typically via scenario.Approvals) so
// the approver role doesn't need to know about YAML.
type ApprovalDecider interface {
	Decide(rec *store.ApprovalRecord, payload runtimeproxy.RuntimeApprovalPayload) string
}

// ApproverDeps is what the approver role needs from the harness.
type ApproverDeps interface {
	PendingApprovals(ctx context.Context, userID string) ([]*store.ApprovalRecord, error)
	ResolveApproval(ctx context.Context, user *store.User, approvalID, resolution string) (int, []byte, error)
}

// Approver polls for pending approvals and resolves them per the decider's
// script. It runs in its own goroutine alongside the responder loop so the
// responder's blocked retry can pick up the one-off and proceed.
type Approver struct {
	Deps     ApproverDeps
	Decider  ApprovalDecider
	User     *store.User
	Interval time.Duration
	Logf     func(string, ...any) // nil-safe verbose logger

	mu       sync.Mutex
	resolved map[string]string
	failures []string
}

// Start runs the approver until ctx is canceled. Cancel ctx after the
// scenario completes to stop polling.
func (a *Approver) Start(ctx context.Context) {
	if a.Interval <= 0 {
		a.Interval = 100 * time.Millisecond
	}
	if a.resolved == nil {
		a.resolved = make(map[string]string)
	}
	go a.loop(ctx)
}

func (a *Approver) loop(ctx context.Context) {
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.tick(ctx)
		}
	}
}

func (a *Approver) tick(ctx context.Context) {
	pending, err := a.Deps.PendingApprovals(ctx, a.User.ID)
	if err != nil {
		// Don't record failures from a shutdown race: cancelApprover()
		// can fire mid-tick and the in-flight ListPendingApprovalRecords
		// will surface ctx.Err() — that's expected, not a bug.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		a.recordFailure(fmt.Sprintf("list pending: %s", err.Error()))
		return
	}
	for _, rec := range pending {
		a.mu.Lock()
		_, seen := a.resolved[rec.ID]
		a.mu.Unlock()
		if seen {
			continue
		}
		var payload runtimeproxy.RuntimeApprovalPayload
		_ = json.Unmarshal(rec.PayloadJSON, &payload)
		resolution := strings.TrimSpace(a.Decider.Decide(rec, payload))
		if resolution == "" {
			// Empty default means "deny" (fail closed).
			resolution = "deny"
		}
		status, body, err := a.Deps.ResolveApproval(ctx, a.User, rec.ID, resolution)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			a.recordFailure(fmt.Sprintf("resolve %s: %s", rec.ID, err.Error()))
			continue
		}
		if status/100 != 2 {
			a.recordFailure(fmt.Sprintf("resolve %s status %d: %s", rec.ID, status, string(body)))
			continue
		}
		a.mu.Lock()
		a.resolved[rec.ID] = resolution
		a.mu.Unlock()
		if a.Logf != nil {
			descriptor := strings.TrimSpace(payload.Method + " " + payload.Host + payload.Path)
			if descriptor == "" {
				// Task / credential kinds don't carry method/host/path. Fall
				// back to the kind so the trace is still readable.
				descriptor = "kind=" + rec.Kind
			}
			a.Logf("\napprover» %s → %s", descriptor, resolution)
		}
	}
}

func (a *Approver) recordFailure(msg string) {
	a.mu.Lock()
	a.failures = append(a.failures, msg)
	a.mu.Unlock()
}

// Resolutions returns a copy of the approval-id → resolution map.
func (a *Approver) Resolutions() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.resolved))
	for k, v := range a.resolved {
		out[k] = v
	}
	return out
}

// Failures returns any failures seen by the approver loop.
func (a *Approver) Failures() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.failures...)
}
