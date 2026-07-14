package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lexa-hub/internal/northbound/run"
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

// TestLoadConfig_PollRateModeDefaultsToHonor verifies TASK-071's PRODUCT
// default: an absent poll_rate_mode key defaults to "honor" (spec-polite
// behavior), never silently to "override". configs/northbound.json (the
// bench-deployed file) must set poll_rate_mode explicitly to opt out of
// this — see that file and PollRateModeStr's doc comment.
func TestLoadConfig_PollRateModeDefaultsToHonor(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker":"tcp://localhost:1883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.PollRateModeStr != string(run.PollRateHonor) {
		t.Errorf("PollRateModeStr = %q, want %q (absent key must default to honor)", cfg.PollRateModeStr, run.PollRateHonor)
	}
	if got := cfg.PollRateMode(); got != run.PollRateHonor {
		t.Errorf("PollRateMode() = %q, want %q", got, run.PollRateHonor)
	}
}

// TestLoadConfig_PollRateModeExplicitOverridePassesThrough verifies the
// bench config's explicit "override" value is not clobbered by the default
// (loadConfig only fills the field when it is the empty string).
func TestLoadConfig_PollRateModeExplicitOverridePassesThrough(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker":"tcp://localhost:1883","poll_rate_mode":"override"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := cfg.PollRateMode(); got != run.PollRateOverride {
		t.Errorf("PollRateMode() = %q, want %q (explicit override must pass through unmodified)", got, run.PollRateOverride)
	}
}

// TestLoadConfig_RedirectMax verifies WP-3's redirect_max key semantics:
// absent defaults to 3, an explicit 0 disables (and is NOT clobbered back to
// the default — the reason the field is *int), an explicit value passes
// through.
func TestLoadConfig_RedirectMax(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int
	}{
		{"absent defaults to 3", `{"mqtt_broker":"tcp://localhost:1883"}`, 3},
		{"explicit zero disables", `{"mqtt_broker":"tcp://localhost:1883","redirect_max":0}`, 0},
		{"explicit value passes through", `{"mqtt_broker":"tcp://localhost:1883","redirect_max":5}`, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(writeTempConfig(t, tc.json))
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got := cfg.RedirectMaxValue(); got != tc.want {
				t.Errorf("RedirectMaxValue() = %d, want %d", got, tc.want)
			}
		})
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
