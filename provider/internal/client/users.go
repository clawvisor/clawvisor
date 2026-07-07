package client

import "context"

// User is the subset of a server user record the provider manages. A user
// materializes when an employee claims their invite; before that only a
// pending UserInvite exists.
type User struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Verified bool   `json:"verified"`
}

// UserInvite is a pending (unclaimed) invite. The one-shot invite_url and
// invite_token are only populated by CreateInvite's response; the list
// endpoint never returns them.
type UserInvite struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	InviteURL string `json:"invite_url"`
	ExpiresAt string `json:"expires_at"`
}

// CreateInviteRequest is the POST /api/users/invites body. Email pins the
// invite to one address (an any-email invite is member-only server-side).
type CreateInviteRequest struct {
	Email string `json:"email,omitempty"`
	Role  string `json:"role,omitempty"`
}

// CreateInvite mints a single-use invite. The invite_url (and invite_token)
// are revealed exactly once, in this response.
func (c *Client) CreateInvite(ctx context.Context, req CreateInviteRequest) (*UserInvite, error) {
	var inv UserInvite
	if err := c.do(ctx, "POST", "/api/users/invites", req, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// ListUsers returns the real (claimed) users. System rows are excluded by the
// server.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out struct {
		Users []User `json:"users"`
	}
	if err := c.do(ctx, "GET", "/api/users", nil, &out); err != nil {
		return nil, err
	}
	return out.Users, nil
}

// ListPendingInvites returns unclaimed invites (never the invite_url/token).
func (c *Client) ListPendingInvites(ctx context.Context) ([]UserInvite, error) {
	var out struct {
		Invites []UserInvite `json:"invites"`
	}
	if err := c.do(ctx, "GET", "/api/users/invites", nil, &out); err != nil {
		return nil, err
	}
	return out.Invites, nil
}

// UpdateUserRole changes a claimed user's role in place ("admin"|"member").
func (c *Client) UpdateUserRole(ctx context.Context, id, role string) error {
	return c.do(ctx, "PUT", "/api/users/"+id+"/role", map[string]string{"role": role}, nil)
}

// DeleteUser offboards a claimed user: it immediately invalidates the user's
// cvis_ agent tokens server-side; audit/cost history is retained.
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/users/"+id, nil, nil)
}

// DeleteInvite revokes a pending (unclaimed) invite.
func (c *Client) DeleteInvite(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/users/invites/"+id, nil, nil)
}
