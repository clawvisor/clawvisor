// Package sheets implements the Clawvisor adapter for Google Sheets.
package sheets

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

const serviceID = "google.sheets"

// SheetsAdapter implements adapters.Adapter for Google Sheets.
type SheetsAdapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

func New(provider adapters.OAuthCredentialProvider) *SheetsAdapter {
	return &SheetsAdapter{oauthProvider: provider}
}

func (a *SheetsAdapter) ServiceID() string { return serviceID }

func (a *SheetsAdapter) SupportedActions() []string {
	return []string{
		"list_spreadsheets",
		"get_spreadsheet",
		"read_range",
		"append_rows",
		"update_cells",
		"create_spreadsheet",
	}
}

func (a *SheetsAdapter) RequiredScopes() []string { return sheetsScopes }

func (a *SheetsAdapter) OAuthConfig() *oauth2.Config {
	if a.oauthProvider == nil {
		return nil
	}
	clientID, clientSecret := a.oauthProvider.OAuthClientCredentials()
	if clientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       sheetsScopes,
		Endpoint:     google.Endpoint,
	}
}

func (a *SheetsAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, sheetsScopes, false)
}

func (a *SheetsAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
}

// FetchIdentity returns the Google account email for auto-alias detection.
func (a *SheetsAdapter) FetchIdentity(ctx context.Context, credBytes []byte, _ map[string]string) (string, error) {
	client, err := a.httpClient(ctx, credBytes)
	if err != nil {
		return "", err
	}
	return credential.FetchGoogleEmail(ctx, client)
}

func (a *SheetsAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.httpClient(ctx, req.Credential)
	if err != nil {
		return nil, err
	}

	switch req.Action {
	case "list_spreadsheets":
		return a.listSpreadsheets(ctx, client, req.Params)
	case "get_spreadsheet":
		return a.getSpreadsheet(ctx, client, req.Params)
	case "read_range":
		return a.readRange(ctx, client, req.Params)
	case "append_rows":
		return a.appendRows(ctx, client, req.Params)
	case "update_cells":
		return a.updateCells(ctx, client, req.Params)
	case "create_spreadsheet":
		return a.createSpreadsheet(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("sheets: unsupported action %q", req.Action)
	}
}

func (a *SheetsAdapter) httpClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("sheets: %w", err)
	}
	cfg := a.OAuthConfig()
	if cfg == nil {
		return nil, fmt.Errorf("sheets: OAuth client credentials not configured")
	}
	ts := cfg.TokenSource(ctx, cred.ToOAuth2Token())
	return oauth2.NewClient(ctx, ts), nil
}
