package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newStatusPayloadDevices(devices map[string]bool, stale []string) *statusPayload {
	sp := &statusPayload{
		Devices: make(map[string]struct {
			Connected bool `json:"connected"`
		}),
		StaleSources: stale,
	}
	for name, connected := range devices {
		sp.Devices[name] = struct {
			Connected bool `json:"connected"`
		}{Connected: connected}
	}
	return sp
}

func TestEvalModbus(t *testing.T) {
	tests := []struct {
		name    string
		devices []string
		sp      *statusPayload
		want    Status
	}{
		{
			name:    "all present, connected, not stale",
			devices: []string{"inverter-0", "battery-0", "meter-0"},
			sp: newStatusPayloadDevices(map[string]bool{
				"inverter-0": true, "battery-0": true, "meter-0": true,
			}, nil),
			want: StatusPass,
		},
		{
			name:    "device absent from status",
			devices: []string{"inverter-0", "battery-0"},
			sp:      newStatusPayloadDevices(map[string]bool{"inverter-0": true}, nil),
			want:    StatusFail,
		},
		{
			name:    "device present but stale",
			devices: []string{"meter-0"},
			sp:      newStatusPayloadDevices(map[string]bool{"meter-0": true}, []string{"meter-0"}),
			want:    StatusFail,
		},
		{
			name:    "device present but disconnected",
			devices: []string{"meter-0"},
			sp:      newStatusPayloadDevices(map[string]bool{"meter-0": false}, nil),
			want:    StatusFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalModbus(tt.devices, tt.sp)
			if got.Status != tt.want {
				t.Errorf("evalModbus(%v) = %v (%s), want %v", tt.devices, got.Status, got.Detail, tt.want)
			}
		})
	}
}

func TestCheckModbus_SkipWhenNoDevicesConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modbus.json"), []byte(`{"devices":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &Environment{ConfigDir: dir}
	res := checkModbus(context.Background(), env)
	if res.Status != StatusSkip {
		t.Fatalf("checkModbus with zero devices = %+v, want SKIP", res)
	}
}

func TestLoadModbusDeviceNames(t *testing.T) {
	dir := t.TempDir()
	body := `{"devices":[{"name":"inverter-0"},{"name":"battery-0"}]}`
	if err := os.WriteFile(filepath.Join(dir, "modbus.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := loadModbusDeviceNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "inverter-0" || names[1] != "battery-0" {
		t.Errorf("loadModbusDeviceNames = %v, want [inverter-0 battery-0]", names)
	}
}

func TestCheckModbus_MissingConfigFails(t *testing.T) {
	dir := t.TempDir() // no modbus.json at all
	env := &Environment{ConfigDir: dir}
	res := checkModbus(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkModbus with missing modbus.json = %+v, want FAIL", res)
	}
}
