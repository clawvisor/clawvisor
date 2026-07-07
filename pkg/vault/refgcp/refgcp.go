// Package refgcp resolves external-secret references against GCP Secret
// Manager using Application Default Credentials (workload identity / metadata
// server) — AMBIENT identity only. This is distinct from and unrelated to the
// push-mode pkg/vault.GCPVault, which stores a Clawvisor-encrypted blob; here
// the secret lives in the customer's project and Clawvisor only holds a
// reference.
package refgcp

import (
	"context"
	"strings"
	"sync"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

// Resolver is the gcp-sm backend with lazy client init.
type Resolver struct {
	once sync.Once
	err  error
	cli  *secretmanager.Client

	// endpoint, when non-empty, overrides the API endpoint for deterministic
	// testing against a local mock. Production leaves it empty.
	endpoint string
}

var _ vault.Resolver = (*Resolver)(nil)

// New returns a gcp-sm resolver. endpoint is a test-only override; pass "" in
// production.
func New(endpoint string) *Resolver {
	return &Resolver{endpoint: endpoint}
}

func (r *Resolver) client() (*secretmanager.Client, error) {
	r.once.Do(func() {
		var opts []option.ClientOption
		if r.endpoint != "" {
			// Insecure local mock: skip auth + TLS so a gRPC mock server works.
			opts = append(opts,
				option.WithEndpoint(r.endpoint),
				option.WithoutAuthentication(),
				option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
			)
		}
		// Construct the client with a background context so a cancelled or
		// short-deadline first-caller request context cannot permanently
		// poison r.err for every subsequent Resolve. The caller ctx is
		// reserved for the per-request AccessSecretVersion call in Resolve.
		r.cli, r.err = secretmanager.NewClient(context.Background(), opts...)
	})
	return r.cli, r.err
}

// Resolve fetches {id}/versions/latest (unless the id already pins a version)
// and applies the optional json_key.
func (r *Resolver) Resolve(ctx context.Context, ref vault.RefEnvelope) ([]byte, error) {
	cli, err := r.client()
	if err != nil {
		return nil, err
	}
	name := ref.ID
	if !strings.Contains(name, "/versions/") {
		name = strings.TrimSuffix(name, "/") + "/versions/latest"
	}
	resp, err := cli.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return nil, mapGCPError(err)
	}
	if resp.GetPayload() == nil {
		return nil, vault.ErrRefNotFound
	}
	return vault.ExtractJSONKey(resp.GetPayload().GetData(), ref.JSONKey)
}

// mapGCPError converts gRPC status codes into the vault's typed, content-free
// error classes.
func mapGCPError(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return vault.ErrRefNotFound
	case codes.PermissionDenied, codes.Unauthenticated:
		return vault.ErrRefAccessDenied
	case codes.ResourceExhausted, codes.Unavailable, codes.Aborted, codes.DeadlineExceeded:
		return vault.ErrRefThrottled
	default:
		return vault.ErrRefThrottled
	}
}
