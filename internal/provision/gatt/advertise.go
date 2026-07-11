package gatt

import (
	"errors"
	"io/fs"
	"os"
	"sync"
)

// AdManager is the advertising radio, abstracted so the reconcile logic is
// testable without BlueZ. The real implementation (bluez.go's bluezAdManager)
// exports an org.bluez.LEAdvertisement1 object and calls
// LEAdvertisingManager1.RegisterAdvertisement / UnregisterAdvertisement.
// Register and Unregister must be idempotent — Advertiser only calls them on a
// state transition, but a defensive double call must not error.
type AdManager interface {
	Register() error
	Unregister() error
}

// Gate decides, at a moment in time, whether the radio should be advertising.
// The production implementation is MarkerGate; tests inject a trivial one.
type Gate interface {
	ShouldAdvertise() bool
}

// GateFunc adapts a bare predicate to Gate.
type GateFunc func() bool

// ShouldAdvertise implements Gate.
func (f GateFunc) ShouldAdvertise() bool { return f() }

// MarkerGate is the ADR-0002 advertising policy: advertise ONLY while the unit
// is uncommissioned (the commissioned marker file is absent) OR while an
// explicit re-provision window is open. Both a committed marker mid-run and an
// expiring window are picked up on the next Advertiser.Reconcile, so
// advertising stops as soon as commissioning completes.
type MarkerGate struct {
	// MarkerPath is the commissioned-marker file (default
	// /etc/lexa/commissioned). Its PRESENCE means "commissioned — stay off the
	// radio"; its ABSENCE means "uncommissioned — advertise".
	MarkerPath string
	// Window is the B4 re-provision-window seam. When non-nil and it returns
	// true, advertising is forced on even if the marker is present (physical
	// button hold / `lexactl provision --window`). B2 always passes nil, so
	// the gate is purely the marker.
	Window func() bool
	// stat is injectable for tests; nil uses os.Stat.
	stat func(string) (os.FileInfo, error)
}

// ShouldAdvertise implements Gate.
func (g MarkerGate) ShouldAdvertise() bool {
	if g.Window != nil && g.Window() {
		return true
	}
	statFn := g.stat
	if statFn == nil {
		statFn = os.Stat
	}
	_, err := statFn(g.MarkerPath)
	// Advertise only when the marker is provably absent. Any other stat error
	// (permission, I/O) is treated as "present/unknown → do NOT advertise":
	// fail closed, never leave a commissioned unit broadcasting because its
	// marker was momentarily unreadable.
	return errors.Is(err, fs.ErrNotExist)
}

// Advertiser reconciles desired advertising state (from a Gate) against actual
// radio state (via an AdManager), calling Register/Unregister only on a
// transition. It is safe for concurrent Reconcile/Stop calls (the reconcile
// ticker and a SIGTERM handler may race).
type Advertiser struct {
	mgr  AdManager
	gate Gate

	mu          sync.Mutex
	advertising bool
	stopped     bool
	onChange    func(advertising bool)
}

// NewAdvertiser builds an Advertiser. onChange (optional) fires on every actual
// transition with the new state — cmd/provision wires it to the
// lexa_provision_advertising gauge.
func NewAdvertiser(mgr AdManager, gate Gate, onChange func(bool)) *Advertiser {
	return &Advertiser{mgr: mgr, gate: gate, onChange: onChange}
}

// Reconcile brings the radio into line with the gate: registers advertising
// when it should be on and is off, unregisters when it should be off and is on.
// A no-op when already in the desired state. Errors from the AdManager are
// returned but leave the tracked state unchanged so the next Reconcile retries.
func (a *Advertiser) Reconcile() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil
	}
	want := a.gate.ShouldAdvertise()
	switch {
	case want && !a.advertising:
		if err := a.mgr.Register(); err != nil {
			return err
		}
		a.advertising = true
		a.fireLocked(true)
	case !want && a.advertising:
		if err := a.mgr.Unregister(); err != nil {
			return err
		}
		a.advertising = false
		a.fireLocked(false)
	}
	return nil
}

// Advertising reports the last reconciled radio state.
func (a *Advertiser) Advertising() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.advertising
}

// Stop unregisters advertising if active and latches the Advertiser off so a
// racing Reconcile cannot bring the radio back up during shutdown.
func (a *Advertiser) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	if !a.advertising {
		return nil
	}
	err := a.mgr.Unregister()
	a.advertising = false
	a.fireLocked(false)
	return err
}

func (a *Advertiser) fireLocked(advertising bool) {
	if a.onChange != nil {
		a.onChange(advertising)
	}
}

// LocalName builds the ADR-0002 advertised local name, "LEXA-<last 6 of
// serial>". A serial shorter than 6 characters is used whole; an empty serial
// yields "LEXA-unknown" so the device is still discoverable during a
// no-identity bench bring-up (matching cmd/api's resolveSerial fallback
// philosophy).
func LocalName(serial string) string {
	if serial == "" {
		return "LEXA-unknown"
	}
	r := []rune(serial)
	if len(r) > 6 {
		r = r[len(r)-6:]
	}
	return "LEXA-" + string(r)
}
