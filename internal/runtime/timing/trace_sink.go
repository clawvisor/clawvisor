package timing

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type TraceSpan struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
}

type TraceEntry struct {
	TraceType       string         `json:"trace_type,omitempty"`
	Timestamp       time.Time      `json:"timestamp"`
	RequestID       string         `json:"request_id"`
	SessionID       string         `json:"session_id,omitempty"`
	AgentID         string         `json:"agent_id,omitempty"`
	Method          string         `json:"method,omitempty"`
	Host            string         `json:"host,omitempty"`
	Path            string         `json:"path,omitempty"`
	Provider        string         `json:"provider,omitempty"`
	ObservationMode bool           `json:"observation_mode,omitempty"`
	StatusCode      int            `json:"status_code,omitempty"`
	TotalMS         int64          `json:"total_ms"`
	Summary         string         `json:"summary,omitempty"`
	Spans           []TraceSpan    `json:"spans,omitempty"`
	Attrs           map[string]any `json:"attrs,omitempty"`
}

type LauncherTraceEntry struct {
	TraceType       string    `json:"trace_type,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	SessionID       string    `json:"session_id,omitempty"`
	AgentID         string    `json:"agent_id,omitempty"`
	AgentAlias      string    `json:"agent_alias,omitempty"`
	BaseURL         string    `json:"base_url,omitempty"`
	Command         []string  `json:"command,omitempty"`
	WorkingDir      string    `json:"working_dir,omitempty"`
	Phase           string    `json:"phase,omitempty"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	ObservationMode *bool     `json:"observation_mode,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Message         string    `json:"message,omitempty"`
}

type BodyCapture struct {
	RelativePath string
	SHA256       string
	Bytes        int
}

type FileSink struct {
	dir string
	mu  sync.Mutex
}

func EnsureDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return nil
}

func NewFileSink(dir string) (*FileSink, error) {
	if dir == "" {
		return nil, fmt.Errorf("timing trace dir is required")
	}
	if err := EnsureDir(dir); err != nil {
		return nil, err
	}
	return &FileSink{dir: dir}, nil
}

func (s *FileSink) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *FileSink) Write(entry TraceEntry) error {
	entry.TraceType = firstNonEmpty(entry.TraceType, "proxy_request")
	return s.writeAt(entry.Timestamp, entry)
}

func (s *FileSink) WriteLauncher(entry LauncherTraceEntry) error {
	entry.TraceType = firstNonEmpty(entry.TraceType, "launcher_phase")
	return s.writeAt(entry.Timestamp, entry)
}

func (s *FileSink) WriteBody(baseDir string, ts time.Time, requestID, kind string, body []byte) (*BodyCapture, error) {
	if s == nil {
		return nil, nil
	}
	if requestID == "" {
		return nil, fmt.Errorf("request id is required for body capture")
	}
	if kind == "" {
		return nil, fmt.Errorf("body capture kind is required")
	}
	if err := EnsureDir(baseDir); err != nil {
		return nil, err
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	dayDir := filepath.Join(baseDir, ts.UTC().Format("20060102"))
	if err := EnsureDir(dayDir); err != nil {
		return nil, err
	}
	filename := requestID + "." + kind + ".body"
	path := filepath.Join(dayDir, filename)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return nil, fmt.Errorf("write body capture: %w", err)
	}
	sum := sha256.Sum256(body)
	return &BodyCapture{
		RelativePath: filepath.Join(ts.UTC().Format("20060102"), filename),
		SHA256:       fmt.Sprintf("%x", sum[:]),
		Bytes:        len(body),
	}, nil
}

func (s *FileSink) writeAt(ts time.Time, entry any) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	path := filepath.Join(s.dir, ts.UTC().Format("20060102")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open timing trace file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	if err := enc.Encode(entry); err != nil {
		return fmt.Errorf("encode timing trace entry: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
