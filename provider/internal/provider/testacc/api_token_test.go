package testacc

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorAPIToken_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_api_token" "test" {
  name = "acc-token-basic"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("clawvisor_api_token.test", "id"),
					resource.TestCheckResourceAttrSet("clawvisor_api_token.test", "token"),
					resource.TestCheckResourceAttr("clawvisor_api_token.test", "name", "acc-token-basic"),
					resource.TestCheckResourceAttr("clawvisor_api_token.test", "scope", "instance-admin"),
				),
			},
		},
	})
}

func TestAccClawvisorAPIToken_update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_api_token" "test" {
  name = "acc-token-v1"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_api_token.test", "name", "acc-token-v1"),
			},
			{
				// name is RequiresReplace (no update endpoint) — this converges
				// via replacement and must leave an empty plan afterward.
				Config: `resource "clawvisor_api_token" "test" {
  name = "acc-token-v2"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_api_token.test", "name", "acc-token-v2"),
					resource.TestCheckResourceAttrSet("clawvisor_api_token.test", "token"),
				),
			},
		},
	})
}

func TestAccClawvisorAPIToken_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_api_token" "test" {
  name = "acc-token-import"
}`,
			},
			{
				ResourceName:            "clawvisor_api_token.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"token", "expires_at"},
			},
		},
	})
}

func TestAccClawvisorAPIToken_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_api_token" "test" {
  name = "acc-token-disappears"
}`,
				Check: func(s *terraform.State) error {
					id, err := resourceID(s, "clawvisor_api_token.test")
					if err != nil {
						return err
					}
					return accClient().DeleteToken(accCtx(), id)
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
