//go:build localstack

// Package refaws localstack sub-lane. Build-tagged so it only runs in the
// docker-based install CI job (which stands up localstack). It exercises the
// REAL refaws code path (SigV4 signing, endpoint override, SDK response
// parsing) against a localstack Secrets Manager, closing the gap between the
// httptest mock (deterministic lane) and a live cloud account (keyed lane).
//
// Setup the CI job performs before running:
//
//	awslocal secretsmanager create-secret --name prod/anthropic \
//	    --secret-string '{"api_key":"sk-ant-localstack"}'
//
// Env:
//   - CLAWVISOR_TEST_LOCALSTACK_ENDPOINT (default http://localhost:4566)
//   - CLAWVISOR_TEST_LOCALSTACK_ARN      (the created secret's ARN or name)
//   - AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_REGION (dummy is fine)
package refaws

import (
	"context"
	"os"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

func TestRefAWS_Localstack(t *testing.T) {
	endpoint := os.Getenv("CLAWVISOR_TEST_LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}
	arn := os.Getenv("CLAWVISOR_TEST_LOCALSTACK_ARN")
	if arn == "" {
		t.Skip("set CLAWVISOR_TEST_LOCALSTACK_ARN to run the localstack AWS SM lane")
	}

	r := New(endpoint)
	got, err := r.Resolve(context.Background(), vault.RefEnvelope{
		Backend: vault.BackendAWSSM,
		ID:      arn,
		JSONKey: "api_key",
	})
	if err != nil {
		t.Fatalf("Resolve against localstack: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("resolved secret is empty")
	}
}
