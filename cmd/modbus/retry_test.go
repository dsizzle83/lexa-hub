package main

import (
	"errors"
	"math"
	"testing"

	"lexa-hub/internal/southbound/device"
	model "lexa-proto/csipmodel"
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

// === Reconnect hook (TASK-032: reassert is the reconciler's job) ============

// The bare transport wrapper issues NO unsolicited write on (re)connect for ANY
// role — reassert-on-reconnect is owned entirely by the reconciler via the
// onReconnect hook (ledger L4). This pins that the deleted lastCtrl replay /
// reassertLocked stale-ceiling clear leaves no write behind on the transport
// path itself.
func TestRetryDevice_NoUnsolicitedWriteOnConnect(t *testing.T) {
	for _, role := range []string{"meter", "battery", "inverter"} {
		dev := &fakeDevice{w: 100}
		rd := &retryDevice{
			cfg:  DeviceConfig{Name: role, Role: role},
			open: func() (device.Device, error) { return dev, nil },
		}
		if _, err := rd.ReadMeasurements(); err != nil {
			t.Fatalf("%s read: %v", role, err)
		}
		if len(dev.applied) != 0 {
			t.Errorf("%s received %d unsolicited controls on connect, want 0 (reassert is the reconciler's job)", role, len(dev.applied))
		}
	}
}

// TestRetryDevice_OnReconnectFiresPerReopen (TASK-032): an active-reconciled
// device fires its onReconnect hook on every successful reopen (initial open and
// each reconnect), and the transport itself never writes on reconnect — the
// reconciler shell is the single reasserter, driven off this signal. An explicit
// ApplyControl still reaches hardware but is never replayed on the next reopen
// (no lastCtrl exists anymore).
func TestRetryDevice_OnReconnectFiresPerReopen(t *testing.T) {
	dev := &fakeDevice{w: -500}
	reconnects := 0
	rd := &retryDevice{
		cfg:         DeviceConfig{Name: "battery-0", Role: "battery"},
		onReconnect: func() { reconnects++ },
		open:        func() (device.Device, error) { return dev, nil },
	}

	// Initial open: onReconnect fires; no unsolicited write.
	if _, err := rd.ReadMeasurements(); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if reconnects != 1 {
		t.Fatalf("onReconnect calls = %d, want 1", reconnects)
	}
	if len(dev.applied) != 0 {
		t.Fatalf("no unsolicited write on connect, got %d", len(dev.applied))
	}

	// An explicit control reaches hardware.
	if err := rd.ApplyControl(model.DERControlBase{OpModConnect: bptr(true)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(dev.applied) != 1 {
		t.Errorf("the control write should reach hardware, applied=%d", len(dev.applied))
	}

	// Drop and reconnect: onReconnect fires again, and the earlier control is
	// NOT replayed by the transport (applied stays at the one explicit write).
	dev.readErr = errors.New("write: broken pipe")
	if _, err := rd.ReadMeasurements(); err == nil {
		t.Fatal("expected the dropped session to error")
	}
	dev.readErr = nil
	if _, err := rd.ReadMeasurements(); err != nil {
		t.Fatalf("reconnect read: %v", err)
	}
	if reconnects != 2 {
		t.Errorf("onReconnect calls = %d, want 2 after reconnect", reconnects)
	}
	if len(dev.applied) != 1 {
		t.Errorf("transport must not replay a control on reconnect; applied=%d, want 1", len(dev.applied))
	}
}
