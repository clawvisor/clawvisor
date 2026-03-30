// Package yamldef defines the YAML schema types for declarative adapter definitions.
package yamldef

// ServiceDef is the top-level structure of a YAML adapter definition file.
type ServiceDef struct {
	Service           ServiceInfo       `yaml:"service"`
	Auth              AuthDef           `yaml:"auth"`
	API               APIDef            `yaml:"api"`
	VerificationHints string            `yaml:"verification_hints,omitempty"`
	Actions           map[string]Action `yaml:"actions"`
}

// ServiceInfo contains display and identification metadata.
type ServiceInfo struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	Description string `yaml:"description"`
	SetupURL    string `yaml:"setup_url,omitempty"`
}

// AuthDef describes how the adapter authenticates with the remote API.
type AuthDef struct {
	Type         string            `yaml:"type"` // "api_key", "oauth2", "basic", "none"
	Header       string            `yaml:"header,omitempty"`
	HeaderPrefix string            `yaml:"header_prefix,omitempty"`
	ExtraHeaders map[string]string `yaml:"extra_headers,omitempty"`
	OAuth        *OAuthDef         `yaml:"oauth,omitempty"`

	// For basic auth where the credential is a composite "user:pass" string.
	// The token field is split on ":" to produce user/pass for BasicAuth.
	BasicSplit bool `yaml:"basic_split,omitempty"`
}

// OAuthDef holds OAuth2-specific configuration.
type OAuthDef struct {
	Scopes            []string           `yaml:"scopes"`
	ConditionalScopes []ConditionalScope `yaml:"conditional_scopes,omitempty"`
	Endpoint          string             `yaml:"endpoint"`    // "google" — maps to well-known endpoints
	VaultKey          string             `yaml:"vault_key"`   // shared vault key (e.g. "google")
	ScopeMerge        bool               `yaml:"scope_merge"` // whether to merge scopes with existing credential
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
