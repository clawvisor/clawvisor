package adaptergen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FilesystemStore implements AdapterStore by reading and writing YAML files
// in a local directory (typically ~/.clawvisor/adapters/).
type FilesystemStore struct {
	dir string
}

// NewFilesystemStore creates a FilesystemStore rooted at dir.
func NewFilesystemStore(dir string) *FilesystemStore {
	return &FilesystemStore{dir: dir}
}

func (s *FilesystemStore) Save(_ context.Context, serviceID string, yamlContent string) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	path, err := safeFilename(s.dir, serviceID)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(yamlContent), 0o644)
}

func (s *FilesystemStore) Load(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	if info, err := os.Stat(s.dir); err != nil || !info.IsDir() {
		return out, nil // directory doesn't exist yet — no user adapters
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		id := extractServiceID(data)
		if id == "" {
			continue
		}
		out[id] = string(data)
	}
	return out, nil
}

func (s *FilesystemStore) Delete(_ context.Context, serviceID string) error {
	path, err := safeFilename(s.dir, serviceID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// safeFilename converts a service ID to a filename and verifies it contains
// no path traversal characters. This prevents writes outside the adapters directory
// via LLM-generated service IDs.
func safeFilename(adaptersDir, serviceID string) (string, error) {
	if strings.ContainsAny(serviceID, "/\\") {
		return "", fmt.Errorf("service_id %q contains path separators", serviceID)
	}
	if strings.Contains(serviceID, "..") {
		return "", fmt.Errorf("service_id %q contains path traversal sequence", serviceID)
	}

	filename := strings.ReplaceAll(serviceID, ".", "_") + ".yaml"

	// Belt-and-suspenders: verify the resolved path is under adaptersDir.
	absDir, err := filepath.Abs(adaptersDir)
	if err != nil {
		return "", fmt.Errorf("resolving adapters directory: %w", err)
	}
	absPath, err := filepath.Abs(filepath.Join(adaptersDir, filename))
	if err != nil {
		return "", fmt.Errorf("resolving adapter file path: %w", err)
	}
	if !strings.HasPrefix(absPath, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("service_id %q resolves to a path outside the adapters directory", serviceID)
	}
	return absPath, nil
}

// extractServiceID does a minimal YAML parse to get the service.id field.
func extractServiceID(data []byte) string {
	var partial struct {
		Service struct {
			ID string `yaml:"id"`
		} `yaml:"service"`
	}
	if err := yaml.Unmarshal(data, &partial); err != nil {
		return ""
	}
	return partial.Service.ID
}
