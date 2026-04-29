package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDeriveContainerURLRewritesLoopbackHost(t *testing.T) {
	got, err := deriveContainerURL("http://127.0.0.1:25297", "host.docker.internal")
	if err != nil {
		t.Fatalf("deriveContainerURL: %v", err)
	}
	if got != "http://host.docker.internal:25297" {
		t.Fatalf("unexpected container URL %q", got)
	}
}

func TestDeriveContainerURLLeavesRemoteHostUnchanged(t *testing.T) {
	got, err := deriveContainerURL("https://clawvisor.example.com", "host.docker.internal")
	if err != nil {
		t.Fatalf("deriveContainerURL: %v", err)
	}
	if got != "https://clawvisor.example.com" {
		t.Fatalf("unexpected container URL %q", got)
	}
}

func TestBuildDockerAgentEnvVars(t *testing.T) {
	opts := &dockerProxyOptions{
		BaseURL:      "http://127.0.0.1:25297",
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "cvis_test_token",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
		CAHost:       "/host/ca.pem",
	}

	values := map[string]string{}
	for _, v := range buildDockerAgentEnvVars(opts, false) {
		values[v.Key] = v.Value
	}

	if got := values["CLAWVISOR_URL"]; got != "http://host.docker.internal:25297" {
		t.Fatalf("unexpected CLAWVISOR_URL %q", got)
	}
	if got := values["HTTP_PROXY"]; got != "http://clawvisor:cvis_test_token@host.docker.internal:25290" {
		t.Fatalf("unexpected HTTP_PROXY %q", got)
	}
	if got := values["CLAWVISOR_RUNTIME_CA_CERT_FILE"]; got != "/clawvisor/ca.pem" {
		t.Fatalf("unexpected CA path %q", got)
	}
	if got := values["NO_PROXY"]; got != "localhost,127.0.0.1,::1,host.docker.internal" {
		t.Fatalf("unexpected NO_PROXY %q", got)
	}
}

func TestBuildDockerAgentEnvVarsTemplated(t *testing.T) {
	opts := &dockerProxyOptions{
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "ignored",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
	}
	values := map[string]string{}
	for _, v := range buildDockerAgentEnvVars(opts, true) {
		values[v.Key] = v.Value
	}
	if got := values["CLAWVISOR_AGENT_TOKEN"]; got != "${CLAWVISOR_AGENT_TOKEN}" {
		t.Fatalf("unexpected templated agent token %q", got)
	}
	if got := values["HTTP_PROXY"]; !strings.Contains(got, "${CLAWVISOR_AGENT_TOKEN}") {
		t.Fatalf("expected templated token in HTTP_PROXY, got %q", got)
	}
	if _, ok := values["CLAWVISOR_RUNTIME_SESSION_ID"]; ok {
		t.Fatalf("durable docker env should not pre-mint runtime session ids, got %+v", values)
	}
	if strings.Contains(values["HTTP_PROXY"], "runtime-secret") {
		t.Fatalf("durable docker env should not embed runtime session secrets, got %q", values["HTTP_PROXY"])
	}
}

func TestBuildDockerRunInjection(t *testing.T) {
	injected := buildDockerRunInjection([]dockerEnvVar{
		{Key: "HTTP_PROXY", Value: "http://proxy"},
		{Key: "SSL_CERT_FILE", Value: "/clawvisor/ca.pem"},
	}, "/host/ca.pem", "/clawvisor/ca.pem", "host.docker.internal")
	got := strings.Join(injected, " ")
	if !strings.Contains(got, "--add-host host.docker.internal:host-gateway") {
		t.Fatalf("expected host gateway alias, got %q", got)
	}
	if !strings.Contains(got, "-v /host/ca.pem:/clawvisor/ca.pem:ro") {
		t.Fatalf("expected CA mount, got %q", got)
	}
	if !strings.Contains(got, "-e HTTP_PROXY=http://proxy") {
		t.Fatalf("expected HTTP_PROXY env, got %q", got)
	}
}

func TestFindDockerRunImageIndex(t *testing.T) {
	idx, err := findDockerRunImageIndex([]string{"--rm", "-it", "--name", "agent", "-v", "/tmp:/tmp", "my-image", "run"})
	if err != nil {
		t.Fatalf("findDockerRunImageIndex: %v", err)
	}
	if idx != 6 {
		t.Fatalf("unexpected image index %d", idx)
	}
}

func TestEmitDockerComposeOverrideTemplated(t *testing.T) {
	opts := &dockerProxyOptions{
		ContainerURL: "http://host.docker.internal:25297",
		AgentToken:   "ignored",
		ProxyHost:    "host.docker.internal",
		ProxyPort:    25290,
		CAInside:     "/clawvisor/ca.pem",
		CAHost:       "/host/ca.pem",
	}
	var buf bytes.Buffer
	emitDockerComposeOverride(&buf, dockerComposeOverrideOptions{
		Service:      "agent",
		Opts:         opts,
		Templated:    true,
		EnvVars:      buildDockerAgentEnvVars(opts, true),
		ProxyHost:    opts.ProxyHost,
		ContainerURL: opts.ContainerURL,
	})
	out := buf.String()
	if !strings.Contains(out, `CLAWVISOR_AGENT_TOKEN: "${CLAWVISOR_AGENT_TOKEN}"`) {
		t.Fatalf("expected templated agent token in compose override, got:\n%s", out)
	}
	if !strings.Contains(out, `HTTP_PROXY: "http://clawvisor:${CLAWVISOR_AGENT_TOKEN}@host.docker.internal:25290"`) {
		t.Fatalf("expected templated HTTP_PROXY in compose override, got:\n%s", out)
	}
	if !strings.Contains(out, `- "/host/ca.pem:/clawvisor/ca.pem:ro"`) {
		t.Fatalf("expected CA mount in compose override, got:\n%s", out)
	}
}

func TestPrintDockerEnvAsArgsUsesShellQuoting(t *testing.T) {
	var buf bytes.Buffer
	printDockerEnvAsArgs(&buf, []dockerEnvVar{
		{Key: "TOKEN", Value: `value with spaces $HOME ! backtick` + "`" + ` and 'quote'`},
	})
	got := strings.TrimSpace(buf.String())
	want := "-e 'TOKEN=value with spaces $HOME ! backtick` and '\\''quote'\\'''"
	if got != want {
		t.Fatalf("unexpected docker args output:\n got: %s\nwant: %s", got, want)
	}
}
