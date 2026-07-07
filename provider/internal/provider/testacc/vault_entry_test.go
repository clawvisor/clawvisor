package testacc

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorVaultEntry_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_vault_entry" "test" {
  service_id = "acc-vault-basic"
  value      = "s3cr3t-value"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_vault_entry.test", "id", "acc-vault-basic"),
					resource.TestCheckResourceAttr("clawvisor_vault_entry.test", "service_id", "acc-vault-basic"),
					resource.TestCheckResourceAttr("clawvisor_vault_entry.test", "value", "s3cr3t-value"),
				),
			},
		},
	})
}

func TestAccClawvisorVaultEntry_update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_vault_entry" "test" {
  service_id = "acc-vault-update"
  value      = "first-value"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_vault_entry.test", "value", "first-value"),
			},
			{
				Config: `resource "clawvisor_vault_entry" "test" {
  service_id = "acc-vault-update"
  value      = "second-value"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_vault_entry.test", "value", "second-value"),
			},
		},
	})
}

func TestAccClawvisorVaultEntry_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_vault_entry" "test" {
  service_id = "acc-vault-import"
  value      = "import-value"
}`,
			},
			{
				ResourceName:      "clawvisor_vault_entry.test",
				ImportState:       true,
				ImportStateVerify: true,
				// value is write-only server-side and cannot be recovered on import.
				ImportStateVerifyIgnore: []string{"value"},
			},
		},
	})
}

func TestAccClawvisorVaultEntry_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_vault_entry" "test" {
  service_id = "acc-vault-disappears"
  value      = "disappears-value"
}`,
				Check: func(s *terraform.State) error {
					id, err := resourceID(s, "clawvisor_vault_entry.test")
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

// TestAccClawvisorVaultEntry_neverEchoes proves the value only ever exists in
// state as the configured input, and that the server never echoes it back
// through Read.
func TestAccClawvisorVaultEntry_neverEchoes(t *testing.T) {
	const secret = "n3v3r-3ch03d-s3cr3t"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "clawvisor_vault_entry" "test" {
  service_id = "acc-vault-echo"
  value      = %q
}`, secret),
				Check: resource.ComposeAggregateTestCheckFunc(
					// State holds exactly the configured value (Read never rewrote it).
					resource.TestCheckResourceAttr("clawvisor_vault_entry.test", "value", secret),
					// The server's GET response carries only metadata — never the value.
					func(s *terraform.State) error {
						item, err := accClient().GetVaultEntry(accCtx(), "acc-vault-echo")
						if err != nil {
							return err
						}
						if item.Name == secret || item.ID == secret {
							return fmt.Errorf("server echoed the secret value in vault metadata")
						}
						return nil
					},
				),
			},
		},
	})
}
