package llmproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// TraceLogger writes structured JSON-line trace events for lite-proxy
// decision points. It's strictly observational — never affects the
// returned verdict — and disabled by default. Operators turn it on by
// setting `proxy_lite.trace_log_path` in config.yaml or the
// `CLAWVISOR_PROXY_LITE_TRACE` env var.
//
// One file, append mode, no rotation. Operators can `rm` or rename
// between runs. The output format is JSON-lines so it's easy to
// `tail -f` and post-process with `jq`.
//
// A nil receiver is a no-op — the trace methods can be called
// unconditionally from production paths without a branch at each site.
type TraceLogger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// OpenTraceLogger opens path for append + create and returns a
// TraceLogger that writes to it. Empty path returns (nil, nil) so
// callers can plug the result straight into a struct field with no
// nil-guard.
func OpenTraceLogger(path string) (*TraceLogger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lite-proxy: open trace log %q: %w", path, err)
	}
	return NewTraceLogger(f), nil
}

// NewTraceLogger wraps an existing io.Writer. Useful in tests where a
// bytes.Buffer stands in for the file.
func NewTraceLogger(w io.Writer) *TraceLogger {
	return &TraceLogger{w: w, now: time.Now}
}

// Emit serializes a single event as one JSON line. A timestamp field
// is always added. Marshal errors are silently dropped — trace logging
// must never disrupt the request path.
func (t *TraceLogger) Emit(event map[string]any) {
	if t == nil || t.w == nil {
		return
	}
	if event == nil {
		event = map[string]any{}
	}
	event["timestamp"] = t.now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.w.Write(line)
	_, _ = t.w.Write([]byte{'\n'})
}

// Event-name constants. Operators grep on these to find specific
// stages of the decision pipeline.
const (
	TraceEventToolUseEntry   = "tool_use_entry"
	TraceEventInspectVerdict = "inspect_verdict"
	TraceEventValidatorCall  = "validator_call"
	TraceEventBoundaryCheck  = "boundary_check"
	TraceEventDecision       = "decision"
	TraceEventFinalVerdict   = "final_verdict"
	TraceEventControlRewrite = "control_rewrite"
	TraceEventNonceMint      = "nonce_mint"
	TraceEventRewriteApplied = "rewrite_applied"
	TraceEventSecretPipeline = "secret_pipeline"
	TraceEventTaskSlide      = "task_slide"
	// TraceEventScopeDriftRegistered fires once per scope-drift menu
	// emission, carrying the minted drift_id and the source/reason that
	// triggered the menu. Lets operators correlate a "menu fired"
	// audit row with the registry record the agent will resolve.
	TraceEventScopeDriftRegistered = "scope_drift_registered"
)

const (
	traceInputPreviewLimit = 2048
	traceRawResponseLimit  = 4096
)

// truncateForTrace caps long strings so a trace line stays bounded.
// Adds a literal `...<truncated>` suffix so operators can tell the
// preview was cut.
func truncateForTrace(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...<truncated>"
}
