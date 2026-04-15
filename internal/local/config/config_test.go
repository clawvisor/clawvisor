package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndLoadToolbarSettings(t *testing.T) {
	baseDir := t.TempDir()
	cfg := DefaultConfig(baseDir)
	cfg.Name = "menu-bar-test"
	cfg.KeepAwake = true
	cfg.AutoUpdateEnabled = true
	cfg.DisabledServices = []string{"apple.imessage", "local.files"}
	cfg.ServiceDirs = []string{filepath.Join(baseDir, "services")}

	if err := Save(baseDir, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load(baseDir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if !loaded.KeepAwake {
		t.Fatal("expected keep_awake to persist")
	}
	if !loaded.AutoUpdateEnabled {
		t.Fatal("expected auto_update_enabled to persist")
	}
	if len(loaded.DisabledServices) != 2 {
		t.Fatalf("expected disabled_services to persist, got %#v", loaded.DisabledServices)
	}
	if loaded.AutoUpdateInterval.Duration != 6*time.Hour {
		t.Fatalf("expected default auto-update interval to persist, got %s", loaded.AutoUpdateInterval.Duration)
	}
	if loaded.Name != cfg.Name {
		t.Fatalf("expected name %q, got %q", cfg.Name, loaded.Name)
	}
}
