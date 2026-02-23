package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: record not found")

// ErrConflict is returned when a uniqueness constraint is violated.
var ErrConflict = errors.New("store: record already exists")

// Store is the primary data access interface. All database operations go
// through this interface; no direct queries are made outside the store package.
type Store interface {
	// Users
	CreateUser(ctx context.Context, email, passwordHash string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)

	// Agent roles
	CreateRole(ctx context.Context, userID, name, description string) (*AgentRole, error)
	UpdateRole(ctx context.Context, id, userID, name, description string) (*AgentRole, error)
	GetRole(ctx context.Context, id, userID string) (*AgentRole, error)
	GetRoleByName(ctx context.Context, name, userID string) (*AgentRole, error)
	ListRoles(ctx context.Context, userID string) ([]*AgentRole, error)
	DeleteRole(ctx context.Context, id, userID string) error

	// Policies
	CreatePolicy(ctx context.Context, userID string, p *PolicyRecord) (*PolicyRecord, error)
	UpdatePolicy(ctx context.Context, id, userID string, p *PolicyRecord) (*PolicyRecord, error)
	DeletePolicy(ctx context.Context, id, userID string) error
	GetPolicy(ctx context.Context, id, userID string) (*PolicyRecord, error)
	GetPolicyBySlug(ctx context.Context, slug, userID string) (*PolicyRecord, error)
	ListPolicies(ctx context.Context, userID string, filter PolicyFilter) ([]*PolicyRecord, error)

	// Agents
	CreateAgent(ctx context.Context, userID, name, tokenHash string, roleID *string) (*Agent, error)
	UpdateAgentRole(ctx context.Context, id, userID string, roleID *string) (*Agent, error)
	GetAgentByToken(ctx context.Context, tokenHash string) (*Agent, error)
	ListAgents(ctx context.Context, userID string) ([]*Agent, error)
	DeleteAgent(ctx context.Context, id, userID string) error

	// Sessions (refresh tokens)
	CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (*Session, error)
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error

	// Service credentials metadata (vault stores the actual bytes)
	UpsertServiceMeta(ctx context.Context, userID, serviceID string, activatedAt time.Time) error
	GetServiceMeta(ctx context.Context, userID, serviceID string) (*ServiceMeta, error)
	ListServiceMetas(ctx context.Context, userID string) ([]*ServiceMeta, error)
	DeleteServiceMeta(ctx context.Context, userID, serviceID string) error

	// Notification configs
	UpsertNotificationConfig(ctx context.Context, userID, channel string, config json.RawMessage) error
	GetNotificationConfig(ctx context.Context, userID, channel string) (*NotificationConfig, error)

	// Health
	Ping(ctx context.Context) error
	Close() error
}

// User represents a registered Clawvisor account.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Session holds a hashed refresh token.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentRole is an optional label assigned to one or more agents.
// Policies can target a role; agents with no role receive only global policies.
type AgentRole struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// Agent is an AI agent that authenticates via a long-lived bearer token.
type Agent struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"-"`
	RoleID    *string   `json:"role_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ServiceMeta records that a user has activated a given service.
// The actual credential bytes live in the vault.
type ServiceMeta struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	ServiceID   string    `json:"service_id"`
	ActivatedAt time.Time `json:"activated_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PolicyRecord is a policy as stored in the database.
type PolicyRecord struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	RoleID      *string   `json:"role_id"`
	RulesYAML   string    `json:"rules_yaml"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PolicyFilter controls which policies are returned by ListPolicies.
// Zero value returns all policies for the user.
type PolicyFilter struct {
	RoleID     *string // only policies for this role
	GlobalOnly bool    // only policies with no role (role_id IS NULL)
}

// NotificationConfig stores per-user, per-channel notification settings.
type NotificationConfig struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	Channel   string          `json:"channel"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}
