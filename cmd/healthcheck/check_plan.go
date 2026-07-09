package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// commissionedMarkerName is the DEVICE_ROADMAP.md §9 marker: its ABSENCE
// means the unit is uncommissioned and idling by design (no devices/
// stations configured yet).
const commissionedMarkerName = "commissioned"

func isCommissioned(configDir string) bool {
	_, err := os.Stat(filepath.Join(configDir, commissionedMarkerName))
	return err == nil
}

// checkPlanHeartbeat fetches /status (with a bearer token if api.json
// configures api_token_file) and evaluates plan_heartbeat.state via the
// pure evalPlanHeartbeat below.
func checkPlanHeartbeat(ctx context.Context, env *Environment) Result {
	cfg, err := loadAPIConfig(env.ConfigDir)
	if err != nil {
		return fail("plan_heartbeat", err.Error())
	}
	host, port, err := apiHostPort(cfg.ListenAddr)
	if err != nil {
		return fail("plan_heartbeat", err.Error())
	}
	token, err := loadAPIToken(cfg.APITokenFile)
	if err != nil {
		return fail("plan_heartbeat", err.Error())
	}
	sp, _, err := fetchStatus(ctx, env, host, port, token)
	if err != nil {
		return fail("plan_heartbeat", err.Error())
	}
	return evalPlanHeartbeat(sp, isCommissioned(env.ConfigDir))
}

// evalPlanHeartbeat is the pure decision, table-tested in
// check_plan_test.go against fake statusPayloads with no I/O:
//
//   - state == "ok"                      → PASS always.
//   - state == "never" && !commissioned  → PASS (TASK-045's documented
//     safe-by-design silent state: an uncommissioned unit with no
//     devices/stations has legitimately never produced a plan).
//   - state == "never" && commissioned   → FAIL (a commissioned unit is
//     expected to be running the engine; "never" here means it isn't).
//   - anything else ("stalled", empty, unknown) → FAIL.
func evalPlanHeartbeat(sp *statusPayload, commissioned bool) Result {
	state := sp.PlanHeartbeat.State
	switch {
	case state == "ok":
		return pass("plan_heartbeat", fmt.Sprintf("ok, age_s=%.1f", sp.PlanHeartbeat.AgeS))
	case state == "never" && !commissioned:
		return pass("plan_heartbeat", "never, but uncommissioned — idle by design")
	case state == "never" && commissioned:
		return fail("plan_heartbeat", "never — commissioned unit has produced no plan")
	default:
		return fail("plan_heartbeat", fmt.Sprintf("state=%q age_s=%.1f", state, sp.PlanHeartbeat.AgeS))
	}
}
