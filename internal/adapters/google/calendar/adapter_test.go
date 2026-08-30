package calendar

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type timeoutAfterCommitError struct{}

func (timeoutAfterCommitError) Error() string   { return "timed out reading provider response" }
func (timeoutAfterCommitError) Timeout() bool   { return true }
func (timeoutAfterCommitError) Temporary() bool { return true }

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(body)
}

func fixtureWithoutETag(t *testing.T, name string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(fixture(t, name)), &body); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	delete(body, "etag")
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode fixture %s without etag: %v", name, err)
	}
	return string(encoded)
}

func response(status int, body string, headers ...http.Header) *http.Response {
	header := make(http.Header)
	if len(headers) > 0 {
		header = headers[0].Clone()
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func requireFailure(t *testing.T, err error, kind adapters.ExecutionFailureKind) *adapters.ExecutionFailure {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s failure, got nil", kind)
	}
	failure, ok := adapters.AsExecutionFailure(err)
	if !ok {
		t.Fatalf("error %T (%v) is not a classified execution failure", err, err)
	}
	if failure.Kind != kind {
		t.Fatalf("failure kind = %q, want %q (error: %v)", failure.Kind, kind, err)
	}
	return failure
}

func TestCalendarAttendeePatchAcceptsEmailObjects(t *testing.T) {
	attendees, err := calendarAttendeePatch([]any{
		"first@example.com",
		map[string]any{"email": " second@example.com "},
		map[string]string{"email": "third@example.com"},
	})
	if err != nil {
		t.Fatalf("calendarAttendeePatch returned error: %v", err)
	}
	want := []map[string]string{
		{"email": "first@example.com"},
		{"email": "second@example.com"},
		{"email": "third@example.com"},
	}
	if fmt.Sprint(attendees) != fmt.Sprint(want) {
		t.Fatalf("attendees = %#v, want %#v", attendees, want)
	}
}

func TestGetEventExposesProviderETagAndVersion(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/calendar/v3/calendars/primary/events/event-123" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		if req.URL.RawQuery != "" {
			t.Fatalf("events.get must not receive unsupported query parameters: %s", req.URL.RawQuery)
		}
		// A body ETag is canonical when Google returns both representations.
		return response(http.StatusOK, fixture(t, "event.json"), http.Header{"Etag": {`"header-v1"`}}), nil
	})}

	result, err := (&CalendarAdapter{}).getEvent(t.Context(), client, map[string]any{"event_id": "event-123"})
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	if result.Summary != "Event: Calendar approval review" {
		t.Fatalf("summary = %q, want catalog-compatible summary", result.Summary)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data = %T, want map[string]any", result.Data)
	}
	for _, field := range []string{"etag", "version"} {
		if got := data[field]; got != `"event-v1"` {
			t.Errorf("%s = %v, want %q", field, got, `"event-v1"`)
		}
	}
	if got := data["sequence"]; got != int64(7) {
		t.Errorf("sequence = %v (%T), want 7", got, got)
	}
	if got := data["response_status"]; got != "needsAction" {
		t.Errorf("response_status = %v, want needsAction", got)
	}
}

func TestGetEventUsesHeaderETagFallback(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(
			http.StatusOK,
			fixtureWithoutETag(t, "event.json"),
			http.Header{"Etag": {`"header-v1"`}},
		), nil
	})}

	result, err := (&CalendarAdapter{}).getEvent(t.Context(), client, map[string]any{"event_id": "event-123"})
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	data := result.Data.(map[string]any)
	if data["etag"] != `"header-v1"` || data["version"] != `"header-v1"` {
		t.Fatalf("etag/version = %v/%v, want header fallback", data["etag"], data["version"])
	}
}

func TestGetEventMissingVersionFailsClosed(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, fixtureWithoutETag(t, "event.json")), nil
	})}

	_, err := (&CalendarAdapter{}).getEvent(t.Context(), client, map[string]any{"event_id": "event-123"})
	requireFailure(t, err, adapters.ExecutionFailureDefinite)
}

