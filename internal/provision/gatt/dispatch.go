// Package gatt is the BlueZ-facing half of the LEXA Provision v1 commissioning
// service (ADR-0002, unit B2). It has two clearly separated layers:
//
//   - A pure, transport-free logic layer — Dispatcher (write → sec1 handshake
//     → framed indications) and Advertiser/MarkerGate (advertise iff the unit
//     is uncommissioned). These have NO D-Bus dependency and are unit-tested
//     against fakes and the real sec1 peripheral, exactly as B1's
//     appDriver drives peripheral.HandleChunk.
//   - A hardware layer — Server (bluez.go), the godbus/dbus/v5 GATT server and
//     LEAdvertisingManager1 advertiser that exports the ADR-0002 object tree
//     onto org.bluez. That layer needs a live BlueZ D-Bus and is validated on
//     the dev kit in Phase C, not in CI.
//
// The seam between the two is deliberately narrow: the Server calls
// Dispatcher.OnWrite for every GATT WriteValue and pushes the returned framed
// chunks back out as notifications/indications, and reconciles advertising by
// calling Advertiser.Reconcile. Nothing in the pure layer imports godbus.
package gatt

import (
	"encoding/json"
	"sync"

	"lexa-hub/internal/provision/sec1"
)

// Observer receives edge notifications the metrics layer turns into counters.
// Both hooks are optional (nil is a no-op) so gatt stays decoupled from any
// particular metrics implementation — cmd/provision wires internal/metrics
// counters in, the same function-value pattern mqttutil.Instrumentation uses.
type Observer struct {
	// OnSessionEstablished fires once each time a sec1 handshake newly
	// completes (a false→true edge of Peripheral.SessionEstablished). A
	// client that re-runs the handshake within the same connection fires it
	// again.
	OnSessionEstablished func()
	// OnPopFailure fires once per wrong-PoP confirm (each increment of
	// Peripheral.PopFailures). The advertising brute-force throttle (B4) is a
	// separate concern; this is purely the metric.
	OnPopFailure func()
}

// Dispatcher is the pure routing layer between GATT characteristic writes and
// the sec1 handshake state machine. It owns exactly one *sec1.Peripheral at a
// time (rebuilt via Reset for a fresh connection — the B4 per-connection
// lifecycle seam) and is safe for concurrent use: BlueZ may deliver WriteValue
// callbacks from its own goroutine, so every entry point is mutex-guarded and
// the peripheral (itself not concurrency-safe) is only ever touched under that
// lock.
type Dispatcher struct {
	newPeripheral func() *sec1.Peripheral
	obs           Observer

	mu   sync.Mutex
	p    *sec1.Peripheral
	last dispatchSnapshot
}

// dispatchSnapshot is the previous observed peripheral state, used to detect
// the edges Observer reports.
type dispatchSnapshot struct {
	established bool
	popFailures int
}

// NewDispatcher builds a Dispatcher. newPeripheral is called immediately for
// the first session and again on every Reset; it must return a freshly
// constructed peripheral (see cmd/provision for the sec1.PeripheralConfig it
// closes over). obs may be the zero Observer.
func NewDispatcher(newPeripheral func() *sec1.Peripheral, obs Observer) *Dispatcher {
	d := &Dispatcher{newPeripheral: newPeripheral, obs: obs}
	d.p = newPeripheral()
	return d
}

// OnWrite feeds one characteristic write (a single GATT chunk arriving on uuid)
// to the sec1 peripheral and returns the framed hub responses to push back as
// notifications/indications — each Outbound.Chunk on Outbound.UUID. The
// returned slice IS the indication stream; a test can reassemble and decrypt it
// exactly as B1's appDriver does. A protocol fault is swallowed by the
// peripheral (recorded in its LastError) and simply yields no responses, so
// OnWrite never returns an error: an unauthenticated or misbehaving central
// must learn nothing and must never crash the service.
func (d *Dispatcher) OnWrite(uuid string, chunk []byte) []sec1.Outbound {
	d.mu.Lock()
	defer d.mu.Unlock()
	outs, _ := d.p.HandleChunk(uuid, chunk)
	d.observeLocked()
	return outs
}

// InfoValue is the plaintext JSON a read of the info characteristic returns:
// the peripheral's InfoDoc marshaled compactly. cmd/provision wires the real
// build version and serial into the peripheral's config, so this reports truth.
func (d *Dispatcher) InfoValue() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return json.Marshal(d.p.InfoDoc())
}

// Reset abandons the current session and installs a fresh peripheral. The B4
// seam: the Server calls this when a central disconnects (or after done) so the
// next commissioning attempt starts from a clean handshake and clean receive
// counters. Edge-detection state is reset too, so the next handshake counts as
// a new session.
func (d *Dispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.p = d.newPeripheral()
	d.last = dispatchSnapshot{}
}

// DoneReceived reports whether the current session's central has sent the
// terminal done message — the Server's cue to stop advertising and (via Reset)
// recycle the peripheral.
func (d *Dispatcher) DoneReceived() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.p.DoneReceived
}

// observeLocked turns the peripheral's post-write state into Observer edges.
// Must be called with d.mu held.
func (d *Dispatcher) observeLocked() {
	// Standard rising-edge detector: a re-handshake within the same connection
	// first drives established back to false (fresh HelloApp clears confirmed),
	// so the subsequent completion is a new false→true edge and counts again.
	est := d.p.SessionEstablished()
	if est && !d.last.established && d.obs.OnSessionEstablished != nil {
		d.obs.OnSessionEstablished()
	}
	d.last.established = est

	if pf := d.p.PopFailures; pf > d.last.popFailures {
		for i := d.last.popFailures; i < pf; i++ {
			if d.obs.OnPopFailure != nil {
				d.obs.OnPopFailure()
			}
		}
		d.last.popFailures = pf
	}
}
