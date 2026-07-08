package clawvisor

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/govlocal"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/orggov"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// composeGovernanceCallbacks builds the orggov.Callbacks + agent→org
// resolver the LLM-proxy pipeline consults, applying per-hook precedence:
//
//	cloud (opts.OrgGov) > local (govlocal) > allow
//
// Cloud hooks win for whichever fields they populate; govlocal fills every
// remaining nil hook when govEnabled. In a pure-OSS build (cloud == nil)
// every hook comes from govlocal. The returned resolver is the cloud's
// OrgIDForAgent when provided, otherwise the "local" sentinel (installed
// only when local governance is active and the cloud left it nil — the
// sentinel must never override a real cloud resolver, and never leak into a
// cloud build that resolves real orgs).
//
// wire reports whether any hook was populated (so the caller only registers
// governance when something will actually run).
func composeGovernanceCallbacks(cloud *OrgGovOptions, st store.Store, govEnabled bool) (callbacks orggov.Callbacks, orgIDForAgent func(context.Context, string) string, wire bool) {
	if cloud != nil {
		callbacks.CheckModelPolicy = cloud.CheckModelPolicy
		callbacks.CheckSpendCap = cloud.CheckSpendCap
		callbacks.ScanContentPolicy = cloud.ScanContentPolicy
		if rv := cloud.RecordViolation; rv != nil {
			callbacks.RecordViolation = func(ctx context.Context, evt orggov.ViolationEvent) {
				rv(ctx, OrgGovViolation{
					OrgID:       evt.OrgID,
					UserID:      evt.UserID,
					AgentID:     evt.AgentID,
					TaskID:      evt.TaskID,
					PolicyKind:  evt.PolicyKind,
					ActionTaken: evt.ActionTaken,
					Detail:      evt.Detail,
				})
			}
		}
		orgIDForAgent = cloud.OrgIDForAgent
	}

	if govEnabled && st != nil {
		if callbacks.CheckModelPolicy == nil {
			callbacks.CheckModelPolicy = govlocal.BuildCheckModelPolicy(st)
		}
		if callbacks.CheckSpendCap == nil {
			callbacks.CheckSpendCap = govlocal.BuildCheckSpendCap(st)
		}
		if callbacks.ScanContentPolicy == nil {
			callbacks.ScanContentPolicy = govlocal.BuildScanContentPolicy(st)
		}
		if callbacks.RecordViolation == nil {
			callbacks.RecordViolation = govlocal.BuildRecordViolation(st)
		}
		if orgIDForAgent == nil {
			orgIDForAgent = func(context.Context, string) string { return govlocal.LocalOrgID }
		}
	}

	wire = callbacks.CheckModelPolicy != nil || callbacks.CheckSpendCap != nil ||
		callbacks.ScanContentPolicy != nil || callbacks.RecordViolation != nil
	return callbacks, orgIDForAgent, wire
}
