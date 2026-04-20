package installer

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRender_ProducesParseableComposeOverride(t *testing.T) {
	in := Input{
		BridgeID:    "bridge-abc",
		ServerURL:   "http://host.docker.internal:25297",
		ProxyToken:  "cvisproxy_abc123",
		BridgeToken: "cvisbr_xyz789",
		AgentTokens: map[string]string{"main": "cvis_agent1", "helper": "cvis_agent2"},
	}
	art, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The compose override must be parseable as YAML. Regression test for
	// the plugin-secrets JSON breaking the block-scalar indentation (issue
	// observed post-commit a1fd6022 — multi-line JSON in a YAML block
	// scalar terminates the scalar early).
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(art.DockerComposeYAML), &parsed); err != nil {
		t.Fatalf("generated docker-compose.yml is not valid YAML:\n%v\n--- compose ---\n%s", err, art.DockerComposeYAML)
	}

	// Sanity check the services are there.
	services, ok := parsed["services"].(map[string]any)
	if !ok {
		t.Fatalf("services top-level key missing or wrong type: %T", parsed["services"])
	}
	for _, want := range []string{"clawvisor-proxy", "clawvisor-bootstrap", "openclaw-gateway"} {
		if _, ok := services[want]; !ok {
			t.Errorf("missing service %q in compose override", want)
		}
	}

	// Token + bridge ID should appear verbatim in the rendered compose.
	if !strings.Contains(art.DockerComposeYAML, in.ProxyToken) {
		t.Error("proxy token not embedded")
	}
	if !strings.Contains(art.DockerComposeYAML, in.BridgeID) {
		t.Error("bridge ID not embedded")
	}

	// Plugin secrets file should also be valid JSON (compact or indented).
	if art.PluginSecretsJSON == "" {
		t.Error("plugin secrets JSON is empty")
	}
	if !strings.Contains(art.PluginSecretsJSON, in.BridgeToken) {
		t.Error("plugin secrets doesn't include bridge token")
	}
}

func TestRender_MissingRequiredFields(t *testing.T) {
	if _, err := Render(Input{}); err == nil {
		t.Error("expected error on empty input")
	}
	if _, err := Render(Input{BridgeID: "b"}); err == nil {
		t.Error("expected error missing ProxyToken")
	}
	if _, err := Render(Input{BridgeID: "b", ProxyToken: "t"}); err == nil {
		t.Error("expected error missing ServerURL")
	}
}
