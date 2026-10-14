package handlers

import (
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/gateway"
)

// Classified adapter failures are persisted as audit outcomes so a retry with
// the same request_id can reconstruct the original machine-readable response
// without invoking the provider again.
const (
	auditOutcomeDefiniteFailure = "provider_definite_failure"
	auditOutcomeStaleVersion    = "provider_stale_version"
	auditOutcomeAmbiguous       = "provider_ambiguous"
	auditOutcomeTimeout         = "provider_timeout"
)

type adapterExecutionFailureDescription struct {
	AuditOutcome string
	Code         string
	Kind         adapters.ExecutionFailureKind
	TimedOut     bool
}

func describeAdapterExecutionFailure(err error) (adapterExecutionFailureDescription, bool) {
	failure, ok := adapters.AsExecutionFailure(err)
	if !ok {
		return adapterExecutionFailureDescription{}, false
	}

	switch failure.Kind {
	case adapters.ExecutionFailureDefinite:
		return adapterExecutionFailureDescription{
			AuditOutcome: auditOutcomeDefiniteFailure,
			Code:         gateway.CodeProviderDefiniteFailure,
			Kind:         adapters.ExecutionFailureDefinite,
		}, true
	case adapters.ExecutionFailureStaleVersion:
		return adapterExecutionFailureDescription{
			AuditOutcome: auditOutcomeStaleVersion,
			Code:         gateway.CodeStaleVersion,
			Kind:         adapters.ExecutionFailureStaleVersion,
		}, true
	case adapters.ExecutionFailureAmbiguous:
		if failure.TimedOut {
			return adapterExecutionFailureDescription{
				AuditOutcome: auditOutcomeTimeout,
				Code:         gateway.CodeProviderTimeout,
				Kind:         adapters.ExecutionFailureAmbiguous,
				TimedOut:     true,
			}, true
		}
		return adapterExecutionFailureDescription{
			AuditOutcome: auditOutcomeAmbiguous,
			Code:         gateway.CodeAmbiguousOutcome,
			Kind:         adapters.ExecutionFailureAmbiguous,
		}, true
	default:
		return adapterExecutionFailureDescription{}, false
	}
}

func describeAuditExecutionFailure(outcome string) (adapterExecutionFailureDescription, bool) {
	switch outcome {
	case auditOutcomeDefiniteFailure:
		return adapterExecutionFailureDescription{
			AuditOutcome: outcome,
			Code:         gateway.CodeProviderDefiniteFailure,
			Kind:         adapters.ExecutionFailureDefinite,
		}, true
	case auditOutcomeStaleVersion:
		return adapterExecutionFailureDescription{
			AuditOutcome: outcome,
			Code:         gateway.CodeStaleVersion,
			Kind:         adapters.ExecutionFailureStaleVersion,
		}, true
	case auditOutcomeAmbiguous:
		return adapterExecutionFailureDescription{
			AuditOutcome: outcome,
			Code:         gateway.CodeAmbiguousOutcome,
			Kind:         adapters.ExecutionFailureAmbiguous,
		}, true
	case auditOutcomeTimeout:
		return adapterExecutionFailureDescription{
			AuditOutcome: outcome,
			Code:         gateway.CodeProviderTimeout,
			Kind:         adapters.ExecutionFailureAmbiguous,
			TimedOut:     true,
		}, true
	default:
		return adapterExecutionFailureDescription{}, false
	}
}

func addAdapterExecutionFailureFields(resp map[string]any, failure adapterExecutionFailureDescription) {
	resp["code"] = failure.Code
	resp["failure_kind"] = string(failure.Kind)
	if failure.TimedOut {
		resp["timed_out"] = true
	}
}
