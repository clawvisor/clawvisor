// Package slack implements the Clawvisor adapter for Slack actions that
// cannot be expressed in YAML. File content lives on files.slack.com rather
// than the slack.com/api base URL, and is returned as raw bytes instead of
// JSON, so the YAML REST runtime cannot fetch it.
package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// apiBase and hostCheck are variables rather than constants so tests can
// point the adapter at an httptest server.
var (
	apiBase   = "https://slack.com/api"
	hostCheck = checkSlackHost
)

// maxFileInfoBytes caps the files.info metadata read. Slack embeds preview and
// plain_text content for textual files, so the metadata scales with the file
// rather than being a small fixed envelope. Derived from the download ceiling
// so a file that max_bytes permits can never be rejected at the metadata step:
// twice the payload covers JSON escaping, plus headroom for the other fields.
const maxFileInfoBytes = 2*format.MaxDownloadBytes + (1 << 20)

// HTTP timeouts. Without them a stalled files.slack.com response would pin a
// gateway goroutine for as long as the caller's context allows, which may be
// no deadline at all.
const (
	fileInfoTimeout = 30 * time.Second

	// baseDownloadTimeout covers connection setup and the first bytes.
	baseDownloadTimeout = 30 * time.Second
	// minDownloadThroughput is the pessimistic floor used to turn a byte
	// budget into a time budget.
	minDownloadThroughput = 128 << 10 // 128 KB/s
	// maxDownloadTimeout bounds the result regardless of max_bytes.
	maxDownloadTimeout = 5 * time.Minute
)

// downloadTimeoutFor scales the transfer budget with the number of bytes the
// caller asked for. http.Client.Timeout is wall-clock and includes reading the
// body, so a single fixed value would either kill a legitimate 10 MB download
// on a slow link or leave a 1 MB one hanging far longer than it should.
func downloadTimeoutFor(maxBytes int64) time.Duration {
	d := baseDownloadTimeout + time.Duration(maxBytes/minDownloadThroughput)*time.Second
	return min(d, maxDownloadTimeout)
}

// Adapter handles Slack actions that require fetching file content.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

// Execute dispatches to the appropriate action handler.
func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, fmt.Errorf("slack: %w", err)
	}
	switch req.Action {
	case "download_file":
		return a.downloadFile(ctx, token, req.Params)
	default:
		return nil, fmt.Errorf("slack: unsupported action %q", req.Action)
	}
}

// ── download_file ────────────────────────────────────────────────────────────

// fileInfo is the subset of Slack's files.info response we need.
type fileInfo struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	Size               int64  `json:"size"`
	URLPrivateDownload string `json:"url_private_download"`
	URLPrivate         string `json:"url_private"`
}