func TestUpdateEventUsesIfMatchAndSendUpdates(t *testing.T) {
	tests := []struct {
		name       string
		param      any
		want       string
		versionKey string
	}{
		{name: "default none", want: "none", versionKey: "expected_etag"},
		{name: "all", param: "all", want: "all", versionKey: "expected_etag"},
		{name: "external only and version alias", param: "externalOnly", want: "externalOnly", versionKey: "expected_version"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.Method != http.MethodPatch || req.URL.Path != "/calendar/v3/calendars/primary/events/event-123" {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				}
				if got := req.Header.Get("If-Match"); got != `"event-v1"` {
					t.Fatalf("If-Match = %q, want %q", got, `"event-v1"`)
				}
				if got := req.URL.Query().Get("sendUpdates"); got != tc.want {
					t.Fatalf("sendUpdates = %q, want %q", got, tc.want)
				}
				var payload map[string]any
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode update payload: %v", err)
				}
				if payload["summary"] != "Calendar approval review (updated)" {
					t.Errorf("summary = %v", payload["summary"])
				}
				for _, forbidden := range []string{"expected_etag", "expected_version", "send_updates"} {
					if _, exists := payload[forbidden]; exists {
						t.Errorf("provider payload unexpectedly contains %q: %v", forbidden, payload)
					}
				}
				return response(http.StatusOK, fixture(t, "updated_event.json")), nil
			})}

			params := map[string]any{
				"event_id":    "event-123",
				"summary":     "Calendar approval review (updated)",
				tc.versionKey: `"event-v1"`,
			}
			if tc.param != nil {
				params["send_updates"] = tc.param
			}
			result, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, params)
			if err != nil {
				t.Fatalf("updateEvent: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("provider calls = %d, want 1", calls.Load())
			}
			data := result.Data.(map[string]any)
			if data["etag"] != `"event-v2"` || data["version"] != `"event-v2"` {
				t.Errorf("etag/version = %v/%v, want event-v2", data["etag"], data["version"])
			}
			if data["send_updates"] != tc.want {
				t.Errorf("result send_updates = %v, want %s", data["send_updates"], tc.want)
			}
		})
	}
}

func TestUpdateEventRejectsInvalidSendUpdatesBeforeProviderCall(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("unexpected provider call")
	})}

	_, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, map[string]any{
		"event_id":      "event-123",
		"expected_etag": `"event-v1"`,
		"summary":       "changed",
		"send_updates":  "yes",
	})
	requireFailure(t, err, adapters.ExecutionFailureDefinite)
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", calls.Load())
	}
}

func TestUpdateEventStaleVersionIsClassified(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("If-Match"); got != `"event-v1"` {
			t.Fatalf("If-Match = %q, want event-v1", got)
		}
		return response(http.StatusPreconditionFailed, fixture(t, "provider_error.json")), nil
	})}

	_, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, map[string]any{
		"event_id":      "event-123",
		"expected_etag": `"event-v1"`,
		"summary":       "changed",
	})
	requireFailure(t, err, adapters.ExecutionFailureStaleVersion)
}

func TestUpdateEventProviderRejectionIsDefinite(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusBadRequest, `{"error":{"message":"invalid event"}}`), nil
	})}

	_, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, map[string]any{
		"event_id":      "event-123",
		"expected_etag": `"event-v1"`,
		"summary":       "changed",
	})
	requireFailure(t, err, adapters.ExecutionFailureDefinite)
}

func TestUpdateEventRequiresConcreteExpectedVersion(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
	}{
		{name: "missing", params: map[string]any{}},
		{name: "empty", params: map[string]any{"expected_etag": "  "}},
		{name: "wildcard", params: map[string]any{"expected_etag": "*"}},
		{name: "conflicting aliases", params: map[string]any{
			"expected_etag": `"event-v1"`, "expected_version": `"event-v0"`,
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, fmt.Errorf("unexpected provider call")
			})}
			tc.params["event_id"] = "event-123"
			tc.params["summary"] = "changed"

			_, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, tc.params)
			requireFailure(t, err, adapters.ExecutionFailureDefinite)
			if calls.Load() != 0 {
				t.Fatalf("provider calls = %d, want 0", calls.Load())
			}
		})
	}
}

func TestUpdateEventTimeoutAfterProviderCommitIsAmbiguous(t *testing.T) {
	var committed atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("If-Match") != `"event-v1"` {
			t.Fatal("conditional write was not sent before simulated commit")
		}
		committed.Add(1)
		return nil, timeoutAfterCommitError{}
	})}

	_, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, map[string]any{
		"event_id":      "event-123",
		"expected_etag": `"event-v1"`,
		"summary":       "changed",
	})
	failure := requireFailure(t, err, adapters.ExecutionFailureAmbiguous)
	if !failure.TimedOut {
		t.Fatal("timeout-after-commit must set TimedOut")
	}
	if committed.Load() != 1 {
		t.Fatalf("simulated provider commits = %d, want 1", committed.Load())
	}
}

