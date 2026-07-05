package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeModbusConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "modbus.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadConfig_ReconcilerModes covers every value the "reconciler" config
// map may hold (TASK-027): absent/empty/off/shadow load cleanly, "active" is
// rejected at load with a message naming TASK-028 (so a premature flip is
// impossible, not a silent downgrade), and any other value is rejected too.
func TestLoadConfig_ReconcilerModes(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string // substring, empty = no error
	}{
		{"absent", `{"devices":[]}`, ""},
		{"empty string", `{"reconciler":{"battery":""},"devices":[]}`, ""},
		{"off", `{"reconciler":{"battery":"off"},"devices":[]}`, ""},
		{"shadow", `{"reconciler":{"battery":"shadow"},"devices":[]}`, ""},
		{"active rejected", `{"reconciler":{"battery":"active"},"devices":[]}`, "TASK-028"},
		{"unknown rejected", `{"reconciler":{"battery":"bogus"},"devices":[]}`, "unknown mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeModbusConfig(t, c.json)
			cfg, err := loadConfig(path)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				_ = cfg
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestConfig_ReconcilerMode covers the default-resolution helper: missing
// map, missing class, and explicit "off" all resolve to ReconcilerOff; a
// configured class returns exactly what was configured.
func TestConfig_ReconcilerMode(t *testing.T) {
	var nilMap Config
	if got := nilMap.ReconcilerMode("battery"); got != ReconcilerOff {
		t.Errorf("nil Reconciler map: got %q, want %q", got, ReconcilerOff)
	}

	empty := Config{Reconciler: map[string]string{}}
	if got := empty.ReconcilerMode("battery"); got != ReconcilerOff {
		t.Errorf("class absent: got %q, want %q", got, ReconcilerOff)
	}

	shadow := Config{Reconciler: map[string]string{"battery": "shadow"}}
	if got := shadow.ReconcilerMode("battery"); got != ReconcilerShadow {
		t.Errorf("got %q, want %q", got, ReconcilerShadow)
	}
	if got := shadow.ReconcilerMode("solar"); got != ReconcilerOff {
		t.Errorf("unconfigured class: got %q, want %q", got, ReconcilerOff)
	}
}
