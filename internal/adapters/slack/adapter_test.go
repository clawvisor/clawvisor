package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// slackServer stands in for both slack.com/api and files.slack.com. The
// adapter builds absolute URLs, so tests point url_private_download at the
// test server and relax the host check via hostCheckOverride.
type slackServer struct {
	*httptest.Server
	fileMeta           map[string]any
	content            []byte
	contentType        string
	contentDisposition string
	contentCode        int
	gotAuth            string
	redirectTo         string // when set, /files/download 302s here
}

func newSlackServer(t *testing.T) *slackServer {
	t.Helper()
	s := &slackServer{contentType: "image/png", contentCode: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/files.info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s.fileMeta == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "file_not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": s.fileMeta})
	})
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, r *http.Request) {
		s.gotAuth = r.Header.Get("Authorization")
		if s.redirectTo != "" {
			http.Redirect(w, r, s.redirectTo, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", s.contentType)
		if s.contentDisposition != "" {
			w.Header().Set("Content-Disposition", s.contentDisposition)
		}
		w.WriteHeader(s.contentCode)
		_, _ = w.Write(s.content)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// withTestHosts points the adapter at the test server and allows its host.
func withTestHosts(t *testing.T, base string) {
	t.Helper()
	oldAPI, oldCheck := apiBase, hostCheck
	apiBase = base + "/api"
	hostCheck = func(string) error { return nil }
	t.Cleanup(func() { apiBase, hostCheck = oldAPI, oldCheck })
}

func cred(token string) []byte {
	return []byte(fmt.Sprintf(`{"access_token":%q}`, token))
}

func run(t *testing.T, params map[string]any, c []byte) (*adapters.Result, error) {
	t.Helper()
	return New().Execute(context.Background(), adapters.Request{
		Action: "download_file", Params: params, Credential: c,
	})
}

func TestDownloadFileBinaryReturnsBase64(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff}
	s.fileMeta = map[string]any{
		"id": "F1", "name": "shot.png", "mimetype": "image/png",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	res, err := run(t, map[string]any{"file_id": "F1"}, cred("xoxp-tok"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["encoding"] != "base64" {
		t.Errorf("encoding = %v, want base64", data["encoding"])
	}
	got, err := base64.StdEncoding.DecodeString(data["content"].(string))
	if err != nil {
		t.Fatalf("content is not decodable base64: %v", err)
	}
	if string(got) != string(s.content) {
		t.Errorf("round-trip mismatch: got %v want %v", got, s.content)
	}
	if data["mime_type"] != "image/png" {
		t.Errorf("mime_type = %v", data["mime_type"])
	}
	if s.gotAuth != "Bearer xoxp-tok" {
		t.Errorf("content request auth = %q, want bearer token", s.gotAuth)
	}
}

// Every content type comes back base64, including text — a text path could
// not guarantee byte-exactness.
func TestDownloadFileTextIsAlsoBase64AndByteExact(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	// Angle brackets would be eaten by the HTML stripper; the trailing bytes
	// are invalid UTF-8, which a JSON string would replace with U+FFFD.
	s.content = append([]byte("col_a,col_b\n\"a<b>c\",2\n"), 0xff, 0xfe)
	s.contentType = "text/csv"
	s.fileMeta = map[string]any{
		"id": "F2", "name": "data.csv", "mimetype": "text/csv",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	res, err := run(t, map[string]any{"file_id": "F2"}, cred("t"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["encoding"] != "base64" {
		t.Fatalf("encoding = %v, want base64 for all types", data["encoding"])
	}
	got, err := base64.StdEncoding.DecodeString(data["content"].(string))
	if err != nil {
		t.Fatalf("undecodable base64: %v", err)
	}
	if !bytes.Equal(got, s.content) {
		t.Errorf("text was altered:\n got %v\nwant %v", got, s.content)
	}
}

// The result must survive JSON marshalling unchanged — this is the hop that
// would corrupt raw non-UTF-8 bytes.
func TestDownloadFileSurvivesJSONRoundTrip(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte{0x00, 0x01, 0xff, 0xfe, 0x80, 'h', 'i', 0xc3, 0x28}
	s.contentType = "application/octet-stream"
	s.fileMeta = map[string]any{
		"id": "F11", "name": "raw.bin", "mimetype": "application/octet-stream",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	res, err := run(t, map[string]any{"file_id": "F11"}, cred("t"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	encoded, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatalf("marshalling result: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshalling result: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(back["content"].(string))
	if err != nil {
		t.Fatalf("undecodable base64 after round trip: %v", err)
	}
	if !bytes.Equal(got, s.content) {
		t.Errorf("bytes changed across JSON:\n got %v\nwant %v", got, s.content)
	}
}

// files.slack.com 302s to a CDN host. Slack requires the bearer token at the
// final hop — the opposite of OneDrive's pre-signed URLs, which must have it
// stripped — so assert the token survives the redirect and that the hop is
// still host-checked. Without this the token could be forwarded to an
// arbitrary host, or dropped, and CI would stay green.
func TestDownloadFileForwardsTokenAcrossRedirectAndChecksHost(t *testing.T) {
	var finalAuth string
	var checked []string

	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer final.Close()

	s := newSlackServer(t)
	// Must be a genuinely different hostname, not just a different port:
	// net/http only strips Authorization when the redirect leaves the origin
	// host, so a 127.0.0.1 -> 127.0.0.1 hop would preserve the header on its
	// own and prove nothing. httptest binds 127.0.0.1; "localhost" resolves to
	// the same listener but is a distinct host string, which is what triggers
	// the strip this test exists to catch.
	s.redirectTo = strings.Replace(final.URL, "127.0.0.1", "localhost", 1) + "/cdn/blob"
	s.fileMeta = map[string]any{
		"id": "F20", "name": "shot.png", "mimetype": "image/png",
		"size": 4, "url_private_download": s.URL + "/files/download",
	}

	// Record every URL the host check sees instead of disabling it.
	oldAPI, oldCheck := apiBase, hostCheck
	apiBase = s.URL + "/api"
	hostCheck = func(u string) error { checked = append(checked, u); return nil }
	t.Cleanup(func() { apiBase, hostCheck = oldAPI, oldCheck })

	if _, err := run(t, map[string]any{"file_id": "F20"}, cred("xoxp-tok")); err != nil {
		t.Fatalf("redirected download should succeed: %v", err)
	}
	if finalAuth != "Bearer xoxp-tok" {
		t.Errorf("token did not survive the redirect: final hop saw %q", finalAuth)
	}
	// Initial URL plus the redirect target.
	if len(checked) < 2 {
		t.Fatalf("expected the redirect target to be host-checked, saw %v", checked)
	}
	if !strings.Contains(checked[len(checked)-1], "/cdn/blob") {
		t.Errorf("redirect target was not host-checked, saw %v", checked)
	}
}

// A redirect to a non-Slack host must abort the download.
func TestDownloadFileRejectsRedirectToForeignHost(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the foreign host — the token may have leaked")
		w.WriteHeader(200)
	}))
	defer evil.Close()

	s := newSlackServer(t)
	s.redirectTo = evil.URL + "/steal"
	s.fileMeta = map[string]any{
		"id": "F21", "name": "shot.png", "mimetype": "image/png",
		"size": 4, "url_private_download": s.URL + "/files/download",
	}

	// Real host check, with only the API base treated as Slack.
	oldAPI, oldCheck := apiBase, hostCheck
	apiBase = s.URL + "/api"
	hostCheck = func(u string) error {
		if strings.HasPrefix(u, s.URL) {
			return nil
		}
		return checkSlackHost(u)
	}
	t.Cleanup(func() { apiBase, hostCheck = oldAPI, oldCheck })

	if _, err := run(t, map[string]any{"file_id": "F21"}, cred("t")); err == nil {
		t.Fatal("expected a redirect to a non-Slack host to be refused")
	}
}

// A truncated binary download is undecodable, so an oversized file must be a
// hard error rather than a success-shaped partial result.
func TestDownloadFileRefusesOversizedFile(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.fileMeta = map[string]any{
		"id": "F3", "name": "big.zip", "mimetype": "application/zip",
		"size": format.DefaultDownloadBytes + 1, "url_private_download": s.URL + "/files/download",
	}

	_, err := run(t, map[string]any{"file_id": "F3"}, cred("t"))
	if err == nil {
		t.Fatal("expected an error for an oversized file")
	}
	if !strings.Contains(err.Error(), "max_bytes") {
		t.Errorf("error should point at max_bytes, got: %v", err)
	}
}

// files.info's size can disagree with the actual body, so the read itself is
// also bounded.
func TestDownloadFileRefusesUnderreportedSize(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = make([]byte, 2048)
	s.fileMeta = map[string]any{
		"id": "F4", "name": "liar.bin", "mimetype": "application/octet-stream",
		"size": 10, "url_private_download": s.URL + "/files/download",
	}

	_, err := run(t, map[string]any{"file_id": "F4", "max_bytes": float64(1024)}, cred("t"))
	if err == nil {
		t.Fatal("expected an error when the body exceeds max_bytes")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("got: %v", err)
	}
}

func TestDownloadFileAllowsRaisedLimit(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = make([]byte, format.DefaultDownloadBytes+1024)
	s.fileMeta = map[string]any{
		"id": "F5", "name": "big.png", "mimetype": "image/png",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	res, err := run(t, map[string]any{"file_id": "F5", "max_bytes": float64(format.MaxDownloadBytes)}, cred("t"))
	if err != nil {
		t.Fatalf("raising max_bytes should permit the download: %v", err)
	}
	if got := res.Data.(map[string]any)["size"].(int); got != len(s.content) {
		t.Errorf("size = %d, want %d", got, len(s.content))
	}
}

// Slack answers an under-scoped token with 200 + a sign-in page, so status
// alone cannot be treated as success.
func TestDownloadFileDetectsHTMLLoginPage(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte("<!DOCTYPE html><html><body>Sign in to Slack</body></html>")
	s.contentType = "text/html; charset=utf-8"
	s.fileMeta = map[string]any{
		"id": "F6", "name": "shot.png", "mimetype": "image/png",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	_, err := run(t, map[string]any{"file_id": "F6"}, cred("t"))
	if err == nil {
		t.Fatal("expected an error for an HTML sign-in page")
	}
	if !strings.Contains(err.Error(), "files:read") {
		t.Errorf("error should mention the likely scope cause, got: %v", err)
	}
}

// Regression: an uploaded .html file is legitimately HTML. The first cut
// rejected every one of them as a sign-in page.
func TestDownloadFileAcceptsGenuineHTMLFile(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte("<!DOCTYPE html><html><body><h1>Report</h1></body></html>")
	s.contentType = "text/html"
	s.contentDisposition = `attachment; filename="report.html"`
	s.fileMeta = map[string]any{
		"id": "F7", "name": "report.html", "mimetype": "text/html",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	res, err := run(t, map[string]any{"file_id": "F7"}, cred("t"))
	if err != nil {
		t.Fatalf("an HTML file should download, got: %v", err)
	}
	data := res.Data.(map[string]any)
	// HTML must round-trip byte-exact, not go through the HTML-stripping
	// text path.
	if data["encoding"] != "base64" {
		t.Fatalf("expected base64 for HTML, got encoding=%v", data["encoding"])
	}
	got, err := base64.StdEncoding.DecodeString(data["content"].(string))
	if err != nil {
		t.Fatalf("undecodable base64: %v", err)
	}
	if string(got) != string(s.content) {
		t.Errorf("HTML was altered:\n got %q\nwant %q", got, s.content)
	}
}

// An HTML file whose length matches files.info is the file even without a
// Content-Disposition header.
func TestDownloadFileAcceptsHTMLFileWithoutDisposition(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte("<html><body>ok</body></html>")
	s.contentType = "text/html"
	s.fileMeta = map[string]any{
		"id": "F8", "name": "page.html", "mimetype": "text/html",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
	}

	if _, err := run(t, map[string]any{"file_id": "F8"}, cred("t")); err != nil {
		t.Fatalf("expected the HTML file to download, got: %v", err)
	}
}

// The sign-in page must still be caught: HTML, no attachment disposition, and
// a length that disagrees with files.info.
func TestDownloadFileStillDetectsSignInPageForHTMLFile(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte("<!DOCTYPE html><html><body>Sign in to Slack</body></html>")
	s.contentType = "text/html; charset=utf-8"
	s.fileMeta = map[string]any{
		"id": "F9", "name": "page.html", "mimetype": "text/html",
		"size":                 4096, // real file is 4 KB; we got a short interstitial
		"url_private_download": s.URL + "/files/download",
	}

	_, err := run(t, map[string]any{"file_id": "F9"}, cred("t"))
	if err == nil {
		t.Fatal("expected the sign-in page to be rejected")
	}
	if !strings.Contains(err.Error(), "sign-in page") {
		t.Errorf("got: %v", err)
	}
}

// Regression: files.info embeds preview/plain_text for textual files, so its
// response scales with file size. Capping the read at 200 KB truncated the
// JSON and surfaced "unexpected end of JSON input".
func TestFileInfoHandlesLargeMetadataResponse(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.content = []byte("<html>small body</html>")
	s.contentType = "text/html"
	s.contentDisposition = `attachment; filename="big.html"`
	s.fileMeta = map[string]any{
		"id": "F10", "name": "big.html", "mimetype": "text/html",
		"size": len(s.content), "url_private_download": s.URL + "/files/download",
		// Slack embeds the file's text; ~400 KB of it, well past the old cap.
		"plain_text": strings.Repeat("x", 400*1024),
	}

	if _, err := run(t, map[string]any{"file_id": "F10"}, cred("t")); err != nil {
		t.Fatalf("large files.info metadata should parse, got: %v", err)
	}
}

func TestDownloadFilePropagatesSlackAPIError(t *testing.T) {
	s := newSlackServer(t)
	withTestHosts(t, s.URL)
	s.fileMeta = nil // files.info returns ok:false

	_, err := run(t, map[string]any{"file_id": "nope"}, cred("t"))
	if err == nil || !strings.Contains(err.Error(), "file_not_found") {
		t.Fatalf("want the Slack error surfaced, got: %v", err)
	}
}

func TestDownloadFileRequiresFileID(t *testing.T) {
	if _, err := run(t, map[string]any{}, cred("t")); err == nil {
		t.Fatal("expected an error when file_id is missing")
	}
}

// The download URL arrives from an API response, so the host is pinned rather
// than trusted.
func TestCheckSlackHost(t *testing.T) {
	ok := []string{
		"https://files.slack.com/files-pri/T1-F1/shot.png",
		"https://slack.com/files/x",
	}
	for _, u := range ok {
		if err := checkSlackHost(u); err != nil {
			t.Errorf("checkSlackHost(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"https://evil.com/x",
		"https://files.slack.com.evil.com/x",
		"http://files.slack.com/x", // not https
		"https://127.0.0.1/x",
	}
	for _, u := range bad {
		if err := checkSlackHost(u); err == nil {
			t.Errorf("checkSlackHost(%q) = nil, want an error", u)
		}
	}
}

func TestUnsupportedAction(t *testing.T) {
	_, err := New().Execute(context.Background(), adapters.Request{
		Action: "send_message", Credential: cred("t"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported action") {
		t.Fatalf("got: %v", err)
	}
}
