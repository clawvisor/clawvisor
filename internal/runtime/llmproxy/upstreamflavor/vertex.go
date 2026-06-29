package upstreamflavor

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// VertexConfig parameterizes a VertexFlavor instance.
//
// ProjectID and Region are required. They are baked into the upstream
// URL: https://{Region}-aiplatform.googleapis.com/v1/projects/{ProjectID}
// /locations/{Region}/publishers/anthropic/models/{model}:streamRawPredict
// (or :rawPredict when stream=false in the body — but Claude Code always
// streams, so the proxy targets :streamRawPredict).
//
// ModelOverride forces a specific Vertex model ID regardless of what
// the client sent in the body's `model` field. Most clients use raw
// Anthropic names like "claude-opus-4-7"; Vertex's catalog uses
// versioned IDs like "claude-opus-4-7@20260101". Leave empty to
// passthrough the client's value.
type VertexConfig struct {
	ProjectID     string
	Region        string
	ModelOverride string
}

// NewVertex returns a Flavor that adapts Anthropic Messages API requests
// to Vertex AI's :streamRawPredict endpoint.
func NewVertex(cfg VertexConfig) (Flavor, error) {
	if strings.TrimSpace(cfg.ProjectID) == "" {
		return nil, errors.New("upstreamflavor: vertex: ProjectID required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("upstreamflavor: vertex: Region required")
	}
	return &vertexFlavor{cfg: cfg}, nil
}

type vertexFlavor struct {
	cfg VertexConfig
}

func (v *vertexFlavor) Name() string { return "vertex" }

func (v *vertexFlavor) BuildURL(inboundPath, model string) (*url.URL, error) {
	if v.cfg.ModelOverride != "" {
		model = v.cfg.ModelOverride
	}
	if model == "" {
		return nil, errors.New("upstreamflavor: vertex: request body has no `model` field and no ModelOverride configured")
	}

	// Vertex only exposes :rawPredict and :streamRawPredict for
	// Anthropic publisher models. Count_tokens is not supported on
	// Vertex's Anthropic surface — surface a clear error.
	suffix, err := vertexSuffix(inboundPath)
	if err != nil {
		return nil, err
	}

	host := fmt.Sprintf("%s-aiplatform.googleapis.com", v.cfg.Region)
	path := fmt.Sprintf(
		"/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
		v.cfg.ProjectID, v.cfg.Region, model, suffix,
	)
	return &url.URL{Scheme: "https", Host: host, Path: path}, nil
}

func vertexSuffix(inboundPath string) (string, error) {
	switch inboundPath {
	case "/v1/messages":
		return "streamRawPredict", nil
	case "/v1/messages/count_tokens":
		return "", fmt.Errorf("upstreamflavor: vertex: %s is not supported by Vertex AI", inboundPath)
	default:
		return "", fmt.Errorf("upstreamflavor: vertex: unsupported inbound path %q", inboundPath)
	}
}

// TransformBody strips the top-level `model` field (Vertex rejects it —
// the model is encoded in the URL) and injects `anthropic_version`
// (Vertex requires it in the body, not the header).
//
// Byte fidelity for the remaining fields is preserved via jsonsurgery
// (same helpers the inbound sanitize layer uses) so thinking-block
// signature verification on continuation turns continues to work.
func (v *vertexFlavor) TransformBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	out := body
	if stripped, ok := stripModel(out); ok {
		out = stripped
	}
	versioned, err := setAnthropicVersion(out)
	if err != nil {
		return nil, fmt.Errorf("upstreamflavor: vertex: inject anthropic_version: %w", err)
	}
	return versioned, nil
}

// InjectAuth sets Authorization: Bearer <GCP access token>.
//
// credBytes is the raw access token bytes (or the JSON service-account
// key — see InjectAuth contract). For the initial draft, the proxy
// expects credBytes to already be a usable bearer token; minting from a
// service-account JSON via google.golang.org/api/oauth2 is deferred to
// a follow-up (it adds a non-trivial dependency and a token cache).
//
// When credBytes is empty (passthrough mode), the caller's
// Authorization header is preserved as-is and only flavor-specific
// headers are cleaned up.
func (v *vertexFlavor) InjectAuth(req *http.Request, credBytes []byte) error {
	// Vertex puts the version in the body — strip any header form so
	// the upstream doesn't see two conflicting copies.
	req.Header.Del("anthropic-version")
	// x-api-key is meaningless to Vertex; remove it if present.
	req.Header.Del("x-api-key")

	if len(credBytes) == 0 {
		// Passthrough — Authorization header is already attached.
		return nil
	}
	token := strings.TrimSpace(string(credBytes))
	if token == "" {
		return errors.New("upstreamflavor: vertex: empty access token")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("upstreamflavor: vertex: access token contains illegal control bytes")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// VaultServiceID stores Vertex access tokens (or SA keys, once auto-mint
// lands) under the conventional "vertex" namespace. Returning "vertex"
// here lets the forwarder's agent-scoped + user-scoped lookup chain
// reuse its existing logic without flavor-specific branches.
func (v *vertexFlavor) VaultServiceID() string { return "vertex" }
