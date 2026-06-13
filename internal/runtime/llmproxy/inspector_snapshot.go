package llmproxy

import (
	"github.com/clawvisor/clawvisor/internal/runtime/eval"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

// InspectorSnapshot translates an inspector.Verdict to the
// audit-row projection conversation.AuditEvent carries. This keeps the
// conversation package out of the inspector dependency by running the
// conversion at the llmproxy boundary where both types are already
// imported.
func InspectorSnapshot(v inspector.Verdict) eval.InspectorVerdictSnapshot {
	snap := eval.InspectorVerdictSnapshot{
		Source:           string(v.Source),
		Host:             v.Host,
		Method:           v.Method,
		Path:             v.Path,
		Reason:           v.Reason,
		IsAPICall:        v.IsAPICall,
		Ambiguous:        v.Ambiguous,
		AgentRecoverable: v.AgentRecoverable,
		Placeholders:     append([]string(nil), v.Placeholders...),
	}
	if len(v.CredentialLocations) > 0 {
		snap.CredentialLocations = make([]eval.CredentialLocation, len(v.CredentialLocations))
		for i, c := range v.CredentialLocations {
			snap.CredentialLocations[i] = eval.CredentialLocation{
				Kind:   c.Kind,
				Name:   c.Name,
				Scheme: c.Scheme,
			}
		}
	}
	return snap
}

// InspectorVerdictFromSnapshot is the reverse of InspectorSnapshot.
// Used by postproc when it reconstructs an inspector.Verdict from a
// buffered AuditEvent for downstream book-keeping (PendingLiteApproval
// carries a typed Verdict).
func InspectorVerdictFromSnapshot(snap eval.InspectorVerdictSnapshot) inspector.Verdict {
	v := inspector.Verdict{
		Source:           inspector.VerdictSource(snap.Source),
		Host:             snap.Host,
		Method:           snap.Method,
		Path:             snap.Path,
		Reason:           snap.Reason,
		IsAPICall:        snap.IsAPICall,
		Ambiguous:        snap.Ambiguous,
		AgentRecoverable: snap.AgentRecoverable,
		Placeholders:     append([]string(nil), snap.Placeholders...),
	}
	if len(snap.CredentialLocations) > 0 {
		v.CredentialLocations = make([]inspector.CredentialLocation, len(snap.CredentialLocations))
		for i, c := range snap.CredentialLocations {
			v.CredentialLocations[i] = inspector.CredentialLocation{
				Kind:   c.Kind,
				Name:   c.Name,
				Scheme: c.Scheme,
			}
		}
	}
	return v
}
