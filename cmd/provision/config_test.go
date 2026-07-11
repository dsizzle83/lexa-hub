package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "provision.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	checks := map[string]struct{ got, want string }{
		"adapter":     {cfg.Adapter, "hci0"},
		"pop_file":    {cfg.PopFile, "/etc/lexa/provision-pop"},
		"serial_file": {cfg.SerialFile, "/etc/lexa/identity/serial"},
		"marker_file": {cfg.MarkerFile, "/etc/lexa/commissioned"},
		"metrics":     {cfg.MetricsAddr, "127.0.0.1:9107"},
		"log_level":   {cfg.LogLevel, "info"},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
	if cfg.HandoffPort != 9100 {
		t.Errorf("handoff_port = %d, want 9100", cfg.HandoffPort)
	}
	if cfg.ReconcileIntervalS != 10 {
		t.Errorf("reconcile_interval_s = %d, want 10", cfg.ReconcileIntervalS)
	}
	if cfg.ReconcileInterval().Seconds() != 10 {
		t.Errorf("ReconcileInterval() = %v, want 10s", cfg.ReconcileInterval())
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{
		"adapter": "hci1",
		"pop_file": "/custom/pop",
		"marker_file": "/custom/marker",
		"reconcile_interval_s": 3,
		"metrics_addr": "off",
		"log_level": "debug"
	}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Adapter != "hci1" || cfg.PopFile != "/custom/pop" || cfg.MarkerFile != "/custom/marker" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.ReconcileIntervalS != 3 || cfg.MetricsAddr != "off" || cfg.LogLevel != "debug" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadConfig_BadJSON(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, `{not json`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadConfig_Missing(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadPoP_FileVsDevkitDefault(t *testing.T) {
	dir := t.TempDir()
	popPath := filepath.Join(dir, "pop")

	// Absent → dev-kit default, fromFile=false.
	cfg := &Config{PopFile: popPath}
	if pop, fromFile := cfg.loadPoP(); pop != devkitPoP || fromFile {
		t.Fatalf("absent pop: got %q fromFile=%v, want devkit default", pop, fromFile)
	}
	// Empty/whitespace file → dev-kit default.
	if err := os.WriteFile(popPath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pop, fromFile := cfg.loadPoP(); pop != devkitPoP || fromFile {
		t.Fatalf("empty pop file: got %q fromFile=%v, want devkit default", pop, fromFile)
	}
	// Real content (trimmed) → used, fromFile=true.
	if err := os.WriteFile(popPath, []byte("  REAL-POP-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pop, fromFile := cfg.loadPoP(); pop != "REAL-POP-123" || !fromFile {
		t.Fatalf("real pop: got %q fromFile=%v, want REAL-POP-123/true", pop, fromFile)
	}
}

func TestResolveSerial(t *testing.T) {
	dir := t.TempDir()
	serialPath := filepath.Join(dir, "serial")
	if err := os.WriteFile(serialPath, []byte(" LX93-ABCDEF \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{SerialFile: serialPath}
	if got := cfg.resolveSerial(); got != "LX93-ABCDEF" {
		t.Fatalf("resolveSerial = %q, want LX93-ABCDEF", got)
	}

	// Missing file → hostname fallback (non-empty, not the file's content).
	cfg2 := &Config{SerialFile: filepath.Join(dir, "absent")}
	if got := cfg2.resolveSerial(); got == "" || got == "LX93-ABCDEF" {
		t.Fatalf("resolveSerial fallback = %q, want a hostname", got)
	}
}
