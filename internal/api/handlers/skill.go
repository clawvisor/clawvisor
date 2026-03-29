package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// SkillHandler serves the dynamic skill catalog endpoint.
type SkillHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	logger     *slog.Logger
}

func NewSkillHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	logger *slog.Logger,
) *SkillHandler {
	return &SkillHandler{
		st: st, vault: v, adapterReg: adapterReg, logger: logger,
	}
}

// actionMeta provides static descriptions and parameter docs for each service action.
// These will move to the Adapter interface (ActionDescription/ActionSchema) in Phase 8.
var actionMeta = map[string]struct{ Desc, Params string }{
	// google.gmail
	"google.gmail:list_messages": {"List and search emails", "query (string, optional), max_results (int, default 10)"},
	"google.gmail:get_message":   {"Fetch a single email by ID", "message_id (string, required)"},
	"google.gmail:send_message":  {"Send an email", "to (string, required), subject (string, required), body (string, required), cc (string, optional)"},

	// google.calendar
	"google.calendar:list_events":    {"List upcoming calendar events", "calendar_id (string, default \"primary\"), from (date YYYY-MM-DD, optional), to (date YYYY-MM-DD, optional), max_results (int, default 10)"},
	"google.calendar:list_calendars": {"List all calendars", "(none)"},
	"google.calendar:get_event":      {"Fetch a single event by ID", "calendar_id (string, required), event_id (string, required)"},
	"google.calendar:create_event":   {"Create a calendar event", "calendar_id (string, required), summary (string, required), start (RFC3339 datetime, required), end (RFC3339 datetime, required), description (string, optional), attendees ([]string, optional)"},
	"google.calendar:update_event":   {"Update an existing event", "calendar_id (string, required), event_id (string, required), plus fields to update"},
	"google.calendar:delete_event":   {"Delete a calendar event", "calendar_id (string, required), event_id (string, required)"},

	// google.drive
	"google.drive:list_files":   {"List files matching a query", "query (string, optional), max_results (int, default 10)"},
	"google.drive:get_file":     {"Get file metadata and content", "file_id (string, required)"},
	"google.drive:create_file":  {"Create a file", "name (string, required), content (string, required), mime_type (string, required), parent_id (string, optional)"},
	"google.drive:update_file":  {"Update file content", "file_id (string, required), content (string, required)"},
	"google.drive:search_files": {"Search files by name or content", "query (string, required), max_results (int, default 10)"},

	// google.contacts
	"google.contacts:list_contacts":   {"List contacts", "query (string, optional), max_results (int, default 10)"},
	"google.contacts:get_contact":     {"Get a contact's details", "contact_id (string, required)"},
	"google.contacts:search_contacts": {"Search contacts by name or email", "query (string, required), max_results (int, default 10)"},

	// github
	"github:list_issues":  {"List issues on a repo", "owner (string, required), repo (string, required), state (string, optional)"},
	"github:get_issue":    {"Get issue details", "owner (string, required), repo (string, required), number (int, required)"},
	"github:create_issue": {"Create an issue", "owner (string, required), repo (string, required), title (string, required), body (string, required)"},
	"github:comment_issue": {"Add a comment to an issue", "owner (string, required), repo (string, required), number (int, required), body (string, required)"},
	"github:list_prs":     {"List pull requests", "owner (string, required), repo (string, required), state (string, optional)"},
	"github:get_pr":       {"Get PR details", "owner (string, required), repo (string, required), number (int, required)"},
	"github:list_repos":   {"List repositories", "org (string, optional)"},
	"github:search_code":  {"Search code", "query (string, required), repo (string, optional)"},

	// apple.imessage
	"apple.imessage:search_messages": {"Search messages by content/sender", "query (string, required), contact (string, optional), days_back (int, optional)"},
	"apple.imessage:list_threads":    {"List recent conversation threads", "max_results (int, optional)"},
	"apple.imessage:get_thread":      {"Get messages from a conversation", "contact (string) or thread_id (string), days_back (int, optional)"},
	"apple.imessage:send_message":    {"Send an iMessage (always requires approval)", "to (string, required), text (string, required)"},
}

// serviceDesc provides one-line summaries for the "not activated" section.
var serviceDesc = map[string]string{
	"google.gmail":     "Gmail — read, search, and send email",
	"google.calendar":  "Google Calendar — events and scheduling",
	"google.drive":     "Google Drive — file access (list, read, upload)",
	"google.contacts":  "Google Contacts — contact management",
	"github":           "GitHub — issues, PRs, and code review",
	"apple.imessage":   "iMessage — read and send messages (macOS only)",
}