// downloadFile fetches a file's content from Slack and returns it
// base64-encoded, for every content type — see the encoding note below.
//
// Unlike Dropbox and Drive, this is a two-host operation: files.info is served
// from slack.com/api, but the bytes come from files.slack.com and require the
// same bearer token to be presented again.
func (a *Adapter) downloadFile(ctx context.Context, token string, params map[string]any) (*adapters.Result, error) {
	fileID, _ := params["file_id"].(string)
	if fileID == "" {
		return nil, fmt.Errorf("slack download_file: file_id is required")
	}

	maxBytes, err := resolveMaxBytes(params["max_bytes"])
	if err != nil {
		return nil, fmt.Errorf("slack download_file: %w", err)
	}

	meta, err := a.fileInfo(ctx, token, fileID)
	if err != nil {
		return nil, fmt.Errorf("slack download_file: %w", err)
	}

	// Pre-flight on the declared size. Truncating binary content produces an
	// undecodable prefix under a success-shaped summary, so refuse instead.
	if meta.Size > maxBytes {
		return nil, fmt.Errorf(
			"slack download_file: %q is %s, which exceeds the %s limit; "+
				"raise max_bytes (up to %s). The content is base64-encoded into the "+
				"response, so save that response to a file and decode it rather than "+
				"reading it into a model context",
			meta.Name, humanBytes(meta.Size), humanBytes(maxBytes), humanBytes(format.MaxDownloadBytes))
	}

	downloadURL := meta.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = meta.URLPrivate
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("slack download_file: file %q has no download URL (external or deleted file?)", fileID)
	}
	if err := hostCheck(downloadURL); err != nil {
		return nil, fmt.Errorf("slack download_file: %w", err)
	}

	body, contentType, err := a.fetchContent(ctx, token, downloadURL, maxBytes, meta)
	if err != nil {
		return nil, fmt.Errorf("slack download_file: %w", err)
	}

	// Trust files.info's mimetype over the transport's, which is often
	// application/octet-stream; fall back to the extension.
	mimeType := meta.Mimetype
	if mimeType == "" {
		mimeType = contentType
	}
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(meta.Name))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Always base64, for every content type. A download must round-trip the
	// file byte for byte, and the alternatives cannot:
	//
	//   - format.SanitizeText strips HTML and truncates at MaxBodyLen runes.
	//   - Even unsanitized, returning bytes as a JSON string mangles anything
	//     that is not valid UTF-8 — encoding/json substitutes U+FFFD — which
	//     silently corrupts CSVs in legacy encodings and every binary format.
	//
	// The cost is that callers always decode, which is also what makes the
	// contract uniform: one `base64 -d` regardless of file type.
	result := map[string]any{
		"id":        meta.ID,
		"name":      format.SanitizeText(meta.Name, format.MaxFieldLen),
		"mime_type": mimeType,
		"size":      len(body),
		"encoding":  "base64",
		"content":   base64.StdEncoding.EncodeToString(body),
	}

	return &adapters.Result{
		Summary: format.Summary("Downloaded %s (%s, %s)", meta.Name, mimeType, humanBytes(int64(len(body)))),
		Data:    result,
	}, nil
}

