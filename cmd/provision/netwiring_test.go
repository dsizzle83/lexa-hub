package main

import (
	"context"
	"testing"
	"time"

	"lexa-hub/internal/provision/frame"
	"lexa-hub/internal/provision/gatt"
	"lexa-hub/internal/provision/netmgr"
	"lexa-hub/internal/provision/sec1"
)

const (
	testPop    = "8F3K2M9Q"
	testSerial = "LX93-000042"
	testAtt    = 244
)

func TestUpdateToState(t *testing.T) {
	// joining
	if m := updateToState(netmgr.JoinUpdate{State: sec1.StateJoining}, 9100); m.State != sec1.StateJoining || m.Handoff != nil || m.Reason != nil {
		t.Fatalf("joining → %+v", m)
	}
	// joined carries ip + port; serial is left for the peripheral to fill.
	m := updateToState(netmgr.JoinUpdate{State: sec1.StateJoined, IP: "192.168.1.42"}, 9100)
	if m.State != sec1.StateJoined || m.Handoff == nil {
		t.Fatalf("joined → %+v", m)
	}
	if m.Handoff.IP != "192.168.1.42" || m.Handoff.Port != 9100 || m.Handoff.Serial != "" {
		t.Fatalf("joined handoff = %+v (serial must be empty — B3 leaves cert_fp/token empty too)", m.Handoff)
	}
	if m.Handoff.APICertFP != "" || m.Handoff.Token != "" {
		t.Fatalf("B4 seam must be empty in B3: %+v", m.Handoff)
	}
	// failed carries the reason.
	f := updateToState(netmgr.JoinUpdate{State: sec1.StateFailed, Reason: sec1.ReasonAuthFailed}, 9100)
	if f.State != sec1.StateFailed || f.Reason == nil || *f.Reason != sec1.ReasonAuthFailed {
		t.Fatalf("failed → %+v", f)
	}
}

// appSide is a minimal GATT central driving a gatt.Dispatcher through OnWrite —
// the same "full flow without a real bus" harness B2 uses, reduced to what the
// live-join streaming test needs (handshake + one encrypted config write, then
// decrypt the ASYNC status stream collected off AsyncSend).
type appSide struct {
	t     *testing.T
	d     *gatt.Dispatcher
	sess  *sec1.Session
	reasm map[string]*frame.Reassembler
}

func newAppSide(t *testing.T, d *gatt.Dispatcher) *appSide {
	return &appSide{t: t, d: d, reasm: map[string]*frame.Reassembler{}}
}

// write frames msg (encrypting if asked) and feeds it through OnWrite, returning
// the SYNCHRONOUS framed responses decoded.
func (a *appSide) write(uuid string, msg sec1.Message, encrypt bool) []sec1.Message {
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
	return a.decode(outs)
}

