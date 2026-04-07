// Package calendar implements the Clawvisor adapter for Google Calendar.
package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
)

const serviceID = "google.calendar"

// calendarScopes are the OAuth scopes required by the Calendar adapter.
var calendarScopes = []string{
	"https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/userinfo.email",
}

// CalendarAdapter implements adapters.Adapter for Google Calendar.
type CalendarAdapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

func New(provider adapters.OAuthCredentialProvider) *CalendarAdapter {
	return &CalendarAdapter{oauthProvider: provider}
}

func (a *CalendarAdapter) ServiceID() string { return serviceID }

func (a *CalendarAdapter) SupportedActions() []string {
	return []string{"list_events", "get_event", "create_event", "update_event", "delete_event", "list_calendars"}
}

func (a *CalendarAdapter) RequiredScopes() []string { return calendarScopes }

func (a *CalendarAdapter) OAuthConfig() *oauth2.Config {
	clientID, clientSecret := a.oauthProvider.OAuthClientCredentials()
	if clientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       calendarScopes,
		Endpoint:     google.Endpoint,
	}
}

func (a *CalendarAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, calendarScopes)
}

func (a *CalendarAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
}

// FetchIdentity returns the Google account email for auto-alias detection.
func (a *CalendarAdapter) FetchIdentity(ctx context.Context, credBytes []byte) (string, error) {
	client, err := a.httpClient(ctx, credBytes)
	if err != nil {
		return "", err
	}
	return credential.FetchGoogleEmail(ctx, client)
}

func (a *CalendarAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.httpClient(ctx, req.Credential)
	if err != nil {
		return nil, err
	}
	switch req.Action {
	case "list_events":
		return a.listEvents(ctx, client, req.Params)
	case "get_event":
		return a.getEvent(ctx, client, req.Params)
	case "create_event":
		return a.createEvent(ctx, client, req.Params)
	case "update_event":
		return a.updateEvent(ctx, client, req.Params)
	case "delete_event":
		return a.deleteEvent(ctx, client, req.Params)
	case "list_calendars":
		return a.listCalendars(ctx, client)
	default:
		return nil, fmt.Errorf("calendar: unsupported action %q", req.Action)
	}
}

func (a *CalendarAdapter) httpClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	ts := a.OAuthConfig().TokenSource(ctx, cred.ToOAuth2Token())
	return oauth2.NewClient(ctx, ts), nil
}

// ── list_events ───────────────────────────────────────────────────────────────

type calendarEvent struct {
	ID          string   `json:"id"`
	Summary     string   `json:"summary"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Location    string   `json:"location,omitempty"`
	Description string   `json:"description,omitempty"`
	Attendees   []string `json:"attendees,omitempty"`
	Status      string   `json:"status"`
}

func (a *CalendarAdapter) listEvents(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	calendarID := "primary"
	if v, ok := params["calendar_id"].(string); ok && v != "" {
		calendarID = v
	}
	timeMin := dateToRFC3339(params, "time_min", "from")
	timeMax := dateToRFC3339Max(params, "time_max", "to")
	// Default to now if no start time — avoids returning old recurring events.
	if timeMin == "" {
		timeMin = time.Now().UTC().Format(time.RFC3339)
	}
	maxResults := 10
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("maxResults", fmt.Sprintf("%d", maxResults))
	q.Set("singleEvents", "true")
	q.Set("orderBy", "startTime")
	if timeMin != "" {
		q.Set("timeMin", timeMin)
	}
	if timeMax != "" {
		q.Set("timeMax", timeMax)
	}

	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events?%s",
		url.PathEscape(calendarID), q.Encode())

	var resp struct {
		Items []struct {
			ID          string `json:"id"`
			Summary     string `json:"summary"`
			Location    string `json:"location"`
			Description string `json:"description"`
			Status      string `json:"status"`
			Start       struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"start"`
			End struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"end"`
			Attendees []struct {
				Email string `json:"email"`
			} `json:"attendees"`
		} `json:"items"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("calendar list_events: %w", err)
	}

	events := make([]calendarEvent, 0, len(resp.Items))
	for _, item := range resp.Items {
		startStr := item.Start.DateTime
		if startStr == "" {
			startStr = item.Start.Date
		}
		endStr := item.End.DateTime
		if endStr == "" {
			endStr = item.End.Date
		}
		attendees := make([]string, 0, len(item.Attendees))
		for _, att := range item.Attendees {
			attendees = append(attendees, format.SanitizeText(att.Email, format.MaxFieldLen))
		}
		events = append(events, calendarEvent{
			ID:          item.ID,
			Summary:     format.SanitizeText(item.Summary, format.MaxFieldLen),
			Start:       startStr,
			End:         endStr,
			Location:    format.SanitizeText(item.Location, format.MaxFieldLen),
			Description: format.SanitizeText(item.Description, format.MaxSnippetLen),
			Attendees:   attendees,
			Status:      item.Status,
		})
	}

	summary := format.Summary("%d event(s)", len(events))
	if len(events) == 1 {
		summary = format.Summary("1 event: %s", events[0].Summary)
	}
	return &adapters.Result{Summary: summary, Data: events}, nil
}

