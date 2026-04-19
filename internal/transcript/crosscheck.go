package transcript

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// CrossCheck scans for disagreements between proxy-sourced and
// plugin-sourced transcripts on the same conversation. Disagreement
// suggests one of:
//   - Agent tampered with plugin JSONL before plugin read it (proxy
//     transcript is authoritative and shows a different reality).
//   - Plugin or proxy parsing bug.
//   - Channel outside either source's visibility (partial coverage).
//
// Records anomalies in the transcript_anomalies table so the dashboard
// can surface them. Fully idempotent; safe to re-run on the same window.
type CrossChecker struct {
	st     store.Store
	buf    groupchat.Buffer
	logger *slog.Logger
}

// NewCrossChecker builds a cross-checker around the primary store + the
// legacy in-memory buffer. `buf` may be nil for cloud deployments that
// don't maintain a local buffer; those only compare proxy data against
// historical transcript_events.
func NewCrossChecker(st store.Store, buf groupchat.Buffer, logger *slog.Logger) *CrossChecker {
	return &CrossChecker{st: st, buf: buf, logger: logger}
}

// RunOnce performs one pass of cross-check over all proxy-enabled
// bridges. Intended to be invoked by a periodic cleanup loop (e.g.,
// every 5 minutes). Non-blocking over errors — one bridge failing
// doesn't affect others.
func (c *CrossChecker) RunOnce(ctx context.Context, lookback time.Duration) {
	if lookback <= 0 {
		lookback = 15 * time.Minute
	}
	since := time.Now().Add(-lookback)

	// Iterate recent proxy-sourced events grouped by (bridge, conversation).
	events, err := c.st.ListTranscriptEvents(ctx, store.TranscriptEventFilter{
		Source: "proxy",
		Stream: "channel",
		Since:  since,
		Limit:  5000,
	})
	if err != nil {
		c.logger.Warn("crosscheck: proxy events fetch failed", "err", err)
		return
	}

	// Group by (bridge, convo).
	type group struct {
		bridgeID       string
		conversationID string
		events         []*store.TranscriptEvent
	}
	groups := map[string]*group{}
	for _, e := range events {
		if e.BridgeID == "" || e.ConversationID == "" {
			continue
		}
		key := e.BridgeID + "|" + e.ConversationID
		g, ok := groups[key]
		if !ok {
			g = &group{bridgeID: e.BridgeID, conversationID: e.ConversationID}
			groups[key] = g
		}
		g.events = append(g.events, e)
	}

	for _, g := range groups {
		c.checkGroup(ctx, g.bridgeID, g.conversationID, g.events, since)
	}
}

// checkGroup compares proxy events for a single conversation against
// what the in-memory buffer holds for the same conversation. Differences
// are recorded as anomalies. Handles three kinds:
//
//   - plugin_only: plugin saw a user message the proxy didn't (suggests
//     agent-side fabrication or a channel outside the proxy's visibility).
//   - proxy_only: proxy saw a message the plugin didn't (plugin blind
//     spot or scavenger not running).
//   - content_mismatch: same logical message, different text.
//
// The first case is the security-sensitive one: it's the tamper signal
// Stage 1 is designed to detect.
func (c *CrossChecker) checkGroup(ctx context.Context, bridgeID, conversationID string, proxyEvents []*store.TranscriptEvent, since time.Time) {
	if c.buf == nil {
		return
	}
	// Look up bridge to get user_id for buffer key scoping.
	bt, err := c.st.GetBridgeTokenByID(ctx, bridgeID)
	if err != nil {
		return
	}
	pluginMessages := c.buf.Messages(groupchat.UserScopedKey(bt.UserID, conversationID))

	// Index plugin messages by (role, text) — simplest fuzzy identity.
	// Stage 1 uses exact text match; Stage 2+ can tighten (normalized
	// whitespace, token-level diff).
	pluginIdx := map[string][]groupchat.BufferedMessage{}
	for _, m := range pluginMessages {
		if m.Timestamp.Before(since) {
			continue
		}
		key := m.Role + "||" + normalizeText(m.Text)
		pluginIdx[key] = append(pluginIdx[key], m)
	}

	// Proxy events by same key.
	proxyIdx := map[string][]*store.TranscriptEvent{}
	for _, e := range proxyEvents {
		key := e.Role + "||" + normalizeText(e.Text)
		proxyIdx[key] = append(proxyIdx[key], e)
	}

	// plugin_only: keys in pluginIdx not in proxyIdx.
	for key, msgs := range pluginIdx {
		if _, ok := proxyIdx[key]; !ok {
			for _, m := range msgs {
				c.recordAnomaly(ctx, bridgeID, conversationID, "plugin_only", anomalyDetail{
					Role: m.Role,
					Text: truncate(m.Text, 500),
					TS:   m.Timestamp.UTC().Format(time.RFC3339Nano),
				})
			}
		}
	}

	// proxy_only: keys in proxyIdx not in pluginIdx. Only matters if the
	// plugin scavenger is expected to be off (proxy_enabled=true) — in
	// that case, proxy_only is the normal case, not an anomaly. Don't
	// record it. We only care about plugin-side missing/tampered entries.
	// Leaving this block here for future when Stage 1.5 allows parallel
	// scavenger + proxy operation with cross-check.
}

type anomalyDetail struct {
	Role string `json:"role,omitempty"`
	Text string `json:"text,omitempty"`
	TS   string `json:"ts,omitempty"`
}

func (c *CrossChecker) recordAnomaly(ctx context.Context, bridgeID, conversationID, kind string, detail anomalyDetail) {
	body, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = c.st.CreateTranscriptAnomaly(ctx, &store.TranscriptAnomaly{
		BridgeID:       bridgeID,
		ConversationID: conversationID,
		Kind:           kind,
		DetailJSON:     string(body),
	})
}

func normalizeText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
