package adaptergen

import "context"

// AdapterStore abstracts persistence for generated adapter YAML definitions.
// The filesystem implementation is used for local daemon mode; the database
// implementation is used for cloud deployments where there is no writable home directory.
type AdapterStore interface {
	// Save persists a generated adapter YAML, keyed by service ID.
	Save(ctx context.Context, serviceID string, yamlContent string) error
	// Load returns all stored adapter definitions as a map of serviceID → YAML content.
	Load(ctx context.Context) (map[string]string, error)
	// Delete removes a stored adapter by service ID.
	Delete(ctx context.Context, serviceID string) error
}
