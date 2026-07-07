// Package govlocal implements the four governance policy callbacks
// (CheckModelPolicy, CheckSpendCap, ScanContentPolicy, RecordViolation)
// LOCALLY against instance-scoped tables, so a self-hosted OSS instance
// gets model allow/deny, spend caps, content policy, and violation
// logging without the cloud layer (spec 06a, PRD §8).
//
// Nil governance callbacks no longer mean "no governance" — they mean "no
// CLOUD governance". The run.go wiring fills each nil hook from these
// builders (cloud wins per-hook, local fills gaps).
//
// Verdict shapes mirror cloud internal/governance/callbacks.go exactly:
// CheckSpendCap returns warning levels "" / "80" / "100"; ScanContentPolicy
// returns the first block-mode policy's admin-authored block_message and
// accumulates flag-mode policy names. This parity is what keeps the
// Terraform provider schemas identical across OSS and cloud (spec 06b).
//
// The orgID parameter is IGNORED by every local implementation — policies
// are instance-scoped, one flat set. Do not filter on it.
package govlocal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/orggov"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// BuildCheckModelPolicy returns the local model-policy check. Loads the
// active instance_model_policy and applies allow/deny exactly like cloud's
// BuildCheckModelPolicy. No active policy → allow. Store errors fail open
// (best-effort; logged) — a self-hosted DB blip must not hard-block every
// request.
func BuildCheckModelPolicy(st store.Store) func(ctx context.Context, orgID, model string) (bool, string) {
	return func(ctx context.Context, _, model string) (bool, string) {
		mp, err := st.GetActiveInstanceModelPolicy(ctx)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return true, ""
			}
			slog.WarnContext(ctx, "govlocal: load model policy failed", "err", err)
			return true, ""
		}
		matched := false
		for _, m := range mp.Models {
			if m == model {
				matched = true
				break
			}
		}
		switch mp.Mode {
		case "deny":
			if matched {
				return false, fmt.Sprintf("model %q is denied by instance policy", model)
			}
		case "allow":
			if !matched {
				return false, fmt.Sprintf("model %q is not in the instance allow-list", model)
			}
		}
		return true, ""
	}
}

// BuildCheckSpendCap returns the local spend-cap check. Evaluates every
// configured instance cap (daily + monthly) against the instance-wide cost
// sum for each window and returns the STRICTEST verdict: any hard block
// wins; otherwise the highest warning level wins. Ported from cloud's
// BuildCheckSpendCap (org/team scopes collapsed to a single instance
// scope). Returns (allow, warningLevel, reason); warningLevel ∈ "" / "80"
// / "100". The 60s per-window cache spares the DB a SUM() per request.
func BuildCheckSpendCap(st store.Store) func(ctx context.Context, orgID, agentID string) (bool, string, string) {
	cache := newSpendCache()
	return func(ctx context.Context, _, _ string) (bool, string, string) {
		caps, err := st.ListInstanceSpendCaps(ctx)
		if err != nil {
			slog.WarnContext(ctx, "govlocal: list spend caps failed", "err", err)
			return true, "", ""
		}
		strictest := capVerdict{allow: true}
		for _, c := range caps {
			since, until := windowBounds(c.WindowKind)
			total, err := cache.sum(ctx, st, c.WindowKind, since, until)
			if err != nil {
				slog.WarnContext(ctx, "govlocal: spend sum failed", "window", c.WindowKind, "err", err)
				continue
			}
			v := evaluateCap(total, c.CapMicros, c.Enforcement, c.WindowKind)
			strictest = strictest.merge(v)
		}
		return strictest.allow, strictest.warning, strictest.reason
	}
}

