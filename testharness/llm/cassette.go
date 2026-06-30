// Package llm provides record/replay/passthrough middleware for LLM API
// calls (Anthropic, OpenAI). Tests run in one of three modes:
//
//	LLM_MODE=replay       — read cassette from disk; never hit network (default)
//	LLM_MODE=record       — hit real API; write cassette to disk
//	LLM_MODE=passthrough  — hit real API; do not write cassette
//
// Cassettes are committed JSON files under testdata/llm-cassettes/<test>/
// — one file per request, named by sequence ("000.json", "001.json", …).
// Request matching uses (method, host, path, body-hash) so test reruns
// replay deterministically.
//
// Wiring:
//
//	The middleware is an http.RoundTripper. Tests inject it as the Transport
//	on the http.Client passed to the Anthropic / OpenAI SDK constructors.
//	For subprocess tests, point ANTHROPIC_BASE_URL / OPENAI_BASE_URL at a
//	local proxy that records/replays — see Server() in proxy.go.
package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Mode controls cassette behavior. Default is replay; tests can override per
// test via SetMode(t, "passthrough") if they really need live LLM output.
type Mode string

const (
	ModeReplay      Mode = "replay"
	ModeRecord      Mode = "record"
	ModePassthrough Mode = "passthrough"
)

// CurrentMode returns the active mode from $LLM_MODE, defaulting to replay.
func CurrentMode() Mode {
	switch strings.ToLower(os.Getenv("LLM_MODE")) {
	case "record":
		return ModeRecord
	case "passthrough":
		return ModePassthrough
	default:
		return ModeReplay
	}
}

// Cassette wraps an http.RoundTripper. Tests construct one per-test (so
// recordings end up isolated per scenario file/test name) and inject it
// into LLM client construction.
type Cassette struct {
	dir       string
	mode      Mode
	upstream  http.RoundTripper
	seq       atomic.Int64
	mu        sync.Mutex
	turnIndex map[string]int // request-key → index of next take

	// loaded is the in-memory cache of cassette entries on disk. Populated
	// on first replay; subsequent replays consult this rather than
	// re-reading and re-parsing every JSON file (which was O(n²) total
	// I/O for n requests).
	loaded     []cassetteEntry
	loadedOnce sync.Once
	loadedErr  error
}

// NewCassette creates a cassette for testName, rooted at dir (typically
// testdata/llm-cassettes). The directory is created if missing in record
// mode and read on every roundtrip in replay mode.
func NewCassette(dir, testName string, mode Mode) *Cassette {
	d := filepath.Join(dir, sanitize(testName))
	return &Cassette{
		dir:       d,
		mode:      mode,
		upstream:  http.DefaultTransport,
		turnIndex: map[string]int{},
	}
}

// RoundTrip implements http.RoundTripper.
func (c *Cassette) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readAndRestore(req)
	if err != nil {
		return nil, err
	}
	key := requestKey(req, body)

	switch c.mode {
	case ModePassthrough:
		return c.upstream.RoundTrip(req)

	case ModeRecord:
		if err := os.MkdirAll(c.dir, 0755); err != nil {
			return nil, fmt.Errorf("cassette mkdir: %w", err)
		}
		// Re-build the body since RoundTrip below consumes it.
		req.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := c.upstream.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		entry := cassetteEntry{
			Request: cassetteRequest{
				Method: req.Method,
				URL:    req.URL.String(),
				Body:   string(body),
				Key:    key,
			},
			Response: cassetteResponse{
				Status:  resp.StatusCode,
				Headers: flattenHeaders(resp.Header),
				Body:    string(respBody),
			},
		}
		if err := c.writeNext(entry); err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		return resp, nil

	default: // ModeReplay
		entry, err := c.readNext(key)
		if err != nil {
			return nil, fmt.Errorf("cassette: %w (set LLM_MODE=record to recreate)", err)
		}
		// Build a response that matches the shape http.DefaultTransport
		// would have returned for a real call. Some clients (and middleware
		// layers like net/http itself) read Status/Proto/ContentLength —
		// leaving them zero diverges from production behavior.
		bodyBytes := []byte(entry.Response.Body)
		resp := &http.Response{
			Status:        fmt.Sprintf("%d %s", entry.Response.Status, http.StatusText(entry.Response.Status)),
			StatusCode:    entry.Response.Status,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        unflattenHeaders(entry.Response.Headers),
			Body:          io.NopCloser(bytes.NewReader(bodyBytes)),
			ContentLength: int64(len(bodyBytes)),
			Request:       req,
		}
		return resp, nil
	}
}

