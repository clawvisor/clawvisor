package sheets

import "github.com/clawvisor/clawvisor/pkg/adapters"

// ServiceMetadata provides display + risk metadata for the Google Sheets adapter.
func (a *SheetsAdapter) ServiceMetadata() adapters.ServiceMetadata {
	maxResults := 50
	maxRows := 200

	return adapters.ServiceMetadata{
		DisplayName:   "Google Sheets",
		Description:   "Read and write Google Sheets spreadsheets",
		IconURL:       "/logos/google-sheets.svg",
		VaultKey:      "google",
		OAuthEndpoint: "google",
		AutoIdentity:  true,
		ActionMeta: map[string]adapters.ActionMeta{
			"list_spreadsheets": {
				DisplayName: "List spreadsheets",
				Category:    "read",
				Sensitivity: "low",
				Description: "List the user's spreadsheets (via Google Drive)",
				Params: []adapters.ParamMeta{
					{Name: "query", Type: "string"},
					{Name: "max_results", Type: "int", Default: 10, Max: &maxResults},
				},
			},
			"get_spreadsheet": {
				DisplayName: "Get spreadsheet",
				Category:    "read",
				Sensitivity: "low",
				Description: "Fetch spreadsheet metadata (sheets, named ranges)",
				Params: []adapters.ParamMeta{
					{Name: "spreadsheet_id", Type: "string", Required: true},
				},
			},
			"read_range": {
				DisplayName: "Read range",
				Category:    "read",
				Sensitivity: "low",
				Description: "Read values from a range such as Sheet1!A1:D10",
				Params: []adapters.ParamMeta{
					{Name: "spreadsheet_id", Type: "string", Required: true},
					{Name: "range", Type: "string", Required: true},
					{Name: "major_dimension", Type: "string", Default: "ROWS"},
					{Name: "max_rows", Type: "int", Default: 200, Max: &maxRows},
				},
			},
			"append_rows": {
				DisplayName: "Append rows",
				Category:    "write",
				Sensitivity: "medium",
				Description: "Append rows to a sheet range (adds rows after the last row)",
				Params: []adapters.ParamMeta{
					{Name: "spreadsheet_id", Type: "string", Required: true},
					{Name: "range", Type: "string", Required: true},
					{Name: "rows", Type: "array", Required: true},
					{Name: "value_input_option", Type: "string", Default: "USER_ENTERED"},
				},
			},
			"update_cells": {
				DisplayName: "Update cells",
				Category:    "write",
				Sensitivity: "medium",
				Description: "Write values to a range (overwrites existing cells)",
				Params: []adapters.ParamMeta{
					{Name: "spreadsheet_id", Type: "string", Required: true},
					{Name: "range", Type: "string", Required: true},
					{Name: "values", Type: "array", Required: true},
					{Name: "value_input_option", Type: "string", Default: "USER_ENTERED"},
				},
			},
			"create_spreadsheet": {
				DisplayName: "Create spreadsheet",
				Category:    "write",
				Sensitivity: "medium",
				Description: "Create a new spreadsheet",
				Params: []adapters.ParamMeta{
					{Name: "title", Type: "string", Required: true},
					{Name: "sheets", Type: "array"},
				},
			},
		},
		VerificationHints: "Google Sheets actions that write data (append_rows, update_cells, create_spreadsheet) should be verified carefully: confirm the spreadsheet_id and range refer to the intended document and that row values do not contain secrets or unintended PII.",
	}
}
