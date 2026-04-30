package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"

	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type PlaceholderHooks struct {
	Store  store.Store
	Vault  vault.Vault
	Config *config.Config
}

type RuntimeCredentialReviewPayload struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	CredentialRef string `json:"credential_ref"`
	Service       string `json:"service"`
	Host          string `json:"host"`
	HeaderName    string `json:"header_name"`
	Scheme        string `json:"scheme"`
	Detector      string `json:"detector,omitempty"`
}

func (s *Server) InstallPlaceholderSwap(hooks PlaceholderHooks) {
	if hooks.Store == nil || hooks.Vault == nil {
		return
	}
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Header.Get(internalBypassHeader) != "" {
			return req, nil
		}
		st := StateOf(ctx)
		if st == nil || st.Session == nil {
			return req, nil
		}

		injectedAuthorization := false
		if sessionShouldInjectStoredBearer(st.Session, hooks.Config) {
			injectedAuthorization, _ = injectStoredBearer(req, hooks.Vault, st.Session.UserID)
		}

		for headerName, values := range req.Header {
			if len(values) == 0 {
				continue
			}
			if strings.EqualFold(headerName, "Proxy-Authorization") || strings.EqualFold(headerName, "Proxy-Connection") {
				continue
			}
			replacedValues := make([]string, len(values))
			for i, value := range values {
				replaced, placeholders, err := runtimeautovault.ReplaceHeaderValue(value, func(placeholder string) (string, error) {
					meta, err := hooks.Store.GetRuntimePlaceholder(req.Context(), placeholder)
					if err != nil {
						return "", err
					}
					if meta.AgentID != st.Session.AgentID || meta.UserID != st.Session.UserID {
						return "", store.ErrNotFound
					}
					credBytes, err := hooks.Vault.Get(req.Context(), meta.UserID, meta.ServiceID)
					if err != nil {
						return "", err
					}
					return runtimeautovault.ExtractCredentialValue(credBytes)
				})
				if err != nil {
					return req, goproxy.NewResponse(req, "application/json", http.StatusForbidden, `{"error":"runtime placeholder rejected","code":"PLACEHOLDER_REJECTED"}`)
				}
				replacedValues[i] = replaced
				for _, placeholder := range placeholders {
					_ = hooks.Store.TouchRuntimePlaceholder(req.Context(), placeholder, time.Now().UTC())
				}
				if len(placeholders) > 0 {
					continue
				}
				if injectedAuthorization && strings.EqualFold(headerName, "Authorization") {
					continue
				}
				if detection := detectHeaderCredential(req, headerName, replaced); detection != nil {
					mode := sessionAutovaultMode(st.Session, hooks.Config)
					emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
						EventType:  "runtime.autovault.observed",
						ActionKind: "egress",
						Decision:   stringPtr("observe"),
						Outcome:    stringPtr("detected"),
						Reason:     stringPtr("runtime autovault observed an outbound credential-bearing header"),
						Metadata: map[string]any{
							"host":           requestHost(req),
							"header_name":    headerName,
							"scheme":         detection.Scheme,
							"service_guess":  detection.Service,
							"detector":       detection.Detector,
							"mode":           mode,
							"credential_ref": detection.CredentialRef,
						},
					})
					if mode == "strict" {
						if auth, err := hooks.Store.ConsumeMatchingCredentialAuthorization(req.Context(), store.CredentialAuthorizationMatch{
							UserID:        st.Session.UserID,
							AgentID:       st.Session.AgentID,
							SessionID:     st.Session.ID,
							CredentialRef: detection.CredentialRef,
							Service:       detection.Service,
							Host:          requestHost(req),
							HeaderName:    headerName,
							Scheme:        detection.Scheme,
						}, time.Now().UTC()); err == nil {
							emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
								EventType:  "runtime.autovault.authorized",
								ActionKind: "egress",
								ApprovalID: auth.ApprovalID,
								Decision:   stringPtr("allow"),
								Outcome:    stringPtr("authorized"),
								Reason:     stringPtr("runtime strict credential authorization matched"),
								Metadata: map[string]any{
									"scope":          auth.Scope,
									"host":           auth.Host,
									"header_name":    auth.HeaderName,
									"scheme":         auth.Scheme,
									"service_guess":  auth.Service,
									"credential_ref": auth.CredentialRef,
								},
							})
							continue
						} else if err != store.ErrNotFound {
							return req, goproxy.NewResponse(req, "application/json", http.StatusInternalServerError, `{"error":"could not evaluate runtime credential authorization"}`)
						}
						rec, err := ensureRuntimeCredentialReview(req.Context(), hooks.Store, st.Session, requestHost(req), headerName, detection)
						if err != nil {
							return req, goproxy.NewResponse(req, "application/json", http.StatusInternalServerError, `{"error":"could not create runtime credential approval"}`)
						}
						emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
							EventType:  "runtime.autovault.review_required",
							ActionKind: "egress",
							ApprovalID: &rec.ID,
							Decision:   stringPtr("review"),
							Outcome:    stringPtr("pending"),
							Reason:     stringPtr("runtime strict mode requires review for outbound credential use"),
							Metadata: map[string]any{
								"host":           requestHost(req),
								"header_name":    headerName,
								"scheme":         detection.Scheme,
								"service_guess":  detection.Service,
								"credential_ref": detection.CredentialRef,
							},
						})
						respBody, _ := json.Marshal(map[string]any{
							"error":          "runtime credential approval required",
							"code":           "RUNTIME_CREDENTIAL_REVIEW_REQUIRED",
							"approval_id":    rec.ID,
							"credential_ref": detection.CredentialRef,
						})
						return req, goproxy.NewResponse(req, "application/json", http.StatusForbidden, string(respBody))
					}
				}
			}
			req.Header[headerName] = replacedValues
		}
		return req, nil
	})
}

