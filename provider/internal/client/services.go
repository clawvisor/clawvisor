package client

import (
	"context"
	"encoding/json"
	"net/url"
)

// ServiceConfig is a per-service configuration document (spec 06b's
// clawvisor_service_config). Config is an opaque JSON object the server
// stores verbatim.
type ServiceConfig struct {
	ServiceID string          `json:"service_id"`
	Alias     string          `json:"alias"`
	Config    json.RawMessage `json:"config"`
}

// GetServiceConfig fetches the config document for serviceID/alias. Returns a
// 404 *APIError when none is set.
func (c *Client) GetServiceConfig(ctx context.Context, serviceID, alias string) (*ServiceConfig, error) {
	var sc ServiceConfig
	path := "/api/services/" + url.PathEscape(serviceID) + "/config?alias=" + url.QueryEscape(alias)
	if err := c.do(ctx, "GET", path, nil, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// PutServiceConfig upserts the config document for serviceID/alias.
func (c *Client) PutServiceConfig(ctx context.Context, serviceID, alias string, config json.RawMessage) (*ServiceConfig, error) {
	body := struct {
		Alias  string          `json:"alias"`
		Config json.RawMessage `json:"config"`
	}{Alias: alias, Config: config}
	var sc ServiceConfig
	path := "/api/services/" + url.PathEscape(serviceID) + "/config"
	if err := c.do(ctx, "PUT", path, body, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// DeleteServiceConfig removes the config document for serviceID/alias.
func (c *Client) DeleteServiceConfig(ctx context.Context, serviceID, alias string) error {
	path := "/api/services/" + url.PathEscape(serviceID) + "/config?alias=" + url.QueryEscape(alias)
	return c.do(ctx, "DELETE", path, nil, nil)
}
