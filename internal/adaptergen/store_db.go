package adaptergen

import (
	"context"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// DBStore implements AdapterStore using the database via store.Store.
// Used for cloud deployments where there is no persistent filesystem.
type DBStore struct {
	store  store.Store
	userID string // scoped to a single user for the generator's lifetime
}

// NewDBStore creates a DBStore scoped to a specific user.
func NewDBStore(st store.Store, userID string) *DBStore {
	return &DBStore{store: st, userID: userID}
}

func (s *DBStore) Save(ctx context.Context, serviceID string, yamlContent string) error {
	return s.store.SaveGeneratedAdapter(ctx, s.userID, serviceID, yamlContent)
}

func (s *DBStore) Load(ctx context.Context) (map[string]string, error) {
	adapters, err := s.store.ListGeneratedAdapters(ctx, s.userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(adapters))
	for _, a := range adapters {
		out[a.ServiceID] = a.YAMLContent
	}
	return out, nil
}

func (s *DBStore) Delete(ctx context.Context, serviceID string) error {
	return s.store.DeleteGeneratedAdapter(ctx, s.userID, serviceID)
}

