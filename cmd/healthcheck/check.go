package main

import (
	"context"
	"time"
)

// Status is a check's outcome.
type Status string

const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result is one check's outcome plus a short human-readable detail.
// Printed to stderr as "[healthcheck] <name> <status> (<detail>)" and
// embedded verbatim in the JSON summary (run.go).
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func pass(name, detail string) Result { return Result{Name: name, Status: StatusPass, Detail: detail} }
func fail(name, detail string) Result { return Result{Name: name, Status: StatusFail, Detail: detail} }
func skip(name, detail string) Result { return Result{Name: name, Status: StatusSkip, Detail: detail} }

// Check pairs a name with a bounded-timeout function. Every check gets its
// OWN sub-context (see run.go's runOnce) so one slow check — a wedged
// systemctl, a TCP dial that never resets — can never eat another check's
// share of the overall -budget or hang the whole run past its own bound.
type Check struct {
	Name    string
	Timeout time.Duration
	Run     func(ctx context.Context, env *Environment) Result
}

// allChecks returns the seven DEVICE_ROADMAP.md §8.1 checks, in report
// order. Each Run closure is a thin wrapper around a check_*.go file's
// gather+evaluate pair.
func allChecks() []Check {
	return []Check{
		{Name: "systemd", Timeout: 5 * time.Second, Run: checkSystemd},
		{Name: "api", Timeout: 3 * time.Second, Run: checkAPI},
		{Name: "plan_heartbeat", Timeout: 3 * time.Second, Run: checkPlanHeartbeat},
		{Name: "northbound", Timeout: 3 * time.Second, Run: checkNorthbound},
		{Name: "modbus", Timeout: 3 * time.Second, Run: checkModbus},
		{Name: "clock", Timeout: 3 * time.Second, Run: checkClock},
		{Name: "cloudlink", Timeout: 3 * time.Second, Run: checkCloudlink},
	}
}
