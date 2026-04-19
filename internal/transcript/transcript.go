// Package transcript bridges the Stage 0 legacy buffer (plugin-sourced)
// and the Stage 1+ transcript_events table (proxy-sourced). Callers
// (auto-approval, dashboard, cross-check) fetch a unified, source-
// ranked view without caring which source produced each message.
//
// Source preference (highest first) when the bridge has proxy_enabled=true:
//   1. transcript_events rows with source='proxy', stream='channel'
//   2. (fallback) legacy buffer entries for the same key
//
// For proxy_enabled=false bridges, legacy buffer entries are the only
// source. See docs/design-proxy-stage1.md §5.3.
package transcript

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Recent returns the most recent channel-stream messages for a bridge+
// conversation, as groupchat.BufferedMessage values so existing auto-
// approval code consumes them unchanged. Source-ranked: proxy > plugin.
//
// When proxyEnabled is true, we ALSO overlay the legacy buffer entries
// for the same scope — this helps during the opt-in cutover window and
// covers channels the proxy cannot see (webchat).
//
// `since` bounds the lookback; zero means "last 15 minutes" (matches the
// legacy buffer's retention). `limit` caps the returned count; zero means
// no cap.
func Recent(
	ctx context.Context,
	st store.Store,
	buf groupchat.Buffer,
	bridgeID, userID, conversationID string,
	proxyEnabled bool,
	since time.Time,
	limit int,
) ([]groupchat.BufferedMessage, error) {
	if since.IsZero() {
		since = time.Now().Add(-15 * time.Minute)
	}

	var out []groupchat.BufferedMessage

	// Primary source (proxy, channel stream) — only if proxy is enabled.
	if proxyEnabled && st != nil && bridgeID != "" && conversationID != "" {
		events, err := st.ListTranscriptEvents(ctx, store.TranscriptEventFilter{
			BridgeID:       bridgeID,
			ConversationID: conversationID,
			Source:         "proxy",
			Stream:         "channel",
			Since:          since,
			Limit:          limit,
		})
		if err == nil {
			for _, e := range events {
				out = append(out, transcriptToBufferedMessage(e))
			}
		}
	}

	// Secondary source (plugin scavenger). Still useful for channels the
	// proxy cannot observe (e.g., in-container webchat) and during the
	// cutover window where both sources coexist.
	if buf != nil && userID != "" && conversationID != "" {
		seen := make(map[string]struct{}, len(out))
		for _, m := range out {
			seen[m.EventID] = struct{}{}
		}
		legacy := buf.Messages(groupchat.UserScopedKey(userID, conversationID))
		for _, m := range legacy {
			if _, dup := seen[m.EventID]; dup {
				continue
			}
			if m.Timestamp.Before(since) {
				continue
			}
			out = append(out, m)
		}
	}

	// Order by timestamp ascending so the LLM sees chronological context.
	// Simple insertion sort (usually 0-20 messages; not worth heap).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Timestamp.Before(out[j-1].Timestamp); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}

	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func transcriptToBufferedMessage(e *store.TranscriptEvent) groupchat.BufferedMessage {
	return groupchat.BufferedMessage{
		Text:       e.Text,
		SenderID:   e.AgentTokenID,
		SenderName: senderLabelForTurn(e),
		Timestamp:  e.TS,
		EventID:    e.EventID,
		BridgeID:   e.BridgeID,
		Channel:    e.Provider,
		ThreadID:   "",
		MessageID:  e.EventID,
		Role:       e.Role,
	}
}

// senderLabelForTurn produces a human-readable sender label for the LLM
// prompt. Proxy-sourced channel events have the platform-native ID in
// AgentTokenID; we expose a role-shaped label so the approval LLM can't
// be tricked by a user naming themselves "assistant".
func senderLabelForTurn(e *store.TranscriptEvent) string {
	switch e.Role {
	case "assistant":
		return "agent"
	case "tool":
		return "tool"
	case "system":
		return "system"
	default:
		// "user" — use the native sender id if we have it.
		if e.AgentTokenID != "" {
			return e.AgentTokenID
		}
		return "user"
	}
}
