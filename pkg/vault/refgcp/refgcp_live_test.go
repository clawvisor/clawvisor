package refgcp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

// TestRefGCP_Live resolves a real GCP Secret Manager secret using Application
// Default Credentials. Gated on CLAWVISOR_TEST_GCP_SECRET_NAME (a full
// resource name projects/{p}/secrets/{s}, optionally with /versions/N).
// Optionally CLAWVISOR_TEST_GCP_SECRET_NAME_DENIED asserts the access-denied
// mapping. No credential value is logged.
func TestRefGCP_Live(t *testing.T) {
	name := os.Getenv("CLAWVISOR_TEST_GCP_SECRET_NAME")
	if name == "" {
		t.Skip("set CLAWVISOR_TEST_GCP_SECRET_NAME (+ ADC) to run the live GCP SM lane")
	}
	ctx := context.Background()
	r := New("")

	got, err := r.Resolve(ctx, vault.RefEnvelope{
		Backend: vault.BackendGCPSM,
		ID:      name,
		JSONKey: os.Getenv("CLAWVISOR_TEST_GCP_SECRET_JSON_KEY"),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("resolved secret is empty")
	}

	if denied := os.Getenv("CLAWVISOR_TEST_GCP_SECRET_NAME_DENIED"); denied != "" {
		_, err := New("").Resolve(ctx, vault.RefEnvelope{Backend: vault.BackendGCPSM, ID: denied})
		if !errors.Is(err, vault.ErrRefAccessDenied) {
			t.Fatalf("unauthorized fixture: got %v, want ErrRefAccessDenied", err)
		}
	}
}