// BuildScanContentPolicy returns the local content-scan callback. Loads
// the enabled instance_content_policy rows and evaluates each against the
// extracted content. Returns the first block-action match (with its
// admin-authored block_message), or aggregates flag-action match names
// into flagged. Ported from cloud's BuildScanContentPolicy.
func BuildScanContentPolicy(st store.Store) func(ctx context.Context, orgID, content string) (bool, string, string, []string) {
	return func(ctx context.Context, _, content string) (bool, string, string, []string) {
		policies, err := st.ListInstanceContentPolicies(ctx)
		if err != nil {
			slog.WarnContext(ctx, "govlocal: list content policies failed", "err", err)
			return true, "", "", nil
		}
		var flagged []string
		for _, p := range policies {
			if !p.Enabled {
				continue
			}
			if !patternMatchSafe(p, content) {
				continue
			}
			if p.Action == "block" {
				return false, p.BlockMessage, fmt.Sprintf("content matched policy %q", p.Name), nil
			}
			// flag — keep going to collect all matches
			flagged = append(flagged, p.Name)
		}
		if len(flagged) > 0 {
			return true, "", fmt.Sprintf("content flagged by %d policy/policies", len(flagged)), flagged
		}
		return true, "", "", nil
	}
}

// BuildRecordViolation returns the local violation recorder. Maps the
// pipeline's orggov.ViolationEvent onto instance_policy_violation, dropping
// the OrgID (the "local" sentinel is never persisted). Best-effort: errors
// are logged, never returned upstream.
func BuildRecordViolation(st store.Store) func(ctx context.Context, evt orggov.ViolationEvent) {
	return func(ctx context.Context, evt orggov.ViolationEvent) {
		err := st.RecordInstancePolicyViolation(ctx, &store.InstancePolicyViolation{
			UserID:      evt.UserID,
			AgentID:     evt.AgentID,
			TaskID:      evt.TaskID,
			PolicyKind:  evt.PolicyKind,
			ActionTaken: evt.ActionTaken,
			Detail:      evt.Detail,
		})
		if err != nil {
			slog.WarnContext(ctx, "govlocal: record violation failed",
				"policy_kind", evt.PolicyKind, "err", err)
		}
	}
}

// capVerdict captures one cap's evaluation result. Ported from cloud.
type capVerdict struct {
	allow   bool
	warning string // "" | "80" | "100"
	reason  string
}

// merge folds a single cap result into the running strictest verdict.
// Rules (identical to cloud): any !allow wins; among allows, the higher
// warning level wins (100 > 80 > ""); reason follows the strictest cap.
func (v capVerdict) merge(o capVerdict) capVerdict {
	if !o.allow {
		if v.allow {
			return o
		}
		return v
	}
	if !v.allow {
		return v
	}
	if warningRank(o.warning) > warningRank(v.warning) {
		return o
	}
	return v
}

func warningRank(w string) int {
	switch w {
	case "100":
		return 2
	case "80":
		return 1
	}
	return 0
}

// evaluateCap returns the verdict for a single cap. Hard caps block at
// 100%; soft caps emit warning levels but always allow. Ported from cloud
// evaluateCap (scope label dropped — one instance scope). The reason
// strings are instance-scoped variants of cloud's.
func evaluateCap(currentMicros, capMicros int64, enforcement, window string) capVerdict {
	if capMicros <= 0 {
		return capVerdict{allow: true}
	}
	pct := float64(currentMicros) / float64(capMicros)
	if pct >= 1.0 {
		reason := fmt.Sprintf("%s spend cap reached ($%.2f / $%.2f)", window, microsToUSD(currentMicros), microsToUSD(capMicros))
		if enforcement == "hard" {
			return capVerdict{allow: false, warning: "100", reason: reason}
		}
		return capVerdict{allow: true, warning: "100", reason: reason}
	}
	if pct >= 0.80 {
		reason := fmt.Sprintf("%s spend at %.0f%% of cap ($%.2f / $%.2f)", window, pct*100, microsToUSD(currentMicros), microsToUSD(capMicros))
		return capVerdict{allow: true, warning: "80", reason: reason}
	}
	return capVerdict{allow: true}
}

func microsToUSD(m int64) float64 { return float64(m) / 1_000_000.0 }
