package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// Slack action IDs. The approve/deny pair carries a single-use callback token
// in the button value; the noop pair is used for link buttons, which Slack
// still reports as interactions even though they need no server action.
const (
	actionApprove     = "clawvisor_approve"
	actionDeny        = "clawvisor_deny"
	actionNoopApprove = "clawvisor_link_approve"
	actionNoopDeny    = "clawvisor_link_deny"
)

// maxInteractionBody bounds how much of an interaction payload we will read.
// Slack payloads are a few KB; anything far larger is not a real interaction.
const maxInteractionBody = 1 << 20 // 1 MiB

// slackTimestampSkew is how far a request timestamp may drift before we
// reject it. Slack documents 5 minutes.
const slackTimestampSkew = 5 * time.Minute

// ReplayGuard records recently-seen request signatures so a captured,
// still-in-window Slack payload cannot be replayed. Implementations must be
// shared across replicas in multi-instance deployments.
type ReplayGuard interface {
	// SeenBefore atomically records sig and reports whether it was already
	// present.
	SeenBefore(ctx context.Context, sig string, ttl time.Duration) (bool, error)
}

type noopReplayGuard struct{}

func (noopReplayGuard) SeenBefore(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}

// interactionPayload is the subset of Slack's block_actions payload we use.
type interactionPayload struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// HandleInteraction is the HTTP handler for Slack's Interactivity Request
// URL. It is mounted unauthenticated — the Slack request signature is the
// only credential — so every branch must fail closed.
//
// Slack retries any response that is not a prompt 2xx, so decision failures
// are surfaced to the clicker as an ephemeral message rather than a non-2xx
// status; a 500 here would make Slack redeliver and double-resolve.
func (n *Notifier) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInteractionBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Slack-Signature")
	if err := n.verifySignature(r.Header.Get("X-Slack-Request-Timestamp"), sig, body); err != nil {
		// Deliberately terse: a detailed reason would help an attacker
		// distinguish a bad secret from a stale timestamp.
		n.logger.WarnContext(r.Context(), "slack: rejected interaction", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// The signature is a deterministic function of (secret, timestamp, body),
	// so it doubles as a replay key for the duration of the skew window.
	if seen, err := n.replay.SeenBefore(r.Context(), sig, slackTimestampSkew); err != nil {
		n.logger.WarnContext(r.Context(), "slack: replay guard unavailable", "err", err)
	} else if seen {
		n.logger.WarnContext(r.Context(), "slack: dropped replayed interaction")
		w.WriteHeader(http.StatusOK)
		return
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	var p interactionPayload
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	// Acknowledge immediately. Slack gives us 3 seconds, and resolving a
	// decision can outlast that.
	w.WriteHeader(http.StatusOK)
	go n.processInteraction(context.WithoutCancel(r.Context()), p)
}

func (n *Notifier) processInteraction(ctx context.Context, p interactionPayload) {
	if p.Type != "block_actions" || len(p.Actions) == 0 {
		return
	}
	act := p.Actions[0]

	var action string
	switch act.ActionID {
	case actionApprove:
		action = "approve"
	case actionDeny:
		action = "deny"
	case actionNoopApprove, actionNoopDeny:
		return // link buttons resolve in the dashboard
	default:
		return
	}

	// Peek before consuming: an unauthorized click must not burn the token,
	// or anyone in the channel could disable approvals by clicking first.
	entry, err := n.cbTokens.Peek(act.Value)
	if err != nil {
		n.reportStaleToken(ctx, p.ResponseURL, err)
		return
	}

	cfg, err := n.userConfig(ctx, entry.UserID)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: config unavailable for interaction", "err", err)
		n.ephemeral(ctx, p.ResponseURL, ":warning: This workspace is no longer connected to Clawvisor.")
		return
	}

	// Channel membership must not confer approval rights — check the
	// allowlist, and confirm the click came from the channel and workspace
	// the token was minted for.
	if cfg.TeamID != "" && p.Team.ID != "" && cfg.TeamID != p.Team.ID {
		n.ephemeral(ctx, p.ResponseURL, ":no_entry: This request belongs to a different workspace.")
		return
	}
	if entry.ChannelID != "" && p.Channel.ID != "" && entry.ChannelID != p.Channel.ID {
		n.ephemeral(ctx, p.ResponseURL, ":no_entry: This request cannot be resolved from this channel.")
		return
	}
	if !cfg.CanApprove(p.User.ID) {
		n.logger.InfoContext(ctx, "slack: rejected decision from non-allowlisted user",
			"slack_user_id", p.User.ID, "user_id", entry.UserID)
		n.ephemeral(ctx, p.ResponseURL,
			":no_entry: You're not on the approver allowlist for this workspace. Ask an admin to add you in Clawvisor → Settings → Slack.")
		return
	}

	// Authorized — now retire the token. Losing this race means someone
	// else resolved it first.
	if _, err := n.cbTokens.Consume(act.Value); err != nil {
		n.reportStaleToken(ctx, p.ResponseURL, err)
		return
	}

	// Record who clicked so the resolved message can attribute it. Written
	// before publishing: the decision is consumed asynchronously and may
	// reach the update path immediately.
	n.msgCtx.SetApprover(
		contextKey(targetTypeForDecision(entry.Type), entry.TargetID),
		mention(p.User.ID),
	)

	select {
	case n.decisionCh <- notify.CallbackDecision{
		Type:        entry.Type,
		Action:      action,
		TargetID:    entry.TargetID,
		TaskID:      entry.TaskID,
		UserID:      entry.UserID,
		ApproverRef: approverRef(p),
	}:
	case <-ctx.Done():
	}
}

// approverRef renders the clicking Slack user for the audit trail. The
// account owner is not necessarily the person who clicked, so this is what
// distinguishes "Eric approved" from "Jane approved on Eric's account".
func approverRef(p interactionPayload) string {
	name := p.User.Username
	if name == "" {
		name = p.User.Name
	}
	if name == "" {
		return "slack:" + p.User.ID
	}
	return fmt.Sprintf("slack:%s (%s)", p.User.ID, name)
}

// reportStaleToken explains why a click did nothing, and repairs the message
// when it is safe to do so.
//
// An expired request was never resolved, so its message is stale and still
// showing live-looking buttons — replacing it in place clears them and
// leaves a permanent record, rather than an ephemeral notice the user cannot
// dismiss. The other two cases must not replace anything: an already-resolved
// message already shows its outcome ("Approved by @jane"), and an unknown
// token could belong to a message resolved long ago, so overwriting either
// would destroy accurate history.
func (n *Notifier) reportStaleToken(ctx context.Context, responseURL string, err error) {
	switch {
	case errors.Is(err, errTokenExpired):
		n.replaceOriginal(ctx, responseURL,
			":hourglass: *Expired* — this request timed out and can no longer be approved.",
			[]block{
				sectionRaw(":hourglass: *Expired* — this request timed out and can no longer be approved."),
				contextBlock("Ask the agent to retry if it is still needed."),
			})
	case errors.Is(err, errTokenUsed):
		n.ephemeral(ctx, responseURL, ":information_source: This request has already been resolved.")
	default:
		n.ephemeral(ctx, responseURL, ":warning: This request is no longer available.")
	}
}

// replaceOriginal rewrites the message the button was attached to, via the
// interaction's response_url. This needs no channel/ts and no extra scope,
// and clearing the blocks is what removes the buttons.
func (n *Notifier) replaceOriginal(ctx context.Context, responseURL, text string, blocks []block) {
	if responseURL == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"replace_original": true,
		"text":             plainText(text),
		"blocks":           blocks,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: could not replace stale message", "err", err)
		return
	}
	_ = resp.Body.Close()
}

// ephemeral posts a message visible only to the clicker, via the payload's
// response_url. Best-effort: failure to explain a rejection must not affect
// the rejection itself.
func (n *Notifier) ephemeral(ctx context.Context, responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"response_type":    "ephemeral",
		"replace_original": false,
		"text":             text,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: ephemeral reply failed", "err", err)
		return
	}
	_ = resp.Body.Close()
}

// verifySignature implements Slack's v0 request signing scheme.
func (n *Notifier) verifySignature(timestamp, signature string, body []byte) error {
	if n.signingSecret == "" {
		return errors.New("no signing secret configured")
	}
	if timestamp == "" || signature == "" {
		return errors.New("missing signature headers")
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("malformed timestamp")
	}
	// Reject stale requests in both directions — a far-future timestamp is
	// as suspect as an old one.
	if d := time.Since(time.Unix(ts, 0)); d > slackTimestampSkew || d < -slackTimestampSkew {
		return errors.New("timestamp outside acceptable window")
	}

	mac := hmac.New(sha256.New, []byte(n.signingSecret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(":"))
	mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return errors.New("signature mismatch")
	}
	return nil
}
