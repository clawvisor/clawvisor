package sheets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

const (
	sheetsAPIBase = "https://sheets.googleapis.com/v4"
	driveAPIBase  = "https://www.googleapis.com/drive/v3"
)

// driveQueryAllowlist permits only characters that appear in ordinary file
// names: letters, digits, whitespace, hyphens, dots, underscores, commas,
// parentheses, ampersands, @, !, #, +.  Drive query operators (quotes,
// backslashes, =, <, >, and/or keywords, etc.) are intentionally excluded.
var driveQueryAllowlist = regexp.MustCompile(`^[a-zA-Z0-9\s\-._,()&@!#+]+$`)

// escapeDriveLiteral doubles single quotes so a user-provided string can be
// safely embedded inside Drive's single-quoted 'name contains' operand.
// This is defense-in-depth: driveQueryAllowlist already rejects '.
func escapeDriveLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

type spreadsheetListItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ModifiedTime string `json:"modified_time"`
	WebViewLink  string `json:"web_view_link,omitempty"`
}

func (a *SheetsAdapter) listSpreadsheets(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	// Implemented via Drive API because Sheets API has no "list".
	query, _ := params["query"].(string)
	maxResults := 10
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("pageSize", fmt.Sprintf("%d", maxResults))
	q.Set("fields", "files(id,name,modifiedTime,webViewLink)")

	// Default query: spreadsheets only, not trashed.
	driveQ := "mimeType='application/vnd.google-apps.spreadsheet' and trashed=false"
	if strings.TrimSpace(query) != "" {
		if !driveQueryAllowlist.MatchString(query) {
			return nil, fmt.Errorf("sheets list_spreadsheets: query contains disallowed characters; use only letters, digits, spaces, and common punctuation (- . , ( ) & @ ! # +)")
		}
		// Escape embedded single quotes in the user-provided query.
		// Drive API uses single quotes to delimit string values inside
		// 'name contains'. This is defense-in-depth: the query has already
		// been validated against driveQueryAllowlist, which excludes '.
		// Adding the escape here guards against a future allowlist widening.
		driveQ += fmt.Sprintf(" and name contains '%s'", escapeDriveLiteral(query))
	}
	q.Set("q", driveQ)

	apiURL := driveAPIBase + "/files?" + q.Encode()
	var resp struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ModifiedTime string `json:"modifiedTime"`
			WebViewLink  string `json:"webViewLink"`
		} `json:"files"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("sheets list_spreadsheets: %w", err)
	}

	items := make([]spreadsheetListItem, 0, len(resp.Files))
	for _, f := range resp.Files {
		items = append(items, spreadsheetListItem{
			ID:           f.ID,
			Name:         format.SanitizeText(f.Name, format.MaxFieldLen),
			ModifiedTime: f.ModifiedTime,
			WebViewLink:  f.WebViewLink,
		})
	}
	return &adapters.Result{Summary: format.Summary("%d spreadsheet(s)", len(items)), Data: items}, nil
}

type spreadsheetMeta struct {
	SpreadsheetID string      `json:"spreadsheet_id"`
	Title         string      `json:"title"`
	Locale        string      `json:"locale,omitempty"`
	Timezone      string      `json:"timezone,omitempty"`
	Sheets        []sheetMeta `json:"sheets"`
}

type sheetMeta struct {
	SheetID int    `json:"sheet_id"`
	Title   string `json:"title"`
}

func (a *SheetsAdapter) getSpreadsheet(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	ssid, err := requireSpreadsheetID(params)
	if err != nil {
		return nil, fmt.Errorf("sheets get_spreadsheet: %w", err)
	}

	u := fmt.Sprintf("%s/spreadsheets/%s?fields=spreadsheetId,properties(title,locale,timeZone),sheets(properties(sheetId,title))",
		sheetsAPIBase, url.PathEscape(ssid))
	var resp struct {
		SpreadsheetID string `json:"spreadsheetId"`
		Properties    struct {
			Title    string `json:"title"`
			Locale   string `json:"locale"`
			TimeZone string `json:"timeZone"`
		} `json:"properties"`
		Sheets []struct {
			Properties struct {
				SheetID int    `json:"sheetId"`
				Title   string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := apiGET(ctx, client, u, &resp); err != nil {
		return nil, fmt.Errorf("sheets get_spreadsheet: %w", err)
	}

	out := spreadsheetMeta{
		SpreadsheetID: resp.SpreadsheetID,
		Title:         format.SanitizeText(resp.Properties.Title, format.MaxFieldLen),
		Locale:        resp.Properties.Locale,
		Timezone:      resp.Properties.TimeZone,
		Sheets:        make([]sheetMeta, 0, len(resp.Sheets)),
	}
	for _, s := range resp.Sheets {
		out.Sheets = append(out.Sheets, sheetMeta{SheetID: s.Properties.SheetID, Title: format.SanitizeText(s.Properties.Title, format.MaxFieldLen)})
	}

	return &adapters.Result{Summary: format.Summary("Spreadsheet: %s", out.Title), Data: out}, nil
}

type rangeValues struct {
	SpreadsheetID string          `json:"spreadsheet_id"`
	Range         string          `json:"range"`
	MajorDim      string          `json:"major_dimension"`
	Values        [][]interface{} `json:"values"`
}

func (a *SheetsAdapter) readRange(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	ssid, err := requireSpreadsheetID(params)
	if err != nil {
		return nil, fmt.Errorf("sheets read_range: %w", err)
	}
	rng, err := requireA1Range(params)
	if err != nil {
		return nil, fmt.Errorf("sheets read_range: %w", err)
	}
	majorDim := "ROWS"
	if v, _ := params["major_dimension"].(string); v != "" {
		majorDim = strings.ToUpper(v)
	}
	if majorDim != "ROWS" && majorDim != "COLUMNS" {
		return nil, fmt.Errorf("major_dimension must be ROWS or COLUMNS")
	}

	q := url.Values{}
	q.Set("majorDimension", majorDim)

	u := fmt.Sprintf("%s/spreadsheets/%s/values/%s?%s",
		sheetsAPIBase,
		url.PathEscape(ssid),
		url.PathEscape(rng),
		q.Encode())

	var resp struct {
		SpreadsheetID  string          `json:"spreadsheetId"`
		Range          string          `json:"range"`
		MajorDimension string          `json:"majorDimension"`
		Values         [][]interface{} `json:"values"`
	}
	if err := apiGET(ctx, client, u, &resp); err != nil {
		return nil, fmt.Errorf("sheets read_range: %w", err)
	}

	values := resp.Values
	if maxRows, ok := paramInt(params, "max_rows"); ok && maxRows > 0 {
		if maxRows > 200 {
			maxRows = 200
		}
		if len(values) > maxRows {
			values = values[:maxRows]
		}
	}

	out := rangeValues{SpreadsheetID: resp.SpreadsheetID, Range: resp.Range, MajorDim: resp.MajorDimension, Values: values}

	rows := len(out.Values)
	summary := format.Summary("Read %d row(s) from %s", rows, format.SanitizeText(resp.Range, format.MaxFieldLen))
	return &adapters.Result{Summary: summary, Data: out}, nil
}

type appendResult struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	TableRange    string `json:"table_range"`
	UpdatedRange  string `json:"updated_range"`
	UpdatedRows   int    `json:"updated_rows"`
}

func (a *SheetsAdapter) appendRows(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	ssid, err := requireSpreadsheetID(params)
	if err != nil {
		return nil, fmt.Errorf("sheets append_rows: %w", err)
	}
	rng, err := requireA1Range(params)
	if err != nil {
		return nil, fmt.Errorf("sheets append_rows: %w", err)
	}
	rows, err := require2DValues(params, "rows")
	if err != nil {
		return nil, fmt.Errorf("sheets append_rows: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("rows must not be empty")
	}
	if len(rows) > 200 {
		return nil, fmt.Errorf("too many rows (max 200)")
	}

	valueInput := "USER_ENTERED"
	if v, _ := params["value_input_option"].(string); v != "" {
		valueInput = strings.ToUpper(v)
	}
	if valueInput != "USER_ENTERED" && valueInput != "RAW" {
		return nil, fmt.Errorf("value_input_option must be USER_ENTERED or RAW")
	}

	q := url.Values{}
	q.Set("valueInputOption", valueInput)
	q.Set("insertDataOption", "INSERT_ROWS")

	payload := map[string]any{
		"majorDimension": "ROWS",
		"values":         rows,
	}

	u := fmt.Sprintf("%s/spreadsheets/%s/values/%s:append?%s",
		sheetsAPIBase,
		url.PathEscape(ssid),
		url.PathEscape(rng),
		q.Encode())

	var resp struct {
		SpreadsheetID string `json:"spreadsheetId"`
		TableRange    string `json:"tableRange"`
		Updates       struct {
			UpdatedRange string `json:"updatedRange"`
			UpdatedRows  int    `json:"updatedRows"`
		} `json:"updates"`
	}
	if err := apiPOST(ctx, client, u, payload, &resp); err != nil {
		return nil, fmt.Errorf("sheets append_rows: %w", err)
	}

	out := appendResult{SpreadsheetID: resp.SpreadsheetID, TableRange: resp.TableRange, UpdatedRange: resp.Updates.UpdatedRange, UpdatedRows: resp.Updates.UpdatedRows}
	return &adapters.Result{Summary: format.Summary("Appended %d row(s) to %s", len(rows), rangeSheetLabel(rng)), Data: out}, nil
}

type updateResult struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	UpdatedRange  string `json:"updated_range"`
	UpdatedRows   int    `json:"updated_rows"`
	UpdatedCols   int    `json:"updated_columns"`
	UpdatedCells  int    `json:"updated_cells"`
}

func (a *SheetsAdapter) updateCells(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	ssid, err := requireSpreadsheetID(params)
	if err != nil {
		return nil, fmt.Errorf("sheets update_cells: %w", err)
	}
	rng, err := requireA1Range(params)
	if err != nil {
		return nil, fmt.Errorf("sheets update_cells: %w", err)
	}
	values, err := require2DValues(params, "values")
	if err != nil {
		return nil, fmt.Errorf("sheets update_cells: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("values must not be empty")
	}
	if len(values) > 200 {
		return nil, fmt.Errorf("too many rows (max 200)")
	}

	valueInput := "USER_ENTERED"
	if v, _ := params["value_input_option"].(string); v != "" {
		valueInput = strings.ToUpper(v)
	}
	if valueInput != "USER_ENTERED" && valueInput != "RAW" {
		return nil, fmt.Errorf("value_input_option must be USER_ENTERED or RAW")
	}

	q := url.Values{}
	q.Set("valueInputOption", valueInput)

	payload := map[string]any{
		"majorDimension": "ROWS",
		"values":         values,
	}

	u := fmt.Sprintf("%s/spreadsheets/%s/values/%s?%s",
		sheetsAPIBase,
		url.PathEscape(ssid),
		url.PathEscape(rng),
		q.Encode())

	var resp struct {
		SpreadsheetID  string `json:"spreadsheetId"`
		UpdatedRange   string `json:"updatedRange"`
		UpdatedRows    int    `json:"updatedRows"`
		UpdatedColumns int    `json:"updatedColumns"`
		UpdatedCells   int    `json:"updatedCells"`
	}
	if err := apiPUT(ctx, client, u, payload, &resp); err != nil {
		return nil, fmt.Errorf("sheets update_cells: %w", err)
	}

	out := updateResult{SpreadsheetID: resp.SpreadsheetID, UpdatedRange: resp.UpdatedRange, UpdatedRows: resp.UpdatedRows, UpdatedCols: resp.UpdatedColumns, UpdatedCells: resp.UpdatedCells}
	return &adapters.Result{Summary: format.Summary("Updated %s", format.SanitizeText(resp.UpdatedRange, format.MaxFieldLen)), Data: out}, nil
}

type createResult struct {
	SpreadsheetID  string `json:"spreadsheet_id"`
	Title          string `json:"title"`
	SpreadsheetURL string `json:"spreadsheet_url,omitempty"`
}

func (a *SheetsAdapter) createSpreadsheet(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	title, _ := params["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}
	if len(title) > 200 {
		return nil, fmt.Errorf("title too long")
	}

	// Optional: sheets: ["Sheet1", "Sheet2"]
	var sheetTitles []string
	if raw, ok := params["sheets"]; ok {
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("sheets must be an array of strings")
		}
		for _, v := range arr {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("sheets must be an array of strings")
			}
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if len(s) > 100 {
				return nil, fmt.Errorf("sheet name too long")
			}
			sheetTitles = append(sheetTitles, s)
		}
		if len(sheetTitles) > 20 {
			return nil, fmt.Errorf("too many sheets (max 20)")
		}
	}

	payload := map[string]any{"properties": map[string]any{"title": title}}
	if len(sheetTitles) > 0 {
		sheets := make([]map[string]any, 0, len(sheetTitles))
		for _, s := range sheetTitles {
			sheets = append(sheets, map[string]any{"properties": map[string]any{"title": s}})
		}
		payload["sheets"] = sheets
	}

	u := sheetsAPIBase + "/spreadsheets"
	var resp struct {
		SpreadsheetID  string `json:"spreadsheetId"`
		SpreadsheetURL string `json:"spreadsheetUrl"`
		Properties     struct {
			Title string `json:"title"`
		} `json:"properties"`
	}
	if err := apiPOST(ctx, client, u, payload, &resp); err != nil {
		return nil, fmt.Errorf("sheets create_spreadsheet: %w", err)
	}

	out := createResult{SpreadsheetID: resp.SpreadsheetID, Title: format.SanitizeText(resp.Properties.Title, format.MaxFieldLen), SpreadsheetURL: resp.SpreadsheetURL}
	return &adapters.Result{Summary: format.Summary("Created spreadsheet: %s", out.Title), Data: out}, nil
}

// HTTP helpers

func apiGET(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiPOST(ctx context.Context, client *http.Client, url string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiPUT(ctx context.Context, client *http.Client, url string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
