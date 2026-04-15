package remoteinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultGitHubAPIBase = "https://api.github.com"

func NormalizeRepoRef(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, "/")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "github.com/")

	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("unknown repo format %q", raw)
	}
	return parts[0] + "/" + parts[1], nil
}

func (m *Manager) fetchLatestRelease(ctx context.Context, repo string) (*Release, error) {
	apiBase := m.githubAPIBase
	if apiBase == "" {
		apiBase = defaultGitHubAPIBase
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(apiBase, "/"), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d for %s", resp.StatusCode, repo)
	}

	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding release metadata: %w", err)
	}

	release := &Release{
		Repo:    repo,
		TagName: payload.TagName,
		HTMLURL: payload.HTMLURL,
		Assets:  make(map[string]ReleaseAsset, len(payload.Assets)),
	}
	for _, asset := range payload.Assets {
		release.Assets[asset.Name] = ReleaseAsset{Name: asset.Name, URL: asset.URL}
	}
	return release, nil
}

func (m *Manager) FetchManifest(ctx context.Context, repoRef string) (*ManifestBundle, error) {
	repo, err := NormalizeRepoRef(repoRef)
	if err != nil {
		return nil, err
	}
	release, err := m.fetchLatestRelease(ctx, repo)
	if err != nil {
		return nil, err
	}
	asset, ok := release.Assets[ManifestAssetName]
	if !ok {
		return nil, fmt.Errorf("repo %s is not installable by clawvisor-local: missing %s", repo, ManifestAssetName)
	}

	body, err := m.downloadBytes(ctx, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("downloading manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if err := manifest.Validate(repo); err != nil {
		return nil, err
	}

	return &ManifestBundle{
		Repo:     repo,
		Manifest: &manifest,
		Release:  release,
	}, nil
}

func (m *Manager) downloadBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func (m *Manager) httpClient() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return &http.Client{Timeout: 2 * time.Minute}
}