type headerCredentialDetection struct {
	Service       string
	Scheme        string
	Value         string
	CredentialRef string
	Detector      string
	KnownService  bool
}

func detectHeaderCredential(req *http.Request, headerName, value string) *headerCredentialDetection {
	header := strings.ToLower(strings.TrimSpace(headerName))
	host := requestHost(req)
	if header == "authorization" {
		scheme, rest, ok := strings.Cut(value, " ")
		if ok && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
			token := strings.TrimSpace(rest)
			if token == "" || runtimeautovault.LooksLikeShadow(token) {
				return nil
			}
			if service, ok := knownServiceForToken(token); ok {
				return &headerCredentialDetection{Service: service, Scheme: "bearer", Value: token, CredentialRef: credentialReference(token), Detector: "known_service", KnownService: true}
			}
			if candidates := runtimeautovault.DetectCandidates(token); len(candidates) > 0 {
				return &headerCredentialDetection{Service: guessHostService(host), Scheme: "bearer", Value: token, CredentialRef: credentialReference(token), Detector: "heuristic_bearer"}
			}
		}
		if ok && strings.EqualFold(strings.TrimSpace(scheme), "basic") {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
			if err != nil {
				return nil
			}
			_, password, ok := strings.Cut(string(decoded), ":")
			if !ok || password == "" || runtimeautovault.LooksLikeShadow(password) {
				return nil
			}
			if service, ok := knownServiceForToken(password); ok {
				return &headerCredentialDetection{Service: service, Scheme: "basic", Value: password, CredentialRef: credentialReference(password), Detector: "known_service", KnownService: true}
			}
		}
	}
	if header == "x-api-key" || header == "api-key" {
		token := strings.TrimSpace(value)
		if token == "" || runtimeautovault.LooksLikeShadow(token) {
			return nil
		}
		if service, ok := knownServiceForToken(token); ok {
			return &headerCredentialDetection{Service: service, Scheme: "api_key", Value: token, CredentialRef: credentialReference(token), Detector: "known_service", KnownService: true}
		}
		if candidates := runtimeautovault.DetectCandidates(token); len(candidates) > 0 {
			return &headerCredentialDetection{Service: guessHostService(host), Scheme: "api_key", Value: token, CredentialRef: credentialReference(token), Detector: "heuristic_bearer"}
		}
	}
	return nil
}