// ── get_event ─────────────────────────────────────────────────────────────────

func (a *CalendarAdapter) getEvent(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	eventID, _ := params["event_id"].(string)
	if eventID == "" {
		return nil, fmt.Errorf("calendar get_event: event_id is required")
	}
	calendarID := "primary"
	if v, ok := params["calendar_id"].(string); ok && v != "" {
		calendarID = v
	}

	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events/%s",
		url.PathEscape(calendarID), url.PathEscape(eventID))

	var item struct {
		ID          string `json:"id"`
		Summary     string `json:"summary"`
		Location    string `json:"location"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Start       struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"end"`
		Attendees []struct {
			Email       string `json:"email"`
			DisplayName string `json:"displayName"`
		} `json:"attendees"`
	}
	if err := apiGET(ctx, client, apiURL, &item); err != nil {
		return nil, fmt.Errorf("calendar get_event: %w", err)
	}

	startStr := item.Start.DateTime
	if startStr == "" {
		startStr = item.Start.Date
	}
	endStr := item.End.DateTime
	if endStr == "" {
		endStr = item.End.Date
	}
	attendees := make([]string, 0, len(item.Attendees))
	for _, att := range item.Attendees {
		name := att.DisplayName
		if name == "" {
			name = att.Email
		}
		attendees = append(attendees, format.SanitizeText(name, format.MaxFieldLen))
	}
	event := calendarEvent{
		ID:          item.ID,
		Summary:     format.SanitizeText(item.Summary, format.MaxFieldLen),
		Start:       startStr,
		End:         endStr,
		Location:    format.SanitizeText(item.Location, format.MaxFieldLen),
		Description: format.SanitizeText(item.Description, format.MaxBodyLen),
		Attendees:   attendees,
		Status:      item.Status,
	}
	return &adapters.Result{
		Summary: format.Summary("Event: %s on %s", event.Summary, event.Start),
		Data:    event,
	}, nil
}

// ── create_event ──────────────────────────────────────────────────────────────

func (a *CalendarAdapter) createEvent(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	calendarID := "primary"
	if v, ok := params["calendar_id"].(string); ok && v != "" {
		calendarID = v
	}
	eventSummary, _ := params["summary"].(string)
	start, _ := params["start"].(string)
	end, _ := params["end"].(string)
	description, _ := params["description"].(string)
	if eventSummary == "" || start == "" || end == "" {
		return nil, fmt.Errorf("calendar create_event: summary, start, and end are required")
	}

	body := map[string]any{
		"summary":     eventSummary,
		"description": description,
		"start":       calendarDtField(start),
		"end":         calendarDtField(end),
	}
	if rawAttendees, ok := params["attendees"].([]any); ok {
		attendees := make([]map[string]string, 0, len(rawAttendees))
		for _, att := range rawAttendees {
			if email, ok := att.(string); ok {
				attendees = append(attendees, map[string]string{"email": email})
			}
		}
		if len(attendees) > 0 {
			body["attendees"] = attendees
		}
	}

	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events",
		url.PathEscape(calendarID))
	var created struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
		HTMLLink string `json:"htmlLink"`
	}
	if err := apiWrite(ctx, client, http.MethodPost, apiURL, body, &created); err != nil {
		return nil, fmt.Errorf("calendar create_event: %w", err)
	}
	return &adapters.Result{
		Summary: format.Summary("Created event: %s", created.Summary),
		Data:    map[string]any{"event_id": created.ID, "summary": created.Summary, "link": created.HTMLLink},
	}, nil
}

// ── update_event ──────────────────────────────────────────────────────────────

func (a *CalendarAdapter) updateEvent(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	eventID, _ := params["event_id"].(string)
	if eventID == "" {
		return nil, fmt.Errorf("calendar update_event: event_id is required")
	}
	calendarID := "primary"
	if v, ok := params["calendar_id"].(string); ok && v != "" {
		calendarID = v
	}

	patch := map[string]any{}
	if v, ok := params["summary"].(string); ok {
		patch["summary"] = v
	}
	if v, ok := params["description"].(string); ok {
		patch["description"] = v
	}
	if v, ok := params["start"].(string); ok {
		patch["start"] = calendarDtField(v)
	}
	if v, ok := params["end"].(string); ok {
		patch["end"] = calendarDtField(v)
	}

	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events/%s",
		url.PathEscape(calendarID), url.PathEscape(eventID))
	var updated struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	}
	if err := apiWrite(ctx, client, http.MethodPatch, apiURL, patch, &updated); err != nil {
		return nil, fmt.Errorf("calendar update_event: %w", err)
	}
	return &adapters.Result{
		Summary: format.Summary("Updated event: %s", updated.Summary),
		Data:    map[string]string{"event_id": updated.ID, "summary": updated.Summary},
	}, nil
}

