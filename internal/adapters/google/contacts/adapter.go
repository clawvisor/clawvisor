// Package contacts implements the Clawvisor adapter for Google Contacts (People API).
package contacts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
)

const serviceID = "google.contacts"

// contactsScopes are the OAuth scopes required by the Contacts adapter.
var contactsScopes = []string{
	"https://www.googleapis.com/auth/contacts.readonly",
}

// ContactsAdapter implements adapters.Adapter for Google Contacts.
// It also implements adapters.ContactsChecker to support the recipient_in_contacts policy condition.
type ContactsAdapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

func New(provider adapters.OAuthCredentialProvider) *ContactsAdapter {
	return &ContactsAdapter{oauthProvider: provider}
}

func (a *ContactsAdapter) ServiceID() string { return serviceID }

func (a *ContactsAdapter) SupportedActions() []string {
	return []string{"list_contacts", "get_contact", "search_contacts"}
}

func (a *ContactsAdapter) RequiredScopes() []string { return contactsScopes }

func (a *ContactsAdapter) OAuthConfig() *oauth2.Config {
	clientID, clientSecret := a.oauthProvider.OAuthClientCredentials()
	if clientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       contactsScopes,
		Endpoint:     google.Endpoint,
	}
}

func (a *ContactsAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, contactsScopes)
}

func (a *ContactsAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
}

