package upstreamflavor

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// AzureConfig parameterizes an AzureFlavor instance.
//
// ResourceName is the Azure AI Foundry account name (the leading subdomain
// of {ResourceName}.services.ai.azure.com).
//
// APIVersion is the Azure API version query parameter required on every
// request. It is operator-managed because Azure publishes new versions
// quarterly and the proxy should not silently pin a value that the
// resource may not support.
//
// PathPrefix overrides the default "/anthropic" path prefix used by
// Foundry's Anthropic surface. Leave empty for the standard layout.
type AzureConfig struct {
	ResourceName string
	APIVersion   string
	PathPrefix   string
}

// NewAzure returns a Flavor that targets Azure AI Foundry's hosted
// Anthropic API. The Foundry surface mirrors Anthropic's request body
// and SSE format; the differences are URL, auth header, and the
// mandatory api-version query parameter.
func NewAzure(cfg AzureConfig) (Flavor, error) {
	if strings.TrimSpace(cfg.ResourceName) == "" {
		return nil, errors.New("upstreamflavor: azure: ResourceName required")
	}
	if strings.TrimSpace(cfg.APIVersion) == "" {
		return nil, errors.New("upstreamflavor: azure: APIVersion required")
	}
	prefix := cfg.PathPrefix
	if prefix == "" {
		prefix = "/anthropic"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")
	return &azureFlavor{cfg: cfg, prefix: prefix}, nil
}

type azureFlavor struct {
	cfg    AzureConfig
	prefix string
}

func (a *azureFlavor) Name() string { return "azure" }

func (a *azureFlavor) BuildURL(inboundPath, _ string) (*url.URL, error) {
	switch inboundPath {
	case "/v1/messages", "/v1/messages/count_tokens":
		// Both routes exist on Foundry's Anthropic surface.
	default:
		return nil, fmt.Errorf("upstreamflavor: azure: unsupported inbound path %q", inboundPath)
	}
	host := fmt.Sprintf("%s.services.ai.azure.com", a.cfg.ResourceName)
	q := url.Values{}
	q.Set("api-version", a.cfg.APIVersion)
	return &url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     a.prefix + inboundPath,
		RawQuery: q.Encode(),
	}, nil
}

// TransformBody is a pass-through: Azure Foundry accepts the native
// Anthropic Messages API body without modification.
func (a *azureFlavor) TransformBody(body []byte) ([]byte, error) {
	return body, nil
}

// InjectAuth sets the api-key header (Azure's convention — distinct
// from Anthropic's x-api-key) using the raw credential bytes from
// vault. In passthrough mode (empty credBytes), the caller's
// Authorization header is forwarded unchanged — Azure accepts an
// Azure AD bearer token as an alternative to api-key.
func (a *azureFlavor) InjectAuth(req *http.Request, credBytes []byte) error {
	// Azure does not honor the anthropic-version header — the
	// equivalent is the api-version query parameter, set in BuildURL.
	req.Header.Del("anthropic-version")
	req.Header.Del("x-api-key")

	if len(credBytes) == 0 {
		return nil
	}
	key := strings.TrimSpace(string(credBytes))
	if key == "" {
		return errors.New("upstreamflavor: azure: empty api key")
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return errors.New("upstreamflavor: azure: api key contains illegal control bytes")
	}
	req.Header.Set("api-key", key)
	req.Header.Del("Authorization")
	return nil
}

// VaultServiceID returns "azure" for vault-stored API keys.
func (a *azureFlavor) VaultServiceID() string { return "azure" }
