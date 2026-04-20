package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigure_Persistence checks that Configure writes config + token
// and LoadConfig rehydrates identical values. Covers the "daemon boots
// → reads disk → starts proxy if Enabled" path that replaces the old
// service-manifest approach.
func TestConfigure_Persistence(t *testing.T) {
	dir := t.TempDir()
	// Create a throwaway "binary" file so BinaryPath passes existence check.
	binPath := filepath.Join(dir, "fake-proxy")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}

	m := New(dir, nil)
	cfg := Config{
		BridgeID:   "bridge-xyz",
		ServerURL:  "http://localhost:25297",
		ListenPort: 25298,
		BinaryPath: binPath,
	}
	if err := m.Configure(cfg, "cvisproxy_test"); err != nil {
		t.Fatalf("configure: %v", err)
	}
	s := m.Status()
	if s.BridgeID != "bridge-xyz" || s.Mode != "observe" || s.ListenPort != 25298 {
		t.Errorf("status after configure: %+v", s)
	}
	// Fresh manager — should rehydrate from disk.
	m2 := New(dir, nil)
	if err := m2.LoadConfig(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s2 := m2.Status()
	if s2.BridgeID != "bridge-xyz" || s2.ServerURL != "http://localhost:25297" {
		t.Errorf("rehydrated status: %+v", s2)
	}
}

func TestConfigure_Validations(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-proxy")
	_ = os.WriteFile(binPath, []byte("x"), 0755)

	m := New(dir, nil)
	cases := []struct {
		name   string
		cfg    Config
		token  string
		wantOK bool
	}{
		{"missing bridge_id", Config{BinaryPath: binPath}, "tok", false},
		{"missing binary_path", Config{BridgeID: "b"}, "tok", false},
		{"first configure requires token", Config{BridgeID: "b", BinaryPath: binPath}, "", false},
		{"bad mode", Config{BridgeID: "b", BinaryPath: binPath, Mode: "destroy"}, "tok", false},
		{"happy path", Config{BridgeID: "b", BinaryPath: binPath, Mode: "observe"}, "tok", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.Configure(c.cfg, c.token)
			if c.wantOK && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !c.wantOK && err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestEnable_RequiresConfigure(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, nil)
	if err := m.Enable(); err == nil {
		t.Error("expected Enable to fail before Configure")
	}
}

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.ListenPort != 25298 || d.ListenHost != "127.0.0.1" || d.Mode != "observe" {
		t.Errorf("defaults changed: %+v", d)
	}
}
