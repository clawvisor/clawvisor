package upstreamflavor

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// BedrockConfig parameterizes a BedrockFlavor instance.
//
// Region is required and is baked into the upstream host:
// bedrock-runtime.{Region}.amazonaws.com.
//
// ModelOverride is required because Bedrock's model identifiers are not
// the Anthropic-native names Claude Code sends. Examples:
//   - "anthropic.claude-3-5-sonnet-20241022-v2:0"
//   - "anthropic.claude-opus-4-20260101-v1:0"
// The client-supplied `model` field is ignored — Bedrock derives the
// target model from the URL path.
//
// SignerFactory returns a SigV4 signer the flavor uses to sign each
// upstream request. The factory pattern lets callers wire in
// aws-sdk-go-v2's signer (or a vendored equivalent) without forcing
// this package to take a hard SDK dependency.
type BedrockConfig struct {
	Region        string
	ModelOverride string
	SignerFactory func() Signer
}

// Signer signs an outbound *http.Request using SigV4 credentials. The
// llmproxy package does not import aws-sdk-go-v2 directly; an adapter
// elsewhere in the repo will wrap aws.SignHTTP.
type Signer interface {
	Sign(req *http.Request, body []byte) error
}

// NewBedrock returns a Flavor that targets AWS Bedrock's
// :invoke-with-response-stream endpoint for Anthropic models.
//
// SCAFFOLD ONLY: the request half (URL + body transform) is complete
// and tested; the response half — translating AWS EventStream
// (binary-framed application/vnd.amazon.eventstream) back into the
// Anthropic SSE shape the proxy's postprocess pipeline expects — is
// deferred to a follow-up PR. SigV4 signing is also deferred (callers
// must supply a Signer; the placeholder NoopSigner returns
// ErrNotImplemented).
func NewBedrock(cfg BedrockConfig) (Flavor, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("upstreamflavor: bedrock: Region required")
	}
	if strings.TrimSpace(cfg.ModelOverride) == "" {
		return nil, errors.New("upstreamflavor: bedrock: ModelOverride required (client `model` field is ignored)")
	}
	if cfg.SignerFactory == nil {
		cfg.SignerFactory = func() Signer { return noopSigner{} }
	}
	return &bedrockFlavor{cfg: cfg}, nil
}

type bedrockFlavor struct {
	cfg BedrockConfig
}

func (b *bedrockFlavor) Name() string { return "bedrock" }

func (b *bedrockFlavor) BuildURL(inboundPath, _ string) (*url.URL, error) {
	suffix, err := bedrockSuffix(inboundPath)
	if err != nil {
		return nil, err
	}
	host := fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", b.cfg.Region)
	path := fmt.Sprintf("/model/%s/%s", b.cfg.ModelOverride, suffix)
	return &url.URL{Scheme: "https", Host: host, Path: path}, nil
}

func bedrockSuffix(inboundPath string) (string, error) {
	switch inboundPath {
	case "/v1/messages":
		return "invoke-with-response-stream", nil
	case "/v1/messages/count_tokens":
		return "", fmt.Errorf("upstreamflavor: bedrock: %s is not supported by Bedrock", inboundPath)
	default:
		return "", fmt.Errorf("upstreamflavor: bedrock: unsupported inbound path %q", inboundPath)
	}
}

// TransformBody applies the same Vertex-style transforms: strip `model`
// (Bedrock derives it from the URL) and inject `anthropic_version`
// (Bedrock requires it inside the body).
func (b *bedrockFlavor) TransformBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	out := body
	if stripped, ok := stripModel(out); ok {
		out = stripped
	}
	versioned, err := setAnthropicVersion(out)
	if err != nil {
		return nil, fmt.Errorf("upstreamflavor: bedrock: inject anthropic_version: %w", err)
	}
	return versioned, nil
}

// InjectAuth invokes the configured Signer. Bedrock's SigV4 covers the
// method, path, headers (including X-Amz-Date and X-Amz-Content-Sha256),
// and the body — so the proxy must finalize the body and headers before
// signing. The forwarder is responsible for calling InjectAuth as the
// last step before req.Do.
func (b *bedrockFlavor) InjectAuth(req *http.Request, credBytes []byte) error {
	req.Header.Del("anthropic-version")
	req.Header.Del("x-api-key")

	signer := b.cfg.SignerFactory()
	if signer == nil {
		return errors.New("upstreamflavor: bedrock: nil Signer from factory")
	}
	// Caller stashes the (already-transformed) body in a context value
	// in a follow-up; for now we pass credBytes as a hint — the
	// noopSigner ignores it and returns ErrNotImplemented so callers
	// learn at integration time that signing isn't wired yet.
	return signer.Sign(req, credBytes)
}

// VaultServiceID returns "bedrock"; credentials are typically stored as
// a JSON blob containing access_key_id + secret_access_key + optional
// session_token, parsed by the SignerFactory.
func (b *bedrockFlavor) VaultServiceID() string { return "bedrock" }

type noopSigner struct{}

func (noopSigner) Sign(_ *http.Request, _ []byte) error { return ErrNotImplemented }
