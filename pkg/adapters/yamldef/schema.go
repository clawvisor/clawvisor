// Package yamldef defines the YAML schema types for declarative adapter definitions.
package yamldef

// ServiceDef is the top-level structure of a YAML adapter definition file.
type ServiceDef struct {
	Service           ServiceInfo                `yaml:"service"`
	Auth              AuthDef                    `yaml:"auth"`
	API               APIDef                     `yaml:"api"`
	Variables         map[string]VariableDef     `yaml:"variables,omitempty"`
	VerificationHints string                     `yaml:"verification_hints,omitempty"`
	Actions           map[string]Action          `yaml:"actions"`
}

// VariableDef defines a user-configurable variable that is collected during
// service activation and interpolated into fields like base_url at runtime.
type VariableDef struct {
	DisplayName string `yaml:"display_name"`
	Description string `yaml:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
	Default     string `yaml:"default,omitempty"`
}

// ServiceInfo contains display and identification metadata.
type ServiceInfo struct {
	ID          string       `yaml:"id"`
	DisplayName string       `yaml:"display_name"`
	Description string       `yaml:"description"`
	SetupURL    string       `yaml:"setup_url,omitempty"`
	KeyHint     string       `yaml:"key_hint,omitempty"`
	IconSVG     string       `yaml:"icon_svg,omitempty"` // optional: inline SVG markup (mutually exclusive with icon_url)
	IconURL     string       `yaml:"icon_url,omitempty"` // optional: absolute or site-relative URL to the icon (e.g. "/logos/github.svg")
	Identity    *IdentityDef `yaml:"identity,omitempty"`
}

// IdentityDef configures automatic account identity detection after activation.
// The service makes a request to the endpoint and extracts the identity
// from the JSON response using the specified field path.
type IdentityDef struct {
	Endpoint string `yaml:"endpoint"`         // URL to fetch identity (e.g. "/user")
	Field    string `yaml:"field"`            // dot-delimited JSON field path (e.g. "login", "email")
	Method   string `yaml:"method,omitempty"` // HTTP method, default "GET"
	Body     string `yaml:"body,omitempty"`   // request body (e.g. GraphQL query JSON)
}

// AuthDef describes how the adapter authenticates with the remote API.
type AuthDef struct {
	Type         string            `yaml:"type"` // "api_key", "oauth2", "basic", "none"
	Header       string            `yaml:"header,omitempty"`
	HeaderPrefix string            `yaml:"header_prefix,omitempty"`
	ExtraHeaders map[string]string `yaml:"extra_headers,omitempty"`
	OAuth      *OAuthDef      `yaml:"oauth,omitempty"`
	DeviceFlow *DeviceFlowDef `yaml:"device_flow,omitempty"`
	PKCEFlow   *PKCEFlowDef  `yaml:"pkce_flow,omitempty"`
}

// DeviceFlowDef holds configuration for OAuth2 device authorization grant (RFC 8628).
// This allows CLI/native apps to authenticate without a client secret.
type DeviceFlowDef struct {
	ClientID      string   `yaml:"client_id,omitempty"`       // hardcoded client_id (public, safe to ship)
	ClientIDEnv   string   `yaml:"client_id_env,omitempty"`   // env var name for client_id override
	Scopes        []string `yaml:"scopes"`                    // requested OAuth scopes
	DeviceCodeURL string   `yaml:"device_code_url"`           // e.g. "https://github.com/login/device/code"
	TokenURL      string   `yaml:"token_url"`                 // e.g. "https://github.com/login/oauth/access_token"
	GrantType     string   `yaml:"grant_type,omitempty"`      // default: "urn:ietf:params:oauth:grant-type:device_code"
}

// PKCEFlowDef holds configuration for OAuth2 authorization code flow with PKCE (RFC 7636).
// This allows CLI/native apps to authenticate without shipping a client secret.
type PKCEFlowDef struct {
	ClientID     string   `yaml:"client_id,omitempty"`      // hardcoded client_id (public, safe to ship)
	ClientIDEnv  string   `yaml:"client_id_env,omitempty"`  // env var name for client_id override
	Scopes       []string `yaml:"scopes"`                   // requested OAuth scopes
	AuthorizeURL string   `yaml:"authorize_url"`            // e.g. "https://slack.com/oauth/v2/authorize"
	TokenURL     string   `yaml:"token_url"`                // e.g. "https://slack.com/api/oauth.v2.access"
	TokenPath         string   `yaml:"token_path,omitempty"`          // JSON path to access token in response (e.g. "authed_user.access_token")
	LocalhostRedirect bool     `yaml:"localhost_redirect,omitempty"`  // prefer http://localhost redirect over relay
}

// OAuthDef holds OAuth2-specific configuration.
type OAuthDef struct {
	Scopes            []string           `yaml:"scopes"`
	ConditionalScopes []ConditionalScope `yaml:"conditional_scopes,omitempty"`
	Endpoint          string             `yaml:"endpoint,omitempty"`    // "google" — maps to well-known endpoints
	VaultKey          string             `yaml:"vault_key,omitempty"`   // shared vault key (e.g. "google")
	ScopeMerge        bool               `yaml:"scope_merge,omitempty"` // whether to merge scopes with existing credential

	// Custom OAuth endpoint fields — used when Endpoint is not a well-known provider.
	ClientID        string `yaml:"client_id,omitempty"`
	ClientIDEnv     string `yaml:"client_id_env,omitempty"`
	ClientSecret    string `yaml:"client_secret,omitempty"`
	ClientSecretEnv string `yaml:"client_secret_env,omitempty"`
	AuthorizeURL    string `yaml:"authorize_url,omitempty"`
	TokenURL        string `yaml:"token_url,omitempty"`

	// Provider-specific overrides for non-standard OAuth flows.
	ScopeParam string `yaml:"scope_param,omitempty"` // authorize URL param name for scopes (default "scope"; Slack v2 uses "user_scope")
	TokenPath  string `yaml:"token_path,omitempty"`  // JSON path to access token in token response (e.g. "authed_user.access_token")
}

// ConditionalScope is a scope that is only included when an env var condition is met.
type ConditionalScope struct {
	Scope   string `yaml:"scope"`
	EnvGate string `yaml:"env_gate"`
	Default bool   `yaml:"default"` // include if env var is unset
}

// APIDef describes the remote API transport.
type APIDef struct {
	BaseURL string `yaml:"base_url"`
	Type    string `yaml:"type"` // "rest" or "graphql"
}

// Action defines a single adapter action.
type Action struct {
	DisplayName string            `yaml:"display_name"`
	Risk        RiskDef           `yaml:"risk"`
	Scopes      []string          `yaml:"scopes,omitempty"`   // OAuth scopes required by this action (for per-action validation)
	Override    string            `yaml:"override,omitempty"` // "go" signals delegation to a registered Go function

	// REST fields
	Method   string `yaml:"method,omitempty"`    // GET, POST, PUT, PATCH, DELETE
	Path     string `yaml:"path,omitempty"`      // URL path with {{.param}} interpolation
	Encoding string `yaml:"encoding,omitempty"`  // "form" or "json" (default "json")
	BodyMode string `yaml:"body_mode,omitempty"` // "sparse" = only include provided params in body

	// GraphQL fields
	Query    string `yaml:"query,omitempty"`    // GraphQL query/mutation string
	Mutation bool   `yaml:"mutation,omitempty"` // true if this is a mutation

	Params   map[string]Param `yaml:"params,omitempty"`
	Response ResponseDef      `yaml:"response,omitempty"`

	// Slack-style response envelope checking
	ErrorCheck *ErrorCheckDef `yaml:"error_check,omitempty"`
}

// RiskDef holds the risk assessment metadata for an action.
type RiskDef struct {
	Category    string `yaml:"category"`    // "read", "write", "delete", "search"
	Sensitivity string `yaml:"sensitivity"` // "low", "medium", "high"
	Description string `yaml:"description"` // human-readable description for risk prompt
}

// Param defines a single action parameter.
type Param struct {
	Type     string `yaml:"type"`               // "string", "int", "bool", "object", "array"
	Required bool   `yaml:"required,omitempty"`
	Default  any    `yaml:"default,omitempty"`
	Min      *int   `yaml:"min,omitempty"`
	Max      *int   `yaml:"max,omitempty"`
	Location string `yaml:"location,omitempty"` // "query", "body", "path"

	MapTo       string `yaml:"map_to,omitempty"`       // API-side parameter name (if different from YAML name)
	Transform   string `yaml:"transform,omitempty"`     // expr expression applied to the resolved value
	DefaultExpr string `yaml:"default_expr,omitempty"` // expr expression for dynamic default (e.g. "rfc3339(now())")

	// GraphQL-specific
	GraphQLVar bool   `yaml:"graphql_var,omitempty"` // pass as a top-level GraphQL variable
	FilterPath string `yaml:"filter_path,omitempty"` // e.g. "team.id.eq" — builds nested filter object

	// For mutations: maps param to a field in the $input variable
	InputField string `yaml:"input_field,omitempty"` // e.g. "teamId" (the GraphQL field name)
}

// ResponseDef describes how to extract and format the response.
type ResponseDef struct {
	DataPath string     `yaml:"data_path,omitempty"` // dot-delimited path to the data (e.g. "data", "data.issues.nodes")
	Fields   []FieldDef `yaml:"fields"`
	Summary  string     `yaml:"summary"` // Go template string
	Meta     []MetaDef  `yaml:"meta,omitempty"`
}

// MetaDef describes a field to extract from the top-level response as metadata
// (e.g. pagination cursors). These are returned in Result.Meta, separate from
// the primary data items.
type MetaDef struct {
	Name   string `yaml:"name"`             // field name in the raw response (dot-delimited path supported)
	Rename string `yaml:"rename,omitempty"` // output key name (defaults to Name)
}

// FieldDef describes a single field to extract from the response.
type FieldDef struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path,omitempty"`      // nested access path (e.g. "state.name")
	Rename    string `yaml:"rename,omitempty"`     // output key name (defaults to Name)
	Sanitize  bool   `yaml:"sanitize,omitempty"`   // apply format.SanitizeText
	Nullable  bool   `yaml:"nullable,omitempty"`   // tolerate nil without error
	Transform string `yaml:"transform,omitempty"`  // named transform (e.g. "money", "upper")
	Expr      string `yaml:"expr,omitempty"`       // expr-lang expression evaluated against the response object
	Optional  bool   `yaml:"optional,omitempty"`   // omit field from output if expr returns nil
}

// ErrorCheckDef describes how to check for API-level errors in the response envelope.
// Used for APIs like Slack that return 200 OK but include error info in the body.
type ErrorCheckDef struct {
	SuccessPath string `yaml:"success_path"` // path to a boolean success field (e.g. "ok")
	ErrorPath   string `yaml:"error_path"`   // path to the error message field (e.g. "error")
}
