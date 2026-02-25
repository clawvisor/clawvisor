// Package stripe implements the Clawvisor adapter for Stripe.
package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/adapters/format"
)

const serviceID = "stripe"

// storedCredential holds a Stripe API key.
type storedCredential struct {
	Type  string `json:"type"`  // "api_key"
	Token string `json:"token"` // e.g. "sk_..." or "rk_..."
}

// StripeAdapter implements adapters.Adapter for Stripe.
type StripeAdapter struct{}

func New() *StripeAdapter { return &StripeAdapter{} }

func (a *StripeAdapter) ServiceID() string { return serviceID }

func (a *StripeAdapter) SupportedActions() []string {
	return []string{
		"list_customers", "get_customer",
		"list_charges", "get_charge",
		"list_subscriptions", "get_subscription",
		"create_refund", "get_balance",
	}
}

func (a *StripeAdapter) OAuthConfig() *oauth2.Config                         { return nil }
func (a *StripeAdapter) RequiredScopes() []string                            { return nil }
func (a *StripeAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("stripe: OAuth token exchange not supported — use API key activation")
}

func (a *StripeAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("stripe: invalid credential: %w", err)
	}
	if cred.Token == "" {
		return fmt.Errorf("stripe: credential missing token")
	}
	return nil
}

func (a *StripeAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, err
	}
	client := stripeClient(token)

	switch req.Action {
	case "list_customers":
		return a.listCustomers(ctx, client, req.Params)
	case "get_customer":
		return a.getCustomer(ctx, client, req.Params)
	case "list_charges":
		return a.listCharges(ctx, client, req.Params)
	case "get_charge":
		return a.getCharge(ctx, client, req.Params)
	case "list_subscriptions":
		return a.listSubscriptions(ctx, client, req.Params)
	case "get_subscription":
		return a.getSubscription(ctx, client, req.Params)
	case "create_refund":
		return a.createRefund(ctx, client, req.Params)
	case "get_balance":
		return a.getBalance(ctx, client)
	default:
		return nil, fmt.Errorf("stripe: unsupported action %q", req.Action)
	}
}

func extractToken(credBytes []byte) (string, error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("stripe: parsing credential: %w", err)
	}
	if cred.Token == "" {
		return "", fmt.Errorf("stripe: missing token in credential")
	}
	return cred.Token, nil
}

func stripeClient(token string) *http.Client {
	return &http.Client{
		Transport: &tokenTransport{token: token, base: http.DefaultTransport},
	}
}

type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// ── list_customers ────────────────────────────────────────────────────────────

