package testacc

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccClawvisorContentPolicy_basic(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_content_policy" "test" {
  name          = "block-ssn"
  pattern       = "\\d{3}-\\d{2}-\\d{4}"
  pattern_kind  = "regex"
  action        = "block"
  block_message = "SSN-shaped content is not allowed."
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("clawvisor_content_policy.test", "id"),
					resource.TestCheckResourceAttr("clawvisor_content_policy.test", "name", "block-ssn"),
					resource.TestCheckResourceAttr("clawvisor_content_policy.test", "action", "block"),
					// enabled has a server-side default of true; the schema default
					// keeps omitted != unknown (no perma-diff).
					resource.TestCheckResourceAttr("clawvisor_content_policy.test", "enabled", "true"),
				),
			},
		},
	})
}

func TestAccClawvisorContentPolicy_update(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_content_policy" "test" {
  name         = "flag-secret"
  pattern      = "secret"
  pattern_kind = "keyword"
  action       = "flag"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_content_policy.test", "action", "flag"),
			},
			{
				Config: `resource "clawvisor_content_policy" "test" {
  name          = "flag-secret"
  pattern       = "topsecret"
  pattern_kind  = "keyword"
  action        = "block"
  block_message = "no secrets"
  enabled       = false
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_content_policy.test", "pattern", "topsecret"),
					resource.TestCheckResourceAttr("clawvisor_content_policy.test", "action", "block"),
					resource.TestCheckResourceAttr("clawvisor_content_policy.test", "enabled", "false"),
				),
			},
		},
	})
}

func TestAccClawvisorContentPolicy_import(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_content_policy" "test" {
  name         = "import-rule"
  pattern      = "confidential"
  pattern_kind = "keyword"
  action       = "flag"
}`,
			},
			{
				ResourceName:      "clawvisor_content_policy.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccClawvisorContentPolicy_disappears(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_content_policy" "test" {
  name         = "disappears-rule"
  pattern      = "gone"
  pattern_kind = "keyword"
  action       = "flag"
}`,
				Check: func(s *terraform.State) error {
					id, err := resourceID(s, "clawvisor_content_policy.test")
					if err != nil {
						return err
					}
					return accClient().DeleteContentPolicy(accCtx(), id)
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
