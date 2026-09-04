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
	"strings"
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
	actionViewDetails = "clawvisor_view_details"
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
	TriggerID   string `json:"trigger_id"`
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
	//
	// An unavailable guard fails closed. Proceeding would leave a captured,
	// still-in-window payload free to be resubmitted until it lands on a
	// replica that has not seen it — silently voiding replay protection on
	// exactly the multi-instance deployments the shared guard exists for.
	// Refusing costs nothing: the guard is unavailable because Redis is, and
	// the Redis token store's lookup two steps down would fail anyway.
	seen, err := n.replay.SeenBefore(r.Context(), sig, slackTimestampSkew)
	if err != nil {
		n.logger.WarnContext(r.Context(), "slack: replay guard unavailable, dropping interaction", "err", err)
		// Unlike a decision failure, nothing has been consumed here, so a
		// Slack retry is safe and may well succeed once Redis recovers.
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if seen {
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
		n.logger.InfoContext(ctx, "slack: ignoring non-action interaction", "type", p.Type)
		return
	}
	act := p.Actions[0]
	// Entry is logged because every exit below is silent to the channel: the
	// user sees a click that did nothing, and until now the logs could not
	// say which of six exits it took.
	n.logger.InfoContext(ctx, "slack: interaction received",
		"action_id", act.ActionID, "slack_user_id", p.User.ID, "channel", p.Channel.ID)

	var action string
	switch act.ActionID {
	case actionApprove:
		action = "approve"
	case actionDeny:
		action = "deny"
	case actionViewDetails:
		// Read-only: opens a modal showing what the message already
		// represents, so it resolves nothing and consumes no token.
		n.openDetailModal(ctx, p, act.Value)
		return
	case actionNoopApprove, actionNoopDeny:
		return // link buttons resolve in the dashboard
	default:
		n.logger.InfoContext(ctx, "slack: unrecognised action_id", "action_id", act.ActionID)
		return
	}

	// Peek before consuming: an unauthorized click must not burn the token,
	// or anyone in the channel could disable approvals by clicking first.
	entry, err := n.cbTokens.Peek(act.Value)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: interaction token not usable", "err", err, "action", action)
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
	// the token was minted for. Both run before Consume, so a click that
	// fails either one cannot burn the token.
	if msg := identityMismatch(cfg, entry, p); msg != "" {
		n.logger.WarnContext(ctx, "slack: interaction rejected on workspace/channel identity",
			"payload_team", p.Team.ID, "config_team", cfg.TeamID,
			"payload_channel", p.Channel.ID, "token_channel", entry.ChannelID)
		n.ephemeral(ctx, p.ResponseURL, msg)
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
		n.logger.WarnContext(ctx, "slack: token consume lost the race", "err", err, "action", action)
		n.reportStaleToken(ctx, p.ResponseURL, err)
		return
	}

	// Record who clicked so the resolved message can attribute it. Written
	// before publishing: the decision is consumed asynchronously and may
	// reach the update path immediately.
	n.msgCtx.SetApprover(messageContextKey(entry), approverDisplay(p))

	// Publishing is logged either way. A send that blocks on a full channel
	// would otherwise look identical to one that was never attempted, and
	// the decision never arriving at the consumer is exactly the symptom
	// being chased.
	decision := notify.CallbackDecision{
		Type:        entry.Type,
		Action:      action,
		TargetID:    entry.TargetID,
		TaskID:      entry.TaskID,
		UserID:      entry.UserID,
		ApproverRef: approverRef(p),
	}

	select {
	case n.decisionCh <- decision:
		n.logger.InfoContext(ctx, "slack: decision published",
			"type", decision.Type, "action", decision.Action, "target_id", decision.TargetID)
	case <-ctx.Done():
		n.logger.WarnContext(ctx, "slack: decision dropped before publish",
			"type", decision.Type, "action", decision.Action, "target_id", decision.TargetID)
	}
}

// identityMismatch reports why a click may not resolve the token it carries,
// or "" when it came from the workspace and channel the token was minted for.
//
// A payload that omits an identifier is rejected exactly like one that
// contradicts it. Treating absent as matching made the isolation checks
// opt-out: an interaction with no team or channel object skipped them
// entirely and could resolve another workspace's request. Only an identifier
// we never recorded (an install predating TeamID, a prompt minted without a
// channel) leaves nothing to compare, and those cases fall through to the
// approver allowlist.
func identityMismatch(cfg notify.SlackConfig, entry *callbackEntry, p interactionPayload) string {
	if cfg.TeamID != "" && cfg.TeamID != p.Team.ID {
		return ":no_entry: This request belongs to a different workspace."
	}
	if entry.ChannelID != "" && entry.ChannelID != p.Channel.ID {
		return ":no_entry: This request cannot be resolved from this channel."
	}
	return ""
}

// messageContextKey addresses the message context stored when the prompt was
// posted, so the resolve path reads back what the send path wrote.
//
// Approval prompts are recorded under the composite request|task key that
// disambiguates sibling approvals under symmetric dedup, while the callback
// token deliberately carries only the bare RequestID. Rebuilding the
// composite here is what keeps the two in agreement: keying on TargetID alone
// missed for every task-scoped approval, and a miss is silent — the resolved
// message simply loses its "Resolved by @X" attribution and detail thread.
func messageContextKey(entry *callbackEntry) string {
	targetType := targetTypeForDecision(entry.Type)
	if targetType == "approval" {
		return contextKey(targetType, approvalNotifyTargetID(entry.TargetID, entry.TaskID))
	}
	return contextKey(targetType, entry.TargetID)
}

