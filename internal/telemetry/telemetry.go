package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/version"
)

const (
	defaultEndpoint = "https://telemetry.clawvisor.com/v1/events"
	reportInterval  = 24 * time.Hour
	httpTimeout     = 10 * time.Second
)

// event is the anonymous payload sent to the telemetry server.
type event struct {
	Event     string `json:"event"`
	Timestamp string `json:"ts"`
	Version   string `json:"version"`

	// Infrastructure (static, low-entropy).
	OS   string `json:"os"`
	Arch string `json:"arch"`

	// Usage (bucketed to prevent fingerprinting).
	Agents string `json:"agents"`

	// Per-service gateway request counts (bucketed).
	// e.g. {"gmail": "100-1000", "github": "1-10"}
	ServiceUsage map[string]string `json:"service_usage"`
}

// bucket maps an exact count to a coarse range.
func bucket(n int) string {
	switch {
	case n == 0:
		return "0"
	case n <= 5:
		return "1-5"
	case n <= 20:
		return "6-20"
	case n <= 100:
		return "21-100"
	case n <= 1000:
		return "101-1000"
	default:
		return "1000+"
	}
}

// Start begins periodic anonymous telemetry reporting. It sends a "startup"
// event immediately and then a "heartbeat" every 24 hours. The goroutine
// exits when ctx is cancelled.
func Start(ctx context.Context, cfg *config.Config, st store.Store, logger *slog.Logger) {
	if !cfg.Telemetry.Enabled {
		return
	}

	ep := cfg.Telemetry.Endpoint
	if ep == "" {
		ep = defaultEndpoint
	}

	base := event{
		Version: version.Version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}

	// Send startup event immediately (non-blocking).
	go func() {
		e := base
		e.Event = "startup"
		e.Timestamp = time.Now().UTC().Format(time.RFC3339)
		fillUsage(ctx, st, &e, logger)
		send(ctx, ep, &e, logger)
	}()

	// Periodic heartbeat.
	go func() {
		ticker := time.NewTicker(reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e := base
				e.Event = "heartbeat"
				e.Timestamp = time.Now().UTC().Format(time.RFC3339)
				fillUsage(ctx, st, &e, logger)
				send(ctx, ep, &e, logger)
			}
		}
	}()
}

func fillUsage(ctx context.Context, st store.Store, e *event, logger *slog.Logger) {
	counts, err := st.TelemetryCounts(ctx)
	if err != nil {
		logger.Debug("telemetry: count query error", "err", err)
		return
	}
	e.Agents = bucket(counts.Agents)
	e.ServiceUsage = make(map[string]string, len(counts.RequestsByService))
	for svc, n := range counts.RequestsByService {
		e.ServiceUsage[svc] = bucket(n)
	}
}

func send(ctx context.Context, endpoint string, e *event, logger *slog.Logger) {
	body, err := json.Marshal(e)
	if err != nil {
		logger.Debug("telemetry: marshal error", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		logger.Debug("telemetry: request error", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("telemetry: send error", "err", err)
		return
	}
	resp.Body.Close()
	logger.Info("telemetry: sent", "event", e.Event, "endpoint", endpoint, "status", resp.StatusCode)
}
