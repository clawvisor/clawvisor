package httpmock_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/testharness/httpmock"
)

func TestDefaultAndScriptedResponses(t *testing.T) {
	srv := httpmock.New(t, map[string]httpmock.Response{
		"GET /hello":  {Body: map[string]string{"msg": "hi"}},
		"POST /thing": {Status: 201, Body: map[string]string{"id": "x"}},
	})

	// Default GET.
	resp, err := http.Get(srv.URL() + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"msg":"hi"`) {
		t.Fatalf("body=%s", body)
	}

	// OnNext one-shot.
	srv.OnNext("GET /hello", httpmock.Response{Status: 503, Body: map[string]string{"err": "down"}})
	r2, _ := http.Get(srv.URL() + "/hello")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 503 || !strings.Contains(string(b2), `"err":"down"`) {
		t.Fatalf("one-shot didn't fire: status=%d body=%s", r2.StatusCode, b2)
	}

	// Next request falls back to default.
	r3, _ := http.Get(srv.URL() + "/hello")
	if r3.StatusCode != 200 {
		t.Fatalf("expected fall-back to default 200, got %d", r3.StatusCode)
	}
	r3.Body.Close()

	// Capture works.
	captured := srv.Captured()
	if len(captured) != 3 {
		t.Fatalf("captured=%d, want 3", len(captured))
	}
}
