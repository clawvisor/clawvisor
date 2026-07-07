package scenarios_test

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestProxyLiteExportsToRealCollector is a MANUAL live check that the server's
// OTLP exporter reaches a REAL OpenTelemetry Collector (not the in-process
// testharness receiver). Stand up the bundled collector first:
//
//	docker run -d --name cv-otel-live -p 4318:4318 -p 4317:4317 \
//	  -v "$PWD/deploy/otel-collector.yaml:/etc/otel-collector.yaml" \
//	  otel/opentelemetry-collector-contrib:0.104.0 --config=/etc/otel-collector.yaml
//
// then run:
//
//	OTEL_COLLECTOR_LIVE=1 go test ./e2e/scenarios -run TestProxyLiteExportsToRealCollector -v
//
// and confirm the spans/metrics arrived:
//
//	docker logs cv-otel-live 2>&1 | grep -E 'clawvisor\.(proxy_lite\.request|llm\.requests|llm\.tokens)'
func TestProxyLiteExportsToRealCollector(t *testing.T) {
	if os.Getenv("OTEL_COLLECTOR_LIVE") == "" {
		t.Skip("set OTEL_COLLECTOR_LIVE=1 with a collector on :4318 (see doc comment)")
	}
	endpoint := os.Getenv("OTEL_COLLECTOR_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4318"
	}

	h := testharness.New(t)
	stub := stubAnthropic(t, "OK")

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_OTEL_ENABLED":                  "true",
		"CLAWVISOR_OTEL_ENDPOINT":                 endpoint,
		"CLAWVISOR_OTEL_PROTOCOL":                 "http",
		"CLAWVISOR_OTEL_INSECURE":                 "true",
		"CLAWVISOR_OTEL_METRICS_INTERVAL_SECONDS": "1",
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":        stub.URL,
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "otel-live-agent"}, &agent)

	resp := postMessages(t, cv, agent.Token,
		`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly the word OK."}]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	t.Logf("proxy-lite request OK; giving the batch exporter time to flush to %s", endpoint)
	// The server shuts down on test cleanup, which force-flushes spans and the
	// final metric export. Sleep past one metric interval so at least one
	// periodic metric push also lands before shutdown.
	time.Sleep(8 * time.Second)
}
