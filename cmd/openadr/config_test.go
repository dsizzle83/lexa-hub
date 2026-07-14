package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "openadr.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConfigDefaults pins every default the WP-15 key list names:
// poll_interval_s 60, ven_name lexa-hub, report_enabled true (nil ⇒ true),
// broker localhost:1883, client id lexa-openadr, metrics 127.0.0.1:9108,
// log level info — and the shipped uncommissioned state (vtn_url "").
func TestConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, `{}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.Uncommissioned() {
		t.Error("empty vtn_url must read as uncommissioned")
	}
	if cfg.PollIntervalS != 60 || cfg.PollInterval() != 60*time.Second {
		t.Errorf("poll interval = %d, want 60", cfg.PollIntervalS)
	}
	if cfg.VenName != "lexa-hub" {
		t.Errorf("ven_name = %q, want lexa-hub", cfg.VenName)
	}
	if !cfg.ReportsEnabled() {
		t.Error("report_enabled default = false, want true (nil ⇒ true)")
	}
	if cfg.MQTTBroker != "tcp://localhost:1883" {
		t.Errorf("mqtt_broker = %q", cfg.MQTTBroker)
	}
	if cfg.MQTTClientID != "lexa-openadr" {
		t.Errorf("mqtt_client_id = %q", cfg.MQTTClientID)
	}
	if cfg.MetricsAddr != "127.0.0.1:9108" {
		t.Errorf("metrics_addr = %q, want 127.0.0.1:9108", cfg.MetricsAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q", cfg.LogLevel)
	}
}

// TestConfigReportEnabledFalse pins the explicit off switch.
func TestConfigReportEnabledFalse(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, `{"report_enabled": false}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ReportsEnabled() {
		t.Error("report_enabled:false ignored")
	}
}

// TestConfigUnknownKeyWarnsNotFails: architecture §3's rule — a future (or
// typo'd) key must load fine.
func TestConfigUnknownKeyWarnsNotFails(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, `{
		"vtn_url": "https://vtn.example:8443",
		"some_future_key": {"nested": true},
		"poll_interval_s": 30
	}`))
	if err != nil {
		t.Fatalf("unknown key failed load: %v", err)
	}
	if cfg.PollIntervalS != 30 {
		t.Errorf("known keys must still parse: poll_interval_s = %d", cfg.PollIntervalS)
	}
}

// TestConfigShippedExampleParses: configs/openadr.json (the in-repo example)
// must load, read uncommissioned, and produce no validation error.
func TestConfigShippedExampleParses(t *testing.T) {
	cfg, err := loadConfig("../../configs/openadr.json")
	if err != nil {
		t.Fatalf("shipped example: %v", err)
	}
	if !cfg.Uncommissioned() {
		t.Error("shipped example must be uncommissioned (vtn_url \"\")")
	}
	if !cfg.ReportsEnabled() {
		t.Error("shipped example: reports must default on")
	}
}

// TestUncommissionedIsPureConfigRead mirrors cmd/northbound's
// TestUncommissioned_ServerSetCertsMissing_False discipline: a config with a
// real vtn_url but a secret file that does not exist on disk is
// "configured but broken" — Uncommissioned() must still report false (no
// filesystem probing), and the breakage must surface loudly later at
// loadSecret, never as a silent idle.
func TestUncommissionedIsPureConfigRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-secret")
	cfg, err := loadConfig(writeCfg(t, `{
		"vtn_url": "https://vtn.example:8443",
		"client_id": "ven-1",
		"client_secret_file": "`+missing+`"
	}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = true for a configured-but-broken unit — must never mask a broken secret path")
	}
	if _, err := loadSecret(cfg.ClientSecretFile); err == nil {
		t.Fatal("loadSecret on a missing file succeeded — must fail loudly")
	}
}

// TestConfigClientIDRequiresSecretFile: fail loud at load (cloudlink's
// enabled/endpoint discipline).
func TestConfigClientIDRequiresSecretFile(t *testing.T) {
	_, err := loadConfig(writeCfg(t, `{"vtn_url":"https://v.example","client_id":"ven-1"}`))
	if err == nil {
		t.Fatal("client_id without client_secret_file loaded")
	}
}

// TestConfigBadVTNURLRejected: a non-http(s) vtn_url is a startup error, not
// a runtime dial surprise.
func TestConfigBadVTNURLRejected(t *testing.T) {
	for _, bad := range []string{`{"vtn_url":"not a url"}`, `{"vtn_url":"ftp://x"}`} {
		if _, err := loadConfig(writeCfg(t, bad)); err == nil {
			t.Errorf("bad vtn_url %s loaded", bad)
		}
	}
}

// TestLoadSecret covers the mqtt_pass_file-mirroring contract: "" path ⇒
// ("", nil); configured-but-empty errors; a real file round-trips trimmed.
func TestLoadSecret(t *testing.T) {
	if s, err := loadSecret(""); err != nil || s != "" {
		t.Errorf("loadSecret(\"\") = %q, %v", s, err)
	}
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadSecret(p)
	if err != nil || s != "s3cret" {
		t.Errorf("loadSecret = %q, %v", s, err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecret(empty); err == nil {
		t.Error("empty secret file loaded")
	}
	if _, err := loadSecret(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing secret file loaded")
	}
}

// TestNextWait table-tests the poll backoff: configured interval while
// healthy, doubling per failure, capped at 8× and maxPollBackoff.
func TestNextWait(t *testing.T) {
	iv := 60 * time.Second
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 60 * time.Second},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 8 * time.Minute},  // shift capped at 3
		{10, 8 * time.Minute}, // still capped
	}
	for _, tc := range cases {
		if got := nextWait(iv, tc.failures); got != tc.want {
			t.Errorf("nextWait(%v, %d) = %v, want %v", iv, tc.failures, got, tc.want)
		}
	}
	// The absolute cap binds for long intervals.
	if got := nextWait(10*time.Minute, 3); got != maxPollBackoff {
		t.Errorf("nextWait(10m, 3) = %v, want %v (cap)", got, maxPollBackoff)
	}
}
