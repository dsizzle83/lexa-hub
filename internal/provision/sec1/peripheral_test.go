package sec1

import (
	"errors"
	"testing"

	"lexa-hub/internal/provision/frame"
)

const (
	testPop         = "8F3K2M9Q"
	testSerial      = "LX93-000042"
	testFingerprint = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

var testHandoff = HandoffInfo{
	Serial:    testSerial,
	IP:        "192.168.1.42",
	Port:      9100,
	APICertFP: testFingerprint,
	Token:     "secret-bearer-token",
}

var defaultScan = []WifiAp{
	{SSID: "HomeNet", RSSI: -48, Sec: "wpa2"},
	{SSID: "Neighbour", RSSI: -71, Sec: "wpa3"},
	{SSID: "CafeOpen", RSSI: -80, Sec: "open"},
}

// appDriver is the app (central) side of the session used to exercise a hub
// Peripheral end to end: it runs the real app-side sec1 crypto, frames writes,
// and reassembles/decrypts the hub's indications. It is the round-trippable
// harness a later unit (B2) would replace with a real BLE transport.
type appDriver struct {
	t     *testing.T
	p     *Peripheral
	att   int
	sess  *Session
	reasm map[string]*frame.Reassembler
}

type reply struct {
	UUID string
	Msg  Message
}

func newDriver(t *testing.T, p *Peripheral, att int) *appDriver {
	return &appDriver{t: t, p: p, att: att, reasm: map[string]*frame.Reassembler{}}
}

// feed frames msg for uuid (encrypting with the app session when encrypt) and
// delivers every chunk to the hub, returning the hub's raw outbound chunks.
func (d *appDriver) feed(uuid string, msg Message, encrypt bool) []Outbound {
	d.t.Helper()
	payload, err := msg.Encode()
	if err != nil {
		d.t.Fatalf("encode: %v", err)
	}
	if encrypt {
		payload, err = d.sess.Encrypt(payload)
		if err != nil {
			d.t.Fatalf("encrypt: %v", err)
		}
	}
	chunks, err := frame.Chunk(payload, d.att, encrypt)
	if err != nil {
		d.t.Fatalf("chunk: %v", err)
	}
	var outs []Outbound
	for _, c := range chunks {
		o, _ := d.p.HandleChunk(uuid, c)
		outs = append(outs, o...)
	}
	return outs
}

// decode reassembles and (where encrypted) app-decrypts the hub's outbound
// chunks into typed replies, in order. Fatals on any framing/decrypt/decode
// error — happy-path use.
func (d *appDriver) decode(outs []Outbound) []reply {
	d.t.Helper()
	var replies []reply
	for _, o := range outs {
		r := d.reasm[o.UUID]
		if r == nil {
			r = &frame.Reassembler{}
			d.reasm[o.UUID] = r
		}
		msgBytes, done, err := r.Add(o.Chunk)
		if err != nil {
			d.t.Fatalf("reassemble %s: %v", o.UUID, err)
		}
		if !done {
			continue
		}
		clear := msgBytes
		if o.Chunk[0]&frame.FlagENC != 0 {
			clear, err = d.sess.Decrypt(msgBytes)
			if err != nil {
				d.t.Fatalf("decrypt %s: %v", o.UUID, err)
			}
		}
		m, err := Decode(clear)
		if err != nil {
			d.t.Fatalf("decode %s: %v", o.UUID, err)
		}
		replies = append(replies, reply{UUID: o.UUID, Msg: m})
	}
	return replies
}

func (d *appDriver) send(uuid string, msg Message, encrypt bool) []reply {
	return d.decode(d.feed(uuid, msg, encrypt))
}

// hello sends HelloApp, receives HelloHub, derives K under pop, and returns the
// hub challenge. Starts a fresh app session (as the real client does per
// establishSession).
func (d *appDriver) hello(pop string) []byte {
	d.t.Helper()
	sess, err := Generate(RoleApp)
	if err != nil {
		d.t.Fatal(err)
	}
	d.sess = sess
	replies := d.send(UUIDSession, &HelloApp{Pub: d.sess.PublicKey()}, false)
	if len(replies) != 1 {
		d.t.Fatalf("hello: want 1 reply, got %d", len(replies))
	}
	hh, ok := replies[0].Msg.(*HelloHub)
	if !ok {
		d.t.Fatalf("hello: want HelloHub, got %T", replies[0].Msg)
	}
	if err := d.sess.DeriveKey(hh.Pub, pop); err != nil {
		d.t.Fatalf("derive: %v", err)
	}
	return hh.Challenge
}

// establish runs the full handshake under pop and requires an Ok.
func (d *appDriver) establish(pop string) {
	d.t.Helper()
	ch := d.hello(pop)
	replies := d.send(UUIDSession, &Confirm{Challenge: ch}, true)
	if len(replies) != 1 {
		d.t.Fatalf("confirm: want 1 reply, got %d", len(replies))
	}
	if _, ok := replies[0].Msg.(*Ok); !ok {
		d.t.Fatalf("confirm: want Ok, got %T", replies[0].Msg)
	}
	if !d.p.SessionEstablished() {
		d.t.Fatal("hub should report session established after Ok")
	}
}

func newRig(t *testing.T, att int, jb JoinBehavior) (*Peripheral, *appDriver) {
	p := NewPeripheral(PeripheralConfig{
		Pop:            testPop,
		Serial:         testSerial,
		ScanResults:    defaultScan,
		JoinBehavior:   jb,
		AttPayloadSize: att,
	})
	return p, newDriver(t, p, att)
}

func TestPeripheral_HappyPath(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff, JoiningEvents: 2})

	d.establish(testPop)
	if p.PopFailures != 0 {
		t.Fatalf("PopFailures=%d", p.PopFailures)
	}

	scan := d.send(UUIDWifi, &ScanRequest{}, true)
	if len(scan) != 1 {
		t.Fatalf("scan: want 1 reply, got %d", len(scan))
	}
	res, ok := scan[0].Msg.(*WifiScanResult)
	if !ok || len(res.APs) != 3 {
		t.Fatalf("scan result: %#v", scan[0].Msg)
	}
	if res.APs[0].SSID != "HomeNet" || res.APs[0].RSSI != -48 || res.APs[0].Sec != "wpa2" {
		t.Fatalf("first ap: %#v", res.APs[0])
	}
	if res.APs[2].Sec != "open" {
		t.Fatalf("last ap sec: %q", res.APs[2].Sec)
	}

	psk := "hunter22"
	updates := d.send(UUIDConfig, &Join{SSID: "HomeNet", PSK: &psk}, true)
	if len(updates) != 3 {
		t.Fatalf("join: want 3 updates, got %d", len(updates))
	}
	for i := 0; i < 2; i++ {
		sm, ok := updates[i].Msg.(*StateMessage)
		if !ok || sm.State != StateJoining {
			t.Fatalf("update %d: want joining, got %#v", i, updates[i].Msg)
		}
	}
	joined, ok := updates[2].Msg.(*StateMessage)
	if !ok || joined.State != StateJoined || joined.Handoff == nil {
		t.Fatalf("final update: %#v", updates[2].Msg)
	}
	h := joined.Handoff
	if h.Serial != testSerial || h.IP != "192.168.1.42" || h.Port != 9100 ||
		h.APICertFP != testFingerprint || h.Token != "secret-bearer-token" {
		t.Fatalf("handoff: %#v", h)
	}

	if len(p.JoinRequests) != 1 || p.JoinRequests[0].SSID != "HomeNet" ||
		p.JoinRequests[0].PSK == nil || *p.JoinRequests[0].PSK != "hunter22" {
		t.Fatalf("join requests: %#v", p.JoinRequests)
	}

	d.send(UUIDConfig, &Done{}, true)
	if !p.DoneReceived {
		t.Fatal("done not received")
	}
	if p.LastError != nil {
		t.Fatalf("unexpected LastError: %v", p.LastError)
	}
}

