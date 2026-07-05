package main

import (
	"log"
	"math"
	"sync"

	"lexa-hub/internal/bus"
	model "lexa-proto/csipmodel"
	"lexa-hub/internal/southbound/device"
)

// controlApplier is the subset of *registry.Registry the interlock needs, so it
// can be unit-tested with a fake instead of a live Modbus device.
type controlApplier interface {
	ApplyControlTo(name string, ctrl model.DERControlBase) error
}

const (
	// defaultSOCReservePct mirrors the hub optimizer's SOCReserve default so the
	// edge interlock and the hub agree on the reserve floor without extra config.
	defaultSOCReservePct = 20.0
	// interlockReserveMarginPct widens the reserve band so a mis-wired pack is
	// caught as it APPROACHES the floor, matching the hub's reserve-proximity fast
	// trip (SOCReserve+5).
	interlockReserveMarginPct = 5.0
	// interlockDischargeW is the measured discharge (W, +ve = discharge) above
	// which a charge-commanded pack is deemed to be running its setpoint backwards.
	interlockDischargeW = 100.0
)

// batterySafetyInterlock is the Tier-0 edge protection reflex (ADR-0001): a
// local, last-ditch check that runs in the modbus poll path, independent of the
// hub and the MQTT broker.
//
// If a pack the hub commanded to CHARGE is measured DISCHARGING while at/below
// its SOC reserve, the pack is inverting its setpoint (audit: battery-wrong-sign)
// and about to sail through the reserve floor — a dangerous state no correct
// command produces. The interlock force-disconnects it within one poll (~2 s in
// fast mode) without waiting on the hub's decision loop, so the unsafe window is
// bounded even if the hub or broker is down or misbehaving. The hub's Tier-1
// backstop still runs; this is defense in depth, not a replacement.
//
// The interlock never RECONNECTS a pack — that is the hub's decision. It only
// re-evaluates each poll, so once the fault clears (or the hub reconnects a
// genuinely healthy pack) it simply stops tripping.
type batterySafetyInterlock struct {
	reg controlApplier

	mu         sync.Mutex
	reservePct map[string]float64 // device → SOC reserve %
	chargeCmd  map[string]bool    // device → hub's last command was a charge (not a disconnect)
	tripped    map[string]bool    // device → currently force-disconnected by the interlock
}

// newBatterySafetyInterlock builds an interlock covering every battery-role
// device in cfg. Non-battery devices are ignored.
func newBatterySafetyInterlock(reg controlApplier, cfg *Config) *batterySafetyInterlock {
	il := &batterySafetyInterlock{
		reg:        reg,
		reservePct: map[string]float64{},
		chargeCmd:  map[string]bool{},
		tripped:    map[string]bool{},
	}
	for _, dc := range cfg.Devices {
		if dc.Role != "battery" {
			continue
		}
		r := dc.SOCReservePct
		if r <= 0 {
			r = defaultSOCReservePct
		}
		il.reservePct[dc.Name] = r
	}
	return il
}

// protects reports whether dev is a battery the interlock guards.
func (il *batterySafetyInterlock) protects(dev string) bool {
	_, ok := il.reservePct[dev]
	return ok
}

// noteControl records the hub's latest intent for a battery so the interlock can
// tell a measured discharge from a legitimately commanded one. A charge command
// is a negative setpoint; a disconnect command clears the charge intent (the hub
// is no longer trying to charge, so a later discharge is not a sign inversion).
func (il *batterySafetyInterlock) noteControl(dev string, cmd bus.BattCommand) {
	if !il.protects(dev) {
		return
	}
	charge := cmd.SetpointW != nil && *cmd.SetpointW < 0
	disconnect := cmd.Connect != nil && !*cmd.Connect
	il.mu.Lock()
	il.chargeCmd[dev] = charge && !disconnect
	il.mu.Unlock()
}

// check evaluates the interlock for one battery measurement and force-disconnects
// the pack if it is discharging while commanded to charge at/below its reserve.
// Returns true when it issued a protective disconnect this poll.
func (il *batterySafetyInterlock) check(dev string, m device.Measurements) bool {
	if !il.protects(dev) {
		return false
	}
	il.mu.Lock()
	reserve := il.reservePct[dev]
	charge := il.chargeCmd[dev]
	wasTripped := il.tripped[dev]
	il.mu.Unlock()

	discharging := !math.IsNaN(m.W) && m.W > interlockDischargeW
	nearReserve := !math.IsNaN(m.SOC) && m.SOC <= reserve+interlockReserveMarginPct
	if !(charge && discharging && nearReserve) {
		if wasTripped {
			il.mu.Lock()
			il.tripped[dev] = false
			il.mu.Unlock()
		}
		return false
	}

	// Cease discharge NOW, locally — do not wait for the hub.
	f := false
	if err := il.reg.ApplyControlTo(dev, model.DERControlBase{OpModConnect: &f}); err != nil {
		log.Printf("lexa-modbus: INTERLOCK %s force-disconnect FAILED: %v", dev, err)
		return false
	}
	il.mu.Lock()
	il.tripped[dev] = true
	il.mu.Unlock()
	log.Printf("lexa-modbus: INTERLOCK TRIP %s — commanded to charge but discharging %.0fW at SOC %.1f%% ≤ reserve+%.0f%% — force-disconnected locally (Tier-0 edge protection)",
		dev, m.W, m.SOC, interlockReserveMarginPct)
	return true
}
