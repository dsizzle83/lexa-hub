package main

import (
	"math"
	"testing"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/southbound/device"
)

type fakeApplier struct {
	calls []struct {
		dev  string
		ctrl model.DERControlBase
	}
	err error
}

func (f *fakeApplier) ApplyControlTo(name string, ctrl model.DERControlBase) error {
	f.calls = append(f.calls, struct {
		dev  string
		ctrl model.DERControlBase
	}{name, ctrl})
	return f.err
}

func fptr(v float64) *float64 { return &v }
func bptr(v bool) *bool       { return &v }

func newTestInterlock(fa controlApplier) *batterySafetyInterlock {
	cfg := &Config{Devices: []DeviceConfig{
		{Name: "bat", Role: "battery"}, // reserve defaults to 20
		{Name: "pv", Role: "inverter"},
	}}
	return newBatterySafetyInterlock(fa, cfg)
}

// A pack commanded to CHARGE but measured DISCHARGING at/below reserve must be
// force-disconnected locally within one poll. (audit: battery-wrong-sign)
func TestInterlock_TripsOnSignInversionAtReserve(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	il.noteControl("bat", bus.BattCommand{SetpointW: fptr(-3000)})    // hub commands charge
	tripped := il.check("bat", device.Measurements{W: 4800, SOC: 12}) // discharging, low SOC
	if !tripped {
		t.Fatal("expected interlock to trip on sign inversion at reserve")
	}
	if len(fa.calls) != 1 {
		t.Fatalf("expected exactly 1 disconnect write, got %d", len(fa.calls))
	}
	c := fa.calls[0]
	if c.dev != "bat" || c.ctrl.OpModConnect == nil || *c.ctrl.OpModConnect {
		t.Errorf("expected OpModConnect=false to bat, got %+v", c)
	}
}

// A pack commanded to charge and actually charging (negative W) must not trip.
func TestInterlock_NoTripWhenCharging(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	il.noteControl("bat", bus.BattCommand{SetpointW: fptr(-3000)})
	if il.check("bat", device.Measurements{W: -3000, SOC: 12}) {
		t.Error("tripped while the pack is correctly charging")
	}
	if len(fa.calls) != 0 {
		t.Errorf("wrote a disconnect while charging: %+v", fa.calls)
	}
}

// A pack legitimately DISCHARGING (the hub commanded discharge) must not trip —
// the interlock guards sign inversion, not normal peak discharge.
func TestInterlock_NoTripOnCommandedDischarge(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	il.noteControl("bat", bus.BattCommand{SetpointW: fptr(3000)}) // hub commands DISCHARGE
	if il.check("bat", device.Measurements{W: 3000, SOC: 60}) {
		t.Error("tripped on a legitimate commanded discharge")
	}
}

// Discharging while charge-commanded but SOC well above reserve is not the
// reserve-floor emergency; the hub's slower Tier-1 sign-inversion path handles it.
func TestInterlock_NoTripHighSOC(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	il.noteControl("bat", bus.BattCommand{SetpointW: fptr(-3000)})
	if il.check("bat", device.Measurements{W: 4800, SOC: 80}) {
		t.Error("tripped far from reserve; should defer to the hub")
	}
}

// A missing SOC (NaN) must never trip — the interlock only acts on a confirmed
// low reading, so a failed metrics read cannot cause a false disconnect.
func TestInterlock_NoTripOnMissingSOC(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	il.noteControl("bat", bus.BattCommand{SetpointW: fptr(-3000)})
	if il.check("bat", device.Measurements{W: 4800, SOC: math.NaN()}) {
		t.Error("tripped on a NaN SOC reading")
	}
}

// A disconnect command clears the charge intent, so a subsequent discharge does
// not read as a sign inversion.
func TestInterlock_DisconnectClearsChargeIntent(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	il.noteControl("bat", bus.BattCommand{SetpointW: fptr(-3000)})
	il.noteControl("bat", bus.BattCommand{Connect: bptr(false)}) // hub disconnects
	if il.check("bat", device.Measurements{W: 4800, SOC: 12}) {
		t.Error("tripped after the charge intent was cleared by a disconnect")
	}
}

// Non-battery devices are never guarded.
func TestInterlock_IgnoresNonBattery(t *testing.T) {
	fa := &fakeApplier{}
	il := newTestInterlock(fa)
	if il.check("pv", device.Measurements{W: 4800, SOC: 5}) {
		t.Error("interlock acted on a non-battery device")
	}
	if il.protects("pv") {
		t.Error("protects reported true for a non-battery device")
	}
}
