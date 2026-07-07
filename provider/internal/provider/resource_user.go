package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// inviteFieldPlanModifier keeps a computed invite field stable in the plan
// (like UseStateForUnknown) EXCEPT when `role` changes, in which case the
// field is left unknown so Update produces a value consistent with the plan.
// A role change on a claimed user is an in-place PUT that leaves these fields
// alone (Update copies them back from state); a role change on a still-pending
// invite re-mints it, producing fresh id/invite_url/expires_at. Marking them
// unknown up front makes both paths consistent — plain UseStateForUnknown
// would pin the plan to the old values and error on the re-mint path.
// Mirrors clawvisor_agent's tokenPlanModifier (keyed on rotate_trigger).
type inviteFieldPlanModifier struct{}

func (m inviteFieldPlanModifier) Description(_ context.Context) string {
	return "Preserves the value unless role changes, in which case it is known after apply."
}

func (m inviteFieldPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m inviteFieldPlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// Create (no prior state) and known planned values need no adjustment.
	if req.StateValue.IsNull() || !req.PlanValue.IsUnknown() {
		return
	}
	var stateRole, planRole types.String
	req.State.GetAttribute(ctx, path.Root("role"), &stateRole)
	req.Plan.GetAttribute(ctx, path.Root("role"), &planRole)
	if !stateRole.Equal(planRole) {
		// role changed → value may be re-minted; leave it unknown.
		return
	}
	resp.PlanValue = req.StateValue
}

type userResource struct {
	pd *providerData
}

type userModel struct {
	ID        types.String `tfsdk:"id"`
	Email     types.String `tfsdk:"email"`
	Role      types.String `tfsdk:"role"`
	InviteURL types.String `tfsdk:"invite_url"`
	ExpiresAt types.String `tfsdk:"expires_at"`
}

var (
	_ resource.Resource                = (*userResource)(nil)
	_ resource.ResourceWithConfigure   = (*userResource)(nil)
	_ resource.ResourceWithImportState = (*userResource)(nil)
)

