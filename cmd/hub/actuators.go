package main

import (
	"time"
)

// reassertEvery bounds how long an unchanged command may go unpublished.
//
// The optimizer's restore rule re-issues every device's setpoint on every
// tick so stale values never latch downstream; publishing each of those as a
// QoS 1 message is steady-state bus traffic and Modbus register writes that
// grow with device count.  The deduper suppresses publishes whose payload is
// identical to the last one sent, but still re-asserts periodically as the
// watchdog — so if lexa-modbus or lexa-ocpp restarts and loses state, it is
// re-synced within this window.
const reassertEvery = 60 * time.Second

// cmdDeduper suppresses repeat publishes of an identical command.
// Not safe for concurrent use: actuators are invoked only from the engine's
// control-loop goroutine (Engine.executePlan).
type cmdDeduper struct {
	lastSig  string
	lastSent time.Time
}

// shouldSend reports whether a command with signature sig must be published,
// and records it as sent when so.
func (d *cmdDeduper) shouldSend(sig string, now time.Time) bool {
	if sig == d.lastSig && now.Sub(d.lastSent) < reassertEvery {
		return false
	}
	d.lastSig = sig
	d.lastSent = now
	return true
}

// reset forgets the last-sent command so the next apply publishes
// unconditionally. Called when the optimizer records a compliance breach: a
// breach means the MEASURED effect contradicts the commanded state, so the
// device may have reverted behind the hub's back (reboot to defaults, installer
// override — the solar-reboot-forget class) and the "already sent" assumption
// the deduper rests on is exactly what's in doubt. Without this, a device that
// reverted while the commanded value was unchanged got no corrective write for
// up to reassertEvery even as the hub posted CannotComply about the mismatch
// (QA 2026-07-03 spot-run: a 0 W ceiling suppressed for 30 s against an
// uncurtailed inverter). Self-limiting: re-sends happen only on breach ticks
// and stop as soon as the measured effect converges.
func (d *cmdDeduper) reset() { d.lastSig = ""; d.lastSent = time.Time{} }
