package traymgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewAppCanonicalizesChoiceCasing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tray.json")
	cfg := DefaultConfig()
	cfg.Flags["machine-transport"] = "USB"
	writeRawConfig(t, path, cfg)

	app, err := NewApp(path, nil)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := app.Supervisor.Config().Flags["machine-transport"]; got != "usb" {
		t.Fatalf("machine-transport = %q, want usb", got)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := loaded.Flags["machine-transport"]; got != "usb" {
		t.Fatalf("persisted machine-transport = %q, want usb", got)
	}
}

func TestNewAppStartsWithInvalidProxyFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tray.json")
	cfg := DefaultConfig()
	cfg.Flags["machine-transport"] = "bluetooth"
	writeRawConfig(t, path, cfg)

	app, err := NewApp(path, nil)
	if err != nil {
		t.Fatalf("NewApp should not fail on invalid proxy flag: %v", err)
	}
	if got := app.Supervisor.Config().Flags["machine-transport"]; got != "bluetooth" {
		t.Fatalf("machine-transport = %q, want original invalid value", got)
	}
	if err := app.Supervisor.Start(); err == nil {
		t.Fatal("Start should still reject invalid proxy flag")
	}
}

func writeRawConfig(t *testing.T, path string, cfg Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
