package testacc

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// requireLocalGov skips a governance-resource acceptance test cleanly when the
// target server does not report the `local_governance` capability. The four
// policy resources have no OSS server endpoints until spec 06a lands; their
// CRUD/import tests are written to the 06a route/JSON contract and will run
// automatically once the testapp server reports the capability.
func requireLocalGov(t *testing.T) {
	t.Helper()
	if !hasLocalGovernance {
		t.Skip(govSkipReason)
	}
}

// accClient returns a REST client authenticated with the suite's
// instance-admin token — used for out-of-band mutations in _disappears tests
// and secret-leak assertions.
func accClient() *client.Client {
	return client.New(testEndpoint, testToken, "", &http.Client{Timeout: 10 * time.Second})
}

func accCtx() context.Context { return context.Background() }

// resourceID reads the "id" attribute of a named resource from Terraform state.
func resourceID(s *terraform.State, name string) (string, error) {
	rs, ok := s.RootModule().Resources[name]
	if !ok {
		return "", fmt.Errorf("resource %s not found in state", name)
	}
	id := rs.Primary.Attributes["id"]
	if id == "" {
		id = rs.Primary.ID
	}
	if id == "" {
		return "", fmt.Errorf("resource %s has no id", name)
	}
	return id, nil
}

// resourceAttr reads an arbitrary attribute of a named resource from state.
func resourceAttr(s *terraform.State, name, attr string) (string, error) {
	rs, ok := s.RootModule().Resources[name]
	if !ok {
		return "", fmt.Errorf("resource %s not found in state", name)
	}
	return rs.Primary.Attributes[attr], nil
}

// captureAttr returns a TestCheckFunc that copies a resource attribute into
// dst (for comparing values across steps, e.g. token rotation).
func captureAttr(name, attr string, dst *string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		v, err := resourceAttr(s, name, attr)
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}
}
