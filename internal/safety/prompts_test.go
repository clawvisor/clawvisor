package safety_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/safety"
)

func TestParseSafetyResponse_Safe(t *testing.T) {
	result, err := safety.ParseSafetyResponse(`{"decision": "safe", "reason": ""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Safe {
		t.Error("expected Safe=true")
	}
}

func TestParseSafetyResponse_Flag(t *testing.T) {
	result, err := safety.ParseSafetyResponse(`{"decision": "flag", "reason": "prompt injection detected"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Safe {
		t.Error("expected Safe=false")
	}
	if result.Reason != "prompt injection detected" {
		t.Errorf("reason: got %q, want %q", result.Reason, "prompt injection detected")
	}
}

func TestParseSafetyResponse_WithJSONFence(t *testing.T) {
	raw := "```json\n{\"decision\": \"safe\", \"reason\": \"\"}\n```"
	result, err := safety.ParseSafetyResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Safe {
		t.Error("expected Safe=true after JSON fence strip")
	}
}

func TestParseSafetyResponse_WithPlainFence(t *testing.T) {
	raw := "```\n{\"decision\": \"safe\", \"reason\": \"\"}\n```"
	result, err := safety.ParseSafetyResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Safe {
		t.Error("expected Safe=true after plain fence strip")
	}
}

func TestParseSafetyResponse_Invalid_FailsSafe(t *testing.T) {
	// Malformed response → fails safe (Safe=false), no error returned.
	result, err := safety.ParseSafetyResponse("not valid json at all")
	if err != nil {
		t.Fatalf("should not return error on malformed input (fails safe): %v", err)
	}
	if result.Safe {
		t.Error("malformed response: expected Safe=false (fail safe)")
	}
	if result.Reason == "" {
		t.Error("malformed response: expected non-empty reason explaining the failure")
	}
}

func TestParseSafetyResponse_UnknownDecision_NotSafe(t *testing.T) {
	// Any decision value other than "safe" → Safe=false
	result, err := safety.ParseSafetyResponse(`{"decision": "unknown_value", "reason": "hmm"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Safe {
		t.Error("unknown decision: expected Safe=false")
	}
}

func TestParseSafetyResponse_EmptyDecision_NotSafe(t *testing.T) {
	result, err := safety.ParseSafetyResponse(`{"decision": "", "reason": ""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Safe {
		t.Error("empty decision: expected Safe=false")
	}
}

func TestParseSafetyResponse_ReasonPreserved(t *testing.T) {
	result, err := safety.ParseSafetyResponse(`{"decision": "flag", "reason": "exfiltration attempt via bcc field"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "exfiltration attempt via bcc field" {
		t.Errorf("reason not preserved: got %q", result.Reason)
	}
}
