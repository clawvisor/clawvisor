package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// referenceInput is the wire shape of a vault reference in a credential-write
// body: {"backend": "...", "id": "...", "json_key": "..."}. It is an
// alternative to the plain {"value": "..."} push path (spec 10, PRD §7).
type referenceInput struct {
	Backend string `json:"backend"`
	ID      string `json:"id"`
	JSONKey string `json:"json_key,omitempty"`
}

// referenceVaultAPI is the concrete surface a ReferenceVault exposes beyond the
// Vault interface. Handlers reach it by type assertion so every non-reference
// Vault implementer keeps compiling (spec guardrail: SetReference is not on the
// interface).
type referenceVaultAPI interface {
	SetReference(ctx context.Context, userID, serviceID string, env vault.RefEnvelope) error
	Verify(ctx context.Context, env vault.RefEnvelope) error
}

// refAPIError carries a mapped HTTP status + stable code + actionable,
// content-free message for a reference failure.
type refAPIError struct {
	status int
	code   string
	msg    string
}

func (e *refAPIError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.msg)
}

// callerIsInstanceAdmin reports whether the request was authenticated as an
// instance-admin API token (the only admin credential in this build). Plain
// user JWTs are members. When spec 04 lands an admin JWT role, extend this to
// also accept an admin-role user — reference creation must remain admin-gated
// either way (confused-deputy control, spec 10 D3.2).
func callerIsInstanceAdmin(ctx context.Context) bool {
	return middleware.APITokenFromContext(ctx) != nil
}

// storeReference validates the admin gate, backend, and allowlist, optionally
// performs a fail-fast verify resolve (discarding the plaintext), then stores
// the encrypted ref envelope under (userID, serviceID). It returns nil on
// success or a mapped *refAPIError. It NEVER writes, logs, or returns the
// resolved secret value.
func storeReference(ctx context.Context, v vault.Vault, isAdmin bool, userID, serviceID string, in referenceInput, verify bool) *refAPIError {
	if !isAdmin {
		// A plain member cannot mint a reference to an instance-readable
		// secret. Generic 403 — do not reveal the allowlist or the gate detail.
		return &refAPIError{http.StatusForbidden, "REFERENCE_ADMIN_REQUIRED",
			"creating a vault reference requires an instance-admin API token"}
	}
	rv, ok := v.(referenceVaultAPI)
	if !ok {
		return &refAPIError{http.StatusConflict, "REFERENCE_UNSUPPORTED",
			"this vault backend does not support external-secret references"}
	}
	if in.Backend == "" || in.ID == "" {
		return &refAPIError{http.StatusBadRequest, "INVALID_REQUEST",
			"reference requires a backend and an id"}
	}
	env := vault.RefEnvelope{Backend: in.Backend, ID: in.ID, JSONKey: in.JSONKey}

	if verify {
		if err := rv.Verify(ctx, env); err != nil {
			return mapReferenceError(err)
		}
	}
	if err := rv.SetReference(ctx, userID, serviceID, env); err != nil {
		return mapReferenceError(err)
	}
	return nil
}

// mapReferenceError converts a vault reference error into an HTTP response.
// The message for each typed class is the (content-free) sentinel text; an
// unrecognized error collapses to a generic 502 so no backend detail or secret
// structure leaks to the client.
func mapReferenceError(err error) *refAPIError {
	switch {
	case errors.Is(err, vault.ErrRefTargetNotAllowed):
		return &refAPIError{http.StatusBadRequest, "REF_TARGET_NOT_ALLOWED", err.Error()}
	case errors.Is(err, vault.ErrRefBackendUnknown):
		return &refAPIError{http.StatusBadRequest, "REF_BACKEND_UNKNOWN", err.Error()}
	case errors.Is(err, vault.ErrRefNotFound):
		return &refAPIError{http.StatusNotFound, "REF_NOT_FOUND", err.Error()}
	case errors.Is(err, vault.ErrRefAccessDenied):
		return &refAPIError{http.StatusForbidden, "REF_ACCESS_DENIED", err.Error()}
	case errors.Is(err, vault.ErrRefThrottled):
		return &refAPIError{http.StatusServiceUnavailable, "REF_THROTTLED", err.Error()}
	case errors.Is(err, vault.ErrRefKeyMissing):
		return &refAPIError{http.StatusUnprocessableEntity, "REF_KEY_MISSING", err.Error()}
	default:
		return &refAPIError{http.StatusBadGateway, "REF_RESOLUTION_FAILED",
			"resolving the referenced secret failed; check the reference and the instance's access to it"}
	}
}

// wantsVerify reports whether the request asked for a dry-run resolve
// (?verify=1 / ?verify=true).
func wantsVerify(r *http.Request) bool {
	switch r.URL.Query().Get("verify") {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