func (a *StripeAdapter) listCustomers(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	limit := 25
	if v, ok := paramInt(params, "limit"); ok && v > 0 && v <= 100 {
		limit = v
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if email, ok := params["email"].(string); ok && email != "" {
		q.Set("email", email)
	}
	if after, ok := params["starting_after"].(string); ok && after != "" {
		q.Set("starting_after", after)
	}

	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			Email   string `json:"email"`
			Name    string `json:"name"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := stripeGET(ctx, client, "customers", q, &resp); err != nil {
		return nil, fmt.Errorf("stripe list_customers: %w", err)
	}

	type customerItem struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Created int64  `json:"created"`
	}
	items := make([]customerItem, 0, len(resp.Data))
	for _, c := range resp.Data {
		items = append(items, customerItem{
			ID:      c.ID,
			Email:   c.Email,
			Name:    format.SanitizeText(c.Name, format.MaxFieldLen),
			Created: c.Created,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d customer(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_customer ──────────────────────────────────────────────────────────────

func (a *StripeAdapter) getCustomer(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	customerID, _ := params["customer_id"].(string)
	if customerID == "" {
		return nil, fmt.Errorf("stripe get_customer: customer_id is required")
	}

	var resp struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Phone   string `json:"phone"`
		Created int64  `json:"created"`
		Balance int64  `json:"balance"`
		Currency string `json:"currency"`
	}
	if err := stripeGET(ctx, client, "customers/"+customerID, nil, &resp); err != nil {
		return nil, fmt.Errorf("stripe get_customer: %w", err)
	}

	result := map[string]any{
		"id":       resp.ID,
		"email":    resp.Email,
		"name":     format.SanitizeText(resp.Name, format.MaxFieldLen),
		"phone":    resp.Phone,
		"created":  resp.Created,
		"balance":  formatMoney(resp.Balance, resp.Currency),
		"currency": resp.Currency,
	}
	return &adapters.Result{
		Summary: format.Summary("Customer %s: %s", resp.ID, resp.Email),
		Data:    result,
	}, nil
}

// ── list_charges ──────────────────────────────────────────────────────────────

func (a *StripeAdapter) listCharges(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	limit := 25
	if v, ok := paramInt(params, "limit"); ok && v > 0 && v <= 100 {
		limit = v
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if customer, ok := params["customer"].(string); ok && customer != "" {
		q.Set("customer", customer)
	}
	if after, ok := params["starting_after"].(string); ok && after != "" {
		q.Set("starting_after", after)
	}

	var resp struct {
		Data []struct {
			ID       string `json:"id"`
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
			Status   string `json:"status"`
			Customer string `json:"customer"`
			Created  int64  `json:"created"`
		} `json:"data"`
	}
	if err := stripeGET(ctx, client, "charges", q, &resp); err != nil {
		return nil, fmt.Errorf("stripe list_charges: %w", err)
	}

	type chargeItem struct {
		ID       string `json:"id"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
		Customer string `json:"customer"`
		Created  int64  `json:"created"`
	}
	items := make([]chargeItem, 0, len(resp.Data))
	for _, c := range resp.Data {
		items = append(items, chargeItem{
			ID:       c.ID,
			Amount:   formatMoney(c.Amount, c.Currency),
			Currency: c.Currency,
			Status:   c.Status,
			Customer: c.Customer,
			Created:  c.Created,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d charge(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_charge ────────────────────────────────────────────────────────────────

func (a *StripeAdapter) getCharge(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	chargeID, _ := params["charge_id"].(string)
	if chargeID == "" {
		return nil, fmt.Errorf("stripe get_charge: charge_id is required")
	}

	var resp struct {
		ID            string `json:"id"`
		Amount        int64  `json:"amount"`
		AmountRefunded int64 `json:"amount_refunded"`
		Currency      string `json:"currency"`
		Status        string `json:"status"`
		Customer      string `json:"customer"`
		Description   string `json:"description"`
		Created       int64  `json:"created"`
		Refunded      bool   `json:"refunded"`
	}
	if err := stripeGET(ctx, client, "charges/"+chargeID, nil, &resp); err != nil {
		return nil, fmt.Errorf("stripe get_charge: %w", err)
	}

	result := map[string]any{
		"id":              resp.ID,
		"amount":          formatMoney(resp.Amount, resp.Currency),
		"amount_refunded": formatMoney(resp.AmountRefunded, resp.Currency),
		"currency":        resp.Currency,
		"status":          resp.Status,
		"customer":        resp.Customer,
		"description":     format.SanitizeText(resp.Description, format.MaxFieldLen),
		"created":         resp.Created,
		"refunded":        resp.Refunded,
	}
	return &adapters.Result{
		Summary: format.Summary("Charge %s: %s %s [%s]", resp.ID, formatMoney(resp.Amount, resp.Currency), strings.ToUpper(resp.Currency), resp.Status),
		Data:    result,
	}, nil
}

// ── list_subscriptions ────────────────────────────────────────────────────────

func (a *StripeAdapter) listSubscriptions(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	limit := 25
	if v, ok := paramInt(params, "limit"); ok && v > 0 && v <= 100 {
		limit = v
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if customer, ok := params["customer"].(string); ok && customer != "" {
		q.Set("customer", customer)
	}
	if status, ok := params["status"].(string); ok && status != "" {
		q.Set("status", status)
	}

	var resp struct {
		Data []struct {
			ID                 string `json:"id"`
			Status             string `json:"status"`
			Customer           string `json:"customer"`
			CurrentPeriodStart int64  `json:"current_period_start"`
			CurrentPeriodEnd   int64  `json:"current_period_end"`
			Created            int64  `json:"created"`
		} `json:"data"`
	}
	if err := stripeGET(ctx, client, "subscriptions", q, &resp); err != nil {
		return nil, fmt.Errorf("stripe list_subscriptions: %w", err)
	}

	type subItem struct {
		ID                 string `json:"id"`
		Status             string `json:"status"`
		Customer           string `json:"customer"`
		CurrentPeriodStart int64  `json:"current_period_start"`
		CurrentPeriodEnd   int64  `json:"current_period_end"`
		Created            int64  `json:"created"`
	}
	items := make([]subItem, 0, len(resp.Data))
	for _, s := range resp.Data {
		items = append(items, subItem{
			ID:                 s.ID,
			Status:             s.Status,
			Customer:           s.Customer,
			CurrentPeriodStart: s.CurrentPeriodStart,
			CurrentPeriodEnd:   s.CurrentPeriodEnd,
			Created:            s.Created,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d subscription(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_subscription ──────────────────────────────────────────────────────────

func (a *StripeAdapter) getSubscription(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	subID, _ := params["subscription_id"].(string)
	if subID == "" {
		return nil, fmt.Errorf("stripe get_subscription: subscription_id is required")
	}

	var resp struct {
		ID                 string `json:"id"`
		Status             string `json:"status"`
		Customer           string `json:"customer"`
		CurrentPeriodStart int64  `json:"current_period_start"`
		CurrentPeriodEnd   int64  `json:"current_period_end"`
		CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
		Created            int64  `json:"created"`
	}
	if err := stripeGET(ctx, client, "subscriptions/"+subID, nil, &resp); err != nil {
		return nil, fmt.Errorf("stripe get_subscription: %w", err)
	}

	result := map[string]any{
		"id":                   resp.ID,
		"status":               resp.Status,
		"customer":             resp.Customer,
		"current_period_start": resp.CurrentPeriodStart,
		"current_period_end":   resp.CurrentPeriodEnd,
		"cancel_at_period_end": resp.CancelAtPeriodEnd,
		"created":              resp.Created,
	}
	return &adapters.Result{
		Summary: format.Summary("Subscription %s [%s]", resp.ID, resp.Status),
		Data:    result,
	}, nil
}

// ── create_refund ─────────────────────────────────────────────────────────────

func (a *StripeAdapter) createRefund(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	charge, _ := params["charge"].(string)
	if charge == "" {
		return nil, fmt.Errorf("stripe create_refund: charge is required")
	}

	form := url.Values{}
	form.Set("charge", charge)
	if amount, ok := paramInt(params, "amount"); ok && amount > 0 {
		form.Set("amount", fmt.Sprintf("%d", amount))
	}
	if reason, ok := params["reason"].(string); ok && reason != "" {
		form.Set("reason", reason)
	}

	var resp struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Charge   string `json:"charge"`
		Status   string `json:"status"`
	}
	if err := stripeFormPOST(ctx, client, "refunds", form, &resp); err != nil {
		return nil, fmt.Errorf("stripe create_refund: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Refund %s: %s %s for charge %s", resp.ID, formatMoney(resp.Amount, resp.Currency), strings.ToUpper(resp.Currency), resp.Charge),
		Data: map[string]any{
			"id":       resp.ID,
			"amount":   formatMoney(resp.Amount, resp.Currency),
			"currency": resp.Currency,
			"charge":   resp.Charge,
			"status":   resp.Status,
		},
	}, nil
}

// ── get_balance ───────────────────────────────────────────────────────────────

func (a *StripeAdapter) getBalance(ctx context.Context, client *http.Client) (*adapters.Result, error) {
	var resp struct {
		Available []struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"available"`
		Pending []struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"pending"`
	}
	if err := stripeGET(ctx, client, "balance", nil, &resp); err != nil {
		return nil, fmt.Errorf("stripe get_balance: %w", err)
	}

	type balanceEntry struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	available := make([]balanceEntry, 0, len(resp.Available))
	for _, b := range resp.Available {
		available = append(available, balanceEntry{
			Amount:   formatMoney(b.Amount, b.Currency),
			Currency: strings.ToUpper(b.Currency),
		})
	}
	pending := make([]balanceEntry, 0, len(resp.Pending))
	for _, b := range resp.Pending {
		pending = append(pending, balanceEntry{
			Amount:   formatMoney(b.Amount, b.Currency),
			Currency: strings.ToUpper(b.Currency),
		})
	}
	result := map[string]any{
		"available": available,
		"pending":   pending,
	}
	summary := "Balance"
	if len(resp.Available) > 0 {
		summary = fmt.Sprintf("Balance: %s %s available", formatMoney(resp.Available[0].Amount, resp.Available[0].Currency), strings.ToUpper(resp.Available[0].Currency))
	}
	return &adapters.Result{
		Summary: summary,
		Data:    result,
	}, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

const stripeAPIBase = "https://api.stripe.com/v1/"

func stripeGET(ctx context.Context, client *http.Client, path string, q url.Values, out any) error {
	apiURL := stripeAPIBase + path
	if len(q) > 0 {
		apiURL += "?" + q.Encode()
	}
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

func stripeFormPOST(ctx context.Context, client *http.Client, path string, form url.Values, out any) error {
	apiURL := stripeAPIBase + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// ── Money formatting ──────────────────────────────────────────────────────────

// formatMoney converts a Stripe amount (in smallest currency unit) to a display string.
func formatMoney(amount int64, currency string) string {
	return fmt.Sprintf("%.2f", float64(amount)/100.0)
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
