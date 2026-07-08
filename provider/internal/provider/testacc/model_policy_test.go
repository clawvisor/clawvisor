package testacc

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The four governance policy resources (model_policy, spend_cap,
// content_policy, task_policy) target the /api/governance/* routes that spec
// 06a lands in a later wave. Until the testapp server reports the
// `local_governance` capability, these tests skip cleanly (requireLocalGov).
// They are written to 06a's exact route/JSON contract so they pass unchanged
// once 06a is merged and the capability flips true.

func TestAccClawvisorModelPolicy_basic(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_model_policy" "test" {
  mode   = "deny"
  models = ["anthropic/claude-3-opus"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_model_policy.test", "id", "model_policy"),
					resource.TestCheckResourceAttr("clawvisor_model_policy.test", "mode", "deny"),
					resource.TestCheckResourceAttr("clawvisor_model_policy.test", "models.0", "anthropic/claude-3-opus"),
				),
			},
		},
	})
}

func TestAccClawvisorModelPolicy_update(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_model_policy" "test" {
  mode   = "deny"
  models = ["anthropic/claude-3-opus"]
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_model_policy.test", "mode", "deny"),
			},
			{
				Config: `resource "clawvisor_model_policy" "test" {
  mode   = "allow"
  models = ["anthropic/claude-3-5-sonnet", "openai/gpt-4o"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_model_policy.test", "mode", "allow"),
					resource.TestCheckResourceAttr("clawvisor_model_policy.test", "models.#", "2"),
				),
			},
		},
	})
}

func TestAccClawvisorModelPolicy_import(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_model_policy" "test" {
  mode   = "deny"
  models = ["anthropic/claude-3-opus"]
}`,
			},
			{
				ResourceName:      "clawvisor_model_policy.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     "model_policy",
			},
		},
	})
}

func TestAccClawvisorModelPolicy_disappears(t *testing.T) {
	requireLocalGov(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_model_policy" "test" {
  mode   = "deny"
  models = ["anthropic/claude-3-opus"]
}`,
				Check: func(s *terraform.State) error {
					return accClient().DeleteModelPolicy(accCtx())
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
