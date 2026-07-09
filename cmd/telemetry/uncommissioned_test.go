package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUncommissioned_FactoryProfile_True pins Unit 1.7's core contract: the
// shipped factory profile (configs/factory/telemetry.json, Unit 1.6) must
// be read as uncommissioned so cmd/telemetry/main.go's gate takes the idle
// path on a factory-fresh/-reset unit instead of eagerly loading cert files
// that may not exist yet (V1RC FINDING A / configs/factory/README.md "Known
// gaps" #1).
func TestUncommissioned_FactoryProfile_True(t *testing.T) {
	cfg, err := loadConfig(repoPath(t, "configs", "factory", "telemetry.json"))
	if err != nil {
		t.Fatalf("loadConfig(factory profile): %v", err)
	}
	if cfg.Server != "" {
		t.Fatalf("factory profile server = %q, want empty (fixture drifted?)", cfg.Server)
	}
	if !cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = false for the factory profile, want true")
	}
}

// TestUncommissioned_BenchConfig_False pins the other side: the bench-
// deployed example config (configs/telemetry.json) has a real server and
// must be read as commissioned/configured — the gate must not idle a unit
// that is actually supposed to be posting MUP readings.
func TestUncommissioned_BenchConfig_False(t *testing.T) {
	cfg, err := loadConfig(repoPath(t, "configs", "telemetry.json"))
	if err != nil {
		t.Fatalf("loadConfig(bench config): %v", err)
	}
	if cfg.Server == "" {
		t.Fatal("bench config server is empty (fixture drifted?)")
	}
	if cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = true for the bench config, want false")
	}
}

// TestUncommissioned_ServerSetCertsMissing_False is the discipline this
// method must preserve per the unit's SPEC point 4: Uncommissioned() is a
// pure configuration read, never a filesystem probe. A config with a real
// Server but cert files that don't exist on disk is "configured but
// broken" — it MUST still report false here and fail loudly later at TLS
// fetcher construction (log.Fatalf in main()), exactly as it does today. If
// this ever started returning true for a missing-cert case, a genuinely
// commissioned-but-misconfigured unit would silently idle instead of
// alerting an operator to the broken cert path.
func TestUncommissioned_ServerSetCertsMissing_False(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Server:     "69.0.0.20:11111",
		CACert:     filepath.Join(dir, "no-such-ca.pem"),
		ClientCert: filepath.Join(dir, "no-such-client.pem"),
		ClientKey:  filepath.Join(dir, "no-such-client-key.pem"),
	}
	if cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = true for a configured server with missing cert files, want false (configured-but-broken must still crash loudly, not idle)")
	}
}

// TestUncommissioned_PathsConfiguredServerEmpty_True pins SPEC point 4
// directly: factory profiles bake in cert paths that point at the standard
// /etc/lexa/certs/* locations even though the server is empty. Those paths
// being "configured" must not flip Uncommissioned() to false — only the
// server decides.
func TestUncommissioned_PathsConfiguredServerEmpty_True(t *testing.T) {
	cfg := &Config{
		Server:     "",
		CACert:     "/etc/lexa/certs/ca.pem",
		ClientCert: "/etc/lexa/certs/client.pem",
		ClientKey:  "/etc/lexa/certs/client-key.pem",
	}
	if !cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = false with cert paths set but server empty, want true (the server decides, not the paths)")
	}
}

// TestUncommissioned_ZeroValueConfig_True covers the degenerate case (an
// absent/empty config file, e.g. loadConfig on `{}`) — everything defaults
// to zero, Server included, so this must read as uncommissioned too.
func TestUncommissioned_ZeroValueConfig_True(t *testing.T) {
	var cfg Config
	if !cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = false for a zero-value Config, want true")
	}
}

// TestUncommissioned_DevicesSetServerEmpty_True covers a telemetry-specific
// corner the northbound copy of this table doesn't have: a config that
// lists devices to post MUPs for but has no server. Devices alone can't
// make posting possible — only Server does — so this must still read as
// uncommissioned (and main()'s gate must skip straight to idle rather than
// reach the "no MUPs registered" Fatal that an empty Devices list would
// otherwise hit first).
func TestUncommissioned_DevicesSetServerEmpty_True(t *testing.T) {
	cfg := &Config{
		Server:  "",
		Devices: []string{"inverter-0", "battery-0"},
	}
	if !cfg.Uncommissioned() {
		t.Fatal("Uncommissioned() = false with devices configured but server empty, want true")
	}
}

// repoPath resolves a path relative to the repository root from within
// cmd/telemetry's test working directory (go test's cwd is the package
// dir), skipping the test rather than failing outright if this checkout
// doesn't have the expected repo layout (e.g. a vendored/extracted copy).
func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	elems := append([]string{"..", ".."}, parts...)
	p := filepath.Join(elems...)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("fixture %s not found relative to package dir: %v", p, err)
	}
	return p
}
