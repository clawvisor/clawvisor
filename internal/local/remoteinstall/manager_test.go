package remoteinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeRepoRef(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"clawvisor/local-integrations":                    "clawvisor/local-integrations",
		"github.com/clawvisor/local-integrations":         "clawvisor/local-integrations",
		"https://github.com/clawvisor/local-integrations": "clawvisor/local-integrations",
	}

	for input, want := range cases {
		got, err := NormalizeRepoRef(input)
		if err != nil {
			t.Fatalf("NormalizeRepoRef(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeRepoRef(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestManifestRejectsDuplicateAliases(t *testing.T) {
	t.Parallel()

	manifest := &Manifest{
		SchemaVersion:            ManifestSchemaVersion,
		Repo:                     "clawvisor/local-integrations",
		Version:                  "v0.1.0",
		MinClawvisorLocalVersion: "0.1.0",
		Services: []ManifestService{
			testService("apple.imessage", "imessage"),
			testService("apple.messages", "Imessage"),
		},
	}

	if err := manifest.Validate(manifest.Repo); err == nil || !strings.Contains(err.Error(), "duplicate alias") {
		t.Fatalf("expected duplicate alias error, got %v", err)
	}
}

func TestInstallAndUninstallLifecycle(t *testing.T) {
	baseDir := t.TempDir()
	homeDir := t.TempDir()

	serviceBundle := mustTarGz(t, map[string]string{
		"payload/service.yaml": "name: Test Service\nid: apple.imessage\nplatform: " + runtime.GOOS + "\nactions:\n  - id: ping\n    run: ./run.sh\n",
		"payload/run.sh":       "#!/bin/sh\necho ok\n",
	})
	runtimeBundle := mustTarGz(t, map[string]string{
		"helper": "#!/bin/sh\necho helper\n",
	})

	serviceSHA := sha256Hex(serviceBundle)
	runtimeSHA := sha256Hex(runtimeBundle)

	manifest := Manifest{
		SchemaVersion:            ManifestSchemaVersion,
		Repo:                     "clawvisor/local-integrations",
		Version:                  "v0.1.0",
		MinClawvisorLocalVersion: "0.1.0",
		Services: []ManifestService{
			{
				ID:                   "apple.imessage",
				Name:                 "iMessage",
				Description:          "Test",
				Aliases:              []string{"imessage"},
				Platforms:            []string{runtime.GOOS},
				ServiceSchemaVersion: ServiceSchemaVersion,
				ServiceBundle: ManifestAsset{
					AssetName:   "service-apple.imessage.tar.gz",
					SHA256:      serviceSHA,
					ArchiveType: "tar.gz",
					InstallTo:   "services/apple.imessage",
					ReplaceMode: ReplaceModeDir,
				},
				RuntimeAssets: []RuntimeAsset{
					{
						ManifestAsset: ManifestAsset{
							AssetName:   "imessage-helper.tar.gz",
							SHA256:      runtimeSHA,
							ArchiveType: "tar.gz",
							InstallTo:   "bin/imessage-helper",
							ReplaceMode: ReplaceModeFile,
						},
						OS:   runtime.GOOS,
						Arch: runtime.GOARCH,
					},
				},
				PostInstall: PostInstallInfo{
					Notes: []string{"Do a thing"},
				},
			},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/clawvisor/local-integrations/releases/latest":
			writeJSON(t, w, map[string]any{
				"tag_name": "v0.1.0",
				"html_url": server.URL + "/release/v0.1.0",
				"assets": []map[string]string{
					{"name": ManifestAssetName, "browser_download_url": server.URL + "/assets/" + ManifestAssetName},
					{"name": "service-apple.imessage.tar.gz", "browser_download_url": server.URL + "/assets/service-apple.imessage.tar.gz"},
					{"name": "imessage-helper.tar.gz", "browser_download_url": server.URL + "/assets/imessage-helper.tar.gz"},
				},
			})
		case "/assets/" + ManifestAssetName:
			w.Write(manifestBytes)
		case "/assets/service-apple.imessage.tar.gz":
			w.Write(serviceBundle)
		case "/assets/imessage-helper.tar.gz":
			w.Write(runtimeBundle)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewManager(baseDir)
	manager.HomeDir = homeDir
	manager.HTTPClient = server.Client()
	manager.githubAPIBase = server.URL

	ctx := context.Background()
	result, err := manager.Install(ctx, manifest.Repo, []string{"imessage"}, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(result.Services) != 1 {
		t.Fatalf("expected one installed service, got %d", len(result.Services))
	}

	servicePath := filepath.Join(baseDir, "services", "apple.imessage", "service.yaml")
	if _, err := os.Stat(servicePath); err != nil {
		t.Fatalf("expected installed service.yaml: %v", err)
	}

	runtimePath := filepath.Join(homeDir, ".clawvisor", "bin", "imessage-helper")
	if _, err := os.Stat(runtimePath); err != nil {
		t.Fatalf("expected installed runtime asset: %v", err)
	}

	if _, err := manager.Install(ctx, manifest.Repo, []string{"apple.imessage"}, false); err != nil {
		t.Fatalf("idempotent reinstall: %v", err)
	}

	uninstallResult, err := manager.Uninstall("apple.imessage", true)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if uninstallResult.ServiceID != "apple.imessage" {
		t.Fatalf("unexpected uninstall service id: %s", uninstallResult.ServiceID)
	}
	if _, err := os.Stat(runtimePath); !os.IsNotExist(err) {
		t.Fatalf("expected runtime asset to be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "services", "apple.imessage")); !os.IsNotExist(err) {
		t.Fatalf("expected service dir to be removed, stat err = %v", err)
	}
}

func testService(id string, aliases ...string) ManifestService {
	return ManifestService{
		ID:                   id,
		Name:                 id,
		Description:          "desc",
		Aliases:              aliases,
		Platforms:            []string{"darwin"},
		ServiceSchemaVersion: ServiceSchemaVersion,
		ServiceBundle: ManifestAsset{
			AssetName:   "service-" + id + ".tar.gz",
			SHA256:      "deadbeef",
			ArchiveType: "tar.gz",
			InstallTo:   "services/" + id,
			ReplaceMode: ReplaceModeDir,
		},
	}
}

func mustTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, body := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode json: %v", err)
	}
}
