package gatt

import (
	"encoding/json"
	"testing"

	"lexa-hub/internal/provision/frame"
	"lexa-hub/internal/provision/sec1"
)

const (
	testPop    = "8F3K2M9Q"
	testSerial = "LX93-000042"
	testAtt    = 244
)

var testHandoff = sec1.HandoffInfo{
	Serial:    testSerial,
	IP:        "192.168.1.42",
	Port:      9100,
	APICertFP: "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12",
	Token:     "secret-bearer-token",
}

// appDriver is the central (app) side of the session, driving a gatt.Dispatcher
// through its OnWrite seam exactly as a real BLE central would: it runs the
// public sec1 app-side crypto, frames writes, and reassembles/decrypts the
// framed responses the Dispatcher returns. This is the "full handshake without a
// real bus" harness the B2 brief calls for — the same shape as B1's
// peripheral_test appDriver, but against OnWrite instead of HandleChunk.
type appDriver struct {
	t     *testing.T
	d     *Dispatcher
	sess  *sec1.Session
	reasm map[string]*frame.Reassembler
}

type reply struct {
	uuid string
	msg  sec1.Message
}

func newDriver(t *testing.T, d *Dispatcher) *appDriver {
	return &appDriver{t: t, d: d, reasm: map[string]*frame.Reassembler{}}
}

func (a *appDriver) feed(uuid string, msg sec1.Message, encrypt bool) []sec1.Outbound {
	a.t.Helper()
	payload, err := msg.Encode()
	if err != nil {
		a.t.Fatalf("encode: %v", err)
	}
	if encrypt {
		payload, err = a.sess.Encrypt(payload)
		if err != nil {
			a.t.Fatalf("encrypt: %v", err)
		}
	}
	chunks, err := frame.Chunk(payload, testAtt, encrypt)
	if err != nil {
		a.t.Fatalf("chunk: %v", err)
	}
	var outs []sec1.Outbound
	for _, c := range chunks {
		outs = append(outs, a.d.OnWrite(uuid, c)...)
	}
	return outs
}

func (a *appDriver) decode(outs []sec1.Outbound) []reply {
	a.t.Helper()
	var replies []reply
	for _, o := range outs {
		r := a.reasm[o.UUID]
		if r == nil {
			r = &frame.Reassembler{}
			a.reasm[o.UUID] = r
		}
		msgBytes, done, err := r.Add(o.Chunk)
		if err != nil {
			a.t.Fatalf("reassemble %s: %v", o.UUID, err)
		}
		if !done {
			continue
		}
		clear := msgBytes
		if o.Chunk[0]&frame.FlagENC != 0 {
			clear, err = a.sess.Decrypt(msgBytes)
			if err != nil {
				a.t.Fatalf("decrypt %s: %v", o.UUID, err)
			}
		}
		m, err := sec1.Decode(clear)
		if err != nil {
			a.t.Fatalf("decode %s: %v", o.UUID, err)
		}
		replies = append(replies, reply{uuid: o.UUID, msg: m})
	}
	return replies
}

func (a *appDriver) send(uuid string, msg sec1.Message, encrypt bool) []reply {
	return a.decode(a.feed(uuid, msg, encrypt))
}

func (a *appDriver) establish(pop string) {
	a.t.Helper()
	sess, err := sec1.Generate(sec1.RoleApp)
	if err != nil {
		a.t.Fatal(err)
	}
	a.sess = sess
	replies := a.send(sec1.UUIDSession, &sec1.HelloApp{Pub: a.sess.PublicKey()}, false)
	if len(replies) != 1 {
		a.t.Fatalf("hello: want 1 reply, got %d", len(replies))
	}
	hh, ok := replies[0].msg.(*sec1.HelloHub)
	if !ok {
		a.t.Fatalf("hello: want HelloHub, got %T", replies[0].msg)
	}
	if err := a.sess.DeriveKey(hh.Pub, pop); err != nil {
		a.t.Fatalf("derive: %v", err)
	}
	confirm := a.send(sec1.UUIDSession, &sec1.Confirm{Challenge: hh.Challenge}, true)
	if len(confirm) != 1 {
		a.t.Fatalf("confirm: want 1 reply, got %d", len(confirm))
	}
	if _, ok := confirm[0].msg.(*sec1.Ok); !ok {
		a.t.Fatalf("confirm: want Ok, got %T", confirm[0].msg)
	}
}

func newDispatcher(t *testing.T, cfg sec1.PeripheralConfig, obs Observer) *Dispatcher {
	if cfg.AttPayloadSize == 0 {
		cfg.AttPayloadSize = testAtt
	}
	return NewDispatcher(func() *sec1.Peripheral { return sec1.NewPeripheral(cfg) }, obs)
}

