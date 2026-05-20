package registry_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lexa-hub/internal/csip/model"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/southbound/registry"
)

// ── Mock device ───────────────────────────────────────────────────────────────

// mockDevice is a thread-safe device.Device that records calls.
type mockDevice struct {
	mu           sync.Mutex
	measurements device.Measurements
	readErr      error
	applyErr     error
	applyCalls   int
	appliedCtrls []model.DERControlBase
	closeCalled  bool
}

func (m *mockDevice) ReadMeasurements() (device.Measurements, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.measurements, m.readErr
}

func (m *mockDevice) Status() (device.DeviceStatus, error) {
	return device.DeviceStatus{Connected: true, Energized: true}, nil
}

func (m *mockDevice) ApplyControl(ctrl model.DERControlBase) error {
	m.mu.Lock()
	m.applyCalls++
	m.appliedCtrls = append(m.appliedCtrls, ctrl)
	err := m.applyErr
	m.mu.Unlock()
	return err
}

func (m *mockDevice) Close() error {
	m.mu.Lock()
	m.closeCalled = true
	m.mu.Unlock()
	return nil
}

func (m *mockDevice) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyCalls
}

func (m *mockDevice) lastCtrl() model.DERControlBase {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.appliedCtrls) == 0 {
		return model.DERControlBase{}
	}
	return m.appliedCtrls[len(m.appliedCtrls)-1]
}

// ── Add / Remove ──────────────────────────────────────────────────────────────

func TestRegistry_Add_Remove(t *testing.T) {
	reg := registry.New(10 * time.Second)

	d := &mockDevice{}
	reg.Add(&registry.Entry{Name: "dev-0", Device: d})

	if err := reg.Remove("dev-0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !d.closeCalled {
		t.Error("Close should have been called on Remove")
	}
}

func TestRegistry_Remove_NonExistent_NoOp(t *testing.T) {
	reg := registry.New(10 * time.Second)
	if err := reg.Remove("not-there"); err != nil {
		t.Errorf("Remove non-existent: expected nil, got %v", err)
	}
}

func TestRegistry_Remove_OnlyTargetDevice(t *testing.T) {
	reg := registry.New(10 * time.Second)
	d0, d1 := &mockDevice{}, &mockDevice{}
	reg.Add(&registry.Entry{Name: "a", Device: d0})
	reg.Add(&registry.Entry{Name: "b", Device: d1})

	reg.Remove("a")
	if !d0.closeCalled {
		t.Error("device 'a' should be closed")
	}
	if d1.closeCalled {
		t.Error("device 'b' should NOT be closed")
	}
}

// ── ApplyControl ─────────────────────────────────────────────────────────────

func TestRegistry_ApplyControl_AllDevices(t *testing.T) {
	reg := registry.New(10 * time.Second)
	d0, d1 := &mockDevice{}, &mockDevice{}
	reg.Add(&registry.Entry{Name: "a", Device: d0})
	reg.Add(&registry.Entry{Name: "b", Device: d1})

	tr := true
	ctrl := model.DERControlBase{OpModConnect: &tr}
	if err := reg.ApplyControl(ctrl); err != nil {
		t.Fatalf("ApplyControl: %v", err)
	}
	if d0.callCount() != 1 {
		t.Errorf("device a: expected 1 ApplyControl call, got %d", d0.callCount())
	}
	if d1.callCount() != 1 {
		t.Errorf("device b: expected 1 ApplyControl call, got %d", d1.callCount())
	}
}

func TestRegistry_ApplyControl_PartialError(t *testing.T) {
	reg := registry.New(10 * time.Second)
	bad := &mockDevice{applyErr: errors.New("device offline")}
	good := &mockDevice{}
	reg.Add(&registry.Entry{Name: "bad", Device: bad})
	reg.Add(&registry.Entry{Name: "good", Device: good})

	tr := true
	err := reg.ApplyControl(model.DERControlBase{OpModConnect: &tr})
	if err == nil {
		t.Fatal("expected combined error, got nil")
	}
	// Good device should still be called despite bad device error.
	if good.callCount() != 1 {
		t.Errorf("good device should still receive ApplyControl; calls = %d", good.callCount())
	}
}

func TestRegistry_ApplyControl_NoDevices_NoError(t *testing.T) {
	reg := registry.New(10 * time.Second)
	tr := true
	if err := reg.ApplyControl(model.DERControlBase{OpModConnect: &tr}); err != nil {
		t.Errorf("empty registry: %v", err)
	}
}

// ── Poll loop / subscriptions ─────────────────────────────────────────────────

