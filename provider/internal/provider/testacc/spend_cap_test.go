package testacc

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorSpendCap_basic(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_spend_cap" "test" {
  window     = "daily"
  cap_micros = 5000000
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "id", "daily"),
					resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "window", "daily"),
					resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "cap_micros", "5000000"),
					resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "enforcement", "soft"),
				),
			},
		},
	})
}

func TestAccClawvisorSpendCap_update(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_spend_cap" "test" {
  window     = "daily"
  cap_micros = 5000000
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "enforcement", "soft"),
			},
			{
				Config: `resource "clawvisor_spend_cap" "test" {
  window      = "daily"
  cap_micros  = 9000000
  enforcement = "hard"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "cap_micros", "9000000"),
					resource.TestCheckResourceAttr("clawvisor_spend_cap.test", "enforcement", "hard"),
				),
			},
		},
	})
}

func TestAccClawvisorSpendCap_import(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_spend_cap" "test" {
  window     = "monthly"
  cap_micros = 100000000
}`,
			},
			{
				ResourceName:      "clawvisor_spend_cap.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     "monthly",
			},
		},
	})
}

func TestAccClawvisorSpendCap_disappears(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_spend_cap" "test" {
  window     = "daily"
  cap_micros = 5000000
}`,
				Check: func(s *terraform.State) error {
					return accClient().DeleteSpendCap(accCtx(), "daily")
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
