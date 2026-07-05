package main

import (
	"errors"
	"math"
	"testing"

	"lexa-hub/internal/bus"
	model "lexa-proto/csipmodel"
	"lexa-hub/internal/southbound/device"
)

// fakeDevice is a controllable device.Device for retryDevice tests.
type fakeDevice struct {
	w       float64
	readErr error
	ctrlErr error
	closed  bool
	applied []model.DERControlBase // every control this device received, in order
}

func (f *fakeDevice) ReadMeasurements() (device.Measurements, error) {
	if f.readErr != nil {
		return device.Measurements{}, f.readErr
	}
	return device.Measurements{W: f.w, V: 240, Hz: 60}, nil
}
func (f *fakeDevice) ApplyControl(ctrl model.DERControlBase) error {
	if f.ctrlErr != nil {
		return f.ctrlErr
	}
	f.applied = append(f.applied, ctrl)
	return nil
}
func (f *fakeDevice) Status() (device.DeviceStatus, error) {
	return device.DeviceStatus{Connected: true}, nil
}
func (f *fakeDevice) Close() error { f.closed = true; return nil }

// TestRetryDevice_ReconnectsAfterMidSessionError is the regression for the
// device-dropout gap: a device that opened fine but then returns a broken-pipe
// error must be closed and reopened on the next poll, not error forever.
func TestRetryDevice_ReconnectsAfterMidSessionError(t *testing.T) {
	dead := &fakeDevice{readErr: errors.New("write: broken pipe")}
	healthy := &fakeDevice{w: 4800}
	opens := 0
	rd := &retryDevice{open: func() (device.Device, error) {
		opens++
		if opens == 1 {
			return dead, nil
		}
		return healthy, nil
	}}

	// Tick 1: opens the (soon-dead) device, the read fails → session dropped.
	if _, err := rd.ReadMeasurements(); err == nil {
		t.Fatal("expected the broken-pipe error on the first read")
	}
	if !dead.closed {
		t.Error("the dead session must be Closed before reconnecting")
	}

	// Tick 2: reconnects and succeeds.
	m, err := rd.ReadMeasurements()
	if err != nil {
		t.Fatalf("expected reconnect + success, got %v", err)
	}
	if m.W != 4800 {
		t.Errorf("W = %.0f, want 4800 after reconnect", m.W)
	}
	if opens != 2 {
		t.Errorf("opens = %d, want 2 (initial + one reconnect)", opens)
	}
}

// TestRetryDevice_ControlErrorDropsSession: a broken pipe on a control write
// must also drop the session so the next poll reconnects.
func TestRetryDevice_ControlErrorDropsSession(t *testing.T) {
	bad := &fakeDevice{ctrlErr: errors.New("write: broken pipe")}
	opens := 0
	rd := &retryDevice{open: func() (device.Device, error) {
		opens++
		if opens == 1 {
			return bad, nil
		}
		return &fakeDevice{w: 100}, nil
	}}

	rd.ReadMeasurements() // prime live = bad
	if err := rd.ApplyControl(model.DERControlBase{}); err == nil {
		t.Fatal("expected the control write to error")
	}
	if !bad.closed {
		t.Error("a control-write error must drop the session")
	}
	if _, err := rd.ReadMeasurements(); err != nil { // reconnects
		t.Fatalf("expected reconnect after control error, got %v", err)
	}
	if opens != 2 {
		t.Errorf("opens = %d, want 2", opens)
	}
}

