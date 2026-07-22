package upstreamflavor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
)

// ErrNotImplemented is returned by flavors whose forward path has not
// yet shipped (currently: Bedrock SigV4 + EventStream translation).
var ErrNotImplemented = errors.New("upstreamflavor: not implemented")

// Flavor describes a non-native deployment target for an Anthropic-shaped
// Messages API request. Implementations transform the upstream URL, the
// request body, and the auth headers.
//
// Concurrency: implementations must be safe for concurrent use after
// construction.
type Flavor interface {
	// Name returns a stable identifier (e.g. "vertex", "azure",
	// "bedrock"). Used in audit rows and log fields.
	Name() string

	// BuildURL returns the upstream URL for the given inbound path
	// (e.g. "/v1/messages") and model name (parsed from the request
	// body; empty if the body has no `model` field).
	BuildURL(inboundPath, model string) (*url.URL, error)

	// TransformBody returns a body suitable for this flavor's
	// upstream. Native-shape flavors return the input unchanged.
	// Flavors that move `model` into the URL strip it here.
	TransformBody(body []byte) ([]byte, error)

	// InjectAuth attaches credentials to req. The proxy supplies
	// credBytes from vault under VaultServiceID(); passthrough mode
	// passes empty credBytes and relies on the caller's already-set
	// Authorization header — implementations should still perform
	// flavor-specific header cleanup in that case.
	InjectAuth(req *http.Request, credBytes []byte) error

	// VaultServiceID returns the conventional vault service ID for
	// this flavor's stored credentials, used by the forwarder's
	// agent-scoped + user-scoped lookup chain. An empty string means
	// the flavor is passthrough-only and the proxy should skip the
	// vault lookup.
	VaultServiceID() string
}

// AnthropicBodyVersion is the value injected as `anthropic_version`
// into the request body for flavors that require it (Vertex, Bedrock).
// Matches the version sent in the `anthropic-version` header for native
// API requests.
const AnthropicBodyVersion = "vertex-2023-10-16"

// ExtractModel pulls the top-level `model` string from an Anthropic
// Messages API body. Returns empty string + nil error when the field is
// missing or the body is not a JSON object — flavors decide whether
// that is a configuration error (Vertex/Bedrock require a model in the
// URL) or acceptable (Azure can be configured with a fixed deployment).
func ExtractModel(body []byte) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	start, end, ok := jsonsurgery.FindFieldValue(body, "model")
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(body[start:end], &s); err != nil {
		return "", fmt.Errorf("upstreamflavor: parsing model field: %w", err)
	}
	return s, nil
}

// stripModel removes a top-level `model` field from a JSON object body.
// Returns the input unchanged when there is no model field.
func stripModel(body []byte) ([]byte, bool) {
	return jsonsurgery.DeleteField(body, "model")
}

// setAnthropicVersion writes a top-level `anthropic_version` field with
// the constant body version. Replaces an existing field if present.
func setAnthropicVersion(body []byte) ([]byte, error) {
	versionJSON, err := json.Marshal(AnthropicBodyVersion)
	if err != nil {
		return nil, err
	}
	return jsonsurgery.SetField(body, "anthropic_version", versionJSON)
}
