// Package calendar implements the Clawvisor adapter for Google Calendar.
package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/pkg/adapters"
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
	return []string{"list_events", "get_event", "create_event", "update_event", "respond_to_event", "delete_event", "list_calendars"}
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

func (a *CalendarAdapter) OAuthConfigForAlias(alias string) *oauth2.Config {
	return credential.OAuthConfigForAlias(a.OAuthConfig(), alias)
}

func (a *CalendarAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, calendarScopes, false)
}

func (a *CalendarAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
}

// FetchIdentity returns the Google account email for auto-alias detection.
func (a *CalendarAdapter) FetchIdentity(ctx context.Context, credBytes []byte, config map[string]string) (string, error) {
	client, err := a.httpClient(ctx, credBytes, config)
	if err != nil {
		return "", err
	}
	return credential.FetchGoogleEmail(ctx, client)
}

func (a *CalendarAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.httpClient(ctx, req.Credential, req.Config)
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
	case "respond_to_event":
		return a.respondToEvent(ctx, client, req.Params)
	case "delete_event":
		return a.deleteEvent(ctx, client, req.Params)
	case "list_calendars":
		return a.listCalendars(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("calendar: unsupported action %q", req.Action)
	}
}

func (a *CalendarAdapter) httpClient(ctx context.Context, credBytes []byte, config map[string]string) (*http.Client, error) {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	oauthConfig := cred.OAuthConfig(
		a.OAuthConfigForAlias(config["_clawvisor_alias"]),
	)
	ts := oauthConfig.TokenSource(ctx, cred.ToOAuth2Token())
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

type calendarAPIDateTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

type calendarAPIAttendee struct {
	Email            string `json:"email"`
	DisplayName      string `json:"displayName"`
	ResponseStatus   string `json:"responseStatus"`
	Comment          string `json:"comment"`
	Self             bool   `json:"self"`
	Organizer        bool   `json:"organizer"`
	Optional         bool   `json:"optional"`
	Resource         bool   `json:"resource"`
	AdditionalGuests int    `json:"additionalGuests"`
}

type calendarAPIAttachment struct {
	FileURL  string `json:"fileUrl"`
	Title    string `json:"title"`
	MimeType string `json:"mimeType"`
	IconLink string `json:"iconLink"`
	FileID   string `json:"fileId"`
}

type calendarAPIEvent struct {
	ID             string                  `json:"id"`
	ETag           string                  `json:"etag"`
	Sequence       int64                   `json:"sequence"`
	Summary        string                  `json:"summary"`
	Location       string                  `json:"location"`
	Description    string                  `json:"description"`
	Status         string                  `json:"status"`
	Transparency   string                  `json:"transparency"`
	Visibility     string                  `json:"visibility"`
	HangoutLink    string                  `json:"hangoutLink"`
	Start          calendarAPIDateTime     `json:"start"`
	End            calendarAPIDateTime     `json:"end"`
	Attendees      []calendarAPIAttendee   `json:"attendees"`
	Attachments    []calendarAPIAttachment `json:"attachments"`
	RecurringEvent string                  `json:"recurringEventId"`
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
	if pt, _ := params["page_token"].(string); pt != "" {
		q.Set("pageToken", pt)
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
		NextPageToken string `json:"nextPageToken"`
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
	result := &adapters.Result{Summary: summary, Data: events}
	if resp.NextPageToken != "" {
		result.Meta = map[string]any{"next_page_token": resp.NextPageToken}
	}
	return result, nil
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

	var item calendarAPIEvent
	headers, err := apiGETWithHeaders(ctx, client, apiURL, &item)
	if err != nil {
		return nil, fmt.Errorf("calendar get_event: %w", err)
	}
	version := eventVersion(item.ETag, headers.Get("ETag"))
	if version == "" {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("calendar get_event: provider returned no event etag/version"),
		}
	}

	event := calendarEventData(item, version)
	summary, _ := event["summary"].(string)
	return &adapters.Result{
		Summary: format.Summary("Event: %s", summary),
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
		ID       string `json:"id"`
		Summary  string `json:"summary"`
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
	expectedVersion, err := expectedEventVersion(params)
	if err != nil {
		return nil, fmt.Errorf("calendar update_event: %w", err)
	}
	sendUpdates, err := calendarSendUpdates(params)
	if err != nil {
		return nil, fmt.Errorf("calendar update_event: %w", err)
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
	if v, ok := params["location"].(string); ok {
		patch["location"] = v
	}
	if v, ok := params["start"].(string); ok {
		patch["start"] = calendarDtField(v)
	}
	if v, ok := params["end"].(string); ok {
		patch["end"] = calendarDtField(v)
	}
	if raw, ok := params["attendees"]; ok {
		attendees, attendeeErr := calendarAttendeePatch(raw)
		if attendeeErr != nil {
			return nil, fmt.Errorf("calendar update_event: %w", attendeeErr)
		}
		patch["attendees"] = attendees
	}
	if v, ok := params["transparency"].(string); ok {
		patch["transparency"] = v
	}
	if v, ok := params["visibility"].(string); ok {
		patch["visibility"] = v
	}
	if len(patch) == 0 {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("at least one event field is required"),
		}
	}

	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events/%s",
		url.PathEscape(calendarID), url.PathEscape(eventID))
	q := url.Values{}
	q.Set("sendUpdates", sendUpdates)
	apiURL += "?" + q.Encode()

	var updated calendarAPIEvent
	headers, err := apiConditionalWrite(ctx, client, http.MethodPatch, apiURL, expectedVersion, patch, &updated)
	if err != nil {
		return nil, fmt.Errorf("calendar update_event: %w", err)
	}
	updatedVersion := eventVersion(updated.ETag, headers.Get("ETag"))
	if updatedVersion == "" {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureAmbiguous,
			Err:  fmt.Errorf("calendar update_event: provider confirmed the mutation but returned no event etag/version"),
		}
	}
	if updated.ID == "" {
		updated.ID = eventID
	}
	return &adapters.Result{
		Summary: format.Summary("Updated event: %s", updated.Summary),
		Data: map[string]any{
			"event_id":     updated.ID,
			"summary":      format.SanitizeText(updated.Summary, format.MaxFieldLen),
			"etag":         updatedVersion,
			"version":      updatedVersion,
			"send_updates": sendUpdates,
		},
	}, nil
}