func TestUpdateEventSuccessWithoutNewVersionFailsClosedAsAmbiguous(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"id":"event-123","summary":"changed"}`), nil
	})}

	_, err := (&CalendarAdapter{}).updateEvent(t.Context(), client, map[string]any{
		"event_id":      "event-123",
		"expected_etag": `"event-v1"`,
		"summary":       "changed",
	})
	failure := requireFailure(t, err, adapters.ExecutionFailureAmbiguous)
	if failure.TimedOut {
		t.Fatal("missing response version is ambiguous but is not a timeout")
	}
}

func TestRespondToEventUpdatesOnlySelfWithConditionalWrite(t *testing.T) {
	tests := []struct {
		status       string
		versionParam string
		sendUpdates  string
		wantSend     string
	}{
		{status: "accepted", versionParam: "expected_etag", sendUpdates: "all", wantSend: "all"},
		{status: "declined", versionParam: "expected_etag", wantSend: "none"},
		{status: "tentative", versionParam: "expected_version", sendUpdates: "externalOnly", wantSend: "externalOnly"},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch calls.Add(1) {
				case 1:
					if req.Method != http.MethodGet {
						t.Fatalf("pre-read method = %s, want GET", req.Method)
					}
					return response(http.StatusOK, fixture(t, "event.json")), nil
				case 2:
					if req.Method != http.MethodPatch {
						t.Fatalf("mutation method = %s, want PATCH", req.Method)
					}
					if got := req.Header.Get("If-Match"); got != `"event-v1"` {
						t.Fatalf("If-Match = %q, want event-v1", got)
					}
					if got := req.URL.Query().Get("sendUpdates"); got != tc.wantSend {
						t.Fatalf("sendUpdates = %q, want %q", got, tc.wantSend)
					}
					var payload struct {
						AttendeesOmitted bool `json:"attendeesOmitted"`
						Attendees        []struct {
							Email          string `json:"email"`
							ResponseStatus string `json:"responseStatus"`
						} `json:"attendees"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatalf("decode RSVP payload: %v", err)
					}
					if !payload.AttendeesOmitted {
						t.Fatal("attendeesOmitted = false, want true")
					}
					if len(payload.Attendees) != 1 {
						t.Fatalf("attendees = %+v, want exactly self attendee", payload.Attendees)
					}
					if payload.Attendees[0].Email != "self@example.com" || payload.Attendees[0].ResponseStatus != tc.status {
						t.Fatalf("attendee patch = %+v, want self/%s", payload.Attendees[0], tc.status)
					}
					return response(http.StatusOK, fixture(t, "updated_event.json")), nil
				default:
					t.Fatalf("unexpected extra provider request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			})}

			params := map[string]any{
				"event_id":        "event-123",
				"response_status": tc.status,
				tc.versionParam:   `"event-v1"`,
			}
			if tc.sendUpdates != "" {
				params["send_updates"] = tc.sendUpdates
			}
			result, err := (&CalendarAdapter{}).respondToEvent(t.Context(), client, params)
			if err != nil {
				t.Fatalf("respondToEvent: %v", err)
			}
			if calls.Load() != 2 {
				t.Fatalf("provider calls = %d, want GET + PATCH", calls.Load())
			}
			data := result.Data.(map[string]any)
			if data["response_status"] != tc.status || data["send_updates"] != tc.wantSend {
				t.Errorf("result = %v, want status=%s send_updates=%s", data, tc.status, tc.wantSend)
			}
			if data["etag"] != `"event-v2"` {
				t.Errorf("result etag = %v, want event-v2", data["etag"])
			}
		})
	}
}

func TestRespondToEventRejectsInvalidRSVPBeforeProviderCall(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("unexpected provider call")
	})}

	_, err := (&CalendarAdapter{}).respondToEvent(t.Context(), client, map[string]any{
		"event_id":        "event-123",
		"expected_etag":   `"event-v1"`,
		"response_status": "maybe",
	})
	requireFailure(t, err, adapters.ExecutionFailureDefinite)
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", calls.Load())
	}
}

func TestRespondToEventRequiresExplicitSelfAttendee(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) != 1 || req.Method != http.MethodGet {
			return nil, fmt.Errorf("unexpected provider mutation: %s %s", req.Method, req.URL.String())
		}
		return response(http.StatusOK, `{
			"id":"event-123",
			"etag":"\"event-v1\"",
			"attendees":[{"email":"shared-calendar@example.com","responseStatus":"needsAction"}]
		}`), nil
	})}

	_, err := (&CalendarAdapter{}).respondToEvent(t.Context(), client, map[string]any{
		"event_id":        "event-123",
		"calendar_id":     "shared-calendar@example.com",
		"expected_etag":   `"event-v1"`,
		"response_status": "accepted",
	})
	requireFailure(t, err, adapters.ExecutionFailureDefinite)
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want only pre-read", calls.Load())
	}
}

func TestRespondToEventStalePreReadDoesNotMutate(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusOK, fixture(t, "event.json")), nil
	})}

	_, err := (&CalendarAdapter{}).respondToEvent(t.Context(), client, map[string]any{
		"event_id":        "event-123",
		"expected_etag":   `"event-v0"`,
		"response_status": "accepted",
	})
	requireFailure(t, err, adapters.ExecutionFailureStaleVersion)
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want only pre-read", calls.Load())
	}
}

func TestRespondToEventProviderRechecksVersionAtMutation(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return response(http.StatusOK, fixture(t, "event.json")), nil
		case 2:
			if got := req.Header.Get("If-Match"); got != `"event-v1"` {
				t.Fatalf("If-Match = %q, want event-v1", got)
			}
			return response(http.StatusPreconditionFailed, fixture(t, "provider_error.json")), nil
		default:
			return nil, fmt.Errorf("unexpected provider call")
		}
	})}

	_, err := (&CalendarAdapter{}).respondToEvent(t.Context(), client, map[string]any{
		"event_id":        "event-123",
		"expected_etag":   `"event-v1"`,
		"response_status": "accepted",
	})
	requireFailure(t, err, adapters.ExecutionFailureStaleVersion)
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want GET + conditional PATCH", calls.Load())
	}
}
