// Package push implements notify.Notifier using a remote push notification service.
// It sends push notifications to all paired mobile devices for a user.
package push

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Notifier sends push notifications via the Clawvisor push service.
type Notifier struct {
	store      store.Store
	client     *http.Client
	pushURL    string
	daemonID   string
	privateKey ed25519.PrivateKey
	daemonURL  string
	decisionCh chan notify.CallbackDecision
	logger     *slog.Logger
}

// New creates a push Notifier.
func New(st store.Store, pushURL, daemonID string, privateKey ed25519.PrivateKey, daemonURL string, logger *slog.Logger) *Notifier {
	return &Notifier{
		store:      st,
		client:     &http.Client{Timeout: 10 * time.Second},
		pushURL:    pushURL,
		daemonID:   daemonID,
		privateKey: privateKey,
		daemonURL:  daemonURL,
		decisionCh: make(chan notify.CallbackDecision, 32),
		logger:     logger,
	}
}

// DecisionChannel returns a read-only channel that emits callback decisions
// from device action taps (approve/deny on push notifications).
func (n *Notifier) DecisionChannel() <-chan notify.CallbackDecision {
	return n.decisionCh
}

// EmitDecision sends a decision into the channel (called by the action handler).
func (n *Notifier) EmitDecision(d notify.CallbackDecision) {
	select {
	case n.decisionCh <- d:
	default:
		n.logger.Warn("push: decision channel full, dropping", "type", d.Type, "target_id", d.TargetID)
	}
}

// RegisterDevice registers a device token with the push service.
func (n *Notifier) RegisterDevice(ctx context.Context, deviceToken, bundleID string) error {
	payload := map[string]string{
		"daemon_id":    n.daemonID,
		"device_token": deviceToken,
		"bundle_id":    bundleID,
	}
	body, _ := json.Marshal(payload)
	return n.signedPost(ctx, "/api/tokens/register", body)
}

// RegisterPushToStartToken registers a push-to-start token with the push
// service so the daemon is authorized to send Live Activity pushes to it.
// Idempotent — re-registering the same token is a no-op.
func (n *Notifier) RegisterPushToStartToken(ctx context.Context, token string) error {
	body, _ := json.Marshal(map[string]string{"push_to_start_token": token})
	return n.signedPost(ctx, "/api/tokens/push-to-start", body)
}

