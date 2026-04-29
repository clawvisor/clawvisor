package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/tui/client"
)

func TestBuildRuntimeBootstrapEnv(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("NO_PROXY", "metadata.internal,localhost")

	session := &client.CreateRuntimeSessionResponse{
		Session: client.RuntimeSession{
			ID:        "session-123",
			ExpiresAt: time.Unix(1_700_000_000, 0).UTC(),
		},
		ProxyBearer:     "secret-token",
		ProxyURL:        "http://127.0.0.1:4318",
		CACertPEM:       "-----BEGIN CERTIFICATE-----\nunit-test\n-----END CERTIFICATE-----\n",
		ObservationMode: true,
	}

	env, err := buildRuntimeBootstrapEnv("http://127.0.0.1:25297", "agent-token", session)
	if err != nil {
		t.Fatalf("buildRuntimeBootstrapEnv: %v", err)
	}

	values := envMap(env)
	if got := values["HTTP_PROXY"]; got != "http://clawvisor:secret-token@127.0.0.1:4318" {
		t.Fatalf("unexpected HTTP_PROXY %q", got)
	}
	if values["HTTPS_PROXY"] != values["HTTP_PROXY"] || values["ALL_PROXY"] != values["HTTP_PROXY"] {
		t.Fatalf("expected proxy vars to match, got HTTPS_PROXY=%q ALL_PROXY=%q", values["HTTPS_PROXY"], values["ALL_PROXY"])
	}
	if got := values["CLAWVISOR_RUNTIME_PROXY_URL"]; got != "http://127.0.0.1:4318" {
		t.Fatalf("unexpected runtime proxy URL %q", got)
	}
	if got := values["CLAWVISOR_RUNTIME_OBSERVATION_MODE"]; got != "true" {
		t.Fatalf("unexpected observation mode %q", got)
	}
	if got := values["CLAWVISOR_RUNTIME_SESSION_ID"]; got != "session-123" {
		t.Fatalf("unexpected session id %q", got)
	}
	if got := values["CLAWVISOR_AGENT_TOKEN"]; got != "agent-token" {
		t.Fatalf("unexpected agent token %q", got)
	}
	caPath := values["CLAWVISOR_RUNTIME_CA_CERT_FILE"]
	if caPath == "" {
		t.Fatal("expected runtime CA cert path")
	}
	if got := values["SSL_CERT_FILE"]; got != caPath {
		t.Fatalf("expected SSL_CERT_FILE to match runtime CA path, got %q", got)
	}
	if got := values["CURL_CA_BUNDLE"]; got != caPath {
		t.Fatalf("expected CURL_CA_BUNDLE to match runtime CA path, got %q", got)
	}
	if got := values["REQUESTS_CA_BUNDLE"]; got != caPath {
		t.Fatalf("expected REQUESTS_CA_BUNDLE to match runtime CA path, got %q", got)
	}
	if got := values["NODE_EXTRA_CA_CERTS"]; got != caPath {
		t.Fatalf("expected NODE_EXTRA_CA_CERTS to match runtime CA path, got %q", got)
	}
	if got := values["GIT_SSL_CAINFO"]; got != caPath {
		t.Fatalf("expected GIT_SSL_CAINFO to match runtime CA path, got %q", got)
	}
	data, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read runtime CA cert: %v", err)
	}
	if string(data) != session.CACertPEM {
		t.Fatalf("unexpected runtime CA cert contents %q", string(data))
	}
	if filepath.Base(caPath) != "session-123.pem" {
		t.Fatalf("unexpected runtime CA cert filename %q", filepath.Base(caPath))
	}
	if got := values["NO_PROXY"]; got != "metadata.internal,localhost,127.0.0.1,::1" {
		t.Fatalf("unexpected NO_PROXY %q", got)
	}
	if values["no_proxy"] != values["NO_PROXY"] {
		t.Fatalf("expected lowercase no_proxy to match, got %q", values["no_proxy"])
	}
}

func TestMergeNoProxyPreservesOrderAndDeduplicates(t *testing.T) {
	got := mergeNoProxy("localhost, example.com ,localhost", "127.0.0.1", "example.com", "::1")
	if got != "localhost,example.com,127.0.0.1,::1" {
		t.Fatalf("unexpected merged no_proxy %q", got)
	}
}

func TestBuildRuntimeBootstrapEnvSkipsCAEnvWhenUnavailable(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	session := &client.CreateRuntimeSessionResponse{
		Session: client.RuntimeSession{
			ID: "session-123",
		},
		ProxyBearer: "secret-token",
		ProxyURL:    "http://127.0.0.1:4318",
	}

	env, err := buildRuntimeBootstrapEnv("http://127.0.0.1:25297", "agent-token", session)
	if err != nil {
		t.Fatalf("buildRuntimeBootstrapEnv: %v", err)
	}
	values := envMap(env)
	if values["CLAWVISOR_RUNTIME_CA_CERT_FILE"] != "" {
		t.Fatalf("expected runtime CA env to be omitted, got %q", values["CLAWVISOR_RUNTIME_CA_CERT_FILE"])
	}
	if values["SSL_CERT_FILE"] != "" {
		t.Fatalf("expected SSL_CERT_FILE to be omitted, got %q", values["SSL_CERT_FILE"])
	}
}

func TestMergeEnvironmentOverridesExistingKeys(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HTTP_PROXY=http://old-proxy",
		"KEEP=1",
	}
	overrides := []string{
		"HTTP_PROXY=http://new-proxy",
		"NO_PROXY=localhost",
	}

	merged := mergeEnvironment(base, overrides)
	values := envMap(merged)

	if got := values["HTTP_PROXY"]; got != "http://new-proxy" {
		t.Fatalf("expected override to win, got %q", got)
	}
	if got := values["KEEP"]; got != "1" {
		t.Fatalf("expected base env to remain, got %q", got)
	}
	if got := values["NO_PROXY"]; got != "localhost" {
		t.Fatalf("expected new env entry, got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(""); got != "''" {
		t.Fatalf("unexpected empty quote %q", got)
	}
	if got := shellQuote("hello 'world'"); got != "'hello '\\''world'\\'''" {
		t.Fatalf("unexpected shell quote %q", got)
	}
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}
