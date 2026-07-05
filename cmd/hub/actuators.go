package main

import (
	"fmt"
	"math"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
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

// MQTTBatteryActuator publishes battery commands to lexa/control/battery/{device}.
type MQTTBatteryActuator struct {
	mc     mqtt.Client
	device string
	dedupe cmdDeduper
	// dispatches counts successful publishes (lexa_hub_dispatches_total,
	// TASK-044) — nil-safe (see metrics.Counter's doc) so tests that build
	// this struct without wiring a registry need no change.
	dispatches *metrics.Counter
}

func (a *MQTTBatteryActuator) ApplyBatteryCommand(cmd orchestrator.BatteryCommand) error {
	connect := "nil"
	if cmd.Connect != nil {
		connect = fmt.Sprintf("%t", *cmd.Connect)
	}
	sig := fmt.Sprintf("%g|%s", cmd.SetpointW, connect)
	now := time.Now()
	if !a.dedupe.shouldSend(sig, now) {
		return nil
	}

	msg := bus.BattCommand{
		Envelope: bus.Envelope{V: bus.BattCommandV},
		Device:   a.device,
		Connect:  cmd.Connect,
		Ts:       now.Unix(),
	}
	if !math.IsNaN(cmd.SetpointW) {
		msg.SetpointW = &cmd.SetpointW
	}
	if err := mqttutil.PublishJSON(a.mc, bus.CtrlBatteryTopic(a.device), msg); err != nil {
		a.dedupe = cmdDeduper{} // not delivered; retry next tick
		return fmt.Errorf("battery actuator %s: %w", a.device, err)
	}
	a.dispatches.Inc()
	return nil
}

// MQTTSolarActuator publishes solar curtailment commands to lexa/control/solar/{device}.
type MQTTSolarActuator struct {
	mc     mqtt.Client
	device string
	dedupe cmdDeduper
	// dispatches counts successful publishes (lexa_hub_dispatches_total, TASK-044).
	dispatches *metrics.Counter
}

func (a *MQTTSolarActuator) ApplySolarCommand(cmd orchestrator.SolarCommand) error {
	sig := fmt.Sprintf("%g", cmd.CurtailToW)
	now := time.Now()
	if !a.dedupe.shouldSend(sig, now) {
		return nil
	}

	msg := bus.SolarCommand{
		Envelope: bus.Envelope{V: bus.SolarCommandV},
		Device:   a.device,
		Ts:       now.Unix(),
	}
	if !math.IsNaN(cmd.CurtailToW) {
		msg.CurtailToW = &cmd.CurtailToW
	}
	if err := mqttutil.PublishJSON(a.mc, bus.CtrlSolarTopic(a.device), msg); err != nil {
		a.dedupe = cmdDeduper{}
		return fmt.Errorf("solar actuator %s: %w", a.device, err)
	}
	a.dispatches.Inc()
	return nil
}

// MQTTEVSEActuator publishes EVSE commands to lexa/evse/{station}/command.
type MQTTEVSEActuator struct {
	mc        mqtt.Client
	stationID string
	dedupe    cmdDeduper
	// dispatches counts successful publishes (lexa_hub_dispatches_total, TASK-044).
	dispatches *metrics.Counter
}

func (a *MQTTEVSEActuator) ApplyEVSECommand(cmd orchestrator.EVSECommand) error {
	sig := fmt.Sprintf("%s|%d|%g", cmd.StationID, cmd.ConnectorID, cmd.MaxCurrentA)
	now := time.Now()
	if !a.dedupe.shouldSend(sig, now) {
		return nil
	}

	msg := bus.EVSECommand{
		Envelope:    bus.Envelope{V: bus.EVSECommandV},
		StationID:   cmd.StationID,
		ConnectorID: cmd.ConnectorID,
		MaxCurrentA: cmd.MaxCurrentA,
		Ts:          now.Unix(),
	}
	if err := mqttutil.PublishJSON(a.mc, bus.EVSECommandTopic(a.stationID), msg); err != nil {
		a.dedupe = cmdDeduper{}
		return fmt.Errorf("evse actuator %s: %w", a.stationID, err)
	}
	a.dispatches.Inc()
	return nil
}
