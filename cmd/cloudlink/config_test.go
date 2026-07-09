package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudlink.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return path
}

func TestLoadConfig_Defaults(t *testing.T) {
	path := writeTempConfig(t, `{}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"serial_file", cfg.SerialFile, defaultSerialFile},
		{"cert_expiry_warn_days", cfg.CertExpiryWarnDays, defaultCertWarnDays},
		{"spool_dir", cfg.SpoolDir, defaultSpoolDir},
		{"spool_max_bytes", cfg.SpoolMaxBytes, int64(defaultSpoolMaxB)},
		{"uplink.measurements_batch_s", cfg.Uplink.MeasurementsBatchS, 60},
		{"uplink.evse_batch_s", cfg.Uplink.EVSEBatchS, 60},
		{"uplink.plan_interval_s", cfg.Uplink.PlanIntervalS, 300},
		{"uplink.health_interval_s", cfg.Uplink.HealthIntervalS, 900},
		{"mqtt_broker", cfg.MQTTBroker, "tcp://localhost:1883"},
		{"mqtt_client_id", cfg.MQTTClientID, "lexa-cloudlink"},
		{"metrics_addr", cfg.MetricsAddr, defaultMetricsAddr},
		{"log_level", cfg.LogLevel, "info"},
		{"journal.dir", cfg.Journal.Dir, defaultJournalDir},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.Enabled {
		t.Error("Enabled defaults to true, want false (safe shipped default)")
	}
	if cfg.HealthInterval().Seconds() != 900 {
		t.Errorf("HealthInterval() = %s, want 900s", cfg.HealthInterval())
	}
}

func TestLoadConfig_EnabledWithoutEndpointRejected(t *testing.T) {
	path := writeTempConfig(t, `{"enabled": true}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig succeeded with enabled:true and no endpoint; want a fail-loud error")
	}
}

func TestLoadConfig_EnabledWithEndpointSucceeds(t *testing.T) {
	path := writeTempConfig(t, `{"enabled": true, "endpoint": "ssl://example:8883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.Enabled || cfg.Endpoint != "ssl://example:8883" {
		t.Errorf("cfg = %+v, want enabled+endpoint preserved", cfg)
	}
}

func TestLoadConfig_DisabledWithoutEndpointSucceeds(t *testing.T) {
	// The safe shipped default: enabled:false with no endpoint at all must
	// load cleanly (configs/cloudlink.json and configs/factory/cloudlink.json
	// both ship this shape).
	path := writeTempConfig(t, `{"enabled": false}`)
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
}

func TestLoadConfig_MetricsAddrConventions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to product loopback port", "", defaultMetricsAddr},
		{"off stays off", "off", "off"},
		{"custom addr passes through unchanged", "0.0.0.0:9106", "0.0.0.0:9106"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempConfig(t, `{"metrics_addr": "`+tt.in+`"}`)
			cfg, err := loadConfig(path)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.MetricsAddr != tt.want {
				t.Errorf("MetricsAddr = %q, want %q", cfg.MetricsAddr, tt.want)
			}
		})
	}
}

func TestLoadConfig_JournalDirDefaultedWhenBlockAbsent(t *testing.T) {
	path := writeTempConfig(t, `{}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal.Dir != defaultJournalDir {
		t.Errorf("Journal.Dir = %q, want %q", cfg.Journal.Dir, defaultJournalDir)
	}
}

func TestLoadConfig_JournalDirExplicitWins(t *testing.T) {
	path := writeTempConfig(t, `{"journal": {"dir": "/custom/journal/dir"}}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal.Dir != "/custom/journal/dir" {
		t.Errorf("Journal.Dir = %q, want /custom/journal/dir", cfg.Journal.Dir)
	}
}

// TestLoadConfig_FactoryProfileParses is the first real validation of
// configs/factory/cloudlink.json (Unit 1.6 wrote it before cmd/cloudlink
// existed; that directory's README.md says as much: "It has not been
// validated against any real loadConfig because none exists yet.").
func TestLoadConfig_FactoryProfileParses(t *testing.T) {
	cfg, err := loadConfig("../../configs/factory/cloudlink.json")
	if err != nil {
		t.Fatalf("loadConfig(factory profile): %v", err)
	}
	if cfg.Enabled {
		t.Error("factory profile Enabled = true, want false (no identity provisioned yet)")
	}
	if cfg.Endpoint != "" {
		t.Errorf("factory profile Endpoint = %q, want empty", cfg.Endpoint)
	}
	if cfg.SerialFile != defaultSerialFile {
		t.Errorf("factory profile SerialFile = %q, want %q", cfg.SerialFile, defaultSerialFile)
	}
	if cfg.SpoolMaxBytes != int64(defaultSpoolMaxB) {
		t.Errorf("factory profile SpoolMaxBytes = %d, want %d", cfg.SpoolMaxBytes, defaultSpoolMaxB)
	}
	// The factory profile predates this config's "journal" block entirely
	// (Unit 1.6 wrote it before Unit 2.1 existed) — confirm the default
	// fills in cleanly rather than leaving Dir empty (which journal.Open
	// would reject at startup).
	if cfg.Journal.Dir != defaultJournalDir {
		t.Errorf("factory profile Journal.Dir = %q, want default %q (field absent from fixture)", cfg.Journal.Dir, defaultJournalDir)
	}
}

// TestLoadConfig_ShippedProfileParses does the same for configs/cloudlink.json
// (the non-factory example this unit ships), which DOES carry an explicit
// "journal" block.
func TestLoadConfig_ShippedProfileParses(t *testing.T) {
	cfg, err := loadConfig("../../configs/cloudlink.json")
	if err != nil {
		t.Fatalf("loadConfig(shipped profile): %v", err)
	}
	if cfg.Enabled {
		t.Error("shipped profile Enabled = true, want false (safe default)")
	}
	if cfg.Journal.Dir != defaultJournalDir {
		t.Errorf("shipped profile Journal.Dir = %q, want %q", cfg.Journal.Dir, defaultJournalDir)
	}
}