// ── delete_event ──────────────────────────────────────────────────────────────

func (a *CalendarAdapter) deleteEvent(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	eventID, _ := params["event_id"].(string)
	if eventID == "" {
		return nil, fmt.Errorf("calendar delete_event: event_id is required")
	}
	calendarID := "primary"
	if v, ok := params["calendar_id"].(string); ok && v != "" {
		calendarID = v
	}

	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events/%s",
		url.PathEscape(calendarID), url.PathEscape(eventID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("calendar API DELETE: status %d", resp.StatusCode)
	}
	return &adapters.Result{
		Summary: format.Summary("Deleted event %s", eventID),
		Data:    map[string]string{"event_id": eventID},
	}, nil
}

// ── list_calendars ────────────────────────────────────────────────────────────

func (a *CalendarAdapter) listCalendars(ctx context.Context, client *http.Client) (*adapters.Result, error) {
	var resp struct {
		Items []struct {
			ID      string `json:"id"`
			Summary string `json:"summary"`
			Primary bool   `json:"primary"`
		} `json:"items"`
	}
	if err := apiGET(ctx, client, "https://www.googleapis.com/calendar/v3/users/me/calendarList", &resp); err != nil {
		return nil, fmt.Errorf("calendar list_calendars: %w", err)
	}
	type calItem struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
		Primary bool   `json:"primary"`
	}
	items := make([]calItem, 0, len(resp.Items))
	for _, c := range resp.Items {
		items = append(items, calItem{
			ID:      c.ID,
			Summary: format.SanitizeText(c.Summary, format.MaxFieldLen),
			Primary: c.Primary,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d calendar(s)", len(items)),
		Data:    items,
	}, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func apiGET(ctx context.Context, client *http.Client, apiURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func apiWrite(ctx context.Context, client *http.Client, method, apiURL string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

func paramInt(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// dateToRFC3339 reads a date/datetime param (primary key or alias) from params
// and ensures it is in RFC3339 format. Plain ISO dates ("2006-01-02") are
// converted to "2006-01-02T00:00:00Z" so the Google Calendar API accepts them.
func dateToRFC3339(params map[string]any, key, alias string) string {
	s, _ := params[key].(string)
	if s == "" {
		s, _ = params[alias].(string)
	}
	if s == "" {
		return ""
	}
	// Already looks like RFC3339 — pass through.
	if len(s) > 10 {
		return s
	}
	// Plain date "YYYY-MM-DD" → parse and reformat as RFC3339.
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s // return as-is if we can't parse; API will reject with a clear error
	}
	return t.UTC().Format(time.RFC3339)
}

// dateToRFC3339Max is like dateToRFC3339 but treats plain dates as inclusive
// upper bounds: "2006-01-02" becomes "2006-01-02T23:59:59Z" so that events
// on that day are included. Google Calendar's timeMax is exclusive, so passing
// start-of-day would exclude the entire day.
func dateToRFC3339Max(params map[string]any, key, alias string) string {
	s, _ := params[key].(string)
	if s == "" {
		s, _ = params[alias].(string)
	}
	if s == "" {
		return ""
	}
	if len(s) > 10 {
		return s
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	// End of the given day so the date is treated as inclusive.
	return t.Add(24*time.Hour - time.Second).UTC().Format(time.RFC3339)
}

// calendarDtField builds the Google Calendar API start/end object from a
// user-supplied date or datetime string.
//   - "YYYY-MM-DD"          → {"date": "..."} (all-day event)
//   - "YYYY-MM-DDTHH:MM:SS" → {"dateTime": "...Z"} (UTC assumed)
//   - RFC3339 with tz       → {"dateTime": "..."} (passed through)
func calendarDtField(v string) map[string]string {
	if len(v) == 10 {
		// Plain date — all-day event.
		return map[string]string{"date": v}
	}
	// DateTime: ensure it has a timezone. "2006-01-02T15:04:05" has no tz → add Z.
	if len(v) == 19 {
		if t, err := time.Parse("2006-01-02T15:04:05", v); err == nil {
			return map[string]string{"dateTime": t.UTC().Format(time.RFC3339)}
		}
	}
	return map[string]string{"dateTime": v}
}
