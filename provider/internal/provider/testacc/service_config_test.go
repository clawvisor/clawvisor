package testacc

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorServiceConfig_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_service_config" "test" {
  service_id = "acc-svc-basic"
  config     = jsonencode({ region = "us-east-1", retries = 3 })
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_service_config.test", "id", "acc-svc-basic:default"),
					resource.TestCheckResourceAttr("clawvisor_service_config.test", "alias", "default"),
					resource.TestCheckResourceAttr("clawvisor_service_config.test", "config", `{"region":"us-east-1","retries":3}`),
				),
			},
			{
				// Reordered keys must NOT produce a diff (jsontypes.Normalized).
				Config: `resource "clawvisor_service_config" "test" {
  service_id = "acc-svc-basic"
  config     = jsonencode({ retries = 3, region = "us-east-1" })
}`,
				PlanOnly: true,
			},
		},
	})
}

func TestAccClawvisorServiceConfig_update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_service_config" "test" {
  service_id = "acc-svc-update"
  config     = jsonencode({ mode = "a" })
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_service_config.test", "config", `{"mode":"a"}`),
			},
			{
				Config: `resource "clawvisor_service_config" "test" {
  service_id = "acc-svc-update"
  config     = jsonencode({ mode = "b", extra = true })
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_service_config.test", "config", `{"extra":true,"mode":"b"}`),
			},
		},
	})
}

func TestAccClawvisorServiceConfig_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_service_config" "test" {
  service_id = "acc-svc-import"
  config     = jsonencode({ key = "value" })
}`,
			},
			{
				ResourceName:      "clawvisor_service_config.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     "acc-svc-import:default",
			},
		},
	})
}

func TestAccClawvisorServiceConfig_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_service_config" "test" {
  service_id = "acc-svc-disappears"
  config     = jsonencode({ key = "value" })
}`,
				Check: func(s *terraform.State) error {
					serviceID, err := resourceAttr(s, "clawvisor_service_config.test", "service_id")
					if err != nil {
						return err
					}
					alias, err := resourceAttr(s, "clawvisor_service_config.test", "alias")
					if err != nil {
						return err
					}
					return accClient().DeleteServiceConfig(accCtx(), serviceID, alias)
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
