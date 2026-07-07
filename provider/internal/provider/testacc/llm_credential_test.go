package testacc

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The testapp server enables proxy_lite, so it reports the secret_vault
// capability and registers the llm-credentials routes; the provider
// authenticates with the suite's instance-admin token, which resolves to the
// shared _instance scope. Push-mode CRUD + import run green here; reference
// mode is asserted at the schema/allowlist boundary only (no cloud creds).

func TestAccClawvisorLLMCredential_push(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_llm_credential" "test" {
  llm_provider = "anthropic"
  api_key      = "sk-ant-api03-acc-push-first"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_llm_credential.test", "id", "anthropic"),
					resource.TestCheckResourceAttr("clawvisor_llm_credential.test", "llm_provider", "anthropic"),
					resource.TestCheckResourceAttr("clawvisor_llm_credential.test", "api_key", "sk-ant-api03-acc-push-first"),
				),
			},
			{
				// In-place update (llm_provider is RequiresReplace; api_key is not).
				Config: `resource "clawvisor_llm_credential" "test" {
  llm_provider = "anthropic"
  api_key      = "sk-ant-api03-acc-push-rotated"
}`,
				Check: resource.TestCheckResourceAttr("clawvisor_llm_credential.test", "api_key", "sk-ant-api03-acc-push-rotated"),
			},
		},
	})
}

func TestAccClawvisorLLMCredential_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_llm_credential" "test" {
  llm_provider = "openai"
  api_key      = "sk-proj-acc-import-key"
}`,
			},
			{
				ResourceName:      "clawvisor_llm_credential.test",
				ImportState:       true,
				ImportStateId:     "openai",
				ImportStateVerify: true,
				// api_key is write-only server-side and cannot be recovered on import.
				ImportStateVerifyIgnore: []string{"api_key"},
			},
		},
	})
}

func TestAccClawvisorLLMCredential_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_llm_credential" "test" {
  llm_provider = "google"
  api_key      = "AIza-acc-disappears-key"
}`,
				Check: func(s *terraform.State) error {
					if _, err := resourceID(s, "clawvisor_llm_credential.test"); err != nil {
						return err
					}
					return accClient().DeleteLLMCredential(accCtx(), "google", "")
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccClawvisorLLMCredential_referenceValidation asserts the reference-mode
// boundary without any cloud credentials: the api_key XOR reference schema rule
// (plan-time) and the server's allowlist fail-closed (create-time, rejected
// before any backend call).
func TestAccClawvisorLLMCredential_referenceValidation(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				// Both api_key and reference set → rejected at plan by ValidateConfig.
				Config: `resource "clawvisor_llm_credential" "test" {
  llm_provider = "anthropic"
  api_key      = "sk-ant-api03-x"
  reference = {
    backend = "aws-sm"
    ref_id  = "arn:aws:secretsmanager:us-east-1:1:secret:x"
  }
}`,
				ExpectError: regexp.MustCompile(`[Ee]xactly one of api_key or reference`),
			},
			{
				// Non-allowlisted reference target → server rejects at create
				// (verify runs the allowlist check before contacting any backend).
				Config: `resource "clawvisor_llm_credential" "test" {
  llm_provider = "anthropic"
  reference = {
    backend = "aws-sm"
    ref_id  = "arn:aws:kms:us-east-1:999:key/not-on-allowlist"
  }
}`,
				ExpectError: regexp.MustCompile(`REF_TARGET_NOT_ALLOWED|not permitted`),
			},
		},
	})
}
