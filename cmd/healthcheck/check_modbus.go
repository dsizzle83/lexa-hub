package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type modbusDeviceConfig struct {
	Name string `json:"name"`
}

type modbusConfigFile struct {
	Devices []modbusDeviceConfig `json:"devices"`
}

func loadModbusDeviceNames(configDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "modbus.json"))
	if err != nil {
		return nil, fmt.Errorf("read modbus.json: %w", err)
	}
	var cfg modbusConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse modbus.json: %w", err)
	}
	names := make([]string, 0, len(cfg.Devices))
	for _, d := range cfg.Devices {
		names = append(names, d.Name)
	}
	return names, nil
}

// checkModbus verifies every device configured in modbus.json's devices[]
// shows up in /status's devices map as connected and not flagged stale.
//
// /status's devices map is seeded from api.json's OWN device list (see
// cmd/api/config.go's DeviceConfig — "so the API can label it correctly"),
// which is a SEPARATE config file from modbus.json. A device present in
// modbus.json but absent from /status is therefore a real finding either
// way: either genuine config drift between the two files, or the device
// really isn't producing measurements — /status has no way to vouch for a
// device lexa-api was never told about, so "absent" is reported as such
// rather than silently skipped.
func checkModbus(ctx context.Context, env *Environment) Result {
	names, err := loadModbusDeviceNames(env.ConfigDir)
	if err != nil {
		return fail("modbus", err.Error())
	}
	if len(names) == 0 {
		return skip("modbus", "no devices configured")
	}

	apiCfg, err := loadAPIConfig(env.ConfigDir)
	if err != nil {
		return fail("modbus", err.Error())
	}
	host, port, err := apiHostPort(apiCfg.ListenAddr)
	if err != nil {
		return fail("modbus", err.Error())
	}
	token, err := loadAPIToken(apiCfg.APITokenFile)
	if err != nil {
		return fail("modbus", err.Error())
	}
	sp, _, err := fetchStatus(ctx, env, host, port, token)
	if err != nil {
		return fail("modbus", err.Error())
	}
	return evalModbus(names, sp)
}

// evalModbus is the pure decision (table-tested in check_modbus_test.go).
// "Not stale" combines two independent signals from /status: Connected
// (fresh arrival within stale_after_s — catches a silent/dead publisher)
// and absence from stale_sources (catches a publisher that's alive but
// whose value is suspiciously frozen — see cmd/api/state.go's
// staleMeters doc; only ever set for role=="meter" devices today, so this
// is a no-op add-on for inverter/battery entries and the real freshness
// signal for meters).
func evalModbus(names []string, sp *statusPayload) Result {
	stale := make(map[string]bool, len(sp.StaleSources))
	for _, s := range sp.StaleSources {
		stale[s] = true
	}

	var bad []string
	for _, n := range names {
		d, ok := sp.Devices[n]
		switch {
		case !ok:
			bad = append(bad, n+"=absent")
		case stale[n]:
			bad = append(bad, n+"=stale")
		case !d.Connected:
			bad = append(bad, n+"=disconnected")
		}
	}
	if len(bad) > 0 {
		return fail("modbus", strings.Join(bad, ", "))
	}
	return pass("modbus", fmt.Sprintf("%d device(s) present, fresh", len(names)))
}
