package timing

import (
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

type FileSink struct {
	dir string
	mu  sync.Mutex
}

func NewFileSink(dir string) (*FileSink, error) {
	if dir == "" {
		return nil, fmt.Errorf("timing trace dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create timing trace dir: %w", err)
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
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	path := filepath.Join(s.dir, entry.Timestamp.UTC().Format("20060102")+".jsonl")
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