// ── respond_to_event ──────────────────────────────────────────────────────────

func (a *CalendarAdapter) respondToEvent(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	eventID, _ := params["event_id"].(string)
	if eventID == "" {
		return nil, fmt.Errorf("calendar respond_to_event: event_id is required")
	}
	responseStatus, _ := params["response_status"].(string)
	switch responseStatus {
	case "accepted", "declined", "tentative":
	default:
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("calendar RSVP must be accepted, declined, or tentative"),
		}
	}
	expectedVersion, err := expectedEventVersion(params)
	if err != nil {
		return nil, fmt.Errorf("calendar respond_to_event: %w", err)
	}
	sendUpdates, err := calendarSendUpdates(params)
	if err != nil {
		return nil, fmt.Errorf("calendar respond_to_event: %w", err)
	}

	calendarID := "primary"
	if v, ok := params["calendar_id"].(string); ok && v != "" {
		calendarID = v
	}
	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events/%s",
		url.PathEscape(calendarID), url.PathEscape(eventID))

	// Google identifies the attendee that represents this calendar with
	// attendees[].self. Read that participant immediately before the write,
	// then retain the caller's original ETag as an If-Match precondition on
	// the PATCH below.
	var current calendarAPIEvent
	headers, err := apiGETWithHeaders(ctx, client, apiURL, &current)
	if err != nil {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("calendar respond_to_event pre-read: %w", err),
		}
	}
	currentVersion := eventVersion(current.ETag, headers.Get("ETag"))
	if currentVersion == "" {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("calendar respond_to_event: provider returned no current event etag/version"),
		}
	}
	if currentVersion != expectedVersion {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureStaleVersion,
			Err:  fmt.Errorf("calendar respond_to_event: event version is stale"),
		}
	}
	participant, ok := calendarSelfAttendee(current.Attendees)
	if !ok || participant.Email == "" {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("calendar respond_to_event: authenticated calendar is not an attendee"),
		}
	}

	payload := map[string]any{
		"attendees": []map[string]any{{
			"email":          participant.Email,
			"responseStatus": responseStatus,
		}},
		// This Google event flag makes the partial attendee list update only
		// this calendar participant's response instead of replacing guests.
		"attendeesOmitted": true,
	}
	q := url.Values{}
	q.Set("sendUpdates", sendUpdates)
	mutationURL := apiURL + "?" + q.Encode()

	var updated calendarAPIEvent
	headers, err = apiConditionalWrite(ctx, client, http.MethodPatch, mutationURL, expectedVersion, payload, &updated)
	if err != nil {
		return nil, fmt.Errorf("calendar respond_to_event: %w", err)
	}
	updatedVersion := eventVersion(updated.ETag, headers.Get("ETag"))
	if updatedVersion == "" {
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureAmbiguous,
			Err:  fmt.Errorf("calendar respond_to_event: provider confirmed the mutation but returned no event etag/version"),
		}
	}
	if updated.ID == "" {
		updated.ID = eventID
	}
	return &adapters.Result{
		Summary: format.Summary("Responded %s to event %s", responseStatus, updated.ID),
		Data: map[string]any{
			"event_id":        updated.ID,
			"response_status": responseStatus,
			"etag":            updatedVersion,
			"version":         updatedVersion,
			"send_updates":    sendUpdates,
		},
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

func (a *CalendarAdapter) listCalendars(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	maxResults := 50
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 250 {
		maxResults = v
	}
	q.Set("maxResults", fmt.Sprintf("%d", maxResults))
	if pt, _ := params["page_token"].(string); pt != "" {
		q.Set("pageToken", pt)
	}

	apiURL := "https://www.googleapis.com/calendar/v3/users/me/calendarList?" + q.Encode()
	var resp struct {
		Items []struct {
			ID      string `json:"id"`
			Summary string `json:"summary"`
			Primary bool   `json:"primary"`
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
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
	result := &adapters.Result{
		Summary: format.Summary("%d calendar(s)", len(items)),
		Data:    items,
	}
	if resp.NextPageToken != "" {
		result.Meta = map[string]any{"next_page_token": resp.NextPageToken}
	}
	return result, nil
}

func calendarEventData(item calendarAPIEvent, version string) map[string]any {
	start := item.Start.DateTime
	if start == "" {
		start = item.Start.Date
	}
	end := item.End.DateTime
	if end == "" {
		end = item.End.Date
	}

	attendees := make([]map[string]any, 0, len(item.Attendees))
	for _, attendee := range item.Attendees {
		entry := map[string]any{
			"email":          format.SanitizeText(attendee.Email, format.MaxFieldLen),
			"responseStatus": attendee.ResponseStatus,
		}
		if attendee.DisplayName != "" {
			entry["displayName"] = format.SanitizeText(attendee.DisplayName, format.MaxFieldLen)
		}
		if attendee.Comment != "" {
			entry["comment"] = format.SanitizeText(attendee.Comment, format.MaxSnippetLen)
		}
		if attendee.Self {
			entry["self"] = true
		}
		if attendee.Organizer {
			entry["organizer"] = true
		}
		if attendee.Optional {
			entry["optional"] = true
		}
		if attendee.Resource {
			entry["resource"] = true
		}
		if attendee.AdditionalGuests != 0 {
			entry["additionalGuests"] = attendee.AdditionalGuests
		}
		attendees = append(attendees, entry)
	}

	attachments := make([]map[string]any, 0, len(item.Attachments))
	for _, attachment := range item.Attachments {
		attachments = append(attachments, map[string]any{
			"fileUrl":  attachment.FileURL,
			"title":    format.SanitizeText(attachment.Title, format.MaxFieldLen),
			"mimeType": attachment.MimeType,
			"iconLink": attachment.IconLink,
			"fileId":   attachment.FileID,
		})
	}

	transparency := item.Transparency
	if transparency == "" {
		transparency = "opaque"
	}
	visibility := item.Visibility
	if visibility == "" {
		visibility = "default"
	}
	data := map[string]any{
		"id":           item.ID,
		"summary":      format.SanitizeText(item.Summary, format.MaxFieldLen),
		"start":        start,
		"end":          end,
		"location":     format.SanitizeText(item.Location, format.MaxFieldLen),
		"description":  format.SanitizeText(item.Description, format.MaxBodyLen),
		"attendees":    attendees,
		"attachments":  attachments,
		"hangoutLink":  item.HangoutLink,
		"transparency": transparency,
		"visibility":   visibility,
		"status":       item.Status,
		"etag":         version,
		"version":      version,
		"sequence":     item.Sequence,
	}
	if item.RecurringEvent != "" {
		data["recurringEventId"] = item.RecurringEvent
	}
	for _, attendee := range item.Attendees {
		if attendee.Self && attendee.ResponseStatus != "" {
			data["response_status"] = attendee.ResponseStatus
			data["responseStatus"] = attendee.ResponseStatus
			break
		}
	}
	return data
}

func eventVersion(bodyETag, headerETag string) string {
	if version := strings.TrimSpace(bodyETag); version != "" && version != "*" {
		return version
	}
	if version := strings.TrimSpace(headerETag); version != "" && version != "*" {
		return version
	}
	return ""
}

func expectedEventVersion(params map[string]any) (string, error) {
	var versions []string
	for _, key := range []string{"expected_etag", "expected_version"} {
		raw, exists := params[key]
		if !exists {
			continue
		}
		version, ok := raw.(string)
		if !ok {
			return "", &adapters.ExecutionFailure{
				Kind: adapters.ExecutionFailureDefinite,
				Err:  fmt.Errorf("%s must be a string", key),
			}
		}
		version = strings.TrimSpace(version)
		if version == "" || version == "*" || strings.ContainsAny(version, "\r\n") {
			return "", &adapters.ExecutionFailure{
				Kind: adapters.ExecutionFailureDefinite,
				Err:  fmt.Errorf("%s must contain a concrete provider version", key),
			}
		}
		versions = append(versions, version)
	}
	if len(versions) == 0 {
		return "", &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("expected_etag (or expected_version) is required"),
		}
	}
	if len(versions) == 2 && versions[0] != versions[1] {
		return "", &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("expected_etag and expected_version disagree"),
		}
	}
	return versions[0], nil
}

func calendarSendUpdates(params map[string]any) (string, error) {
	raw, exists := params["send_updates"]
	if !exists {
		return "none", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("send_updates must be a string"),
		}
	}
	value = strings.TrimSpace(value)
	switch value {
	case "all", "externalOnly", "none":
		return value, nil
	default:
		return "", &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("send_updates must be all, externalOnly, or none"),
		}
	}
}