// NewUserResource is the resource factory.
func NewUserResource() resource.Resource { return &userResource{} }

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An employee identity, enrolled via a one-shot invite. Create mints a pinned " +
			"invite and exposes its computed, sensitive `invite_url`; deliver that URL to the employee " +
			"over a secure channel (it is a one-shot bearer credential). Destroying the resource offboards " +
			"the employee — it immediately invalidates their `cvis_` agent tokens server-side while " +
			"retaining audit/cost history. To reissue an expired invite, taint the resource (or delete + " +
			"recreate) for a fresh `invite_url`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Server-assigned id. While the invite is pending this is the invite id; " +
					"once the employee claims it, it becomes the user id.",
				PlanModifiers: []planmodifier.String{inviteFieldPlanModifier{}},
			},
			"email": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The employee's email. Pins the invite to this address. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"role": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The employee's role: `member` or `admin`. Changed in place for a claimed " +
					"user (a pinned admin invite is downgraded to member at claim time, so an admin role is " +
					"granted here on the next apply after enrollment).",
			},
			"invite_url": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The one-shot enrollment URL (`<base>/join?token=cvinv_...`), returned only " +
					"at create/reissue and stored in state; treat state as sensitive. Unavailable on import (null).",
				PlanModifiers: []planmodifier.String{inviteFieldPlanModifier{}},
			},
			"expires_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC3339 expiry of the invite (default < 48h). Unavailable on import (null).",
				PlanModifiers:       []planmodifier.String{inviteFieldPlanModifier{}},
			},
		},
	}
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityUserManagement, "clawvisor_user", &resp.Diagnostics) {
		return
	}
	var plan userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	inv, err := r.pd.client.CreateInvite(ctx, client.CreateInviteRequest{
		Email: plan.Email.ValueString(),
		Role:  plan.Role.ValueString(),
	})
	if err != nil {
		diagFromError("Creating clawvisor_user", err, &resp.Diagnostics)
		return
	}

	plan.ID = types.StringValue(inv.ID)
	plan.Role = types.StringValue(inv.Role)
	plan.InviteURL = types.StringValue(inv.InviteURL)
	plan.ExpiresAt = types.StringValue(inv.ExpiresAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityUserManagement, "clawvisor_user", &resp.Diagnostics) {
		return
	}
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The identity is in one of two states. Resolve by id first (claimed user
	// or pending invite), then fall back to email — when a pending invite is
	// claimed the invite id disappears and a NEW user id materializes, so an
	// id-only lookup would spuriously drop the resource; the email fallback
	// adopts the claimed user id instead. invite_url / expires_at are one-shot
	// and never returned by a read, so they are preserved from state untouched.
	users, err := r.pd.client.ListUsers(ctx)
	if err != nil {
		diagFromError("Reading clawvisor_user", err, &resp.Diagnostics)
		return
	}
	if u := userByID(users, state.ID.ValueString()); u != nil {
		state.Email = types.StringValue(u.Email)
		state.Role = types.StringValue(u.Role)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	invites, err := r.pd.client.ListPendingInvites(ctx)
	if err != nil {
		diagFromError("Reading clawvisor_user", err, &resp.Diagnostics)
		return
	}
	if inv := inviteByID(invites, state.ID.ValueString()); inv != nil {
		state.Email = types.StringValue(inv.Email)
		state.Role = types.StringValue(inv.Role)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	if email := state.Email.ValueString(); email != "" {
		if u := userByEmail(users, email); u != nil {
			// The pending invite was claimed: adopt the new user id.
			state.ID = types.StringValue(u.ID)
			state.Email = types.StringValue(u.Email)
			state.Role = types.StringValue(u.Role)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}

	resp.State.RemoveResource(ctx)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !requireCapability(r.pd, client.CapabilityUserManagement, "clawvisor_user", &resp.Diagnostics) {
		return
	}
	var plan, state userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// email is RequiresReplace, so the only in-place change that reaches Update
	// is a role change. Resolve the concrete target.
	users, err := r.pd.client.ListUsers(ctx)
	if err != nil {
		diagFromError("Updating clawvisor_user", err, &resp.Diagnostics)
		return
	}
	user := userByID(users, state.ID.ValueString())
	if user == nil {
		if email := state.Email.ValueString(); email != "" {
			user = userByEmail(users, email)
		}
	}

	if user != nil {
		// Claimed user → change the role in place.
		if err := r.pd.client.UpdateUserRole(ctx, user.ID, plan.Role.ValueString()); err != nil {
			diagFromError("Updating clawvisor_user role", err, &resp.Diagnostics)
			return
		}
		plan.ID = types.StringValue(user.ID)
		plan.InviteURL = state.InviteURL
		plan.ExpiresAt = state.ExpiresAt
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	// Still-pending invite → its role is fixed at mint, so a role change means
	// re-minting the invite (the old URL is invalidated). The plan modifier
	// left id/invite_url/expires_at unknown, so the fresh values are consistent.
	invites, err := r.pd.client.ListPendingInvites(ctx)
	if err != nil {
		diagFromError("Updating clawvisor_user", err, &resp.Diagnostics)
		return
	}
	if inv := inviteByEmailOrID(invites, state.Email.ValueString(), state.ID.ValueString()); inv != nil {
		if err := r.pd.client.DeleteInvite(ctx, inv.ID); err != nil && !client.NotFound(err) {
			diagFromError("Reissuing clawvisor_user invite", err, &resp.Diagnostics)
			return
		}
	}
	newInv, err := r.pd.client.CreateInvite(ctx, client.CreateInviteRequest{
		Email: plan.Email.ValueString(),
		Role:  plan.Role.ValueString(),
	})
	if err != nil {
		diagFromError("Reissuing clawvisor_user invite", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(newInv.ID)
	plan.Role = types.StringValue(newInv.Role)
	plan.InviteURL = types.StringValue(newInv.InviteURL)
	plan.ExpiresAt = types.StringValue(newInv.ExpiresAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !requireCapability(r.pd, client.CapabilityUserManagement, "clawvisor_user", &resp.Diagnostics) {
		return
	}
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Offboard the claimed user if one exists (resolve by id, then email);
	// otherwise revoke the still-pending invite.
	users, err := r.pd.client.ListUsers(ctx)
	if err != nil {
		diagFromError("Deleting clawvisor_user", err, &resp.Diagnostics)
		return
	}
	user := userByID(users, state.ID.ValueString())
	if user == nil {
		if email := state.Email.ValueString(); email != "" {
			user = userByEmail(users, email)
		}
	}
	if user != nil {
		if err := r.pd.client.DeleteUser(ctx, user.ID); err != nil && !client.NotFound(err) {
			diagFromError("Deleting clawvisor_user", err, &resp.Diagnostics)
		}
		return
	}

	invites, err := r.pd.client.ListPendingInvites(ctx)
	if err != nil {
		diagFromError("Deleting clawvisor_user", err, &resp.Diagnostics)
		return
	}
	if inv := inviteByEmailOrID(invites, state.Email.ValueString(), state.ID.ValueString()); inv != nil {
		if err := r.pd.client.DeleteInvite(ctx, inv.ID); err != nil && !client.NotFound(err) {
			diagFromError("Deleting clawvisor_user invite", err, &resp.Diagnostics)
		}
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by user id (or, for a still-pending identity, the invite id). Read
	// resolves email/role from it; invite_url/expires_at are one-shot and
	// unavailable on import (they stay null).
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}

func userByID(users []client.User, id string) *client.User {
	if id == "" {
		return nil
	}
	for i := range users {
		if users[i].ID == id {
			return &users[i]
		}
	}
	return nil
}

func userByEmail(users []client.User, email string) *client.User {
	if email == "" {
		return nil
	}
	for i := range users {
		if users[i].Email == email {
			return &users[i]
		}
	}
	return nil
}

func inviteByID(invites []client.UserInvite, id string) *client.UserInvite {
	if id == "" {
		return nil
	}
	for i := range invites {
		if invites[i].ID == id {
			return &invites[i]
		}
	}
	return nil
}

func inviteByEmailOrID(invites []client.UserInvite, email, id string) *client.UserInvite {
	if email != "" {
		for i := range invites {
			if invites[i].Email == email {
				return &invites[i]
			}
		}
	}
	return inviteByID(invites, id)
}
