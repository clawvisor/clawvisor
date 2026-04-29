package proxy

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/elazarl/goproxy"

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

		if shouldInjectStoredBearer(hooks.Config) {
			_ = injectStoredBearer(req, hooks.Vault, st.Session.UserID)
		}

		for headerName, values := range req.Header {
			if len(values) == 0 {
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
				if detection := detectHeaderCredential(req, headerName, replaced); detection != nil {
					mode := autovaultMode(hooks.Config)
					switch {
					case detection.KnownService && mode == "auto":
						if _, err := captureRuntimeSecret(req.Context(), s, hooks.Store, hooks.Vault, st.Session, detection.Service, detection.Value); err == nil {
							emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
								EventType:  "runtime.autovault.captured",
								ActionKind: "egress",
								Decision:   stringPtr("capture"),
								Outcome:    stringPtr("captured"),
								Reason:     stringPtr("runtime autovault captured an outbound credential"),
								Metadata: map[string]any{
									"host":          requestHost(req),
									"header_name":   headerName,
									"scheme":        detection.Scheme,
									"service_guess": detection.Service,
									"detector":      detection.Detector,
									"mode":          mode,
								},
							})
						}
					default:
						emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
							EventType:  "runtime.autovault.observed",
							ActionKind: "egress",
							Decision:   stringPtr("observe"),
							Outcome:    stringPtr("detected"),
							Reason:     stringPtr("runtime autovault observed an outbound credential"),
							Metadata: map[string]any{
								"host":          requestHost(req),
								"header_name":   headerName,
								"scheme":        detection.Scheme,
								"service_guess": detection.Service,
								"detector":      detection.Detector,
								"mode":          mode,
							},
						})
					}
				}
			}
			req.Header[headerName] = replacedValues
		}
		return req, nil
	})
}

type headerCredentialDetection struct {
	Service      string
	Scheme       string
	Value        string
	Detector     string
	KnownService bool
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
				return &headerCredentialDetection{Service: service, Scheme: "bearer", Value: token, Detector: "known_service", KnownService: true}
			}
			if candidates := runtimeautovault.DetectCandidates(token); len(candidates) > 0 {
				return &headerCredentialDetection{Service: guessHostService(host), Scheme: "bearer", Value: token, Detector: "heuristic_bearer"}
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
				return &headerCredentialDetection{Service: service, Scheme: "basic", Value: password, Detector: "known_service", KnownService: true}
			}
		}
	}
	if header == "x-api-key" || header == "api-key" {
		token := strings.TrimSpace(value)
		if token == "" || runtimeautovault.LooksLikeShadow(token) {
			return nil
		}
		if service, ok := knownServiceForToken(token); ok {
			return &headerCredentialDetection{Service: service, Scheme: "api_key", Value: token, Detector: "known_service", KnownService: true}
		}
		if candidates := runtimeautovault.DetectCandidates(token); len(candidates) > 0 {
			return &headerCredentialDetection{Service: guessHostService(host), Scheme: "api_key", Value: token, Detector: "heuristic_bearer"}
		}
	}
	return nil
}

func autovaultMode(cfg *config.Config) string {
	if cfg == nil {
		return "auto"
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.RuntimePolicy.AutovaultMode))
	if mode == "" {
		return "auto"
	}
	return mode
}

func shouldInjectStoredBearer(cfg *config.Config) bool {
	return cfg != nil && cfg.RuntimePolicy.InjectStoredBearer
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

func injectStoredBearer(req *http.Request, v vault.Vault, userID string) error {
	if req == nil || v == nil || userID == "" {
		return nil
	}
	if req.Header.Get("Authorization") != "" {
		return nil
	}
	service := guessHostService(requestHost(req))
	if service == "" || service == "captured" {
		return nil
	}
	candidates, err := v.List(req.Context(), userID)
	if err != nil {
		return err
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
		return nil
	}
	return nil
}
