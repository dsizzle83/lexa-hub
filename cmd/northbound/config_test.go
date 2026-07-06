package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadConfig_JournalBlockAbsent verifies TASK-040's rollout default: a
// config file with no "journal" key leaves cfg.Journal nil.
func TestLoadConfig_JournalBlockAbsent(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker":"tcp://localhost:1883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal != nil {
		t.Fatalf("Journal = %+v, want nil (absent block)", cfg.Journal)
	}
}

// TestLoadConfig_JournalBlockParses verifies the snake_case "journal" block
// (as shipped in configs/northbound.json) decodes correctly.
func TestLoadConfig_JournalBlockParses(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"journal": {
			"dir": "/var/lib/lexa/journal/northbound",
			"max_bytes": 1048576,
			"max_files": 4
		}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal == nil {
		t.Fatal("Journal = nil, want the parsed block")
	}
	if cfg.Journal.Dir != "/var/lib/lexa/journal/northbound" || cfg.Journal.MaxBytes != 1048576 || cfg.Journal.MaxFiles != 4 {
		t.Fatalf("JournalConfig = %+v, want the literal JSON values", cfg.Journal)
	}
	lib := cfg.Journal.ToLibrary()
	if lib.Dir != cfg.Journal.Dir || lib.MaxBytes != 1048576 || lib.MaxFiles != 4 || lib.FlushInterval != 0*time.Second {
		t.Fatalf("ToLibrary() = %+v, want fields carried through (FlushIntervalS absent -> 0, journal.withDefaults fills it later)", lib)
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}
