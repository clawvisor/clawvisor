package handlers

import (
	"errors"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

func TestAdapterExecutionFailureAuditRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		failure   *adapters.ExecutionFailure
		wantAudit string
		wantCode  string
		wantKind  adapters.ExecutionFailureKind
		wantTimed bool
	}{
		{
			name:      "definite",
			failure:   &adapters.ExecutionFailure{Kind: adapters.ExecutionFailureDefinite, Err: errors.New("rejected")},
			wantAudit: "provider_definite_failure",
			wantCode:  "PROVIDER_DEFINITE_FAILURE",
			wantKind:  adapters.ExecutionFailureDefinite,
		},
		{
			name:      "stale version",
			failure:   &adapters.ExecutionFailure{Kind: adapters.ExecutionFailureStaleVersion, Err: errors.New("stale")},
			wantAudit: "provider_stale_version",
			wantCode:  "STALE_VERSION",
			wantKind:  adapters.ExecutionFailureStaleVersion,
		},
		{
			name:      "ambiguous",
			failure:   &adapters.ExecutionFailure{Kind: adapters.ExecutionFailureAmbiguous, Err: errors.New("unknown")},
			wantAudit: "provider_ambiguous",
			wantCode:  "AMBIGUOUS_OUTCOME",
			wantKind:  adapters.ExecutionFailureAmbiguous,
		},
		{
			name:      "timeout",
			failure:   &adapters.ExecutionFailure{Kind: adapters.ExecutionFailureAmbiguous, TimedOut: true, Err: errors.New("timed out")},
			wantAudit: "provider_timeout",
			wantCode:  "PROVIDER_TIMEOUT",
			wantKind:  adapters.ExecutionFailureAmbiguous,
			wantTimed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			description, ok := describeAdapterExecutionFailure(tc.failure)
			if !ok {
				t.Fatal("execution failure was not classified")
			}
			if description.AuditOutcome != tc.wantAudit ||
				description.Code != tc.wantCode ||
				description.Kind != tc.wantKind ||
				description.TimedOut != tc.wantTimed {
				t.Fatalf("execution classification = %+v", description)
			}

			restored, ok := describeAuditExecutionFailure(description.AuditOutcome)
			if !ok {
				t.Fatal("persisted failure was not restored")
			}
			if restored != description {
				t.Fatalf("restored classification = %+v, want %+v", restored, description)
			}
		})
	}
}

func TestApprovalTimeoutIsNotClassifiedAsProviderTimeout(t *testing.T) {
	if got, ok := describeAuditExecutionFailure("timeout"); ok {
		t.Fatalf("approval timeout was misclassified as provider failure: %+v", got)
	}
}
