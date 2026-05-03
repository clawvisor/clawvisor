package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
	"github.com/clawvisor/clawvisor/pkg/config"
)

type launcherTraceRecorder struct {
	sink       *runtimetiming.FileSink
	baseURL    string
	agentID    string
	agentAlias string
	command    []string
	workingDir string
}

func newLauncherTraceRecorder(creds *resolvedAgentCredentials, command []string) *launcherTraceRecorder {
	cfg := loadLocalRuntimeTraceConfig()
	if cfg == nil || !cfg.RuntimeProxy.TimingTraceEnabled {
		return nil
	}
	traceDir := expandConfigPath(cfg.RuntimeProxy.TimingTraceDir)
	sink, err := runtimetiming.NewFileSink(traceDir)
	if err != nil {
		return nil
	}
	wd, _ := os.Getwd()
	rec := &launcherTraceRecorder{
		sink:       sink,
		command:    append([]string(nil), command...),
		workingDir: wd,
	}
	if creds != nil {
		rec.baseURL = creds.BaseURL
		rec.agentID = creds.AgentID
		rec.agentAlias = creds.Alias
	}
	return rec
}

func (r *launcherTraceRecorder) recordPhase(phase, sessionID string, startedAt time.Time, observation *bool, exitCode *int, message string) {
	if r == nil || r.sink == nil {
		return
	}
	entry := runtimetiming.LauncherTraceEntry{
		TraceType:       "launcher_phase",
		Timestamp:       time.Now().UTC(),
		SessionID:       sessionID,
		AgentID:         r.agentID,
		AgentAlias:      r.agentAlias,
		BaseURL:         r.baseURL,
		Command:         append([]string(nil), r.command...),
		WorkingDir:      r.workingDir,
		Phase:           phase,
		ObservationMode: observation,
		ExitCode:        exitCode,
		Message:         message,
	}
	if !startedAt.IsZero() {
		entry.DurationMS = time.Since(startedAt).Milliseconds()
	}
	_ = r.sink.WriteLauncher(entry)
}

var (
	localRuntimeTraceConfigOnce sync.Once
	localRuntimeTraceConfig     *config.Config
)

func loadLocalRuntimeTraceConfig() *config.Config {
	localRuntimeTraceConfigOnce.Do(func() {
		cfg := config.Default()
		path := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
		if path == "" {
			home, err := os.UserHomeDir()
			if err == nil {
				path = filepath.Join(home, ".clawvisor", "config.yaml")
			}
		}
		if loaded, err := config.Load(path); err == nil && loaded != nil {
			cfg = loaded
		}
		localRuntimeTraceConfig = cfg
	})
	return localRuntimeTraceConfig
}
