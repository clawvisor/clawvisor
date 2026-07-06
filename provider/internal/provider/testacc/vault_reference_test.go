package testacc

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The acceptance lane cannot resolve a real secret (no cloud creds), so the
// reference is created with verify=false: full CRUD + import of the reference
// RECORD is asserted; resolution itself is covered in the mocked unit/e2e
// layer. ref_id matches the server's reference_allowlist.
const (
	accRefARN  = "arn:aws:secretsmanager:us-east-1:123456789012:secret:acc/anthropic-x9Y"
	accRefARN2 = "arn:aws:secretsmanager:us-east-1:123456789012:secret:acc/openai-a1B"
)

func TestAccClawvisorVaultReference_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "clawvisor_vault_reference" "test" {
  service_id = "acc-ref-basic"
  backend    = "aws-sm"
  ref_id     = %q
  json_key   = "api_key"
  verify     = false
}`, accRefARN),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "id", "acc-ref-basic"),
					resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "service_id", "acc-ref-basic"),
					resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "backend", "aws-sm"),
					resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "ref_id", accRefARN),
					resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "json_key", "api_key"),
				),
			},
		},
	})
}

func TestAccClawvisorVaultReference_update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "clawvisor_vault_reference" "test" {
  service_id = "acc-ref-update"
  backend    = "aws-sm"
  ref_id     = %q
  verify     = false
}`, accRefARN),
				Check: resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "ref_id", accRefARN),
			},
			{
				Config: fmt.Sprintf(`resource "clawvisor_vault_reference" "test" {
  service_id = "acc-ref-update"
  backend    = "aws-sm"
  ref_id     = %q
  verify     = false
}`, accRefARN2),
				Check: resource.TestCheckResourceAttr("clawvisor_vault_reference.test", "ref_id", accRefARN2),
			},
		},
	})
}

func TestAccClawvisorVaultReference_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "clawvisor_vault_reference" "test" {
  service_id = "acc-ref-import"
  backend    = "aws-sm"
  ref_id     = %q
  verify     = false
}`, accRefARN),
			},
			{
				ResourceName:      "clawvisor_vault_reference.test",
				ImportState:       true,
				ImportStateVerify: true,
				// backend/ref_id/json_key/verify are not recoverable on import
				// (the server does not expose the stored envelope); config
				// re-supplies them.
				ImportStateVerifyIgnore: []string{"backend", "ref_id", "json_key", "verify"},
			},
		},
	})
}

func TestAccClawvisorVaultReference_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "clawvisor_vault_reference" "test" {
  service_id = "acc-ref-disappears"
  backend    = "aws-sm"
  ref_id     = %q
  verify     = false
}`, accRefARN),
				Check: func(s *terraform.State) error {
					id, err := resourceID(s, "clawvisor_vault_reference.test")
					if err != nil {
						return err
					}
					return accClient().DeleteVaultEntry(accCtx(), id)
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccVaultReference_verifyFailsApply proves verify=true surfaces the
// actionable server error through terraform apply when the target is not on
// the allowlist (deterministic — needs no cloud creds).
func TestAccVaultReference_verifyFailsApply(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				// The id does not match any reference_allowlist prefix, so it is
				// rejected at create time WITHOUT contacting any backend.
				Config: `resource "clawvisor_vault_reference" "test" {
  service_id = "acc-ref-badtarget"
  backend    = "aws-sm"
  ref_id     = "arn:aws:kms:us-east-1:999:key/not-on-allowlist"
  verify     = true
}`,
				ExpectError: regexp.MustCompile(`REF_TARGET_NOT_ALLOWED|not permitted`),
			},
		},
	})
}
