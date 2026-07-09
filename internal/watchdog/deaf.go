package watchdog

import (
	"sync"
	"time"
)

// DeafTracker tracks how long an MQTT client has been continuously
// disconnected, to gate a systemd watchdog kick on more than
// mqtt.Client.IsConnected() — which stays true for the client's entire
// AutoReconnect retry loop (connected, connecting-with-ConnectRetry, and
// reconnecting), not just while actually connected (see
// vendor/github.com/eclipse/paho.mqtt.golang/client.go's IsConnected). A
// sustained broker outage therefore never trips IsConnected() to false, so a
// kick gated on it alone keeps the watchdog happy for the entire outage —
// exactly the "alive but deaf" failure mode this type exists to detect.
//
// Wire OnConnectionLost/OnReconnect into mqttutil.Instrumentation (WS-9.1).
//
// DeafTracker is pure and dependency-free: no I/O, no reference to
// internal/mqttutil, and no time.Now() buried inside DeafFor — the caller
// supplies "now", matching this codebase's injected-wall-clock discipline
// (see internal/reconcile's package doc) so the type is fake-clock-testable.
type DeafTracker struct {
	mu             sync.Mutex
	disconnectedAt time.Time // zero = currently connected (or never lost)
}

// NewDeafTracker returns a DeafTracker in the "currently connected" state.
func NewDeafTracker() *DeafTracker {
	return &DeafTracker{}
}

// OnConnectionLost records the start of a disconnection. Call from
// mqttutil.Instrumentation.OnConnectionLost. Idempotent: a second
// OnConnectionLost call without an intervening OnReconnect does NOT push
// disconnectedAt forward — paho's SetConnectionLostHandler fires once per
// drop, but a caller wiring this defensively (or a future paho behavior
// change) must not be able to reset the outage clock mid-outage.
func (t *DeafTracker) OnConnectionLost() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disconnectedAt.IsZero() {
		t.disconnectedAt = time.Now()
	}
}

// OnReconnect clears the tracked disconnection. Call from
// mqttutil.Instrumentation.OnReconnect — fired on a genuine reconnect, not
// the initial connect (see mqttutil's noteConnected).
func (t *DeafTracker) OnReconnect() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.disconnectedAt = time.Time{}
}

// DeafFor reports how long the tracker has been continuously disconnected as
// of now (0 if currently connected). now is caller-supplied rather than
// read internally, so this is testable with a fake clock without any
// wall-clock sleeps.
func (t *DeafTracker) DeafFor(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disconnectedAt.IsZero() {
		return 0
	}
	d := now.Sub(t.disconnectedAt)
	if d < 0 {
		return 0
	}
	return d
}
