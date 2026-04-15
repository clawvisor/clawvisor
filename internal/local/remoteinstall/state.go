package remoteinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const installStateSchemaVersion = 1

type InstallState struct {
	SchemaVersion int                              `json:"schema_version"`
	Services      map[string]InstalledServiceState `json:"services"`
	Paths         map[string]InstalledPathState    `json:"paths"`
}

type InstalledServiceState struct {
	Repo         string    `json:"repo"`
	Version      string    `json:"version"`
	InstalledAt  time.Time `json:"installed_at"`
	ServicePath  string    `json:"service_path"`
	RuntimePaths []string  `json:"runtime_paths,omitempty"`
}

type InstalledPathState struct {
	InstalledPath   string   `json:"installed_path"`
	Kind            string   `json:"kind"`
	SourceAssetName string   `json:"source_asset_name"`
	SourceSHA256    string   `json:"source_sha256"`
	ContentDigest   string   `json:"content_digest"`
	Owners          []string `json:"owners"`
}

func loadInstallState(baseDir string) (*InstallState, error) {
	path := installStatePath(baseDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &InstallState{
				SchemaVersion: installStateSchemaVersion,
				Services:      map[string]InstalledServiceState{},
				Paths:         map[string]InstalledPathState{},
			}, nil
		}
		return nil, fmt.Errorf("reading installed service state: %w", err)
	}

	var state InstallState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing installed service state: %w", err)
	}
	if state.SchemaVersion != installStateSchemaVersion {
		return nil, fmt.Errorf("unsupported installed state schema version: %d", state.SchemaVersion)
	}
	if state.Services == nil {
		state.Services = map[string]InstalledServiceState{}
	}
	if state.Paths == nil {
		state.Paths = map[string]InstalledPathState{}
	}
	return &state, nil
}

func saveInstallState(baseDir string, state *InstallState) error {
	if err := os.MkdirAll(filepath.Dir(installStatePath(baseDir)), 0700); err != nil {
		return fmt.Errorf("creating install state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling installed state: %w", err)
	}
	path := installStatePath(baseDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing installed state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming installed state: %w", err)
	}
	return nil
}

func installStatePath(baseDir string) string {
	return filepath.Join(baseDir, "state", "installed-services.json")
}

func addOwner(owners []string, owner string) []string {
	for _, existing := range owners {
		if existing == owner {
			return owners
		}
	}
	owners = append(owners, owner)
	sort.Strings(owners)
	return owners
}

func removeOwner(owners []string, owner string) []string {
	out := make([]string, 0, len(owners))
	for _, existing := range owners {
		if existing != owner {
			out = append(out, existing)
		}
	}
	sort.Strings(out)
	return out
}