// fileInfo calls files.info for the file's metadata and download URL.
func (a *Adapter) fileInfo(ctx context.Context, token, fileID string) (*fileInfo, error) {
	endpoint := apiBase + "/files.info?file=" + url.QueryEscape(fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: fileInfoTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Not MaxBodyLen: for textual files (HTML, snippets, posts) Slack embeds
	// preview/plain_text fields carrying the file's own content, so the
	// metadata response scales with the file and blows past a 200 KB cap.
	// Truncating it yields invalid JSON and an error that points nowhere near
	// the real cause.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFileInfoBytes+1))
	// Status first, as in fetchContent: on a 4xx the partial body carries the
	// error message and is more useful than the read failure.
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("files.info: status %d: %s", resp.StatusCode, format.Truncate(string(body), 200))
	}
	if readErr != nil {
		return nil, fmt.Errorf("files.info: reading response after %d bytes: %w", len(body), readErr)
	}
	if int64(len(body)) > maxFileInfoBytes {
		return nil, fmt.Errorf("files.info: metadata response exceeds %s", humanBytes(maxFileInfoBytes))
	}

	var parsed struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		File  fileInfo `json:"file"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("files.info: parsing response (%d bytes): %w", len(body), err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("files.info: %s", parsed.Error)
	}
	return &parsed.File, nil
}

// fetchContent downloads the raw bytes, returning the body and the transport's
// declared content type. meta is used to tell a genuinely-HTML file apart from
// Slack's sign-in page.
func (a *Adapter) fetchContent(ctx context.Context, token, downloadURL string, maxBytes int64, meta *fileInfo) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	// files.slack.com redirects to a CDN host, and Slack requires the bearer
	// token at the final hop — the opposite of OneDrive's pre-signed URLs,
	// which must have it stripped.
	//
	// net/http drops Authorization when a redirect crosses to a host that is
	// not the origin or a subdomain of it (shouldCopyHeaderOnRedirect), so the
	// header is re-applied explicitly. That is only safe because hostCheck has
	// already confirmed the target is a Slack host; the order matters.
	client := &http.Client{
		Timeout: downloadTimeoutFor(maxBytes),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if err := hostCheck(r.URL.String()); err != nil {
				return err
			}
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	// Read one byte past the limit so truncation is detectable. files.info's
	// size can disagree with reality for some file types.
	//
	// The read error is propagated rather than dropped: a connection broken
	// mid-transfer otherwise yields a short body that base64-encodes cleanly
	// and is returned as a successful — but silently truncated — download.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	// Status first: on a 4xx the partial body is the error message, which is
	// more useful than the read failure.
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("status %d: %s", resp.StatusCode, format.Truncate(string(body), 200))
	}
	if readErr != nil {
		return nil, "", fmt.Errorf("reading content after %d bytes: %w", len(body), readErr)
	}
	if int64(len(body)) > maxBytes {
		return nil, "", fmt.Errorf(
			"content exceeds the %s limit; raise max_bytes (up to %s)",
			humanBytes(maxBytes), humanBytes(format.MaxDownloadBytes))
	}

	contentType := resp.Header.Get("Content-Type")

	// An under-scoped or expired token gets HTTP 200 with an HTML sign-in page
	// rather than an error, so status alone is not a success signal.
	if isSignInPage(resp, body, contentType, meta) {
		return nil, "", fmt.Errorf(
			"received Slack's sign-in page instead of file content — the token likely lacks files:read or cannot access this file")
	}

	return body, contentType, nil
}

// isSignInPage distinguishes Slack's HTML sign-in page from file content.
//
// "The body is HTML" is not sufficient on its own: users upload .html files,
// and rejecting those made every HTML download fail. Two positive signals mark
// a real download — an attachment disposition, which url_private_download sets
// and the sign-in page does not, and a body length matching the size
// files.info reported.
func isSignInPage(resp *http.Response, body []byte, contentType string, meta *fileInfo) bool {
	if !isHTML(contentType, body) {
		return false
	}
	if disp := resp.Header.Get("Content-Disposition"); strings.Contains(strings.ToLower(disp), "attachment") {
		return false
	}
	// A file that is itself HTML and arrived at its declared length is the
	// file, not an interstitial.
	if meta != nil && meta.Size > 0 && int64(len(body)) == meta.Size && isHTMLMime(meta.Mimetype) {
		return false
	}
	return true
}

// ── helpers ──────────────────────────────────────────────────────────────────

// resolveMaxBytes returns the effective download ceiling. Callers that stream
// the response to disk can raise it up to format.MaxDownloadBytes; the default
// stays small enough to survive being read into a model context.
func resolveMaxBytes(v any) (int64, error) {
	if v == nil {
		return format.DefaultDownloadBytes, nil
	}
	var n int64
	switch x := v.(type) {
	case float64: // JSON numbers decode as float64
		n = int64(x)
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return 0, fmt.Errorf("max_bytes must be a number")
	}
	if n <= 0 {
		return 0, fmt.Errorf("max_bytes must be positive")
	}
	if n > format.MaxDownloadBytes {
		return 0, fmt.Errorf("max_bytes may not exceed %s", humanBytes(format.MaxDownloadBytes))
	}
	return n, nil
}

// checkSlackHost rejects download URLs that do not point at Slack. The URL
// comes from an API response rather than the caller, but it still reaches the
// HTTP client as data, so the host is pinned rather than trusted.
func checkSlackHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid download URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("download URL must be https, got %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host != "slack.com" && !strings.HasSuffix(host, ".slack.com") {
		return fmt.Errorf("download URL host %q is not a Slack host", host)
	}
	return nil
}

// isHTMLMime reports whether a declared mimetype means the file is itself HTML.
func isHTMLMime(mimeType string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	return base == "text/html" || base == "application/xhtml+xml"
}

// isHTML reports whether the response looks like a web page rather than file
// content. Slack serves its sign-in page with a 200 status.
func isHTML(contentType string, body []byte) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if base == "text/html" || base == "application/xhtml+xml" {
		return true
	}
	prefix := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 512)])))
	return strings.HasPrefix(prefix, "<!doctype html") || strings.HasPrefix(prefix, "<html")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func extractToken(credBytes []byte) (string, error) {
	var cred struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("parsing credential: %w", err)
	}
	token := cred.Token
	if token == "" {
		token = cred.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("credential missing token")
	}
	return token, nil
}