// Catalog returns a personalized markdown document listing the agent's
// activated services (with restriction-filtered actions) and available services.
//
// GET /api/skill/catalog
// Auth: agent bearer token
func (h *SkillHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	// Determine which vault keys are activated for this user.
	activatedKeys, _ := h.vault.List(ctx, agent.UserID)
	keySet := make(map[string]bool, len(activatedKeys))
	for _, k := range activatedKeys {
		keySet[k] = true
	}

	// Load service metas for alias display.
	metas, _ := h.st.ListServiceMetas(ctx, agent.UserID)

	type catalogService struct {
		id        string // display ID, e.g. "google.gmail" or "google.gmail:personal"
		baseID    string // adapter service ID, e.g. "google.gmail"
		actions   []string
		activated bool
	}

	allAdapters := h.adapterReg.All()
	services := make([]catalogService, 0, len(allAdapters))
	for _, a := range allAdapters {
		credentialFree := a.ValidateCredential(nil) == nil
		if credentialFree {
			// Credential-free services (e.g. iMessage) require explicit
			// activation via service_meta, same as the gateway check.
			activated := false
			for _, m := range metas {
				if m.ServiceID == a.ServiceID() {
					activated = true
					break
				}
			}
			services = append(services, catalogService{
				id: a.ServiceID(), baseID: a.ServiceID(),
				actions: a.SupportedActions(), activated: activated,
			})
			continue
		}

		// Show each activated alias as a separate catalog entry.
		shown := false
		for _, m := range metas {
			if m.ServiceID != a.ServiceID() {
				continue
			}
			vKey := vaultKeyForServiceAlias(a.ServiceID(), m.Alias)
			if !keySet[vKey] {
				continue
			}
			shown = true
			displayID := a.ServiceID()
			if m.Alias != "default" {
				displayID = a.ServiceID() + ":" + m.Alias
			}
			services = append(services, catalogService{
				id: displayID, baseID: a.ServiceID(),
				actions: a.SupportedActions(), activated: true,
			})
		}

		// Fallback: check base vault key.
		baseKey := vaultKeyForService(a.ServiceID())
		if !shown && keySet[baseKey] {
			services = append(services, catalogService{
				id: a.ServiceID(), baseID: a.ServiceID(),
				actions: a.SupportedActions(), activated: true,
			})
			shown = true
		}

		if !shown {
			services = append(services, catalogService{
				id: a.ServiceID(), baseID: a.ServiceID(),
				actions: a.SupportedActions(), activated: false,
			})
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].id < services[j].id })

	var buf strings.Builder
	buf.WriteString("# Your Clawvisor Service Catalog\n\n")

	// Active services
	buf.WriteString("## Active Services\n")
	anyActive := false
	for _, svc := range services {
		if !svc.activated {
			continue
		}
		anyActive = true
		buf.WriteString(fmt.Sprintf("\n### %s\n\n", svc.id))
		for _, action := range svc.actions {
			// If a restriction matches this action (exact or base service), skip it.
			restriction, _ := h.st.MatchRestriction(ctx, agent.UserID, svc.id, action)
			if restriction == nil && svc.id != svc.baseID {
				restriction, _ = h.st.MatchRestriction(ctx, agent.UserID, svc.baseID, action)
			}
			if restriction != nil {
				continue
			}

			key := svc.baseID + ":" + action
			meta, hasMeta := actionMeta[key]

			// All non-restricted actions require approval unless covered by a task.
			label := " (requires approval or task)"

			if hasMeta {
				buf.WriteString(fmt.Sprintf("**%s**%s — %s\n", action, label, meta.Desc))
				buf.WriteString(fmt.Sprintf("  Params: %s\n\n", meta.Params))
			} else {
				buf.WriteString(fmt.Sprintf("**%s**%s\n\n", action, label))
			}
		}
	}
	if !anyActive {
		buf.WriteString("\nNo services are currently activated.\n")
	}

	// Available (not activated) services
	buf.WriteString("---\n\n")
	buf.WriteString("## Available Services (not activated)\n\n")
	anyAvailable := false
	for _, svc := range services {
		if svc.activated {
			continue
		}
		anyAvailable = true
		desc := serviceDesc[svc.id]
		if desc == "" {
			desc = svc.id
		}
		buf.WriteString(fmt.Sprintf("- **%s** — %s\n", svc.id, desc))
	}
	if !anyAvailable {
		buf.WriteString("All supported services are activated.\n")
	} else {
		buf.WriteString("\nTo activate a service, direct the user to the Clawvisor dashboard.\n")
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buf.String()))
}
