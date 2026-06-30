package llm_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	hllm "github.com/clawvisor/clawvisor/testharness/llm"
)

// TestReplayedResponsePreservesProtoStatusAndContentLength — the
// replayed http.Response must carry the same Status / Proto /
// ContentLength shape http.DefaultTransport would have produced.
// Callers that gate body reads on ContentLength or check Status text
// otherwise see zero values on replay and diverge from record.
func TestReplayedResponsePreservesProtoStatusAndContentLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()

	rec := hllm.NewCassette(dir, "t", hllm.ModeRecord)
	rec.SetUpstream(http.DefaultTransport)
	if _, err := rec.Client().Post(upstream.URL, "application/json",
		strings.NewReader(`{}`)); err != nil {
		t.Fatal(err)
	}

	rep := hllm.NewCassette(dir, "t", hllm.ModeReplay)
	resp, err := rep.Client().Post(upstream.URL, "application/json",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Status == "" {
		t.Fatal("replayed Response has empty Status text")
	}
	if !strings.HasPrefix(resp.Status, "200") {
		t.Fatalf("Status=%q, want 200 prefix", resp.Status)
	}
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("Proto=%q, want HTTP/1.1", resp.Proto)
	}
	if resp.ProtoMajor != 1 || resp.ProtoMinor != 1 {
		t.Fatalf("ProtoMajor/Minor=%d/%d", resp.ProtoMajor, resp.ProtoMinor)
	}
	if resp.ContentLength <= 0 {
		t.Fatalf("ContentLength=%d, want >0 for body %q", resp.ContentLength, `{"ok":true}`)
	}
}

// TestReplayedResponsePreservesMultiValuedHeaders — Set-Cookie (and
// other naturally-multi-valued headers) must round-trip with their
// cardinality intact. Earlier the on-disk shape was map[string]string
// joined with ',', which silently fused distinct Set-Cookie values
// into one corrupted header.
func TestReplayedResponsePreservesMultiValuedHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1; Path=/")
		w.Header().Add("Set-Cookie", "b=2; Path=/")
		w.Header().Add("Link", "<https://x/p1>; rel=next")
		w.Header().Add("Link", "<https://x/p2>; rel=prev")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	dir := t.TempDir()

	rec := hllm.NewCassette(dir, "t", hllm.ModeRecord)
	rec.SetUpstream(http.DefaultTransport)
	r1, err := rec.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(r1.Body)
	r1.Body.Close()
	if got, want := len(r1.Header.Values("Set-Cookie")), 2; got != want {
		t.Fatalf("recorded headers wrong: Set-Cookie count=%d, want %d", got, want)
	}

	rep := hllm.NewCassette(dir, "t", hllm.ModeReplay)
	r2, err := rep.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if got, want := len(r2.Header.Values("Set-Cookie")), 2; got != want {
		t.Fatalf("replayed Set-Cookie count=%d, want %d (multi-value fused?)", got, want)
	}
	if r2.Header.Values("Set-Cookie")[0] != "a=1; Path=/" {
		t.Fatalf("replayed Set-Cookie[0]=%q", r2.Header.Values("Set-Cookie")[0])
	}
	if got := r2.Header.Values("Link"); len(got) != 2 {
		t.Fatalf("replayed Link count=%d, want 2", len(got))
	}
}

// TestCassetteEntriesLoadedOnce — replaying N requests must NOT re-read
// the cassette directory N times. We assert that by verifying repeated
// reads after the cassette dir is wiped: with the cache, subsequent
// replays still find the data; without it, they would error.
func TestCassetteEntriesLoadedOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	dir := t.TempDir()

	// Record three calls.
	rec := hllm.NewCassette(dir, "t", hllm.ModeRecord)
	rec.SetUpstream(http.DefaultTransport)
	for i := 0; i < 3; i++ {
		r, err := rec.Client().Post(upstream.URL, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	rep := hllm.NewCassette(dir, "t", hllm.ModeReplay)
	// First replay: cache is populated from disk.
	if _, err := rep.Client().Post(upstream.URL, "application/json", strings.NewReader(`{}`)); err != nil {
		t.Fatalf("first replay: %v", err)
	}
	// Now wipe the cassette dir. If readNext were still reading from disk
	// on every call, the next replays would fail.
	if err := wipeDir(dir); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := rep.Client().Post(upstream.URL, "application/json", strings.NewReader(`{}`)); err != nil {
			t.Fatalf("post-wipe replay %d: %v (cache didn't populate?)", i, err)
		}
	}
}

func wipeDir(dir string) error {
	return os.RemoveAll(dir)
}
