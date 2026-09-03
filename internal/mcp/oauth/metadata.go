package oauth

import (
	"encoding/json"
	"net/http"
)

// ProtectedResourceMetadata serves GET /.well-known/oauth-protected-resource.
// This tells MCP clients which authorization server to use.
func (p *Provider) ProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"resource":              p.baseURL + "/mcp",
		"authorization_servers": []string{p.baseURL},
	})
}

// AuthorizationServerMetadata serves GET /.well-known/oauth-authorization-server.
// RFC 8414 metadata document.
func (p *Provider) AuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                p.baseURL,
		"authorization_endpoint":                p.baseURL + "/oauth/authorize",
		"token_endpoint":                        p.baseURL + "/oauth/token",
		"registration_endpoint":                 p.baseURL + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":       []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}