func (a *ContactsAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.httpClient(ctx, req.Credential)
	if err != nil {
		return nil, err
	}
	switch req.Action {
	case "list_contacts":
		return a.listContacts(ctx, client, req.Params)
	case "get_contact":
		return a.getContact(ctx, client, req.Params)
	case "search_contacts":
		return a.searchContacts(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("contacts: unsupported action %q", req.Action)
	}
}

// IsInContacts checks if the given email address appears in the user's Google Contacts.
// Used by the gateway handler to pre-resolve the recipient_in_contacts policy condition.
func (a *ContactsAdapter) IsInContacts(ctx context.Context, cred []byte, email string) (bool, error) {
	if email == "" {
		return false, nil
	}
	client, err := a.httpClient(ctx, cred)
	if err != nil {
		return false, err
	}

	q := url.Values{}
	q.Set("query", email)
	q.Set("readMask", "emailAddresses")
	q.Set("pageSize", "5")

	apiURL := "https://people.googleapis.com/v1/people:searchContacts?" + q.Encode()
	var resp struct {
		Results []struct {
			Person struct {
				EmailAddresses []struct {
					Value string `json:"value"`
				} `json:"emailAddresses"`
			} `json:"person"`
		} `json:"results"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return false, err
	}

	emailLower := strings.ToLower(email)
	for _, r := range resp.Results {
		for _, e := range r.Person.EmailAddresses {
			if strings.ToLower(e.Value) == emailLower {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *ContactsAdapter) httpClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	ts := a.OAuthConfig().TokenSource(ctx, cred.ToOAuth2Token())
	return oauth2.NewClient(ctx, ts), nil
}

// ── list_contacts ─────────────────────────────────────────────────────────────

type contactItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

func (a *ContactsAdapter) listContacts(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	maxResults := 20
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 100 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("pageSize", fmt.Sprintf("%d", maxResults))
	q.Set("personFields", "names,emailAddresses,phoneNumbers")
	q.Set("sortOrder", "LAST_NAME_ASCENDING")

	apiURL := "https://people.googleapis.com/v1/people/me/connections?" + q.Encode()
	var resp struct {
		Connections []struct {
			ResourceName string `json:"resourceName"`
			Names        []struct {
				DisplayName string `json:"displayName"`
			} `json:"names"`
			EmailAddresses []struct {
				Value string `json:"value"`
			} `json:"emailAddresses"`
			PhoneNumbers []struct {
				Value string `json:"value"`
			} `json:"phoneNumbers"`
		} `json:"connections"`
		TotalPeople int `json:"totalPeople"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("contacts list_contacts: %w", err)
	}

	items := make([]contactItem, 0, len(resp.Connections))
	for _, c := range resp.Connections {
		item := contactItem{ID: c.ResourceName}
		if len(c.Names) > 0 {
			item.Name = format.SanitizeText(c.Names[0].DisplayName, format.MaxFieldLen)
		}
		if len(c.EmailAddresses) > 0 {
			item.Email = c.EmailAddresses[0].Value
		}
		if len(c.PhoneNumbers) > 0 {
			item.Phone = c.PhoneNumbers[0].Value
		}
		items = append(items, item)
	}
	return &adapters.Result{
		Summary: format.Summary("%d contact(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_contact ───────────────────────────────────────────────────────────────

func (a *ContactsAdapter) getContact(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	contactID, _ := params["contact_id"].(string)
	if contactID == "" {
		return nil, fmt.Errorf("contacts get_contact: contact_id is required")
	}

	apiURL := fmt.Sprintf("https://people.googleapis.com/v1/%s?personFields=names,emailAddresses,phoneNumbers,organizations,biographies",
		url.PathEscape(contactID))

	var c struct {
		ResourceName string `json:"resourceName"`
		Names        []struct {
			DisplayName string `json:"displayName"`
		} `json:"names"`
		EmailAddresses []struct {
			Value string `json:"value"`
			Type  string `json:"type"`
		} `json:"emailAddresses"`
		PhoneNumbers []struct {
			Value string `json:"value"`
			Type  string `json:"type"`
		} `json:"phoneNumbers"`
		Organizations []struct {
			Name  string `json:"name"`
			Title string `json:"title"`
		} `json:"organizations"`
	}
	if err := apiGET(ctx, client, apiURL, &c); err != nil {
		return nil, fmt.Errorf("contacts get_contact: %w", err)
	}

	result := map[string]any{"id": c.ResourceName}
	if len(c.Names) > 0 {
		result["name"] = format.SanitizeText(c.Names[0].DisplayName, format.MaxFieldLen)
	}
	if len(c.EmailAddresses) > 0 {
		emails := make([]map[string]string, 0, len(c.EmailAddresses))
		for _, e := range c.EmailAddresses {
			emails = append(emails, map[string]string{"value": e.Value, "type": e.Type})
		}
		result["email_addresses"] = emails
	}
	if len(c.PhoneNumbers) > 0 {
		phones := make([]map[string]string, 0, len(c.PhoneNumbers))
		for _, p := range c.PhoneNumbers {
			phones = append(phones, map[string]string{"value": p.Value, "type": p.Type})
		}
		result["phone_numbers"] = phones
	}
	if len(c.Organizations) > 0 {
		result["organization"] = c.Organizations[0].Name
		result["title"] = c.Organizations[0].Title
	}

	name := contactID
	if n, ok := result["name"].(string); ok {
		name = n
	}
	return &adapters.Result{
		Summary: format.Summary("Contact: %s", name),
		Data:    result,
	}, nil
}

// ── search_contacts ───────────────────────────────────────────────────────────

func (a *ContactsAdapter) searchContacts(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("contacts search_contacts: query is required")
	}
	maxResults := 10
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("query", query)
	q.Set("readMask", "names,emailAddresses,phoneNumbers")
	q.Set("pageSize", fmt.Sprintf("%d", maxResults))

	apiURL := "https://people.googleapis.com/v1/people:searchContacts?" + q.Encode()
	var resp struct {
		Results []struct {
			Person struct {
				ResourceName string `json:"resourceName"`
				Names        []struct {
					DisplayName string `json:"displayName"`
				} `json:"names"`
				EmailAddresses []struct {
					Value string `json:"value"`
				} `json:"emailAddresses"`
				PhoneNumbers []struct {
					Value string `json:"value"`
				} `json:"phoneNumbers"`
			} `json:"person"`
		} `json:"results"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("contacts search_contacts: %w", err)
	}

	items := make([]contactItem, 0, len(resp.Results))
	for _, r := range resp.Results {
		p := r.Person
		item := contactItem{ID: p.ResourceName}
		if len(p.Names) > 0 {
			item.Name = format.SanitizeText(p.Names[0].DisplayName, format.MaxFieldLen)
		}
		if len(p.EmailAddresses) > 0 {
			item.Email = p.EmailAddresses[0].Value
		}
		if len(p.PhoneNumbers) > 0 {
			item.Phone = p.PhoneNumbers[0].Value
		}
		items = append(items, item)
	}
	return &adapters.Result{
		Summary: format.Summary("%d contact(s) matching %q", len(items), query),
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