func TestPeripheral_TinyMTU(t *testing.T) {
	p, d := newRig(t, 23, JoinSucceeds{Handoff: testHandoff})
	d.establish(testPop)
	scan := d.send(UUIDWifi, &ScanRequest{}, true)
	res := scan[0].Msg.(*WifiScanResult)
	got := []string{res.APs[0].SSID, res.APs[1].SSID, res.APs[2].SSID}
	want := []string{"HomeNet", "Neighbour", "CafeOpen"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ap %d: %q != %q", i, got[i], want[i])
		}
	}
	pw := "pw"
	updates := d.send(UUIDConfig, &Join{SSID: "HomeNet", PSK: &pw}, true)
	if _, ok := updates[len(updates)-1].Msg.(*StateMessage); !ok {
		t.Fatalf("final update type %T", updates[len(updates)-1].Msg)
	}
	d.send(UUIDConfig, &Done{}, true)
	if !p.DoneReceived || p.LastError != nil {
		t.Fatalf("done=%v lastErr=%v", p.DoneReceived, p.LastError)
	}
}

func TestPeripheral_OpenNetworkOmitsPSK(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	d.establish(testPop)
	d.send(UUIDConfig, &Join{SSID: "CafeOpen"}, true)
	if len(p.JoinRequests) != 1 {
		t.Fatalf("want 1 join, got %d", len(p.JoinRequests))
	}
	if p.JoinRequests[0].PSK != nil {
		t.Fatalf("open join must omit psk, got %q", *p.JoinRequests[0].PSK)
	}
	// And the encoded wire form must not carry a psk key.
	wire, err := (&Join{SSID: "CafeOpen"}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"op":"join","ssid":"CafeOpen"}` {
		t.Fatalf("open join wire: %s", wire)
	}
}

func TestPeripheral_WrongPop(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	ch := d.hello(testPop) // learn a real challenge under the right key first
	_ = ch
	// Restart with a WRONG pop: the app derives a different K, so its confirm
	// cannot authenticate. Fresh hello re-derives on the hub side too.
	ch = d.hello("WRONG-POP")
	replies := d.send(UUIDSession, &Confirm{Challenge: ch}, true)
	if len(replies) != 1 {
		t.Fatalf("want 1 reply, got %d", len(replies))
	}
	e, ok := replies[0].Msg.(*Err)
	if !ok || e.Code != "pop_mismatch" {
		t.Fatalf("want err pop_mismatch, got %#v", replies[0].Msg)
	}
	if p.SessionEstablished() {
		t.Fatal("session must not be established on wrong pop")
	}
	if p.PopFailures != 1 {
		t.Fatalf("PopFailures=%d, want 1", p.PopFailures)
	}
}

func TestPeripheral_RetryAfterMismatch(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	ch := d.hello("WRONG-POP")
	replies := d.send(UUIDSession, &Confirm{Challenge: ch}, true)
	if _, ok := replies[0].Msg.(*Err); !ok {
		t.Fatalf("want Err, got %#v", replies[0].Msg)
	}
	// Retry with the right pop in the same "connection".
	d.establish(testPop)
	if !p.SessionEstablished() {
		t.Fatal("retry should establish")
	}
	if p.PopFailures != 1 {
		t.Fatalf("PopFailures=%d, want 1", p.PopFailures)
	}
}

// TestPeripheral_WrongChallengeConstantTime exercises the constant-time
// challenge compare path: a client that knows the PoP (so its confirm
// decrypts) but echoes the WRONG challenge value is rejected as pop_mismatch,
// distinct from the wrong-key path.
func TestPeripheral_WrongChallengeConstantTime(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	_ = d.hello(testPop) // correct key; ignore the real challenge
	bogus := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	replies := d.send(UUIDSession, &Confirm{Challenge: bogus}, true)
	e, ok := replies[0].Msg.(*Err)
	if !ok || e.Code != "pop_mismatch" {
		t.Fatalf("want err pop_mismatch, got %#v", replies[0].Msg)
	}
	if p.SessionEstablished() {
		t.Fatal("must not establish on wrong challenge")
	}
	if p.PopFailures != 1 {
		t.Fatalf("PopFailures=%d, want 1", p.PopFailures)
	}
}

func TestPeripheral_JoinFailuresTable(t *testing.T) {
	for _, reason := range AllWifiFailureReasons {
		reason := reason
		t.Run(reason.Wire(), func(t *testing.T) {
			p, d := newRig(t, 244, JoinFails{Reason: reason, JoiningEvents: 1})
			d.establish(testPop)
			updates := d.send(UUIDConfig, &Join{SSID: "HomeNet"}, true)
			if len(updates) != 2 {
				t.Fatalf("want 1 joining + 1 failed, got %d", len(updates))
			}
			failed, ok := updates[1].Msg.(*StateMessage)
			if !ok || failed.State != StateFailed {
				t.Fatalf("final: %#v", updates[1].Msg)
			}
			if failed.Reason == nil || *failed.Reason != reason {
				t.Fatalf("reason: got %v want %v", failed.Reason, reason)
			}
			_ = p
		})
	}
}

func TestPeripheral_RetryJoinAfterFailure(t *testing.T) {
	p, d := newRig(t, 244, JoinFails{Reason: ReasonAuthFailed})
	d.establish(testPop)
	first := d.send(UUIDConfig, &Join{SSID: "HomeNet"}, true)
	if sm := first[len(first)-1].Msg.(*StateMessage); sm.State != StateFailed {
		t.Fatalf("first join should fail, got %v", sm.State)
	}
	// Flip the scripted outcome and retry within the same session.
	p.JoinBehavior = JoinSucceeds{Handoff: testHandoff}
	second := d.send(UUIDConfig, &Join{SSID: "HomeNet"}, true)
	if sm := second[len(second)-1].Msg.(*StateMessage); sm.State != StateJoined {
		t.Fatalf("retry should join, got %v", sm.State)
	}
	if len(p.JoinRequests) != 2 {
		t.Fatalf("want 2 join requests, got %d", len(p.JoinRequests))
	}
}

// TestPeripheral_ReplayedWriteAborts ports "hub enforces its receive counters
// too": replaying a previously accepted encrypted write fails GCM auth under
// the advanced counter and permanently aborts the hub session (it then goes
// silent).
func TestPeripheral_ReplayedWriteAborts(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	d.establish(testPop)

	// Build and capture the framed `done` write.
	payload, err := (&Done{}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := d.sess.Encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := frame.Chunk(ct, d.att, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if _, err := p.HandleChunk(UUIDConfig, c); err != nil {
			t.Fatalf("first done: %v", err)
		}
	}
	if !p.DoneReceived {
		t.Fatal("done should be received once")
	}

	// Replay the identical chunks: the hub's receive counter has advanced.
	p.DoneReceived = false
	for _, c := range chunks {
		if _, err := p.HandleChunk(UUIDConfig, c); err != nil {
			t.Fatalf("replayed done returned error: %v", err)
		}
	}
	if !p.SessionAborted {
		t.Fatal("hub must abort on a replayed write")
	}
	if p.DoneReceived {
		t.Fatal("replayed done must not be re-processed")
	}
}

// TestPeripheral_DowngradeRejected: after ok, a plaintext frame on an
// encrypted characteristic is a protocol error — the hub goes silent and
// records the violation, without a counter abort. Downgrade attempts fail.
func TestPeripheral_DowngradeRejected(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	d.establish(testPop)

	out := d.feed(UUIDWifi, &ScanRequest{}, false) // plaintext where ciphertext is required
	if len(out) != 0 {
		t.Fatalf("downgrade must produce no response, got %d chunks", len(out))
	}
	if p.LastError == nil {
		t.Fatal("downgrade should record a protocol violation")
	}
	if p.SessionAborted {
		t.Fatal("downgrade is not a counter abort")
	}
	// The session is intact: a proper encrypted scan still works.
	p.LastError = nil
	scan := d.send(UUIDWifi, &ScanRequest{}, true)
	if _, ok := scan[0].Msg.(*WifiScanResult); !ok {
		t.Fatalf("post-downgrade encrypted scan failed: %#v", scan)
	}
}

func TestPeripheral_MessageBeforeSession(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff})
	sess, err := Generate(RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	d.sess = sess
	out := d.feed(UUIDConfig, &Done{}, false) // plaintext config, no handshake
	if len(out) != 0 {
		t.Fatalf("pre-session config must be ignored, got %d chunks", len(out))
	}
	if p.DoneReceived {
		t.Fatal("done before session must not be processed")
	}
	if p.LastError == nil {
		t.Fatal("pre-session message should record a violation")
	}
}

// TestPeripheral_AppReplayAborts mirrors the client-side "replayed status
// indication → SessionAborted": the app's own receive counter rejects a
// duplicated hub indication.
func TestPeripheral_AppReplayAborts(t *testing.T) {
	p, d := newRig(t, 244, JoinSucceeds{Handoff: testHandoff, JoiningEvents: 1})
	d.establish(testPop)

	// Drive one join and capture the hub's status chunks WITHOUT decoding them
	// through the app yet (so the app receive counter stays at the Ok value).
	outs := d.feed(UUIDConfig, &Join{SSID: "HomeNet"}, true)

	// Reassemble the first full status message from the hub's chunks.
	var r frame.Reassembler
	var firstStatus []byte
	for _, o := range outs {
		if o.UUID != UUIDStatus {
			continue
		}
		msg, done, err := r.Add(o.Chunk)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			firstStatus = msg
			break
		}
	}
	if firstStatus == nil {
		t.Fatal("expected a status indication")
	}

	// First decrypt succeeds; the identical replay aborts the app session.
	if _, err := d.sess.Decrypt(firstStatus); err != nil {
		t.Fatalf("first status decrypt: %v", err)
	}
	if _, err := d.sess.Decrypt(firstStatus); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("replayed status: want ErrSessionAborted, got %v", err)
	}
	_ = p
}
