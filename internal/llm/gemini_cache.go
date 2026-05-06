package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GeminiCacheManager owns the lifecycle of a single Vertex cachedContents
// resource that holds a static system prompt. It creates the cache at app
// startup and refreshes it before TTL expiry. Clients call CacheName() on
// every request to discover the current resource name; the empty string
// means the cache isn't currently available and the client should fall
// through to the uncached path (inlining systemInstruction).
//
// Refresh failures are non-fatal: the existing cache continues to be used
// until it expires server-side, at which point the client gracefully
// degrades to uncached calls.
type GeminiCacheManager struct {
	cfg          GeminiCacheManagerConfig
	httpClient   *http.Client
	tokenSource  oauth2.TokenSource
	logger       *slog.Logger

	// cacheName is updated atomically so reads from CacheName() never lock.
	cacheName atomic.Value // string

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// GeminiCacheManagerConfig parameterizes the manager. Project, Region,
// Model, and SystemPrompt are required. TTL defaults to 30 minutes.
type GeminiCacheManagerConfig struct {
	Project      string
	Region       string // "global" allowed
	Model        string // bare model name, e.g. "gemini-3.1-flash-lite-preview"
	SystemPrompt string
	TTL          time.Duration // default 30m
	HTTPClient   *http.Client  // optional; defaults to a 30s-timeout client
	TokenSource  oauth2.TokenSource // optional; defaults to ADC
	Logger       *slog.Logger
}

// NewGeminiCacheManager constructs a manager. Call Start to actually
// create the cache and begin the refresh loop.
func NewGeminiCacheManager(cfg GeminiCacheManagerConfig) (*GeminiCacheManager, error) {
	if cfg.Project == "" {
		return nil, fmt.Errorf("gemini cache: Project is required")
	}
	if cfg.Region == "" {
		cfg.Region = "global"
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("gemini cache: Model is required")
	}
	if cfg.SystemPrompt == "" {
		return nil, fmt.Errorf("gemini cache: SystemPrompt is required")
	}
	if cfg.TTL == 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TokenSource == nil {
		ts, err := google.DefaultTokenSource(context.Background(),
			"https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("gemini cache: default token source: %w", err)
		}
		cfg.TokenSource = ts
	}
	m := &GeminiCacheManager{
		cfg:         cfg,
		httpClient:  cfg.HTTPClient,
		tokenSource: cfg.TokenSource,
		logger:      cfg.Logger,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	m.cacheName.Store("")
	return m, nil
}

// Start creates the initial cache and launches the refresh goroutine.
// Returns an error if the initial cache creation fails — callers can
// decide whether to fail startup or proceed without caching.
func (m *GeminiCacheManager) Start(ctx context.Context) error {
	name, err := m.create(ctx)
	if err != nil {
		return fmt.Errorf("create initial cache: %w", err)
	}
	m.cacheName.Store(name)
	m.logger.Info("gemini cache created",
		"name", name, "model", m.cfg.Model, "ttl", m.cfg.TTL)
	go m.refreshLoop()
	return nil
}

// CacheName returns the current cache resource name, or "" if no cache is
// currently registered. Safe for concurrent use; non-blocking.
func (m *GeminiCacheManager) CacheName() string {
	v, _ := m.cacheName.Load().(string)
	return v
}

// Stop signals the refresh loop to exit and best-effort-deletes the
// current cache. Safe to call multiple times.
func (m *GeminiCacheManager) Stop(ctx context.Context) {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		<-m.doneCh
		if name, _ := m.cacheName.Load().(string); name != "" {
			m.cacheName.Store("")
			if err := m.delete(ctx, name); err != nil {
				m.logger.Warn("gemini cache delete on shutdown failed (will auto-expire)",
					"err", err, "name", name)
			}
		}
	})
}

// refreshLoop recreates the cache before its TTL expires. Refresh failures
// don't tear down the existing cache — clients keep using it until it
// server-side-expires, at which point CacheName() effectively becomes
// invalid and the next request will get a 404 (which the client treats as
// a transient error and retries; if the second attempt also fails, the
// caller's retry logic handles it). To avoid that race we refresh well
// before TTL expires (TTL - 5min, or 80% of TTL for short TTLs).
func (m *GeminiCacheManager) refreshLoop() {
	defer close(m.doneCh)
	refreshAt := m.cfg.TTL - 5*time.Minute
	if refreshAt < m.cfg.TTL/2 {
		refreshAt = m.cfg.TTL * 4 / 5
	}
	t := time.NewTicker(refreshAt)
	defer t.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			newName, err := m.create(ctx)
			cancel()
			if err != nil {
				m.logger.Warn("gemini cache refresh failed; keeping old cache until it expires",
					"err", err)
				continue
			}
			old, _ := m.cacheName.Load().(string)
			m.cacheName.Store(newName)
			m.logger.Info("gemini cache refreshed",
				"new_name", newName, "old_name", old)
			// Best-effort delete the old cache. Don't block the loop on it.
			if old != "" {
				go func(name string) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := m.delete(ctx, name); err != nil {
						m.logger.Debug("delete of old cache failed (will auto-expire)",
							"err", err, "name", name)
					}
				}(old)
			}
		}
	}
}

// create POSTs to cachedContents and returns the new resource name.
func (m *GeminiCacheManager) create(ctx context.Context) (string, error) {
	host := m.cfg.Region + "-aiplatform.googleapis.com"
	if m.cfg.Region == "global" {
		host = "aiplatform.googleapis.com"
	}
	url := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/cachedContents",
		host, m.cfg.Project, m.cfg.Region)
	modelPath := fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s",
		m.cfg.Project, m.cfg.Region, m.cfg.Model)
	body := map[string]any{
		"model": modelPath,
		"systemInstruction": map[string]any{
			"parts": []map[string]any{{"text": m.cfg.SystemPrompt}},
		},
		"ttl": fmt.Sprintf("%ds", int(m.cfg.TTL.Seconds())),
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	tok, err := m.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create cachedContents status %d: %s",
			resp.StatusCode, truncate1k(string(respBody)))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	if out.Name == "" {
		return "", fmt.Errorf("create cachedContents returned empty name")
	}
	return out.Name, nil
}

// delete best-effort removes a cachedContents resource. Failure is
// non-fatal because the cache will auto-expire by TTL.
func (m *GeminiCacheManager) delete(ctx context.Context, name string) error {
	host := m.cfg.Region + "-aiplatform.googleapis.com"
	if m.cfg.Region == "global" {
		host = "aiplatform.googleapis.com"
	}
	url := fmt.Sprintf("https://%s/v1/%s", host, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	tok, err := m.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("delete status %d: %s", resp.StatusCode, truncate1k(string(body)))
	}
	return nil
}

func truncate1k(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