func (a *appSide) decode(outs []sec1.Outbound) []sec1.Message {
	a.t.Helper()
	var msgs []sec1.Message
	for _, o := range outs {
		r := a.reasm[o.UUID]
		if r == nil {
			r = &frame.Reassembler{}
			a.reasm[o.UUID] = r
		}
		raw, done, err := r.Add(o.Chunk)
		if err != nil {
			a.t.Fatalf("reassemble: %v", err)
		}
		if !done {
			continue
		}
		clear := raw
		if o.Chunk[0]&frame.FlagENC != 0 {
			clear, err = a.sess.Decrypt(raw)
			if err != nil {
				a.t.Fatalf("decrypt: %v", err)
			}
		}
		m, err := sec1.Decode(clear)
		if err != nil {
			a.t.Fatalf("decode: %v", err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func (a *appSide) handshake(pop string) {
	a.t.Helper()
	sess, err := sec1.Generate(sec1.RoleApp)
	if err != nil {
		a.t.Fatal(err)
	}
	a.sess = sess
	rep := a.write(sec1.UUIDSession, &sec1.HelloApp{Pub: a.sess.PublicKey()}, false)
	hh, ok := rep[0].(*sec1.HelloHub)
	if !ok {
		a.t.Fatalf("want HelloHub, got %T", rep[0])
	}
	if err := a.sess.DeriveKey(hh.Pub, pop); err != nil {
		a.t.Fatal(err)
	}
	ok2 := a.write(sec1.UUIDSession, &sec1.Confirm{Challenge: hh.Challenge}, true)
	if _, isOk := ok2[0].(*sec1.Ok); !isOk {
		a.t.Fatalf("want Ok, got %T", ok2[0])
	}
}

// TestLiveJoinStreamsThroughDispatcher drives a JoinLive runner through the real
// gatt Dispatcher (like B2's tests) and proves its states are encrypted, framed,
// and streamed ASYNCHRONOUSLY on the status characteristic via AsyncSend — with
// the peripheral filling the handoff serial. Exercises the exact B3 wiring:
// sec1.JoinLive + updateToState + the AsyncSend transport.
func TestLiveJoinStreamsThroughDispatcher(t *testing.T) {
	outCh := make(chan sec1.Outbound, 16)

	runner := func(ctx context.Context, req sec1.Join, emit func(sec1.StateMessage)) {
		if req.SSID != "HomeNet" {
			t.Errorf("runner got ssid %q", req.SSID)
		}
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoining}, 9100))
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoining}, 9100))
		emit(updateToState(netmgr.JoinUpdate{State: sec1.StateJoined, IP: "192.168.1.42"}, 9100))
	}

	d := gatt.NewDispatcher(func() *sec1.Peripheral {
		return sec1.NewPeripheral(sec1.PeripheralConfig{
			Pop:            testPop,
			Serial:         testSerial,
			AttPayloadSize: testAtt,
			JoinBehavior:   sec1.JoinLive{Run: runner},
			AsyncSend:      func(o sec1.Outbound) { outCh <- o },
		})
	}, gatt.Observer{})

	a := newAppSide(t, d)
	a.handshake(testPop)

	// The config write is answered immediately (empty sync response); states
	// arrive asynchronously on outCh.
	psk := "hunter22"
	if sync := a.write(sec1.UUIDConfig, &sec1.Join{SSID: "HomeNet", PSK: &psk}, true); len(sync) != 0 {
		t.Fatalf("join write should return no synchronous status, got %d", len(sync))
	}

	var states []*sec1.StateMessage
	deadline := time.After(2 * time.Second)
	for {
		select {
		case o := <-outCh:
			for _, m := range a.decode([]sec1.Outbound{o}) {
				sm, ok := m.(*sec1.StateMessage)
				if !ok {
					t.Fatalf("async message not a StateMessage: %T", m)
				}
				if o.UUID != sec1.UUIDStatus {
					t.Fatalf("state on %s, want status", o.UUID)
				}
				states = append(states, sm)
			}
		case <-deadline:
			t.Fatalf("timed out; got %d states: %+v", len(states), states)
		}
		if len(states) > 0 && states[len(states)-1].State == sec1.StateJoined {
			break
		}
	}

	if len(states) != 3 {
		t.Fatalf("want 3 streamed states, got %d", len(states))
	}
	if states[0].State != sec1.StateJoining || states[1].State != sec1.StateJoining {
		t.Fatalf("first two states must be joining: %+v", states)
	}
	fin := states[2]
	if fin.State != sec1.StateJoined || fin.Handoff == nil {
		t.Fatalf("terminal = %+v", fin)
	}
	if fin.Handoff.IP != "192.168.1.42" || fin.Handoff.Port != 9100 {
		t.Fatalf("handoff ip/port = %+v", fin.Handoff)
	}
	// The peripheral fills the serial from its config (single source of truth).
	if fin.Handoff.Serial != testSerial {
		t.Fatalf("handoff serial = %q, want %q (peripheral-filled)", fin.Handoff.Serial, testSerial)
	}
	// B4 seam stays empty in B3.
	if fin.Handoff.APICertFP != "" || fin.Handoff.Token != "" {
		t.Fatalf("B4 seam must be empty: %+v", fin.Handoff)
	}
}
