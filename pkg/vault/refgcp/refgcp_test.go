package refgcp

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

func TestMapGCPError(t *testing.T) {
	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.NotFound, vault.ErrRefNotFound},
		{codes.PermissionDenied, vault.ErrRefAccessDenied},
		{codes.Unauthenticated, vault.ErrRefAccessDenied},
		{codes.ResourceExhausted, vault.ErrRefThrottled},
		{codes.Unavailable, vault.ErrRefThrottled},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			got := mapGCPError(status.Error(tc.code, "sensitive detail"))
			if !errors.Is(got, tc.want) {
				t.Fatalf("code %v: got %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}
