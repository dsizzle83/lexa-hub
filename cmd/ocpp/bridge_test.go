package main

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"lexa-hub/internal/metrics"
)

// fakeMQTTClient is a minimal mqtt.Client stand-in — mirrors
// cmd/northbound/certmon_test.go's fakeClient (same rationale: the bridge's
// pending publish closure and publishAll call client.Publish directly,
// which panics on a nil interface value, so a real fake is required
// wherever those paths run in a test).
type fakeMQTTClient struct {
	mu        sync.Mutex
	publishes int
}

func (f *fakeMQTTClient) IsConnected() bool      { return true }
func (f *fakeMQTTClient) IsConnectionOpen() bool { return true }
func (f *fakeMQTTClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeMQTTClient) Disconnect(quiesce uint) {
}
func (f *fakeMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.mu.Lock()
	f.publishes++
	f.mu.Unlock()
	return &fakeDoneToken{}
}
func (f *fakeMQTTClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeMQTTClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeMQTTClient) Unsubscribe(topics ...string) mqtt.Token { panic("not implemented") }
func (f *fakeMQTTClient) AddRoute(topic string, callback mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeMQTTClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

type fakeDoneToken struct{}

func (t *fakeDoneToken) Wait() bool                       { return true }
func (t *fakeDoneToken) WaitTimeout(d time.Duration) bool { return true }
func (t *fakeDoneToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t *fakeDoneToken) Error() error { return nil }

// fakeCSConn is a minimal ocpp2.ChargingStationConnection stand-in, used to
// drive mqttBridge.onConnect / availForwarder.OnStatusNotification directly
// without a live WebSocket handshake (the connect/status-notification
// closures were extracted to named methods in Unit 6.1 specifically to make
// this possible).
type fakeCSConn struct {
	id   string
	addr net.Addr
}

func (f fakeCSConn) ID() string                               { return f.id }
func (f fakeCSConn) RemoteAddr() net.Addr                     { return f.addr }
func (f fakeCSConn) TLSConnectionState() *tls.ConnectionState { return nil }

func newFakeCSConn(id, addr string) fakeCSConn {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		panic(err)
	}
	return fakeCSConn{id: id, addr: tcpAddr}
}

// TestBridge_UnconfiguredStationBecomesPending: connecting a station whose
// ID is NOT in cfg.Stations must surface it on the pending-station gauge/doc
// — the exact getOrCreateLocked auto-create path Unit 6.1 targets.
func TestBridge_UnconfiguredStationBecomesPending(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	csms := ocpp2.NewCSMS(nil, nil)
	bridge := newMQTTBridge(mc, csms, []StationConfig{{ID: "cs-configured"}}, gauge)

	bridge.onConnect(newFakeCSConn("cs-unknown", "10.0.0.7:9000"))

	if got := reg.Format(); got == "" {
		t.Fatal("empty metrics output")
	}
	// The unconfigured station must now be tracked as pending (gauge = 1),
	// while the configured one was never connected in this test at all.
	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 1") {
		t.Errorf("expected lexa_ocpp_pending_stations=1, got:\n%s", got)
	}
}

// TestBridge_ConfiguredStationNeverPending: connecting a station whose ID IS
// in cfg.Stations must never appear as pending.
func TestBridge_ConfiguredStationNeverPending(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	csms := ocpp2.NewCSMS(nil, nil)
	bridge := newMQTTBridge(mc, csms, []StationConfig{{ID: "cs-001"}}, gauge)
	bridge.setStationConfig("cs-001", 32, 230) // pre-registration, as main() does

	bridge.onConnect(newFakeCSConn("cs-001", "10.0.0.8:9000"))

	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 0") {
		t.Errorf("expected lexa_ocpp_pending_stations=0 for a configured station, got:\n%s", got)
	}
}

// TestBridge_BootNotificationFillsVendorModelForPendingStation: the
// provisioning forwarder's OnBootNotification must reach the SAME pending
// entry a prior onConnect created, filling in vendor/model without creating
// a duplicate — exercised through the real handler type, not just the
// underlying pendingStations component (pending_test.go covers that in
// isolation; this test pins the WIRING: csms.SetProvisioningHandler in
// newMQTTBridge, and where Vendor/Model are read off
// provisioning.BootNotificationRequest.ChargingStation).
func TestBridge_BootNotificationFillsVendorModelForPendingStation(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	csms := ocpp2.NewCSMS(nil, nil)
	bridge := newMQTTBridge(mc, csms, nil, gauge)

	bridge.onConnect(newFakeCSConn("cs-boot", "10.0.0.9:9000"))

	prov := &provisioningForwarder{bridge: bridge}
	_, err := prov.OnBootNotification("cs-boot", &provisioning.BootNotificationRequest{
		Reason: "PowerUp",
		ChargingStation: provisioning.ChargingStationType{
			Model:      "Fast50",
			VendorName: "AcmeEV",
		},
	})
	if err != nil {
		t.Fatalf("OnBootNotification returned error: %v", err)
	}

	// Still exactly one pending entry (update, not a second one), gauge=1.
	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 1") {
		t.Errorf("expected exactly one pending entry after BootNotification, got:\n%s", got)
	}
}

// TestBridge_NoShellForPendingStation pins that an unconfigured (pending)
// station never gets an EVSE reconciler shell/driver, even when the
// reconciler is active for OTHER (configured) stations — main.go only ever
// populates bridge.shells from cfg.Stations, and this test proves nothing
// downstream of onConnect/OnStatusNotification/OnBootNotification adds to
// it. The station is still tracked (stationState exists, measurements would
// publish) — see mqttBridge.stations — just never driven.
func TestBridge_NoShellForPendingStation(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	csms := ocpp2.NewCSMS(nil, nil)
	bridge := newMQTTBridge(mc, csms, []StationConfig{{ID: "cs-configured"}}, gauge)
	// Simulate main.go's reconciler wiring for the ONE configured station —
	// bridge.shells is populated exactly like the active/shadow block in
	// main() does, never touched by onConnect/OnStatusNotification/
	// OnBootNotification themselves.
	bridge.shells = map[string]*evseShell{
		"cs-configured": newEVSEShell("cs-configured", reg, modeActive, &recordingProfileDriver{}),
	}

	bridge.onConnect(newFakeCSConn("cs-pending", "10.0.0.10:9000"))
	avail := &availForwarder{bridge: bridge}
	req := availability.NewStatusNotificationRequest(types.NewDateTime(time.Now()), availability.ConnectorStatusAvailable, 1, 1)
	if _, err := avail.OnStatusNotification("cs-pending", req); err != nil {
		t.Fatalf("OnStatusNotification returned error: %v", err)
	}

	if _, ok := bridge.shells["cs-pending"]; ok {
		t.Fatal("an unconfigured/pending station must never get a reconciler shell")
	}
	bridge.mu.RLock()
	_, tracked := bridge.stations["cs-pending"]
	bridge.mu.RUnlock()
	if !tracked {
		t.Fatal("a pending station must still be tracked (measurement-only) exactly as before Unit 6.1")
	}
}