// TestRetryDevice_DisconnectedIsSafe: while disconnected, reads report NaN (not a
// stale value) and control writes are a no-op rather than a crash.
func TestRetryDevice_DisconnectedIsSafe(t *testing.T) {
	rd := &retryDevice{open: func() (device.Device, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}

	m, err := rd.ReadMeasurements()
	if err == nil {
		t.Fatal("expected the open error to surface")
	}
	if !math.IsNaN(m.W) {
		t.Errorf("W = %.0f, want NaN while disconnected (no stale value)", m.W)
	}
	if err := rd.ApplyControl(model.DERControlBase{}); err != nil {
		t.Errorf("ApplyControl while disconnected should be a no-op, got %v", err)
	}
	if st, _ := rd.Status(); st.Connected {
		t.Error("Status must report not-connected while disconnected")
	}
}

// === Reconnect reconcile (Phase 4) ========================================

// ctrlCeilingW decodes the OpModMaxLimW of a control, or NaN when absent.
func ctrlCeilingW(ctrl model.DERControlBase) float64 {
	if ctrl.OpModMaxLimW == nil {
		return math.NaN()
	}
	return float64(ctrl.OpModMaxLimW.Value) * math.Pow(10, float64(ctrl.OpModMaxLimW.Multiplier))
}

// A control transition that happens while the device is dark must be delivered
// on reconnect: cap active → device drops → cap released (restore commanded
// into the void) → device returns → the RESTORE must land before the first
// trusted read, or the inverter stays latched at the stale ceiling forever
// (QA 2026-07-02: release-while-rebooting).
func TestRetryDevice_ReassertsDesiredStateOnReconnect(t *testing.T) {
	first := &fakeDevice{w: 1000}
	second := &fakeDevice{w: 1000}
	opens := 0
	rd := &retryDevice{
		cfg: DeviceConfig{Name: "solar", Role: "inverter"},
		open: func() (device.Device, error) {
			opens++
			if opens == 1 {
				return first, nil
			}
			return second, nil
		},
	}

	// Cap active: curtail to 1000 W.
	curtail := model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: 1000}}
	if _, err := rd.ReadMeasurements(); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := rd.ApplyControl(curtail); err != nil {
		t.Fatalf("curtail: %v", err)
	}

	// Device drops; the cap is RELEASED while it is dark.
	first.readErr = errors.New("write: broken pipe")
	if _, err := rd.ReadMeasurements(); err == nil {
		t.Fatal("expected the dead session to error")
	}
	restore := solarCommandToControl(bus.SolarCommand{}) // nil CurtailToW = uncurtail
	if err := rd.ApplyControl(restore); err != nil {
		t.Fatalf("restore into the void must not error: %v", err)
	}

	// Device returns: the restore must be re-asserted before the first read.
	if _, err := rd.ReadMeasurements(); err != nil {
		t.Fatalf("reconnect read: %v", err)
	}
	if len(second.applied) != 1 {
		t.Fatalf("reconnected device received %d controls, want 1 (the re-asserted restore)", len(second.applied))
	}
	if ceil := ctrlCeilingW(second.applied[0]); ceil < 1e6 {
		t.Errorf("re-asserted ceiling = %.0f W — the stale 1000 W curtailment, not the restore", ceil)
	}
}

// A never-commanded inverter may still hold a stale ceiling (latched before
// this process started): reconnect clears it with the restore ceiling.
func TestRetryDevice_ClearsStaleCeilingOnFirstConnect(t *testing.T) {
	dev := &fakeDevice{w: 1000}
	rd := &retryDevice{
		cfg:  DeviceConfig{Name: "solar", Role: "inverter"},
		open: func() (device.Device, error) { return dev, nil },
	}
	if _, err := rd.ReadMeasurements(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(dev.applied) != 1 {
		t.Fatalf("inverter received %d controls on first connect, want 1 (stale-ceiling clear)", len(dev.applied))
	}
	if ceil := ctrlCeilingW(dev.applied[0]); ceil < 1e6 {
		t.Errorf("first-connect clear ceiling = %.0f W, want the restore ceiling", ceil)
	}
}

// Read-only and orchestrator-refreshed devices get no unsolicited write.
func TestRetryDevice_NoUnsolicitedWriteForMeterOrBattery(t *testing.T) {
	for _, role := range []string{"meter", "battery"} {
		dev := &fakeDevice{w: 100}
		rd := &retryDevice{
			cfg:  DeviceConfig{Name: role, Role: role},
			open: func() (device.Device, error) { return dev, nil },
		}
		if _, err := rd.ReadMeasurements(); err != nil {
			t.Fatalf("%s read: %v", role, err)
		}
		if len(dev.applied) != 0 {
			t.Errorf("%s received %d unsolicited controls on connect, want 0", role, len(dev.applied))
		}
	}
}

// A failed reconcile write means the session is suspect: drop it and retry the
// whole open+reconcile+read sequence next poll — never read past a device in
// an unknown control state.
func TestRetryDevice_ReconcileFailureDropsSession(t *testing.T) {
	bad := &fakeDevice{w: 1000, ctrlErr: errors.New("write refused")}
	good := &fakeDevice{w: 1000}
	opens := 0
	rd := &retryDevice{
		cfg: DeviceConfig{Name: "solar", Role: "inverter"},
		open: func() (device.Device, error) {
			opens++
			if opens == 1 {
				return bad, nil
			}
			return good, nil
		},
	}
	if _, err := rd.ReadMeasurements(); err == nil {
		t.Fatal("expected the failed reconcile to surface as an error")
	}
	if !bad.closed {
		t.Error("the un-reconciled session must be dropped")
	}
	if _, err := rd.ReadMeasurements(); err != nil {
		t.Fatalf("next poll should reconnect and reconcile cleanly: %v", err)
	}
	if len(good.applied) != 1 {
		t.Errorf("recovered device received %d controls, want 1", len(good.applied))
	}
}