// Client returns an *http.Client that uses this cassette as its transport.
func (c *Cassette) Client() *http.Client {
	return &http.Client{Transport: c}
}

// SetUpstream overrides the default RoundTripper used in record/passthrough
// modes. Useful when tests want to layer their own logging.
func (c *Cassette) SetUpstream(rt http.RoundTripper) { c.upstream = rt }

// cassetteEntry is one recorded request/response pair.
type cassetteEntry struct {
	Request  cassetteRequest  `json:"request"`
	Response cassetteResponse `json:"response"`
}

type cassetteRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Body   string `json:"body"`
	Key    string `json:"key"`
}

type cassetteResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// requestKey is a stable hash of (method, path, normalized-body) used to
// match record/replay. Host is intentionally omitted so cassettes survive
// upstream-URL changes (per-test ephemeral ports, prod vs staging, etc).
// Body is normalized by parsing JSON and sorting keys so cosmetic
// field-order differences don't break replay.
func requestKey(req *http.Request, body []byte) string {
	h := sha256.New()
	fmt.Fprintln(h, req.Method)
	fmt.Fprintln(h, req.URL.Path)
	if u, err := url.Parse(req.URL.String()); err == nil {
		fmt.Fprintln(h, u.RawQuery)
	}
	h.Write(normalizeJSONBody(body))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func normalizeJSONBody(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b // not JSON; use as-is
	}
	out, _ := json.Marshal(sortMaps(v))
	return out
}

func sortMaps(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := map[string]any{}
		for _, k := range keys {
			out[k] = sortMaps(x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = sortMaps(e)
		}
		return out
	default:
		return v
	}
}

func readAndRestore(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// flattenHeaders converts http.Header to the on-disk shape. Preserves
// per-value cardinality so multi-valued headers (Set-Cookie, Via, Link…)
// round-trip correctly. Earlier versions joined values with ',' which
// silently fused distinct Set-Cookie entries into one corrupted header.
func flattenHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		dup := make([]string, len(v))
		copy(dup, v)
		out[k] = dup
	}
	return out
}

func unflattenHeaders(m map[string][]string) http.Header {
	h := http.Header{}
	for k, vs := range m {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	return h
}

func sanitize(s string) string {
	r := strings.NewReplacer("/", "_", ":", "_", " ", "-", ".", "-")
	return r.Replace(s)
}

// writeNext writes the entry to dir/NNN.json with monotonically increasing
// sequence number across the cassette's lifetime.
func (c *Cassette) writeNext(entry cassetteEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.seq.Add(1) - 1
	path := filepath.Join(c.dir, fmt.Sprintf("%03d.json", idx))
	b, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// readNext returns the next entry matching key. Entries are read in
// directory order and matched per-key — so a test that fires (A, B, A) sees
// the first A, then B, then the second A.
//
// The cassette dir is read + parsed once per Cassette instance (cached on
// the receiver). Earlier implementations re-walked the directory on every
// replay request — O(n²) total I/O for n requests.
func (c *Cassette) readNext(key string) (cassetteEntry, error) {
	c.loadedOnce.Do(func() {
		c.loaded, c.loadedErr = loadAllEntries(c.dir)
	})
	if c.loadedErr != nil {
		return cassetteEntry{}, c.loadedErr
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	matching := 0
	want := c.turnIndex[key]
	for _, e := range c.loaded {
		if e.Request.Key == key {
			if matching == want {
				c.turnIndex[key] = want + 1
				return e, nil
			}
			matching++
		}
	}
	return cassetteEntry{}, fmt.Errorf("no entry %d for key %s in %s", want, key, c.dir)
}

func loadAllEntries(dir string) ([]cassetteEntry, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	out := make([]cassetteEntry, 0, len(files))
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		var e cassetteEntry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