// RegisterDaemon registers this daemon's Ed25519 public key with the push
// service. The push service requires this before it will accept signed
// requests. Idempotent — re-registering with the same key returns 201.
func (n *Notifier) RegisterDaemon(ctx context.Context) error {
	pub := n.privateKey.Public().(ed25519.PublicKey)
	payload := map[string]string{
		"daemon_id":  n.daemonID,
		"public_key": hex.EncodeToString(pub),
	}
	body, _ := json.Marshal(payload)

	// The push service's daemon registration endpoint uses a self-signed
	// proof: method + "\n" + path + "\n" + body + "\n" + timestamp.
	// This differs from the normal signRequest which hashes the body.
	path := "/api/daemons/register"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	message := "POST\n" + path + "\n" + string(body) + "\n" + ts
	sig := ed25519.Sign(n.privateKey, []byte(message))

	url := n.pushURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization",
		fmt.Sprintf("Ed25519-Sig %s:%s:%s", n.daemonID, ts, base64.StdEncoding.EncodeToString(sig)))

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("push: register daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push: register daemon: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// DeregisterDevice removes a device token from the push service.
func (n *Notifier) DeregisterDevice(ctx context.Context, deviceToken string) error {
	url := n.pushURL + "/api/tokens/" + deviceToken
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	n.signRequest(req, nil)
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("push: deregister device: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("push: deregister device: status %d", resp.StatusCode)
	}
	return nil
}

// ── notify.Notifier implementation ────────────────────────────────────────────

func (n *Notifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	summary := req.Service + "/" + req.Action
	body := fmt.Sprintf("%s wants to use %s.%s", req.AgentName, req.Service, req.Action)
	return n.sendToDevices(ctx, req.UserID, pushPayload{
		Category: "GATEWAY_APPROVAL",
		Title:    "Approval Request",
		Body:     body,
		Data: map[string]string{
			"target_id":      req.PendingID,
			"type":           "approval",
			"daemon_url":     n.daemonURL,
			"action_summary": summary,
		},
		LiveActivity: &liveActivityPayload{
			AttributesType: "ApprovalActivityAttributes",
			Attributes: map[string]string{
				"targetID":      req.PendingID,
				"daemonURL":     n.daemonURL,
				"requestType":   "approval",
				"category":      "GATEWAY_APPROVAL",
				"actionSummary": summary,
			},
			ContentState: map[string]any{
				"status":        "pending",
				"resultMessage": nil,
			},
			AlertTitle: "Approval Request",
			AlertBody:  body,
		},
	})
}

func (n *Notifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	_, err := n.sendToDevices(ctx, req.UserID, pushPayload{
		Title: "Service Activation Required",
		Body:  fmt.Sprintf("%s wants to use %s", req.AgentName, req.Service),
	})
	return err
}

func (n *Notifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	summary := actionSummary(req.Actions)
	data := map[string]string{
		"target_id":  req.TaskID,
		"type":       "task",
		"daemon_url": n.daemonURL,
		"purpose":    req.Purpose,
	}
	if req.RiskLevel != "" {
		data["risk_level"] = req.RiskLevel
	}
	if summary != "" {
		data["action_summary"] = summary
	}
	body := fmt.Sprintf("%s: %s", req.AgentName, req.Purpose)
	return n.sendToDevices(ctx, req.UserID, pushPayload{
		Category: "TASK_APPROVAL",
		Title:    "Task Approval Request",
		Body:     body,
		Data:     data,
		LiveActivity: &liveActivityPayload{
			AttributesType: "ApprovalActivityAttributes",
			Attributes: map[string]string{
				"targetID":      req.TaskID,
				"daemonURL":     n.daemonURL,
				"requestType":   "task",
				"category":      "TASK_APPROVAL",
				"purpose":       req.Purpose,
				"riskLevel":     req.RiskLevel,
				"actionSummary": summary,
			},
			ContentState: map[string]any{
				"status":        "pending",
				"resultMessage": nil,
			},
			AlertTitle: "Task Approval Needed",
			AlertBody:  body,
		},
	})
}

func (n *Notifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	summary := req.NewAction.Service + "/" + req.NewAction.Action
	body := fmt.Sprintf("%s wants to expand task scope: %s", req.AgentName, req.Reason)
	return n.sendToDevices(ctx, req.UserID, pushPayload{
		Category: "SCOPE_EXPANSION",
		Title:    "Scope Expansion Request",
		Body:     body,
		Data: map[string]string{
			"target_id":      req.TaskID,
			"type":           "scope_expansion",
			"daemon_url":     n.daemonURL,
			"purpose":        req.Purpose,
			"action_summary": summary,
		},
		LiveActivity: &liveActivityPayload{
			AttributesType: "ApprovalActivityAttributes",
			Attributes: map[string]string{
				"targetID":      req.TaskID,
				"daemonURL":     n.daemonURL,
				"requestType":   "scope_expansion",
				"category":      "SCOPE_EXPANSION",
				"purpose":       req.Purpose,
				"actionSummary": summary,
			},
			ContentState: map[string]any{
				"status":        "pending",
				"resultMessage": nil,
			},
			AlertTitle: "Scope Expansion Request",
			AlertBody:  body,
		},
	})
}

func (n *Notifier) SendConnectionRequest(ctx context.Context, req notify.ConnectionRequest) (string, error) {
	body := fmt.Sprintf("Agent %q is requesting to connect from %s", req.AgentName, req.IPAddress)
	return n.sendToDevices(ctx, req.UserID, pushPayload{
		Category: "AGENT_CONNECTION",
		Title:    "Agent Connection Request",
		Body:     body,
		Data: map[string]string{
			"target_id":      req.ConnectionID,
			"type":           "connection",
			"daemon_url":     n.daemonURL,
			"purpose":        fmt.Sprintf("%s requests to connect", req.AgentName),
			"action_summary": "agent/connect",
		},
		LiveActivity: &liveActivityPayload{
			AttributesType: "ApprovalActivityAttributes",
			Attributes: map[string]string{
				"targetID":      req.ConnectionID,
				"daemonURL":     n.daemonURL,
				"requestType":   "connection",
				"category":      "AGENT_CONNECTION",
				"purpose":       "Agent requests to connect",
				"actionSummary": "agent/connect",
			},
			ContentState: map[string]any{
				"status":        "pending",
				"resultMessage": nil,
			},
			AlertTitle: "Agent Connection Request",
			AlertBody:  body,
		},
	})
}

func (n *Notifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	// Push notifications are ephemeral — no update mechanism.
	return nil
}

func (n *Notifier) SendTestMessage(ctx context.Context, userID string) error {
	_, err := n.sendToDevices(ctx, userID, pushPayload{
		Title: "Clawvisor Test",
		Body:  "Your push notifications are working!",
	})
	return err
}

func (n *Notifier) SendAlert(ctx context.Context, userID, text string) error {
	_, err := n.sendToDevices(ctx, userID, pushPayload{
		Title: "Clawvisor Alert",
		Body:  text,
	})
	return err
}

// ── Internal ──────────────────────────────────────────────────────────────────

type pushPayload struct {
	Category     string            `json:"category,omitempty"`
	Title        string            `json:"title"`
	Body         string            `json:"body"`
	Data         map[string]string `json:"data,omitempty"`
	LiveActivity *liveActivityPayload
}

type liveActivityPayload struct {
	AttributesType string            `json:"attributes_type"`
	Attributes     map[string]string `json:"attributes"`
	ContentState   map[string]any    `json:"content_state"`
	AlertTitle     string            `json:"alert_title"`
	AlertBody      string            `json:"alert_body"`
}

type pushRequest struct {
	DeviceTokens []string          `json:"device_tokens"`
	Title        string            `json:"title"`
	Body         string            `json:"body"`
	Category     string            `json:"category,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
}

type liveActivityRequest struct {
	PushToStartTokens []string          `json:"push_to_start_tokens"`
	Event             string            `json:"event"`
	AttributesType    string            `json:"attributes_type"`
	Attributes        map[string]string `json:"attributes"`
	ContentState      map[string]any    `json:"content_state"`
	AlertTitle        string            `json:"alert_title"`
	AlertBody         string            `json:"alert_body"`
}

func (n *Notifier) sendToDevices(ctx context.Context, userID string, p pushPayload) (string, error) {
	devices, err := n.store.ListPairedDevices(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("push: list devices: %w", err)
	}
	if len(devices) == 0 {
		return "", nil
	}

	tokens := make([]string, len(devices))
	for i, d := range devices {
		tokens[i] = d.DeviceToken
	}

	// Prefer Live Activity over regular push when push-to-start tokens are available.
	if p.LiveActivity != nil {
		var ptsTokens []string
		for _, d := range devices {
			if d.PushToStartToken != "" {
				ptsTokens = append(ptsTokens, d.PushToStartToken)
			}
		}
		if len(ptsTokens) > 0 {
			laReq := liveActivityRequest{
				PushToStartTokens: ptsTokens,
				Event:             "start",
				AttributesType:    p.LiveActivity.AttributesType,
				Attributes:        p.LiveActivity.Attributes,
				ContentState:      p.LiveActivity.ContentState,
				AlertTitle:        p.LiveActivity.AlertTitle,
				AlertBody:         p.LiveActivity.AlertBody,
			}
			laBody, _ := json.Marshal(laReq)
			n.logger.Info("push: sending live activity", "token_count", len(ptsTokens))
			if err := n.signedPost(ctx, "/api/push/live-activity", laBody); err != nil {
				n.logger.Warn("push: live activity failed", "err", err)
			}
			return "push:" + n.daemonID, nil
		}
	}

	// Fall back to regular push notification.
	reqBody := pushRequest{
		DeviceTokens: tokens,
		Title:        p.Title,
		Body:         p.Body,
		Category:     p.Category,
		Data:         p.Data,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	n.logger.Info("push: sending notification", "category", p.Category, "title", p.Title, "data", p.Data, "device_count", len(tokens))

	if err := n.signedPost(ctx, "/api/push", body); err != nil {
		return "", err
	}

	return "push:" + n.daemonID, nil
}

func (n *Notifier) signedPost(ctx context.Context, path string, body []byte) error {
	url := n.pushURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	n.signRequest(req, body)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("push: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push: %s: status %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

// signRequest adds the Ed25519-Sig authorization header.
// Format: Ed25519-Sig <daemon_id>:<timestamp>:<base64(signature)>
// Signed message: "<method>\n<path>\n<body>\n<timestamp>"
func (n *Notifier) signRequest(req *http.Request, body []byte) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	message := req.Method + "\n" + req.URL.Path + "\n" + string(body) + "\n" + ts
	sig := ed25519.Sign(n.privateKey, []byte(message))
	req.Header.Set("Authorization",
		fmt.Sprintf("Ed25519-Sig %s:%s:%s", n.daemonID, ts, base64.StdEncoding.EncodeToString(sig)))
}

// actionSummary builds a comma-separated "service/action" string from task actions.
func actionSummary(actions []store.TaskAction) string {
	if len(actions) == 0 {
		return ""
	}
	var parts []string
	for _, a := range actions {
		parts = append(parts, a.Service+"/"+a.Action)
	}
	return strings.Join(parts, ", ")
}