// approverDisplay names the clicking user for the resolved message.
//
// Plain text, not a `<@U…>` mention: a mention would notify them about an
// action they just took themselves. Falls back to the raw ID rather than
// emitting a mention, so no path can reintroduce the ping.
func approverDisplay(p interactionPayload) string {
	if p.User.Username != "" {
		return p.User.Username
	}
	if p.User.Name != "" {
		return p.User.Name
	}
	return p.User.ID
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
				section(":hourglass: *Expired* — this request timed out and can no longer be approved."),
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
	target := sanitizedResponseURL(responseURL)
	if target == "" {
		n.logger.WarnContext(ctx, "slack: refusing to use a non-Slack response_url")
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.responseClient.Do(req)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: could not replace stale message", "err", err)
		return
	}
	_ = resp.Body.Close()
}

// errRedirectRefused is returned when a response_url tries to redirect.
var errRedirectRefused = errors.New("slack: refusing to follow a redirect from response_url")

// newResponseClient builds the client used for response_url posts.
//
// It refuses redirects. sanitizedResponseURL only constrains the URL we are
// handed, and Go's default client follows up to 10 hops — so a permitted
// hooks.slack.com URL answering 302 would carry the request to an arbitrary
// host and step straight past the hostname lock. An allowlist has to hold
// for every hop of the request, not just the first, or it is not an
// allowlist at all.
//
// 307/308 also preserve the method and body, so following would be an egress
// path as well as a reachability one. Slack answers response_url directly, so
// a redirect here is always unexpected and refusing it loses nothing.
func newResponseClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}
}

// responseURLHost is the only host Slack serves interaction response_urls
// from. Slack's format is https://hooks.slack.com/actions/T…/…/….
const responseURLHost = "hooks.slack.com"

// sanitizedResponseURL validates an interaction response_url and rebuilds it
// on a constant host, returning "" if it is not a Slack response_url.
//
// The URL arrives inside the interaction payload, so it is attacker-chosen
// input to an outbound request from inside the deployment's network — a
// classic SSRF sink. The v0 signature check normally proves the payload came
// from Slack, but that is a single control over a shared secret: if it ever
// leaks, an unvalidated response_url turns this endpoint into a pivot at
// cloud metadata and internal services.
//
// It rebuilds rather than merely checking the input. Returning the original
// string would leave the request target derived from attacker input even
// after validation — and would carry along userinfo, port and fragment,
// which are exactly the parts that make a hostile URL read as Slack's.
// Reconstructing from a constant scheme and host means only the path and
// query survive, and the host cannot be input-derived at all.
//
// Mirrors the hostname lock the cloud governance webhooks already apply.
func sanitizedResponseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	// EqualFold, and Hostname() not Host, so neither casing nor an appended
	// port slips a different origin through.
	if !strings.EqualFold(u.Hostname(), responseURLHost) {
		return ""
	}
	// RawPath as well as Path: url.Parse stores the decoded path in Path and
	// keeps the original encoding in RawPath, and URL.String() only uses
	// RawPath when it is set. Copying Path alone would silently re-encode a
	// percent-escaped segment (%2F becoming /), changing the path Slack
	// gave us into a different one.
	safe := &url.URL{
		Scheme:   "https",
		Host:     responseURLHost,
		Path:     u.Path,
		RawPath:  u.RawPath,
		RawQuery: u.RawQuery,
	}
	return safe.String()
}

// ephemeral posts a message visible only to the clicker, via the payload's
// response_url. Best-effort: failure to explain a rejection must not affect
// the rejection itself.
func (n *Notifier) ephemeral(ctx context.Context, responseURL, text string) {
	target := sanitizedResponseURL(responseURL)
	if target == "" {
		n.logger.WarnContext(ctx, "slack: refusing to use a non-Slack response_url")
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.responseClient.Do(req)
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

// openDetailModal shows the stashed request detail for a resolved prompt.
//
// Slack's trigger_id is valid for three seconds, so this runs on the path
// that has already acked the request rather than doing any further work
// first. A failure is reported to the clicker only — nobody else needs to
// know their modal did not open.
func (n *Notifier) openDetailModal(ctx context.Context, p interactionPayload, token string) {
	if p.TriggerID == "" {
		n.logger.WarnContext(ctx, "slack: view-details interaction carried no trigger_id")
		return
	}

	entry, ok := n.details.GetDetail(ctx, token)
	if !ok || len(entry.Blocks) == 0 {
		n.ephemeral(ctx, p.ResponseURL,
			":hourglass: These request details are no longer available. The dashboard has the full record.")
		return
	}

	// A token minted for one workspace must not open in another, even though
	// only Slack can sign an interaction.
	// Missing identity is rejected like mismatched identity: making the
	// check conditional on p.Team.ID being present turns workspace
	// isolation into something a payload can opt out of by omission. Only
	// a token we never recorded a workspace for has nothing to compare.
	if entry.TeamID != "" && entry.TeamID != p.Team.ID {
		n.logger.WarnContext(ctx, "slack: detail token presented from a different workspace")
		n.ephemeral(ctx, p.ResponseURL, ":no_entry: These details belong to a different workspace.")
		return
	}

	cfg, err := n.userConfig(ctx, entry.UserID)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: cannot open detail modal, config unavailable", "err", err)
		n.ephemeral(ctx, p.ResponseURL, ":warning: Could not open the request details.")
		return
	}

	payload := map[string]any{
		"trigger_id": p.TriggerID,
		"view":       detailModal(entry.Blocks),
	}
	if err := n.call(ctx, cfg.BotToken, "views.open", payload, nil); err != nil {
		n.logger.WarnContext(ctx, "slack: views.open failed", "err", err)
		n.ephemeral(ctx, p.ResponseURL, ":warning: Could not open the request details.")
	}
}
