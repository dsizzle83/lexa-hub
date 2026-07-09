package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// requiredUnits are the seven units every hub — commissioned or not — must
// have active: the broker plus all six lexa services (repo CLAUDE.md's
// "Architecture: separate systemd services" table). lexa-cloudlink is
// deliberately NOT in this base list: fleet units that predate
// cloudlink.json entirely (TASK-085 hasn't landed on them, or it's simply
// disabled) must still pass this check — see unitsToCheck.
var requiredUnits = []string{
	"mosquitto",
	"lexa-hub",
	"lexa-modbus",
	"lexa-ocpp",
	"lexa-api",
	"lexa-northbound",
	"lexa-telemetry",
}

const cloudlinkUnit = "lexa-cloudlink"

// unitsToCheck returns requiredUnits, plus lexa-cloudlink when
// <configDir>/cloudlink.json exists (spec: "cloudlink included ONLY if
// /etc/lexa/cloudlink.json exists").
func unitsToCheck(configDir string) []string {
	units := append([]string(nil), requiredUnits...)
	if _, err := os.Stat(filepath.Join(configDir, "cloudlink.json")); err == nil {
		units = append(units, cloudlinkUnit)
	}
	return units
}

// checkSystemd runs exactly one `systemctl is-active <unit>...` (spec:
// "one exec, parse multi-arg output") and PASSes iff every queried unit
// reports exactly "active". systemctl exits non-zero whenever ANY unit
// isn't active — expected, and NOT itself evidence that systemctl failed
// to run; output is parsed regardless of the command's own exit status
// (see parseIsActive).
func checkSystemd(ctx context.Context, env *Environment) Result {
	units := unitsToCheck(env.ConfigDir)
	out, execErr := env.Runner.Output(ctx, "systemctl", append([]string{"is-active"}, units...)...)

	states, perr := parseIsActive(out, units)
	if perr != nil {
		// The one case output truly can't be parsed at all: systemctl
		// didn't run as expected (missing binary, permission, timeout) —
		// is-active always prints one status word per queried unit
		// regardless of how many report inactive.
		detail := perr.Error()
		if execErr != nil {
			detail = fmt.Sprintf("%s (systemctl error: %v)", detail, execErr)
		}
		return fail("systemd", detail)
	}

	var down []string
	for _, u := range units {
		if states[u] != "active" {
			down = append(down, u+"="+states[u])
		}
	}
	if len(down) > 0 {
		return fail("systemd", strings.Join(down, ", "))
	}
	return pass("systemd", fmt.Sprintf("%d units active", len(units)))
}

// parseIsActive maps `systemctl is-active`'s one-status-word-per-line
// stdout back onto the units it was queried with — systemctl preserves
// argument order, so a positional zip is all that's needed. Returns an
// error if the line count doesn't match the unit count: the signature of a
// systemctl invocation that didn't run as expected at all (as opposed to
// "ran fine, some units are merely inactive/failed").
func parseIsActive(out string, units []string) (map[string]string, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(units) {
		return nil, fmt.Errorf("systemctl is-active: expected %d lines, got %d (output: %q)", len(units), len(lines), out)
	}
	states := make(map[string]string, len(units))
	for i, u := range units {
		states[u] = strings.TrimSpace(lines[i])
	}
	return states, nil
}
