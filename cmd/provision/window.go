package main

// window.go implements the re-provision window (GAP-3): a bounded interval
// during which a hub advertises the commissioning radio even though it is
// already commissioned (the /etc/lexa/commissioned marker is present). This is
// the "explicit re-provision window (physical button hold ≥5 s, or `lexactl
// provision --window 10m`)" of ADR-0002 §Advertising.
//
// Mechanism — no new IPC, no new dependency: a single small file,
// /run/lexa/provision-window, whose contents are one line: the window's expiry
// as a Unix-seconds timestamp. The window is OPEN iff that file exists AND now
// is before the expiry. `lexactl provision --window <dur>` writes it (expiry =
// now + dur); `lexactl provision --close` removes it; the file lives on tmpfs
// (/run) so it evaporates on reboot — a re-provision window never survives a
// power cycle, which is the safe default.
//
// This file is wired into gatt.MarkerGate.Window (a func() bool seam): when it
// returns true the Advertiser advertises regardless of the marker. The
// brute-force throttle (throttle.go) can still suppress advertising on top of an
// open window — the window opens the door, the throttle can still slam it.
//
// PHYSICAL-BUTTON HOOK (stub, GPIO not built here): a factory unit exposes a
// recessed re-provision button. The intended integration is a small userspace
// handler (a gpio-keys / evdev watcher, or a systemd path/service watching a
// GPIO edge) that, on a ≥5 s hold, writes the SAME window file — either by
// shelling out to `lexactl provision --window 5m` or by writing the expiry
// directly, e.g. `date -d '+5 minutes' +%s > /run/lexa/provision-window`. No
// GPIO code lives in this service (the i.MX93 GPIO map is board-specific and
// out of scope for B4); the button handler is deployed by the provisioning
// image. Documented here so the mechanism has exactly one owner: this file.

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultWindowFile is the re-provision window file (a Unix-seconds expiry on
// tmpfs). Overridable via provision.json's "window_file".
const defaultWindowFile = "/run/lexa/provision-window"

// windowOpen reports whether the re-provision window at path is open at now:
// the file exists and its expiry (one line, Unix seconds) is still in the
// future. ANY read or parse error ⇒ closed. This is fail-CLOSED on purpose: a
// missing, corrupt, or garbage window file must never force a commissioned unit
// back onto the radio — advertising while commissioned is only ever permitted
// by a well-formed, unexpired expiry an operator (or the button hook) wrote
// deliberately.
func windowOpen(path string, now time.Time) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return false
	}
	return now.Before(time.Unix(ts, 0))
}
