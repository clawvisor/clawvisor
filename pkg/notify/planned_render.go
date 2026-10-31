package notify

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// plannedParamsMaxLen caps the params text in a chat approval message so one
// long value (an email body, say) can't blow past Slack's or Telegram's
// message limits. The truncation marker counts toward the cap.
const plannedParamsMaxLen = 500

// PlannedParamsTruncatedMarker ends params text that was cut to fit.
const PlannedParamsTruncatedMarker = "… (truncated; review the full params in the dashboard)"

// RenderPlannedCallParams renders a planned call's params as compact JSON for
// approval messages. A planned call whose params match skips per-request
// intent verification, so the approver needs to see exactly what was
// planned. Returns "" when the call has no params (it never matches). Output
// past plannedParamsMaxLen is cut with an explicit marker rather than
// silently, so a truncated plan is never mistaken for a complete one.
func RenderPlannedCallParams(pc store.PlannedCall) string {
	if len(pc.Params) == 0 {
		return ""
	}
	// Each surface escapes for its own markup, so skip encoding/json's HTML
	// escaping, which would show "<a@b>" to the approver as "\u003ca@b\u003e".
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(pc.Params); err != nil {
		return "(params could not be displayed; review them in the dashboard)"
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	if r := []rune(s); len(r) > plannedParamsMaxLen {
		keep := plannedParamsMaxLen - len([]rune(PlannedParamsTruncatedMarker))
		s = string(r[:keep]) + PlannedParamsTruncatedMarker
	}
	return s
}
