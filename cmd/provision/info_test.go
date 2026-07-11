package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"lexa-hub/internal/buildinfo"
	"lexa-hub/internal/provision/gatt"
	"lexa-hub/internal/provision/sec1"
)

// TestInfoDocReflectsRealSerialAndFw exercises the exact wiring cmd/provision's
// peripheral factory uses: the resolved serial (from serial_file) and the
// build-injected buildinfo.Version flow into sec1.PeripheralConfig, so a
// plaintext info read reports TRUTH rather than the B1 "sec1-go" placeholder.
func TestInfoDocReflectsRealSerialAndFw(t *testing.T) {
	dir := t.TempDir()
	serialPath := filepath.Join(dir, "serial")
	if err := os.WriteFile(serialPath, []byte("LX93-777001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, "commissioned") // absent → uncommissioned

	cfg := &Config{SerialFile: serialPath, MarkerFile: markerPath, PopFile: filepath.Join(dir, "pop")}
	serial := cfg.resolveSerial()
	pop, _ := cfg.loadPoP()

	// Mirror main()'s factory + dispatcher wiring.
	disp := gatt.NewDispatcher(func() *sec1.Peripheral {
		return sec1.NewPeripheral(sec1.PeripheralConfig{
			Pop:          pop,
			Serial:       serial,
			Fw:           buildinfo.Version,
			Commissioned: markerPresent(cfg.MarkerFile),
			JoinBehavior: stubJoin(),
		})
	}, gatt.Observer{})

	raw, err := disp.InfoValue()
	if err != nil {
		t.Fatalf("InfoValue: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("info not JSON: %v", err)
	}
	if got["serial"] != "LX93-777001" {
		t.Errorf("serial = %v, want LX93-777001", got["serial"])
	}
	if got["fw"] != buildinfo.Version {
		t.Errorf("fw = %v, want buildinfo.Version %q", got["fw"], buildinfo.Version)
	}
	if got["fw"] == "sec1-go" {
		t.Error("fw is still the B1 placeholder — real build version not wired")
	}
	if got["commissioned"] != false {
		t.Errorf("commissioned = %v, want false (marker absent)", got["commissioned"])
	}
	sec, _ := got["sec"].([]any)
	if len(sec) != 1 || sec[0] != "sec1" {
		t.Errorf("sec = %v, want [sec1]", got["sec"])
	}
}
