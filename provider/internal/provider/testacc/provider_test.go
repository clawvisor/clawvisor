// Package testacc holds the provider's acceptance tests. They boot a real
// clawvisor-server subprocess (hermetic — no cloud creds), mint an
// instance-admin API token via spec 05's bootstrap flow, and drive the
// provider through the terraform-plugin-testing helper/resource harness.
//
// The four core resources (agent, service_config, vault_entry, api_token) run
// full green CRUD+import here. The four governance resources' tests are
// written but t.Skip cleanly until spec 06a lands the /api/governance/*
// routes and the `local_governance` capability (see hasLocalGovernance).
package testacc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/provider/internal/client"
	tfprovider "github.com/clawvisor/clawvisor/provider/internal/provider"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testEndpoint / testToken are the CLAWVISOR_ENDPOINT / CLAWVISOR_API_TOKEN
// the provider reads. Set once in TestMain.
var (
	testEndpoint       string
	testToken          string
	hasLocalGovernance bool
	hasSSO             bool
	govSkipReason      = "server does not report the 'local_governance' capability; unskip when spec 06a lands"
	protoV6Factories   map[string]func() (tfprotov6.ProviderServer, error)
)

func init() {
	protoV6Factories = map[string]func() (tfprotov6.ProviderServer, error){
		"clawvisor": providerserver.NewProtocol6WithError(tfprovider.New("test")()),
	}
}

func TestMain(m *testing.M) {
	// Acceptance tests need the terraform CLI. Pin to the system binary so the
	// harness never tries to download one (deterministic, offline-friendly).
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if tfPath, err := exec.LookPath("terraform"); err == nil {
			os.Setenv("TF_ACC_TERRAFORM_PATH", tfPath)
		}
	}
	// The harness gates on TF_ACC; boot the server and enable it so a plain
	// `go test ./provider/...` runs the acceptance lane (the server is
	// hermetic).
	os.Setenv("TF_ACC", "1")

	shutdown, err := bootServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testacc: failed to boot server: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	shutdown()
	os.Exit(code)
}

// bootServer builds + starts clawvisor-server, mints an instance-admin token
// from the bootstrap token, and exports CLAWVISOR_ENDPOINT/CLAWVISOR_API_TOKEN.
// Modeled on e2e/testapp but process-lifetime scoped (testapp binds teardown
// to a *testing.T, which does not fit a package-wide TestMain server).
func bootServer() (func(), error) {
	bin, err := buildServerBinary()
	if err != nil {
		return nil, fmt.Errorf("build server: %w", err)
	}

	dataDir, err := os.MkdirTemp("", "clawvisor-testacc-")
	if err != nil {
		return nil, err
	}
	cleanupDir := func() { os.RemoveAll(dataDir) }

	port, err := freePort()
	if err != nil {
		cleanupDir()
		return nil, err
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)

	vaultKeyFile := filepath.Join(dataDir, "vault.key")
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		cleanupDir()
		return nil, err
	}
	if err := os.WriteFile(vaultKeyFile, []byte(base64.StdEncoding.EncodeToString(keyBytes)), 0600); err != nil {
		cleanupDir()
		return nil, err
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")
	cfg := fmt.Sprintf(`
server:
  port: %d
  host: "127.0.0.1"
  log_format: "text"
  log_level: "warn"
  public_url: "%s"
database:
  driver: "sqlite"
  sqlite_path: "%s/clawvisor.db"
vault:
  backend: "local"
  local_key_file: "%s"
  reference_allowlist:
    - "arn:aws:secretsmanager:"
    - "projects/"
auth:
  jwt_secret: "test-jwt-secret-must-be-long-enough-32"
  access_token_ttl: "1h"
  refresh_token_ttl: "24h"
approval:
  timeout: 60
  on_timeout: "fail"
task:
  default_expiry_seconds: 1800
relay:
  enabled: false
push:
  enabled: false
telemetry:
  enabled: false
runtime_proxy:
  enabled: false
proxy_lite:
  enabled: true
`, port, endpoint, dataDir, vaultKeyFile)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		cleanupDir()
		return nil, err
	}

	bootstrapToken, err := generateBootstrapToken()
	if err != nil {
		cleanupDir()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "server")
	cmd.Env = append(os.Environ(),
		"CONFIG_FILE="+cfgPath,
		"CLAWVISOR_DATA_DIR="+dataDir,
		"LOG_LEVEL=warn",
		"CLAWVISOR_BOOTSTRAP_TOKEN="+bootstrapToken,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		cleanupDir()
		return nil, fmt.Errorf("start server: %w", err)
	}

	shutdown := func() {
		cancel()
		_ = cmd.Wait()
		cleanupDir()
	}

	if err := waitReady(endpoint, 30*time.Second); err != nil {
		shutdown()
		return nil, err
	}

	// Mint the long-lived instance-admin token using the bootstrap token
	// (spec 05 burn-on-first-mint). This is the credential every acceptance
	// test authenticates with.
	bootClient := client.New(endpoint, bootstrapToken, "", &http.Client{Timeout: 10 * time.Second})
	tok, err := bootClient.CreateToken(ctx, client.CreateTokenRequest{Name: "terraform-acc", Scope: "instance-admin"})
	if err != nil {
		shutdown()
		return nil, fmt.Errorf("mint instance-admin token: %w", err)
	}

	testEndpoint = endpoint
	testToken = tok.Token
	os.Setenv("CLAWVISOR_ENDPOINT", endpoint)
	os.Setenv("CLAWVISOR_API_TOKEN", tok.Token)

	// Negotiate capabilities once so governance tests can skip cleanly.
	adminClient := client.New(endpoint, tok.Token, "", &http.Client{Timeout: 10 * time.Second})
	if features, err := adminClient.Features(ctx); err == nil {
		hasLocalGovernance = features.LocalGovernance
		hasSSO = features.SSO
	}

	return shutdown, nil
}

func generateBootstrapToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 32 bytes → 43 base64url chars (no padding), matching cvat_ format.
	return "cvat_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func waitReady(endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	hc := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := hc.Get(endpoint + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("server at %s did not become ready within %s", endpoint, timeout)
}