func calendarAttendeePatch(raw any) ([]map[string]string, error) {
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []string:
		values = make([]any, len(typed))
		for i, value := range typed {
			values[i] = value
		}
	default:
		return nil, &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("attendees must be an array of email strings"),
		}
	}
	attendees := make([]map[string]string, 0, len(values))
	for _, rawValue := range values {
		var email string
		switch attendee := rawValue.(type) {
		case string:
			email = attendee
		case map[string]any:
			email, _ = attendee["email"].(string)
		case map[string]string:
			email = attendee["email"]
		}
		email = strings.TrimSpace(email)
		if email == "" {
			return nil, &adapters.ExecutionFailure{
				Kind: adapters.ExecutionFailureDefinite,
				Err:  fmt.Errorf("attendees must contain non-empty email strings or email objects"),
			}
		}
		attendees = append(attendees, map[string]string{"email": email})
	}
	return attendees, nil
}

func calendarSelfAttendee(attendees []calendarAPIAttendee) (calendarAPIAttendee, bool) {
	for _, attendee := range attendees {
		if attendee.Self {
			return attendee, true
		}
	}
	return calendarAPIAttendee{}, false
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func apiGET(ctx context.Context, client *http.Client, apiURL string, out any) error {
	_, err := apiGETWithHeaders(ctx, client, apiURL, out)
	return err
}

func apiGETWithHeaders(ctx context.Context, client *http.Client, apiURL string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return resp.Header.Clone(), fmt.Errorf("status %d: %s", resp.StatusCode, format.Truncate(string(body), 200))
	}
	if readErr != nil {
		return resp.Header.Clone(), readErr
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.Header.Clone(), err
	}
	return resp.Header.Clone(), nil
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
		return fmt.Errorf("status %d: %s", resp.StatusCode, format.Truncate(string(body), 200))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

func apiConditionalWrite(
	ctx context.Context,
	client *http.Client,
	method, apiURL, expectedVersion string,
	payload any,
	out any,
) (http.Header, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, &adapters.ExecutionFailure{Kind: adapters.ExecutionFailureDefinite, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, &adapters.ExecutionFailure{Kind: adapters.ExecutionFailureDefinite, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", expectedVersion)

	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, &adapters.ExecutionFailure{
			Kind:     adapters.ExecutionFailureAmbiguous,
			TimedOut: isTimeoutError(err),
			Err:      fmt.Errorf("provider mutation transport failed: %w", err),
		}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		return resp.Header.Clone(), &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureStaleVersion,
			Err:  fmt.Errorf("provider rejected stale event version (status %d): %s", resp.StatusCode, format.Truncate(string(body), 200)),
		}
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= http.StatusInternalServerError:
		return resp.Header.Clone(), &adapters.ExecutionFailure{
			Kind:     adapters.ExecutionFailureAmbiguous,
			TimedOut: resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusGatewayTimeout,
			Err:      fmt.Errorf("provider mutation outcome is ambiguous (status %d): %s", resp.StatusCode, format.Truncate(string(body), 200)),
		}
	case resp.StatusCode >= http.StatusBadRequest:
		return resp.Header.Clone(), &adapters.ExecutionFailure{
			Kind: adapters.ExecutionFailureDefinite,
			Err:  fmt.Errorf("provider rejected mutation (status %d): %s", resp.StatusCode, format.Truncate(string(body), 200)),
		}
	case readErr != nil:
		return resp.Header.Clone(), &adapters.ExecutionFailure{
			Kind:     adapters.ExecutionFailureAmbiguous,
			TimedOut: isTimeoutError(readErr),
			Err:      fmt.Errorf("provider mutation response was incomplete: %w", readErr),
		}
	}

	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.Header.Clone(), &adapters.ExecutionFailure{
				Kind: adapters.ExecutionFailureAmbiguous,
				Err:  fmt.Errorf("provider confirmed mutation but returned an invalid response: %w", err),
			}
		}
	}
	return resp.Header.Clone(), nil
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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
