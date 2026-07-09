package main

import (
	"context"
	"fmt"
	"strings"
)

// minPlausibleYear is the §8.1 clock-sanity floor: a system clock
// reporting a year before this is almost certainly wrong (a never-synced,
// RTC-less boot, or a dead coin-cell RTC that reset to its own epoch
// default). 2026 per the task spec — a literal constant, not derived from
// the build, since a build-year bound would need a reflash to move and the
// spec asks for the simpler fixed floor.
const minPlausibleYear = 2026

// checkClock: year >= minPlausibleYear AND (NTP-synced per timedatectl OR
// the documented WEAK /dev/rtc0-exists fallback).
//
// The RTC fallback does NOT read the RTC's actual value — only that a
// battery-backed RTC device node is present — so by itself it cannot prove
// the clock is correct, only that there's a plausible source for it. It is
// deliberately weaker than an NTP confirmation, and exists because
// `timedatectl`/systemd-timedated is not guaranteed present on this
// platform: DEY ships chrony for time sync (DEVICE_ROADMAP.md §9), not
// necessarily systemd-timedated, and the dev kit's Yocto image already runs
// a systemd whose behavior around adjacent knobs (StartLimit* placement,
// DEVKIT.md) has proven to have version quirks worth not assuming away. A
// box with neither signal fails rather than silently passing on the year
// check alone — an un-synced, RTC-less clock has no plausibility evidence
// at all, only a value in range by chance.
func checkClock(ctx context.Context, env *Environment) Result {
	now := env.Now()
	year := now.Year()
	if year < minPlausibleYear {
		return fail("clock", fmt.Sprintf("system year %d < %d (implausible)", year, minPlausibleYear))
	}

	ntpSynced, ntpDetail := checkNTPSynced(ctx, env.Runner)
	if ntpSynced {
		return pass("clock", fmt.Sprintf("year=%d, %s", year, ntpDetail))
	}

	if env.RTCExists() {
		return pass("clock", fmt.Sprintf(
			"year=%d, ntp not confirmed (%s) — accepted via /dev/rtc0 present (weak fallback: existence only, RTC value unchecked)",
			year, ntpDetail))
	}
	return fail("clock", fmt.Sprintf("year=%d but neither ntp-synced nor /dev/rtc0 present (%s)", year, ntpDetail))
}

// checkNTPSynced runs `timedatectl show -p NTPSynchronized --value` and
// reports true iff it prints exactly "yes". Any failure (binary absent,
// systemd-timedated not running, non-zero exit, timeout) is reported as
// "not synced" rather than propagated as a hard check failure — the RTC
// fallback above exists precisely because this command is not guaranteed
// to exist on every image this tool runs on.
func checkNTPSynced(ctx context.Context, r Runner) (bool, string) {
	out, err := r.Output(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if err != nil {
		return false, fmt.Sprintf("timedatectl unavailable: %v", err)
	}
	val := strings.TrimSpace(out)
	if val == "yes" {
		return true, "ntp synced (NTPSynchronized=yes)"
	}
	return false, fmt.Sprintf("NTPSynchronized=%q", val)
}
