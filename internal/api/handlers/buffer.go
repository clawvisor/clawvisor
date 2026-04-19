package handlers

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Ingest guardrails. Messages flow from the plugin runtime, but the sender
// fields originate in the user's messaging platform (Slack/Discord/etc.) —
// other participants can set their display names to arbitrary strings, so we
// treat these as user-controlled input and sanitize before they reach the
// approval LLM prompt.
const (
	maxSenderLen      = 128
	futureTimestampSkewSeconds = 60
)

// BufferHandler accepts conversation messages forwarded by a paired OpenClaw
// plugin so that auto-approval has the full conversation context, even when
// Clawvisor does not own the messaging I/O. Authenticated via a bridge token
// — never an agent token, so the agent can't plant approval messages.
type BufferHandler struct {
	buffer groupchat.Buffer
	st     store.Store
	logger *slog.Logger
}

func NewBufferHandler(buf groupchat.Buffer, st store.Store, logger *slog.Logger) *BufferHandler {
	return &BufferHandler{buffer: buf, st: st, logger: logger}
}

type bufferIngestRequest struct {
	GroupChatID string `json:"group_chat_id"`
	Text        string `json:"text"`
	SenderID    string `json:"sender_id"`
	SenderName  string `json:"sender_name"`
	Timestamp   int64  `json:"timestamp"`

	// Ingest integrity (v1 hardening per reviewer feedback):
	// - EventID: client-generated idempotency key; exact replay is a no-op.
	// - Seq: per-bridge monotonic counter; server rejects regressions and
	//   dup-seq-with-different-event.
	EventID string `json:"event_id"`
	Seq     int64  `json:"seq"`

	// Structured provenance. Fed into the LLM prompt as explicit fields
	// rather than flattened text, so user-controlled sender names can't
	// break out of the transcript framing.
	Channel   string `json:"channel,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Role      string `json:"role,omitempty"` // user | assistant | tool | system
}

// validRoles bounds the role enum so the plugin can't smuggle arbitrary
// strings into the LLM prompt's role field.
var validRoles = map[string]bool{
	"user":      true,
	"assistant": true,
	"tool":      true,
	"system":    true,
}

// Ingest handles POST /api/buffer/ingest. Messages are appended under a
// user-scoped key (`u:{user_id}:{group_chat_id}`) so that conversations from
// different users cannot collide on shared group-chat identifiers.
func (h *BufferHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bridge := middleware.BridgeFromContext(ctx)
	if bridge == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.buffer == nil {
		writeError(w, http.StatusServiceUnavailable, "BUFFER_UNAVAILABLE", "message buffer is not configured")
		return
	}

	var req bufferIngestRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	groupChatID := strings.TrimSpace(req.GroupChatID)
	text := strings.TrimSpace(req.Text)
	if groupChatID == "" || text == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "group_chat_id and text are required")
		return
	}
	eventID := strings.TrimSpace(req.EventID)
	if eventID == "" || req.Seq <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "event_id and seq (>0) are required")
		return
	}

	ts, ok := resolveIngestTimestamp(req.Timestamp)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dropped": "future_timestamp"})
		return
	}

	key := groupchat.UserScopedKey(bridge.UserID, groupChatID)

	// Exact-replay dedupe: if the same (bridge, event_id) is already in
	// this buffer slice, treat as no-op. The retention-window scan is
	// bounded by the buffer's maxCount, so this is cheap.
	for _, m := range h.buffer.Messages(key) {
		if m.BridgeID == bridge.ID && m.EventID == eventID {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deduped": true})
			return
		}
	}

	// Atomic monotonic advance: advances last_seq iff req.Seq is strictly
	// greater than the current stored value. Rejects dup-seq (same number
	// but different event_id) and out-of-order inserts — a stolen bridge
	// token replaying old events can't slip past.
	advanced, err := h.st.AdvanceBridgeLastSeq(ctx, bridge.ID, req.Seq)
	if err != nil {
		h.logger.Warn("advance bridge last_seq failed", "err", err, "bridge_id", bridge.ID)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not record ingest sequence")
		return
	}
	if !advanced {
		writeError(w, http.StatusConflict, "SEQ_REGRESSION", "seq must be strictly greater than the last ingested event for this bridge")
		return
	}

	senderID := sanitizeSenderField(req.SenderID)
	if senderID == "" {
		senderID = "unknown"
	}
	senderName := sanitizeSenderField(req.SenderName)
	if senderName == "" {
		senderName = senderID
	}
	role := req.Role
	if !validRoles[role] {
		role = ""
	}

	h.buffer.Append(key, groupchat.BufferedMessage{
		Text:       text,
		SenderID:   senderID,
		SenderName: senderName,
		Timestamp:  ts,
		EventID:    eventID,
		Seq:        req.Seq,
		BridgeID:   bridge.ID,
		Channel:    sanitizeSenderField(req.Channel),
		ThreadID:   sanitizeSenderField(req.ThreadID),
		MessageID:  sanitizeSenderField(req.MessageID),
		Role:       role,
	})

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resolveIngestTimestamp rejects far-future timestamps — otherwise a client
// could keep a forged message alive past the buffer's maxAge filter by
// setting `timestamp` arbitrarily far forward. Past timestamps are allowed;
// the buffer's own maxAge filter drops them naturally on read.
func resolveIngestTimestamp(raw int64) (time.Time, bool) {
	now := time.Now().UTC()
	if raw <= 0 {
		return now, true
	}
	ts := time.Unix(raw, 0).UTC()
	if ts.After(now.Add(futureTimestampSkewSeconds * time.Second)) {
		return time.Time{}, false
	}
	return ts, true
}

// sanitizeSenderField strips characters that would let a participant in the
// source conversation forge a transcript turn inside the approval LLM prompt
// (e.g. embedding `\n` + `<msg>...</msg>` in their display name). Also
// length-caps so ridiculously long names don't dominate the prompt.
func sanitizeSenderField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Drop every occurrence of the `<msg>` / `</msg>` delimiters used by the
	// approval prompt format — the prompt wraps the actual text in
	// `<msg>...</msg>`, and a sender_name containing the closing tag could
	// end that wrap early and inject a forged transcript turn after.
	for _, tok := range []string{"<msg>", "</msg>"} {
		for {
			lower := strings.ToLower(s)
			idx := strings.Index(lower, tok)
			if idx < 0 {
				break
			}
			s = s[:idx] + strings.Repeat(" ", len(tok)) + s[idx+len(tok):]
		}
	}
	// Strip control chars (including newlines/tabs) — replace with space so
	// word boundaries survive but transcript framing cannot be injected.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	s = strings.TrimSpace(b.String())
	// Length cap (count runes, not bytes — user display names can be unicode).
	runes := []rune(s)
	if len(runes) > maxSenderLen {
		s = string(runes[:maxSenderLen])
	}
	return s
}
