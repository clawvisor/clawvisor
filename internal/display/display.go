// Package display provides human-readable names for services and actions.
// It is the single source of truth used by both API responses and notifications.
// When Init is called with an adapter registry, metadata is read from adapters
// that implement MetadataProvider; hardcoded maps serve as fallbacks.
package display

import (
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

var registry *adapters.Registry

// Init sets the adapter registry for metadata lookups. Call once at startup
// after all adapters are registered.
func Init(reg *adapters.Registry) {
	registry = reg
}

var serviceNames = map[string]string{
	"google.gmail":     "Gmail",
	"google.calendar":  "Google Calendar",
	"google.drive":     "Google Drive",
	"google.contacts":  "Google Contacts",
	"github":           "GitHub",
	"apple.imessage":   "iMessage",
	"slack":            "Slack",
	"notion":           "Notion",
	"linear":           "Linear",
	"stripe":           "Stripe",
	"twilio":           "Twilio",
}

var serviceDescriptions = map[string]string{
	"google.gmail":     "Read, search, and send email",
	"google.calendar":  "View and manage calendar events",
	"google.drive":     "List, search, and manage files",
	"google.contacts":  "Search and view contacts",
	"github":           "Issues, PRs, and code review",
	"apple.imessage":   "Search and read iMessage threads",
	"slack":            "Channels, messages, and search",
	"notion":           "Pages, databases, and search",
	"linear":           "Issues, projects, and teams",
	"stripe":           "Customers, charges, and subscriptions",
	"twilio":           "SMS, WhatsApp, and messaging",
}

var actionNames = map[string]string{
	// Gmail
	"list_messages": "List messages",
	"get_message":   "Get message",
	"send_message":  "Send message",
	// Calendar
	"list_events":    "List events",
	"get_event":      "Get event",
	"create_event":   "Create event",
	"update_event":   "Update event",
	"delete_event":   "Delete event",
	"list_calendars": "List calendars",
	// Drive
	"list_files":   "List files",
	"get_file":     "Get file",
	"create_file":  "Create file",
	"update_file":  "Update file",
	"search_files": "Search files",
	// Contacts
	"list_contacts":   "List contacts",
	"get_contact":     "Get contact",
	"search_contacts": "Search contacts",
	// GitHub
	"list_issues":   "List issues",
	"get_issue":     "Get issue",
	"create_issue":  "Create issue",
	"comment_issue": "Comment on issue",
	"list_prs":      "List PRs",
	"get_pr":        "Get PR",
	"list_repos":    "List repos",
	"search_code":   "Search code",
	// iMessage
	"search_messages": "Search messages",
	"list_threads":    "List threads",
	"get_thread":      "Get thread",
	// Slack
	"list_channels": "List channels",
	"get_channel":   "Get channel",
	"list_users":    "List users",
	// Notion
	"search":          "Search",
	"get_page":        "Get page",
	"create_page":     "Create page",
	"update_page":     "Update page",
	"query_database":  "Query database",
	"list_databases":  "List databases",
	// Linear
	"update_issue":  "Update issue",
	"add_comment":   "Add comment",
	"list_teams":    "List teams",
	"list_projects": "List projects",
	"search_issues": "Search issues",
	// Stripe
	"list_customers":     "List customers",
	"get_customer":       "Get customer",
	"list_charges":       "List charges",
	"get_charge":         "Get charge",
	"list_subscriptions": "List subscriptions",
	"get_subscription":   "Get subscription",
	"create_refund":      "Create refund",
	"get_balance":        "Get balance",
	// Twilio
	"send_sms":      "Send SMS",
	"send_whatsapp": "Send WhatsApp",
}

// ServiceName returns the human-readable name for a service ID.
// Checks the adapter registry first, then falls back to the hardcoded map,
// then to the raw ID.
func ServiceName(id string) string {
	if registry != nil {
		if a, ok := registry.Get(id); ok {
			if mp, ok := a.(adapters.MetadataProvider); ok {
				if name := mp.ServiceMetadata().DisplayName; name != "" {
					return name
				}
			}
		}
	}
	if name, ok := serviceNames[id]; ok {
		return name
	}
	return id
}

// ServiceDescription returns a one-line description for a service ID.
func ServiceDescription(id string) string {
	if registry != nil {
		if a, ok := registry.Get(id); ok {
			if mp, ok := a.(adapters.MetadataProvider); ok {
				if desc := mp.ServiceMetadata().Description; desc != "" {
					return desc
				}
			}
		}
	}
	return serviceDescriptions[id]
}

// ActionName returns the human-readable label for an action.
// Falls back to replacing underscores with spaces.
func ActionName(action string) string {
	if action == "*" {
		return "All actions"
	}
	if name, ok := actionNames[action]; ok {
		return name
	}
	return strings.ReplaceAll(action, "_", " ")
}

// FormatServiceAction returns "ServiceName: ActionName".
func FormatServiceAction(service, action string) string {
	return ServiceName(service) + ": " + ActionName(action)
}
