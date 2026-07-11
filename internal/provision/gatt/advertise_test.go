package gatt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeAdManager records Register/Unregister calls and can be scripted to fail.
type fakeAdManager struct {
	registered   bool
	registers    int
	unregisters  int
	registerErr  error
	unregisterEr error
}

func (f *fakeAdManager) Register() error {
	if f.registerErr != nil {
		return f.registerErr
	}
	f.registers++
	f.registered = true
	return nil
}

func (f *fakeAdManager) Unregister() error {
	if f.unregisterEr != nil {
		return f.unregisterEr
	}
	f.unregisters++
	f.registered = false
	return nil
}

func TestMarkerGate(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "commissioned")

	g := MarkerGate{MarkerPath: marker}
	// Absent marker → advertise.
	if !g.ShouldAdvertise() {
		t.Fatal("uncommissioned (marker absent) must advertise")
	}
	// Present marker → silent.
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g.ShouldAdvertise() {
		t.Fatal("commissioned (marker present) must NOT advertise")
	}
}

func TestMarkerGate_WindowOverride(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "commissioned")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Marker present but the B4 re-provision window is open → advertise anyway.
	g := MarkerGate{MarkerPath: marker, Window: func() bool { return true }}
	if !g.ShouldAdvertise() {
		t.Fatal("open re-provision window must force advertising even when commissioned")
	}
	// Window closed → back to silent.
	g.Window = func() bool { return false }
	if g.ShouldAdvertise() {
		t.Fatal("closed window + present marker must be silent")
	}
}

func TestMarkerGate_FailClosedOnStatError(t *testing.T) {
	// A non-"not exist" stat error (simulated via an injected stat) must be
	// treated as present → do NOT advertise (never broadcast a commissioned
	// unit because its marker was momentarily unreadable).
	g := MarkerGate{
		MarkerPath: "/whatever",
		stat:       func(string) (os.FileInfo, error) { return nil, errors.New("EIO") },
	}
	if g.ShouldAdvertise() {
		t.Fatal("an ambiguous stat error must fail closed (no advertising)")
	}
}

func TestAdvertiser_ReconcileTransitions(t *testing.T) {
	mgr := &fakeAdManager{}
	should := true
	var changes []bool
	adv := NewAdvertiser(mgr, GateFunc(func() bool { return should }), func(on bool) {
		changes = append(changes, on)
	})

	// Uncommissioned → register once.
	mustReconcile(t, adv)
	if !adv.Advertising() || mgr.registers != 1 {
		t.Fatalf("first reconcile: advertising=%v registers=%d", adv.Advertising(), mgr.registers)
	}
	// Idempotent while state unchanged.
	mustReconcile(t, adv)
	if mgr.registers != 1 {
		t.Fatalf("second reconcile re-registered: registers=%d", mgr.registers)
	}
	// Commit → next reconcile unregisters.
	should = false
	mustReconcile(t, adv)
	if adv.Advertising() || mgr.unregisters != 1 {
		t.Fatalf("after commit: advertising=%v unregisters=%d", adv.Advertising(), mgr.unregisters)
	}
	// Idempotent off.
	mustReconcile(t, adv)
	if mgr.unregisters != 1 {
		t.Fatalf("re-unregistered while off: unregisters=%d", mgr.unregisters)
	}
	// onChange fired exactly on the two transitions.
	if len(changes) != 2 || changes[0] != true || changes[1] != false {
		t.Fatalf("onChange edges = %v, want [true false]", changes)
	}
}

func TestAdvertiser_ReconcileErrorLeavesStateForRetry(t *testing.T) {
	mgr := &fakeAdManager{registerErr: errors.New("bluez busy")}
	adv := NewAdvertiser(mgr, GateFunc(func() bool { return true }), nil)
	if err := adv.Reconcile(); err == nil {
		t.Fatal("expected register error to propagate")
	}
	if adv.Advertising() {
		t.Fatal("advertising must stay false after a failed register")
	}
	// Recovery: clear the fault, next reconcile registers.
	mgr.registerErr = nil
	mustReconcile(t, adv)
	if !adv.Advertising() || mgr.registers != 1 {
		t.Fatalf("retry: advertising=%v registers=%d", adv.Advertising(), mgr.registers)
	}
}

func TestAdvertiser_StopLatchesOff(t *testing.T) {
	mgr := &fakeAdManager{}
	should := true
	adv := NewAdvertiser(mgr, GateFunc(func() bool { return should }), nil)
	mustReconcile(t, adv)
	if err := adv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if adv.Advertising() || mgr.unregisters != 1 {
		t.Fatalf("after Stop: advertising=%v unregisters=%d", adv.Advertising(), mgr.unregisters)
	}
	// A racing Reconcile after Stop must NOT bring the radio back up.
	mustReconcile(t, adv)
	if adv.Advertising() || mgr.registers != 1 {
		t.Fatalf("Reconcile after Stop re-advertised: advertising=%v registers=%d", adv.Advertising(), mgr.registers)
	}
}

func TestLocalName(t *testing.T) {
	cases := map[string]string{
		"LX93-000042": "LEXA-000042",
		"ABC":         "LEXA-ABC",
		"":            "LEXA-unknown",
		"123456":      "LEXA-123456",
		"XX7654321":   "LEXA-654321",
	}
	for in, want := range cases {
		if got := LocalName(in); got != want {
			t.Errorf("LocalName(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustReconcile(t *testing.T, adv *Advertiser) {
	t.Helper()
	if err := adv.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}
