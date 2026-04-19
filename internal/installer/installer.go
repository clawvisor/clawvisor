// Package installer generates the per-bridge install artifact that sets
// up a Clawvisor Proxy + OpenClaw deployment. The artifact is consumed
// by the user running either `clawvisor pair openclaw` at the CLI or a
// "Download" button in the dashboard; it is NOT applied by the plugin
// (which runs unprivileged inside the agent container and cannot mutate
// the trust store, env, or Docker runtime).
//
// See docs/design-proxy-stage1.md §3.2 for the privilege-boundary
// rationale.
package installer

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"text/template"
	"time"
)

//go:embed templates/docker-compose.yml.tmpl
var dockerComposeTmpl string

//go:embed templates/install.sh.tmpl
var installScriptTmpl string

// Input bundles everything the templates need to render a concrete
// artifact for a specific bridge.
type Input struct {
	BridgeID      string
	ServerURL     string // how the proxy reaches Clawvisor server (usually http://clawvisor-server:25297 in compose)
	ProxyToken    string // cvisproxy_...
	BridgeToken   string // cvisbr_... (plugin secrets)
	AgentTokens   map[string]string // agent name → cvis_... (plugin secrets)
}

// Artifact is the rendered bundle. Fields are the file contents the
// install script / compose file / docs reference as-is; callers write
// them to disk or serve them via an endpoint.
type Artifact struct {
	BridgeID            string
	GeneratedAt         string
	DockerComposeYAML   string
	InstallScript       string
	PluginSecretsJSON   string
	ProxyConfigYAML     string
}

// Render produces a concrete Artifact from the given Input. Panics on
// template bugs (compile-time issue, not runtime); returns error for
// any user-supplied-input failures.
func Render(in Input) (*Artifact, error) {
	if in.BridgeID == "" {
		return nil, fmt.Errorf("installer: BridgeID is required")
	}
	if in.ProxyToken == "" {
		return nil, fmt.Errorf("installer: ProxyToken is required")
	}
	if in.ServerURL == "" {
		return nil, fmt.Errorf("installer: ServerURL is required")
	}

	agentTokens := in.AgentTokens
	if agentTokens == nil {
		agentTokens = map[string]string{}
	}
	pluginSecrets := map[string]any{
		"bridgeToken":  in.BridgeToken,
		"agentTokens":  agentTokens,
		"proxyEnabled": true,
	}
	pluginJSON, err := json.MarshalIndent(pluginSecrets, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("installer: marshal plugin secrets: %w", err)
	}

	proxyConfigYAML := fmt.Sprintf(
		"server_url: %q\nproxy_token: %q\nbridge_id: %q\n",
		in.ServerURL, in.ProxyToken, in.BridgeID,
	)

	tmplVars := struct {
		BridgeID          string
		ServerURL         string
		ProxyToken        string
		GeneratedAt       string
		PluginSecretsJSON string
	}{
		BridgeID:          in.BridgeID,
		ServerURL:         in.ServerURL,
		ProxyToken:        in.ProxyToken,
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		PluginSecretsJSON: string(pluginJSON),
	}

	var composeBuf, installBuf bytes.Buffer
	if err := mustTemplate("docker-compose", dockerComposeTmpl).Execute(&composeBuf, tmplVars); err != nil {
		return nil, fmt.Errorf("installer: render docker-compose: %w", err)
	}
	if err := mustTemplate("install-sh", installScriptTmpl).Execute(&installBuf, tmplVars); err != nil {
		return nil, fmt.Errorf("installer: render install.sh: %w", err)
	}

	return &Artifact{
		BridgeID:          in.BridgeID,
		GeneratedAt:       tmplVars.GeneratedAt,
		DockerComposeYAML: composeBuf.String(),
		InstallScript:     installBuf.String(),
		PluginSecretsJSON: string(pluginJSON),
		ProxyConfigYAML:   proxyConfigYAML,
	}, nil
}

func mustTemplate(name, body string) *template.Template {
	return template.Must(template.New(name).Parse(body))
}