func TestRegistry_Subscribe_ReceivedOnPoll(t *testing.T) {
	const pollInterval = 20 * time.Millisecond
	reg := registry.New(pollInterval)

	d := &mockDevice{measurements: device.Measurements{W: 1234, Hz: 60.0}}
	reg.Add(&registry.Entry{Name: "solar", Device: d})
	updates, unsubscribe := reg.Subscribe()
	defer unsubscribe()

	reg.Start()
	defer reg.Stop()

	// Wait for at least one update (up to 3× the interval).
	timeout := time.After(3 * pollInterval)
	select {
	case upd := <-updates:
		if upd.Name != "solar" {
			t.Errorf("update name = %q, want 'solar'", upd.Name)
		}
		if upd.Err != nil {
			t.Errorf("unexpected error: %v", upd.Err)
		}
		if upd.Measurements.W != 1234 {
			t.Errorf("W = %g, want 1234", upd.Measurements.W)
		}
	case <-timeout:
		t.Fatal("timed out waiting for measurement update")
	}
}

func TestRegistry_Subscribe_ErrorPropagated(t *testing.T) {
	const pollInterval = 20 * time.Millisecond
	reg := registry.New(pollInterval)

	d := &mockDevice{readErr: errors.New("Modbus timeout")}
	reg.Add(&registry.Entry{Name: "broken", Device: d})
	updates, unsubscribe := reg.Subscribe()
	defer unsubscribe()

	reg.Start()
	defer reg.Stop()

	timeout := time.After(3 * pollInterval)
	select {
	case upd := <-updates:
		if upd.Err == nil {
			t.Error("expected non-nil error in update")
		}
	case <-timeout:
		t.Fatal("timed out waiting for update")
	}
}

func TestRegistry_PollsMultipleDevices(t *testing.T) {
	const pollInterval = 20 * time.Millisecond
	reg := registry.New(pollInterval)

	var countA, countB atomic.Int32
	dA := &mockDevice{}
	dA.measurements = device.Measurements{W: 100}
	dB := &mockDevice{}
	dB.measurements = device.Measurements{W: 200}
	reg.Add(&registry.Entry{Name: "A", Device: dA})
	reg.Add(&registry.Entry{Name: "B", Device: dB})
	updates, unsubscribe := reg.Subscribe()
	defer unsubscribe()

	reg.Start()
	defer reg.Stop()

	deadline := time.After(5 * pollInterval)
	for countA.Load() < 1 || countB.Load() < 1 {
		select {
		case upd := <-updates:
			switch upd.Name {
			case "A":
				countA.Add(1)
			case "B":
				countB.Add(1)
			}
		case <-deadline:
			t.Fatalf("timed out: A=%d B=%d", countA.Load(), countB.Load())
		}
	}
}

func TestRegistry_Subscribe_MultipleSubscribersReceiveSameUpdate(t *testing.T) {
	const pollInterval = 20 * time.Millisecond
	reg := registry.New(pollInterval)

	d := &mockDevice{measurements: device.Measurements{W: 4321, Hz: 60.0}}
	reg.Add(&registry.Entry{Name: "solar", Device: d})

	subA, unsubscribeA := reg.Subscribe()
	defer unsubscribeA()
	subB, unsubscribeB := reg.Subscribe()
	defer unsubscribeB()

	reg.Start()
	defer reg.Stop()

	updA := waitForUpdate(t, subA, 3*pollInterval)
	updB := waitForUpdate(t, subB, 3*pollInterval)

	if updA.Name != "solar" || updB.Name != "solar" {
		t.Fatalf("subscriber update names = %q/%q, want solar/solar", updA.Name, updB.Name)
	}
	if updA.Measurements.W != 4321 || updB.Measurements.W != 4321 {
		t.Fatalf("subscriber W values = %g/%g, want 4321/4321", updA.Measurements.W, updB.Measurements.W)
	}
}

func waitForUpdate(t *testing.T, updates <-chan registry.MeasurementUpdate, timeout time.Duration) registry.MeasurementUpdate {
	t.Helper()

	select {
	case upd, ok := <-updates:
		if !ok {
			t.Fatal("subscription channel closed")
		}
		return upd
	case <-time.After(timeout):
		t.Fatal("timed out waiting for measurement update")
	}
	return registry.MeasurementUpdate{}
}

// ── Start / Stop ──────────────────────────────────────────────────────────────

func TestRegistry_Start_Stop(t *testing.T) {
	reg := registry.New(10 * time.Millisecond)
	d := &mockDevice{measurements: device.Measurements{W: 5}}
	reg.Add(&registry.Entry{Name: "x", Device: d})

	reg.Start()
	// Stop must return promptly (not block).
	done := make(chan struct{})
	go func() {
		reg.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reg.Stop() blocked")
	}
}

func TestRegistry_UpdatesDropped_WhenConsumerSlow(t *testing.T) {
	// Subscriber channels are buffered; overflow updates are dropped per
	// subscriber, not blocking the poll loop.
	// This test just ensures Start/Stop work when updates pile up.
	reg := registry.New(1 * time.Millisecond)
	d := &mockDevice{measurements: device.Measurements{W: 1}}
	reg.Add(&registry.Entry{Name: "fast", Device: d})
	_, unsubscribe := reg.Subscribe()
	defer unsubscribe()

	reg.Start()
	time.Sleep(50 * time.Millisecond) // let it poll many times
	reg.Stop()                        // must not block
}
