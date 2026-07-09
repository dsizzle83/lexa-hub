// Command lexa-healthcheck runs the DEVICE_ROADMAP.md §8.1 OTA
// commit-or-rollback gate: seven bounded checks against the local lexa
// services (systemd unit health, api liveness, plan heartbeat, northbound
// discovery, modbus device freshness, clock sanity, and — when configured
// — cloudlink connectivity). Exit 0 means every non-SKIP check PASSed;
// exit 1 means at least one FAILed.
//
// Two consumers:
//
//   - The Mender ArtifactCommit_Enter state script
//     (scripts/mender/ArtifactCommit_Enter_00_lexa-health) invokes
//     `lexa-healthcheck -commit -budget 120s` after a new A/B rootfs slot
//     has booted. A non-zero exit there tells Mender the update is BAD:
//     it does not commit, and the device reboots back into the previous
//     (known-good) slot automatically.
//   - Ad-hoc operator use: a plain `lexa-healthcheck` run (no -commit) for
//     a quick point-in-time read, e.g. over SSH during bring-up.
//
// A false PASS ships a broken hub as "good" (bricks trust in OTA); a false
// FAIL rolls back a working release (blocks every release) — both
// directions matter, which is why every check is individually
// timeout-bounded and documents exactly what it does and does not prove
// (see each check_*.go file).
//
// # Design for testability
//
// Every check is a pure function of an injected *Environment (config dir,
// HTTP client, exec Runner, clock/uptime/RTC seams — see env.go). Nothing
// in this package shells out to systemctl/timedatectl except execRunner
// (runner.go), and nothing opens a real network socket except the HTTP
// client built in httpprobe.go. Every *_test.go in this package supplies
// fakes for both: no test here may exercise a real systemd or network.
//
// Usage:
//
//	lexa-healthcheck [-budget 120s] [-commit] [-config-dir /etc/lexa] [-api-scheme ""]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	budget := flag.Duration("budget", 120*time.Second, "total time budget; -commit retries within this window")
	commit := flag.Bool("commit", false, "retry the full check set every 5s until it passes or -budget is exhausted (the Mender ArtifactCommit gate)")
	configDir := flag.String("config-dir", "/etc/lexa", "directory holding *.json configs + the commissioned marker (override for tests/dev)")
	apiScheme := flag.String("api-scheme", "", `force the lexa-api scheme ("http"|"https"); empty probes https then falls back to http`)
	flag.Parse()

	env := newRealEnvironment(*configDir, *apiScheme)

	opts := RunOptions{
		Budget: *budget,
		Commit: *commit,
		Clock:  RealClock(),
		OnAttempt: func(a Attempt) {
			if *commit {
				fmt.Fprintf(os.Stderr, "[healthcheck] --- attempt %d ---\n", a.N)
			}
			for _, r := range a.Checks {
				fmt.Fprintf(os.Stderr, "[healthcheck] %s %s (%s)\n", r.Name, r.Status, r.Detail)
			}
		},
	}

	summary := Run(context.Background(), env, allChecks(), opts)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "[healthcheck] encode summary: %v\n", err)
	}

	if !summary.Pass {
		os.Exit(1)
	}
}
