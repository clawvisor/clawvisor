package testacc

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorAgent_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_agent" "test" {
  name        = "acc-agent-basic"
  description = "created by acceptance test"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("clawvisor_agent.test", "id"),
					resource.TestCheckResourceAttrSet("clawvisor_agent.test", "token"),
					resource.TestCheckResourceAttr("clawvisor_agent.test", "name", "acc-agent-basic"),
					resource.TestCheckResourceAttr("clawvisor_agent.test", "description", "created by acceptance test"),
				),
			},
		},
	})
}

func TestAccClawvisorAgent_update(t *testing.T) {
	var tok1 string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_agent" "test" {
  name           = "acc-agent-update"
  rotate_trigger = "v1"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("clawvisor_agent.test", "token"),
					captureAttr("clawvisor_agent.test", "token", &tok1),
				),
			},
			{
				// Changing rotate_trigger exercises the in-place Update path
				// (token rotation) — id must be preserved, token must change.
				Config: `resource "clawvisor_agent" "test" {
  name           = "acc-agent-update"
  rotate_trigger = "v2"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("clawvisor_agent.test", "token", func(v string) error {
						if v == "" || v == tok1 {
							return fmt.Errorf("expected rotated token, got %q (was %q)", v, tok1)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccClawvisorAgent_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_agent" "test" {
  name = "acc-agent-import"
}`,
			},
			{
				ResourceName:      "clawvisor_agent.test",
				ImportState:       true,
				ImportStateVerify: true,
				// token is create-only (never re-fetchable); rotate_trigger is a
				// client-only signal — neither survives import.
				ImportStateVerifyIgnore: []string{"token", "rotate_trigger"},
			},
		},
	})
}

func TestAccClawvisorAgent_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_agent" "test" {
  name = "acc-agent-disappears"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					func(s *terraform.State) error {
						id, err := resourceID(s, "clawvisor_agent.test")
						if err != nil {
							return err
						}
						return accClient().DeleteAgent(accCtx(), id)
					},
				),
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
