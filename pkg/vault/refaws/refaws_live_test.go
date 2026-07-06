package refaws

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

// TestRefAWS_Live resolves a real AWS Secrets Manager secret using the ambient
// credential chain. Gated on CLAWVISOR_TEST_AWS_SECRET_ARN (the ARN of a
// secret the caller's ambient identity may read). Optionally, if
// CLAWVISOR_TEST_AWS_SECRET_ARN_DENIED is set to an ARN the identity may NOT
// read, the test asserts the access-denied mapping. No credential value is
// logged.
func TestRefAWS_Live(t *testing.T) {
	arn := os.Getenv("CLAWVISOR_TEST_AWS_SECRET_ARN")
	if arn == "" {
		t.Skip("set CLAWVISOR_TEST_AWS_SECRET_ARN (+ ambient AWS creds) to run the live AWS SM lane")
	}
	ctx := context.Background()
	r := New("")

	got, err := r.Resolve(ctx, vault.RefEnvelope{
		Backend: vault.BackendAWSSM,
		ID:      arn,
		JSONKey: os.Getenv("CLAWVISOR_TEST_AWS_SECRET_JSON_KEY"),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("resolved secret is empty")
	}

	if denied := os.Getenv("CLAWVISOR_TEST_AWS_SECRET_ARN_DENIED"); denied != "" {
		_, err := New("").Resolve(ctx, vault.RefEnvelope{Backend: vault.BackendAWSSM, ID: denied})
		if !errors.Is(err, vault.ErrRefAccessDenied) {
			t.Fatalf("unauthorized fixture: got %v, want ErrRefAccessDenied", err)
		}
	}
}
