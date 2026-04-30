package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
)

func TestLauncherTraceRecorderWritesPhaseEntry(t *testing.T) {
	traceDir := t.TempDir()
	sink, err := runtimetiming.NewFileSink(traceDir)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	rec := &launcherTraceRecorder{
		sink:       sink,
		baseURL:    "http://127.0.0.1:25297",
		agentID:    "agent-123",
		agentAlias: "e2e-agent",
		command:    []string{"claude"},
		workingDir: "/tmp/work",
	}
	rec.recordPhase("child.wait", "session-123", time.Now().Add(-250*time.Millisecond), boolPtr(true), intPtr(0), "child exited")

	tracePath := filepath.Join(traceDir, time.Now().UTC().Format("20060102")+".jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("ReadFile(trace): %v", err)
	}
	lines := splitNonEmptyLines(string(data))
	if len(lines) == 0 {
		t.Fatal("expected launcher trace entry")
	}
	var entry runtimetiming.LauncherTraceEntry
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if entry.TraceType != "launcher_phase" {
		t.Fatalf("expected launcher trace type, got %+v", entry)
	}
	if entry.SessionID != "session-123" || entry.AgentAlias != "e2e-agent" {
		t.Fatalf("unexpected launcher trace entry %+v", entry)
	}
	if entry.Phase != "child.wait" || entry.DurationMS <= 0 {
		t.Fatalf("unexpected launcher trace phase %+v", entry)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 0 {
		t.Fatalf("unexpected launcher exit code %+v", entry)
	}
}

func intPtr(v int) *int { return &v }

func splitNonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
