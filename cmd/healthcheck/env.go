package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// Environment is the single injected seam every check function runs
// against: real filesystem/exec/HTTP/clock access is confined to the
// constructor below (newRealEnvironment) plus execRunner (runner.go) and
// newProbeHTTPClient (httpprobe.go), so every check in check_*.go can be
// table-tested against fakes without shelling out to a real systemctl or
// opening a real network socket (house rule — see main.go's package doc).
type Environment struct {
	// ConfigDir holds api.json/modbus.json/northbound.json/cloudlink.json
	// and the `commissioned` marker (DEVICE_ROADMAP.md §9). Default
	// /etc/lexa; overridable via -config-dir so tests point it at a temp
	// directory instead of the real filesystem.
	ConfigDir string

	// Runner executes external commands (systemctl, timedatectl). Real:
	// execRunner. Tests must inject a fake — no test in this package may
	// shell out to a real systemctl/timedatectl.
	Runner Runner

	// HTTPClient serves every loopback HTTP probe (api /healthz, /status,
	// cloudlink /metrics). Built once with InsecureSkipVerify — see
	// httpprobe.go's doc for why that is the right call for a pure
	// liveness probe that never leaves 127.0.0.1.
	HTTPClient *http.Client

	// APIScheme forces the scheme used to reach lexa-api ("http"|"https");
	// empty (the default) probes https first and falls back to http, so
	// this tool works whether or not api.json's `tls` key (landing in a
	// parallel unit, TASK-088) has been enabled on a given box yet.
	APIScheme string

	// Now returns the current wall-clock time. Real: time.Now. Tests
	// inject a fixed clock for determinism (clock-sanity check's year
	// test, and the northbound journal-freshness-vs-boot-time comparison).
	Now func() time.Time

	// Uptime returns how long the system has been up (real: parses
	// /proc/uptime). Combined with Now to derive boot time for the
	// northbound journal-freshness check.
	Uptime func() (time.Duration, error)

	// RTCExists reports whether a battery-backed RTC device node is
	// present (real: os.Stat("/dev/rtc0")). This is the clock check's
	// documented WEAK fallback — see check_clock.go's doc for exactly how
	// weak, and why it exists anyway.
	RTCExists func() bool

	// JournalDir returns the journal directory for a given service name.
	// Real: "/var/lib/lexa/journal/"+service — deliberately NOT under
	// ConfigDir, since journals live on /var/lib/lexa (AD-005), a
	// different partition than /etc/lexa. Overridable so tests point it
	// at a temp directory.
	JournalDir func(service string) string
}

// newRealEnvironment wires every seam to its real, on-device implementation.
func newRealEnvironment(configDir, apiScheme string) *Environment {
	return &Environment{
		ConfigDir:  configDir,
		Runner:     execRunner{},
		HTTPClient: newProbeHTTPClient(),
		APIScheme:  apiScheme,
		Now:        time.Now,
		Uptime:     readProcUptime,
		RTCExists: func() bool {
			_, err := os.Stat("/dev/rtc0")
			return err == nil
		},
		JournalDir: func(service string) string {
			return "/var/lib/lexa/journal/" + service
		},
	}
}

// readProcUptime parses the first field of /proc/uptime (seconds since
// boot, as a float — see proc(5)) into a time.Duration.
func readProcUptime() (time.Duration, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	var seconds float64
	if _, err := fmt.Sscanf(string(data), "%f", &seconds); err != nil {
		return 0, fmt.Errorf("parse /proc/uptime %q: %w", string(data), err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