func autovaultMode(cfg *config.Config) string {
	if cfg == nil {
		return "observe"
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.RuntimePolicy.AutovaultMode))
	if mode == "" {
		return "observe"
	}
	return mode
}

func knownServiceForToken(token string) (string, bool) {
	for _, spec := range knownPrefixSpecs {
		if strings.HasPrefix(token, spec.Prefix) {
			return spec.Service, true
		}
	}
	return "", false
}

func guessHostService(host string) string {
	switch {
	case strings.Contains(host, "github"):
		return "github"
	case strings.Contains(host, "anthropic"):
		return "anthropic"
	case strings.Contains(host, "openai"), strings.Contains(host, "chatgpt"):
		return "openai"
	case strings.Contains(host, "slack"):
		return "slack"
	case strings.Contains(host, "google"):
		return "google"
	default:
		return "captured"
	}
}

func injectStoredBearer(req *http.Request, v vault.Vault, userID string) (bool, error) {
	if req == nil || v == nil || userID == "" {
		return false, nil
	}
	if req.Header.Get("Authorization") != "" {
		return false, nil
	}
	service := guessHostService(requestHost(req))
	if service == "" || service == "captured" {
		return false, nil
	}
	candidates, err := v.List(req.Context(), userID)
	if err != nil {
		return false, err
	}
	for _, serviceID := range candidates {
		if serviceID != service && !strings.HasPrefix(serviceID, service+":") {
			continue
		}
		credBytes, err := v.Get(req.Context(), userID, serviceID)
		if err != nil {
			continue
		}
		value, err := runtimeautovault.ExtractCredentialValue(credBytes)
		if err != nil || strings.TrimSpace(value) == "" {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+value)
		return true, nil
	}
	return false, nil
}

func credentialReference(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ensureRuntimeCredentialReview(ctx context.Context, st store.Store, session *store.RuntimeSession, host, headerName string, detection *headerCredentialDetection) (*store.ApprovalRecord, error) {
	requestID := runtimeCredentialReviewRequestID(session.ID, host, headerName, detection)
	rec, err := st.GetApprovalRecordByRequestID(ctx, requestID)
	if err == nil {
		return rec, nil
	}
	if err != store.ErrNotFound {
		return nil, err
	}
	summaryJSON, _ := json.Marshal(map[string]any{
		"title":          "Review outbound credential use",
		"host":           host,
		"header_name":    headerName,
		"scheme":         detection.Scheme,
		"service_guess":  detection.Service,
		"credential_ref": detection.CredentialRef,
	})
	payloadJSON, _ := json.Marshal(RuntimeCredentialReviewPayload{
		SessionID:     session.ID,
		AgentID:       session.AgentID,
		CredentialRef: detection.CredentialRef,
		Service:       detection.Service,
		Host:          host,
		HeaderName:    headerName,
		Scheme:        detection.Scheme,
		Detector:      detection.Detector,
	})
	rec = &store.ApprovalRecord{
		ID:                  uuid.NewString(),
		Kind:                "credential_review",
		UserID:              session.UserID,
		AgentID:             &session.AgentID,
		RequestID:           &requestID,
		SessionID:           &session.ID,
		Status:              "pending",
		Surface:             "dashboard",
		SummaryJSON:         summaryJSON,
		PayloadJSON:         payloadJSON,
		ResolutionTransport: "create_credential_authorization",
	}
	if err := st.CreateApprovalRecord(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func runtimeCredentialReviewRequestID(sessionID, host, headerName string, detection *headerCredentialDetection) string {
	raw := strings.ToLower(strings.TrimSpace(sessionID)) + "\n" +
		strings.ToLower(strings.TrimSpace(host)) + "\n" +
		strings.ToLower(strings.TrimSpace(headerName)) + "\n" +
		strings.ToLower(strings.TrimSpace(detection.Scheme)) + "\n" +
		detection.CredentialRef
	sum := sha256.Sum256([]byte(raw))
	return "runtime-credential:" + hex.EncodeToString(sum[:])
}
