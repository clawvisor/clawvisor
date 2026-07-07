// Package refaws resolves external-secret references against AWS Secrets
// Manager using the AWS SDK v2 default credential chain (env, IRSA, instance
// profile, SSO) — AMBIENT identity only. No static AWS keys are ever read from
// Clawvisor config; a design that added them must be rejected.
package refaws

import (
	"context"
	"errors"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smithy "github.com/aws/smithy-go"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

// smClient is the subset of the Secrets Manager API the resolver needs. It
// lets tests inject a fake without a network round-trip.
type smClient interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// Resolver is the aws-sm backend. The SDK client is created lazily on first
// Resolve so an unused backend costs nothing at startup.
type Resolver struct {
	once sync.Once
	err  error
	cli  smClient

	// endpoint, when non-empty, overrides the service endpoint. It exists
	// purely for deterministic testing against a local Secrets Manager mock
	// (localstack / httptest); production leaves it empty for the real API.
	endpoint string

	// newClient is swappable in tests to inject a fake smClient.
	newClient func(ctx context.Context) (smClient, error)
}

var _ vault.Resolver = (*Resolver)(nil)

// New returns an aws-sm resolver. endpoint is a test-only base-endpoint
// override; pass "" in production.
func New(endpoint string) *Resolver {
	r := &Resolver{endpoint: endpoint}
	r.newClient = r.defaultNewClient
	return r
}

// newWithClient is used by tests to inject a fake client.
func newWithClient(c smClient) *Resolver {
	r := &Resolver{}
	r.newClient = func(ctx context.Context) (smClient, error) { return c, nil }
	return r
}

func (r *Resolver) defaultNewClient(ctx context.Context) (smClient, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return secretsmanager.NewFromConfig(cfg, func(o *secretsmanager.Options) {
		if r.endpoint != "" {
			o.BaseEndpoint = aws.String(r.endpoint)
		}
	}), nil
}

func (r *Resolver) client() (smClient, error) {
	r.once.Do(func() {
		// Construct the client with a background context so a cancelled or
		// short-deadline first-caller request context cannot permanently
		// poison r.err for every subsequent Resolve. The caller ctx is
		// reserved for the per-request API call in Resolve.
		r.cli, r.err = r.newClient(context.Background())
	})
	return r.cli, r.err
}

// Resolve fetches the referenced secret and applies the optional json_key.
// ref.ID is an ARN or secret name; a version can be pinned by encoding it in
// the ARN, otherwise AWSCURRENT is used.
func (r *Resolver) Resolve(ctx context.Context, ref vault.RefEnvelope) ([]byte, error) {
	cli, err := r.client()
	if err != nil {
		return nil, err
	}
	out, err := cli.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(ref.ID),
	})
	if err != nil {
		return nil, mapAWSError(err)
	}
	var payload []byte
	switch {
	case out.SecretString != nil:
		payload = []byte(*out.SecretString)
	case out.SecretBinary != nil:
		payload = out.SecretBinary
	default:
		return nil, vault.ErrRefNotFound
	}
	return vault.ExtractJSONKey(payload, ref.JSONKey)
}

// mapAWSError converts SDK errors into the vault's typed, content-free error
// classes. Never include the AWS message verbatim beyond the class, so no
// secret metadata leaks.
func mapAWSError(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ResourceNotFoundException":
			return vault.ErrRefNotFound
		case "AccessDeniedException", "UnrecognizedClientException", "InvalidSignatureException":
			return vault.ErrRefAccessDenied
		case "ThrottlingException", "TooManyRequestsException", "RequestLimitExceeded",
			"InternalServiceError", "ServiceUnavailableException":
			return vault.ErrRefThrottled
		}
	}
	// Unknown transport/unavailability: treat as retryable-throttle so a
	// transient blip gets the backoff, not a hard fail.
	return vault.ErrRefThrottled
}
