package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadConfig_JournalBlockAbsent verifies TASK-040's rollout default:
// a config file with no "journal" key leaves cfg.Journal nil — the
// no-op-everywhere gate every emit site in this package relies on.
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
// (as shipped in configs/hub.json) decodes into JournalConfig correctly,
// and ToLibrary carries every field through to journal.Config.
func TestLoadConfig_JournalBlockParses(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"journal": {
			"dir": "/var/lib/lexa/journal/hub",
			"max_bytes": 1048576,
			"max_files": 4,
			"flush_every": 32,
			"flush_interval_s": 5
		}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal == nil {
		t.Fatal("Journal = nil, want the parsed block")
	}
	if cfg.Journal.Dir != "/var/lib/lexa/journal/hub" || cfg.Journal.MaxBytes != 1048576 ||
		cfg.Journal.MaxFiles != 4 || cfg.Journal.FlushEvery != 32 || cfg.Journal.FlushIntervalS != 5 {
		t.Fatalf("JournalConfig = %+v, want the literal JSON values", cfg.Journal)
	}
	lib := cfg.Journal.ToLibrary()
	if lib.Dir != cfg.Journal.Dir || lib.MaxBytes != 1048576 || lib.MaxFiles != 4 ||
		lib.FlushEvery != 32 || lib.FlushInterval != 5*time.Second {
		t.Fatalf("ToLibrary() = %+v, want fields carried through with FlushIntervalS scaled to a Duration", lib)
	}
}

// writeTempConfig writes body to a temp file and returns its path — a tiny
// helper so the two loadConfig tests above don't each hand-roll os.CreateTemp.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return path
}
