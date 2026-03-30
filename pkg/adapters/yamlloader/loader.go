// Package yamlloader loads YAML adapter definitions from embedded and user-local directories.
package yamlloader

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
)

// Loader reads YAML adapter definitions and produces YAMLAdapter instances.
type Loader struct {
	embeddedFS fs.FS
	userDir    string // ~/.clawvisor/adapters/
	overrides  map[string]yamlruntime.ActionFunc
	adapters   []*yamlruntime.YAMLAdapter
	logger     *slog.Logger
}

// New creates a Loader.
// embeddedFS is the compiled-in definitions directory.
// userDir is the user-local adapter directory (may not exist).
// overrides maps "service_id:action_name" to Go functions for complex actions.
func New(embeddedFS fs.FS, userDir string, overrides map[string]yamlruntime.ActionFunc, logger *slog.Logger) *Loader {
	if overrides == nil {
		overrides = map[string]yamlruntime.ActionFunc{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{
		embeddedFS: embeddedFS,
		userDir:    userDir,
		overrides:  overrides,
		logger:     logger,
	}
}

// LoadAll loads all YAML definitions from embedded and user directories.
// User-local files override embedded files with the same service ID.
func (l *Loader) LoadAll() error {
	defs := map[string]yamldef.ServiceDef{} // service_id → def

	// Load embedded definitions.
	if l.embeddedFS != nil {
		entries, err := fs.ReadDir(l.embeddedFS, ".")
		if err != nil {
			return fmt.Errorf("reading embedded adapter definitions: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			data, err := fs.ReadFile(l.embeddedFS, entry.Name())
			if err != nil {
				l.logger.Warn("skipping embedded adapter definition", "file", entry.Name(), "err", err)
				continue
			}
			def, err := parseDefinition(data)
			if err != nil {
				l.logger.Warn("skipping embedded adapter definition", "file", entry.Name(), "err", err)
				continue
			}
			defs[def.Service.ID] = def
			l.logger.Debug("loaded embedded adapter definition", "service", def.Service.ID)
		}
	}

	// Load user-local definitions (override embedded).
	if l.userDir != "" {
		if info, err := os.Stat(l.userDir); err == nil && info.IsDir() {
			entries, err := os.ReadDir(l.userDir)
			if err != nil {
				l.logger.Warn("could not read user adapter directory", "dir", l.userDir, "err", err)
			} else {
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
						continue
					}
					data, err := os.ReadFile(filepath.Join(l.userDir, entry.Name()))
					if err != nil {
						l.logger.Warn("skipping user adapter definition", "file", entry.Name(), "err", err)
						continue
					}
					def, err := parseDefinition(data)
					if err != nil {
						l.logger.Warn("skipping user adapter definition", "file", entry.Name(), "err", err)
						continue
					}
					defs[def.Service.ID] = def
					l.logger.Info("loaded user adapter definition (override)", "service", def.Service.ID)
				}
			}
		}
	}

	// Build adapters.
	l.adapters = make([]*yamlruntime.YAMLAdapter, 0, len(defs))
	for _, def := range defs {
		// Collect Go overrides for this service.
		actionOverrides := map[string]yamlruntime.ActionFunc{}
		for actionName, action := range def.Actions {
			if action.Override == "go" {
				key := def.Service.ID + ":" + actionName
				if fn, ok := l.overrides[key]; ok {
					actionOverrides[actionName] = fn
				}
			}
		}
		adapter, err := yamlruntime.New(def, actionOverrides)
		if err != nil {
			l.logger.Warn("skipping adapter: expression compilation failed", "service", def.Service.ID, "err", err)
			continue
		}
		l.adapters = append(l.adapters, adapter)
	}

	l.logger.Info("loaded adapter definitions", "count", len(l.adapters))
	return nil
}

// Adapters returns the loaded YAML adapters.
func (l *Loader) Adapters() []*yamlruntime.YAMLAdapter {
	return l.adapters
}

// Definitions returns all loaded service definitions (for metadata access without building adapters).
func (l *Loader) Definitions() []yamldef.ServiceDef {
	defs := make([]yamldef.ServiceDef, len(l.adapters))
	for i, a := range l.adapters {
		defs[i] = a.Def()
	}
	return defs
}

// parseDefinition parses a YAML file into a ServiceDef.
func parseDefinition(data []byte) (yamldef.ServiceDef, error) {
	var def yamldef.ServiceDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		return def, fmt.Errorf("parsing YAML: %w", err)
	}
	if def.Service.ID == "" {
		return def, fmt.Errorf("service.id is required")
	}
	return def, nil
}

// LoadFile loads a single YAML file and returns the parsed definition.
func LoadFile(path string) (yamldef.ServiceDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return yamldef.ServiceDef{}, err
	}
	return parseDefinition(data)
}
