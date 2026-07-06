package testacc

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorTaskPolicy_basic(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_task_policy" "test" {
  guidance = "Prefer read-only tools; ask before mutating production."
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_task_policy.test", "id", "task_policy"),
					resource.TestCheckResourceAttr("clawvisor_task_policy.test", "guidance", "Prefer read-only tools; ask before mutating production."),
				),
			},
		},
	})
}

func TestAccClawvisorTaskPolicy_update(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_task_policy" "test" {
  guidance = "first guidance"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_task_policy.test", "guidance", "first guidance"),
			},
			{
				Config: `resource "clawvisor_task_policy" "test" {
  guidance = "second, updated guidance"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_task_policy.test", "guidance", "second, updated guidance"),
			},
		},
	})
}

func TestAccClawvisorTaskPolicy_import(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_task_policy" "test" {
  guidance = "import guidance"
}`,
			},
			{
				ResourceName:      "clawvisor_task_policy.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     "task_policy",
			},
		},
	})
}

func TestAccClawvisorTaskPolicy_disappears(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_task_policy" "test" {
  guidance = "disappears guidance"
}`,
				Check: func(s *terraform.State) error {
					return accClient().DeleteTaskPolicy(accCtx())
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
