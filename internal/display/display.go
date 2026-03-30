// Package display provides human-readable names for services and actions.
// It is the single source of truth used by both API responses and notifications.
// Metadata is read from adapters that implement MetadataProvider; fallbacks
// cover Go-only adapters (iMessage) and the generic underscore→space rule.
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

// ServiceName returns the human-readable name for a service ID.
// Reads from the adapter registry (MetadataProvider), then falls back to the
// raw ID with dots replaced by spaces as a last resort.
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
	return ""
}

// ActionName returns the human-readable label for an action.
// Checks MetadataProvider across all adapters, then falls back to
// replacing underscores with spaces and capitalizing.
func ActionName(action string) string {
	if action == "*" {
		return "All actions"
	}
	// Check all adapters for a matching action display name.
	if registry != nil {
		for _, a := range registry.All() {
			if mp, ok := a.(adapters.MetadataProvider); ok {
				if am, ok := mp.ServiceMetadata().ActionMeta[action]; ok && am.DisplayName != "" {
					return am.DisplayName
				}
			}
		}
	}
	return strings.ReplaceAll(action, "_", " ")
}

// FormatServiceAction returns "ServiceName: ActionName".
func FormatServiceAction(service, action string) string {
	return ServiceName(service) + ": " + ActionName(action)
}
