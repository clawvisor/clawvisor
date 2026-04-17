package yamlruntime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// credential is the JSON shape stored in the vault for API-key and basic-auth services.
// When credentials come from a PKCE flow, the token is in the access_token field instead.
type credential struct {
	Type        string `json:"type"`
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// extractToken parses the vault credential JSON and returns the token string.
// Supports both direct API-key credentials ({"token": "..."}) and PKCE-sourced
// credentials ({"access_token": "..."}).
func extractToken(credBytes []byte) (string, error) {
	var cred credential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("parsing credential: %w", err)
	}
	token := cred.Token
	if token == "" {
		token = cred.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("credential missing token")
	}
	return token, nil
}

// buildHTTPClient creates an *http.Client that injects authentication headers
// based on the YAML auth definition.
func buildHTTPClient(authDef yamldef.AuthDef, credBytes []byte) (*http.Client, error) {
	switch authDef.Type {
	case "api_key":
		token, err := extractToken(credBytes)
		if err != nil {
			return nil, err
		}
		return &http.Client{
			Transport: &headerTransport{
				header:             authDef.Header,
				headerPrefix:       authDef.HeaderPrefix,
				mirrorHeader:       authDef.MirrorHeader,
				mirrorHeaderPrefix: authDef.MirrorHeaderPrefix,
				token:              token,
				extraHeaders:       authDef.ExtraHeaders,
				base:               http.DefaultTransport,
			},
		}, nil

	case "basic":
		token, err := extractToken(credBytes)
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(token, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("basic auth credential must be in format 'user:pass'")
		}
		return &http.Client{
			Transport: &basicAuthTransport{
				user:         parts[0],
				pass:         parts[1],
				extraHeaders: authDef.ExtraHeaders,
				base:         http.DefaultTransport,
			},
		}, nil

	case "oauth2":
		// OAuth credentials are stored as JSON with access_token, refresh_token, etc.
		// Extract the access token and use it as a Bearer token.
		var oauthCred struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(credBytes, &oauthCred); err != nil {
			return nil, fmt.Errorf("parsing oauth2 credential: %w", err)
		}
		if oauthCred.AccessToken == "" {
			return nil, fmt.Errorf("oauth2 credential missing access_token")
		}
		return &http.Client{
			Transport: &headerTransport{
				header:       "Authorization",
				headerPrefix: "Bearer ",
				token:        oauthCred.AccessToken,
				extraHeaders: authDef.ExtraHeaders,
				base:         http.DefaultTransport,
			},
		}, nil

	case "none":
		return &http.Client{
			Transport: &headerTransport{
				extraHeaders: authDef.ExtraHeaders,
				base:         http.DefaultTransport,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported auth type %q", authDef.Type)
	}
}

// headerTransport injects an authorization header and extra headers into every request.
type headerTransport struct {
	header             string
	headerPrefix       string
	mirrorHeader       string
	mirrorHeaderPrefix string
	token              string
	extraHeaders       map[string]string
	base               http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.header != "" && t.token != "" {
		clone.Header.Set(t.header, t.headerPrefix+t.token)
	}
	if t.mirrorHeader != "" && t.token != "" {
		clone.Header.Set(t.mirrorHeader, t.mirrorHeaderPrefix+t.token)
	}
	for k, v := range t.extraHeaders {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}

// basicAuthTransport injects HTTP Basic Auth into every request.
type basicAuthTransport struct {
	user         string
	pass         string
	extraHeaders map[string]string
	base         http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.SetBasicAuth(t.user, t.pass)
	for k, v := range t.extraHeaders {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}

// credentialFields returns the parsed credential fields as a map for template interpolation.
// For basic auth, it includes "user" and "pass" fields; for api_key, just "token".
func credentialFields(authDef yamldef.AuthDef, credBytes []byte) (map[string]string, error) {
	token, err := extractToken(credBytes)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{"token": token}
	if authDef.Type == "basic" {
		parts := strings.SplitN(token, ":", 2)
		if len(parts) == 2 {
			fields["sid"] = parts[0] // Twilio convention
			fields["user"] = parts[0]
			fields["pass"] = parts[1]
		}
	}
	return fields, nil
}
