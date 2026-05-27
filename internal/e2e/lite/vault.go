package lite

import (
	"context"
	"strings"
	"sync"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

// memoryVault is a no-op Vault implementation used by the harness so we
// don't have to materialize a real local vault key + db just to forward
// the Anthropic key.
type memoryVault struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemoryVault() *memoryVault {
	return &memoryVault{data: map[string][]byte{}}
}

func (v *memoryVault) key(userID, serviceID string) string {
	return userID + "/" + serviceID
}

func (v *memoryVault) Set(_ context.Context, userID, serviceID string, c []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data[v.key(userID, serviceID)] = append([]byte{}, c...)
	return nil
}

func (v *memoryVault) SetIfAbsent(_ context.Context, userID, serviceID string, c []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := v.key(userID, serviceID)
	if _, ok := v.data[k]; ok {
		return vault.ErrAlreadyExists
	}
	v.data[k] = append([]byte{}, c...)
	return nil
}

func (v *memoryVault) Get(_ context.Context, userID, serviceID string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.data[v.key(userID, serviceID)]; ok {
		return append([]byte{}, c...), nil
	}
	return nil, vault.ErrNotFound
}

func (v *memoryVault) Delete(_ context.Context, userID, serviceID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.data, v.key(userID, serviceID))
	return nil
}

func (v *memoryVault) List(_ context.Context, userID string) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	prefix := userID + "/"
	var out []string
	for k := range v.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	return out, nil
}
