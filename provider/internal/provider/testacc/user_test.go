package testacc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The acceptance lane never claims invites (no employee magic-link flow), so
// every clawvisor_user here stays a PENDING invite. That exercises the
// pending-invite side of Read/Update/Delete; the claimed-user path (email
// fallback + in-place role PUT) is covered by the server-side enroll e2e and
// the client's own tests.

func TestAccClawvisorUser_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-basic@example.com"
  role  = "member"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("clawvisor_user.test", "id"),
					resource.TestCheckResourceAttr("clawvisor_user.test", "email", "acc-user-basic@example.com"),
					resource.TestCheckResourceAttr("clawvisor_user.test", "role", "member"),
					resource.TestCheckResourceAttrSet("clawvisor_user.test", "expires_at"),
					// invite_url is the one-shot enrollment credential.
					resource.TestCheckResourceAttrWith("clawvisor_user.test", "invite_url", func(v string) error {
						if !strings.Contains(v, "/join?token=cvinv_") {
							return fmt.Errorf("invite_url %q missing /join?token=cvinv_ shape", v)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccClawvisorUser_updateRole(t *testing.T) {
	var url1 string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-role@example.com"
  role  = "member"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_user.test", "role", "member"),
					captureAttr("clawvisor_user.test", "invite_url", &url1),
				),
			},
			{
				// A role change on a still-pending invite re-mints it: role flips
				// and invite_url must change (the old URL is invalidated).
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-role@example.com"
  role  = "admin"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("clawvisor_user.test", "role", "admin"),
					resource.TestCheckResourceAttrWith("clawvisor_user.test", "invite_url", func(v string) error {
						if v == "" || v == url1 {
							return fmt.Errorf("expected a re-minted invite_url, got %q (was %q)", v, url1)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccClawvisorUser_import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-import@example.com"
  role  = "member"
}`,
			},
			{
				ResourceName:      "clawvisor_user.test",
				ImportState:       true,
				ImportStateVerify: true,
				// invite_url / expires_at are one-shot and not recoverable on
				// import (they come back null).
				ImportStateVerifyIgnore: []string{"invite_url", "expires_at"},
			},
		},
	})
}

func TestAccClawvisorUser_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-disappears@example.com"
  role  = "member"
}`,
				Check: func(s *terraform.State) error {
					id, err := resourceID(s, "clawvisor_user.test")
					if err != nil {
						return err
					}
					// The identity is a pending invite in the acceptance lane;
					// revoke it out of band.
					return accClient().DeleteInvite(accCtx(), id)
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccClawvisorUser_reissueViaTaint proves the guide's invite-reissue path
// (§5): tainting the resource destroys + recreates it, yielding a fresh
// invite_url.
func TestAccClawvisorUser_reissueViaTaint(t *testing.T) {
	var url1 string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-reissue@example.com"
  role  = "member"
}`,
				Check: captureAttr("clawvisor_user.test", "invite_url", &url1),
			},
			{
				Taint: []string{"clawvisor_user.test"},
				Config: `resource "clawvisor_user" "test" {
  email = "acc-user-reissue@example.com"
  role  = "member"
}`,
				Check: resource.TestCheckResourceAttrWith("clawvisor_user.test", "invite_url", func(v string) error {
					if v == "" || v == url1 {
						return fmt.Errorf("expected a fresh invite_url after taint, got %q (was %q)", v, url1)
					}
					return nil
				}),
			},
		},
	})
}