// TestDispatcher_FullFlowThroughSeam drives handshake → scan → join(success) →
// done entirely through OnWrite, proving the dispatch seam faithfully passes
// every framed chunk and that responses come back on the right characteristics
// (session/wifi/status) — i.e. the notify path the Server pushes.
func TestDispatcher_FullFlowThroughSeam(t *testing.T) {
	d := newDispatcher(t, sec1.PeripheralConfig{
		Pop:          testPop,
		Serial:       testSerial,
		ScanResults:  []sec1.WifiAp{{SSID: "HomeNet", RSSI: -48, Sec: "wpa2"}},
		JoinBehavior: sec1.JoinSucceeds{Handoff: testHandoff, JoiningEvents: 2},
	}, Observer{})
	a := newDriver(t, d)

	a.establish(testPop)

	scan := a.send(sec1.UUIDWifi, &sec1.ScanRequest{}, true)
	if len(scan) != 1 || scan[0].uuid != sec1.UUIDWifi {
		t.Fatalf("scan replies: %#v", scan)
	}
	res, ok := scan[0].msg.(*sec1.WifiScanResult)
	if !ok || len(res.APs) != 1 || res.APs[0].SSID != "HomeNet" {
		t.Fatalf("scan result: %#v", scan[0].msg)
	}

	psk := "hunter22"
	updates := a.send(sec1.UUIDConfig, &sec1.Join{SSID: "HomeNet", PSK: &psk}, true)
	if len(updates) != 3 {
		t.Fatalf("join: want 3 status updates, got %d", len(updates))
	}
	for _, u := range updates {
		if u.uuid != sec1.UUIDStatus {
			t.Fatalf("join update on %s, want status", u.uuid)
		}
	}
	final, ok := updates[2].msg.(*sec1.StateMessage)
	if !ok || final.State != sec1.StateJoined || final.Handoff == nil {
		t.Fatalf("final update: %#v", updates[2].msg)
	}
	if final.Handoff.IP != "192.168.1.42" || final.Handoff.Serial != testSerial {
		t.Fatalf("handoff: %#v", final.Handoff)
	}

	a.send(sec1.UUIDConfig, &sec1.Done{}, true)
	if !d.DoneReceived() {
		t.Fatal("done not observed through seam")
	}
}

// TestDispatcher_InfoValue proves the info read reports build TRUTH — the fw,
// serial, and commissioned flag the peripheral was configured with, not the B1
// placeholder.
func TestDispatcher_InfoValue(t *testing.T) {
	d := newDispatcher(t, sec1.PeripheralConfig{
		Pop:          testPop,
		Serial:       "LX93-ABCDEF",
		Fw:           "1.4.2-rc1",
		Commissioned: true,
	}, Observer{})

	raw, err := d.InfoValue()
	if err != nil {
		t.Fatalf("InfoValue: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("info not JSON: %v", err)
	}
	if got["fw"] != "1.4.2-rc1" {
		t.Errorf("fw = %v, want 1.4.2-rc1", got["fw"])
	}
	if got["serial"] != "LX93-ABCDEF" {
		t.Errorf("serial = %v, want LX93-ABCDEF", got["serial"])
	}
	if got["commissioned"] != true {
		t.Errorf("commissioned = %v, want true", got["commissioned"])
	}
	if v, _ := got["v"].(float64); v != 1 {
		t.Errorf("v = %v, want 1", got["v"])
	}
}

// TestDispatcher_SessionMetricEdge: OnSessionEstablished fires exactly once per
// completed handshake (rising edge), and again after a fresh handshake.
func TestDispatcher_SessionMetricEdge(t *testing.T) {
	var sessions int
	d := newDispatcher(t, sec1.PeripheralConfig{Pop: testPop, Serial: testSerial},
		Observer{OnSessionEstablished: func() { sessions++ }})
	a := newDriver(t, d)

	a.establish(testPop)
	if sessions != 1 {
		t.Fatalf("sessions after first handshake = %d, want 1", sessions)
	}
	// A second handshake within the same connection is a new rising edge.
	a.establish(testPop)
	if sessions != 2 {
		t.Fatalf("sessions after second handshake = %d, want 2", sessions)
	}
}

// TestDispatcher_PopFailureMetric: OnPopFailure fires once per wrong-PoP confirm.
func TestDispatcher_PopFailureMetric(t *testing.T) {
	var popFails int
	d := newDispatcher(t, sec1.PeripheralConfig{Pop: testPop, Serial: testSerial},
		Observer{OnPopFailure: func() { popFails++ }})
	a := newDriver(t, d)

	// hello under the right key, then confirm under the WRONG pop.
	sess, err := sec1.Generate(sec1.RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	a.sess = sess
	replies := a.send(sec1.UUIDSession, &sec1.HelloApp{Pub: a.sess.PublicKey()}, false)
	hh := replies[0].msg.(*sec1.HelloHub)
	if err := a.sess.DeriveKey(hh.Pub, "WRONG-POP"); err != nil {
		t.Fatal(err)
	}
	out := a.send(sec1.UUIDSession, &sec1.Confirm{Challenge: hh.Challenge}, true)
	if e, ok := out[0].msg.(*sec1.Err); !ok || e.Code != "pop_mismatch" {
		t.Fatalf("want err pop_mismatch, got %#v", out[0].msg)
	}
	if popFails != 1 {
		t.Fatalf("popFails = %d, want 1", popFails)
	}
}

// TestDispatcher_Reset recycles the peripheral: a completed session's DoneReceived
// clears and a fresh handshake counts as a new session.
func TestDispatcher_Reset(t *testing.T) {
	var sessions int
	d := newDispatcher(t, sec1.PeripheralConfig{
		Pop: testPop, Serial: testSerial,
		JoinBehavior: sec1.JoinSucceeds{Handoff: testHandoff},
	}, Observer{OnSessionEstablished: func() { sessions++ }})
	a := newDriver(t, d)

	a.establish(testPop)
	a.send(sec1.UUIDConfig, &sec1.Done{}, true)
	if !d.DoneReceived() {
		t.Fatal("done not observed")
	}

	d.Reset()
	if d.DoneReceived() {
		t.Fatal("DoneReceived should be false after Reset")
	}
	// A fresh central drives a new handshake on the recycled peripheral.
	a2 := newDriver(t, d)
	a2.establish(testPop)
	if sessions != 2 {
		t.Fatalf("sessions after reset+rehandshake = %d, want 2", sessions)
	}
}
