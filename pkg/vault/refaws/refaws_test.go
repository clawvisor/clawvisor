package refaws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smithy "github.com/aws/smithy-go"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

type fakeSM struct {
	out *secretsmanager.GetSecretValueOutput
	err error
	got string
}

func (f *fakeSM) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.got = aws.ToString(in.SecretId)
	return f.out, f.err
}

func TestResolve_SecretString_RawAndJSONKey(t *testing.T) {
	ctx := context.Background()

	raw := &fakeSM{out: &secretsmanager.GetSecretValueOutput{SecretString: aws.String("sk-ant-raw")}}
	r := newWithClient(raw)
	got, err := r.Resolve(ctx, vault.RefEnvelope{Backend: vault.BackendAWSSM, ID: "arn:aws:x"})
	if err != nil {
		t.Fatalf("Resolve raw: %v", err)
	}
	if string(got) != "sk-ant-raw" {
		t.Fatalf("raw = %q, want sk-ant-raw", got)
	}
	if raw.got != "arn:aws:x" {
		t.Fatalf("SecretId = %q, want arn:aws:x", raw.got)
	}

	js := &fakeSM{out: &secretsmanager.GetSecretValueOutput{SecretString: aws.String(`{"api_key":"sk-ant-json"}`)}}
	rj := newWithClient(js)
	got, err = rj.Resolve(ctx, vault.RefEnvelope{Backend: vault.BackendAWSSM, ID: "arn:aws:x", JSONKey: "api_key"})
	if err != nil {
		t.Fatalf("Resolve json: %v", err)
	}
	if string(got) != "sk-ant-json" {
		t.Fatalf("json = %q, want sk-ant-json", got)
	}
}

func TestResolve_ErrorMapping(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"ResourceNotFoundException", vault.ErrRefNotFound},
		{"AccessDeniedException", vault.ErrRefAccessDenied},
		{"ThrottlingException", vault.ErrRefThrottled},
		{"InternalServiceError", vault.ErrRefThrottled},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			f := &fakeSM{err: &smithy.GenericAPIError{Code: tc.code, Message: "sensitive detail"}}
			r := newWithClient(f)
			_, err := r.Resolve(context.Background(), vault.RefEnvelope{Backend: vault.BackendAWSSM, ID: "arn:aws:x"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("code %s: got %v, want %v", tc.code, err, tc.want)
			}
		})
	}
}
