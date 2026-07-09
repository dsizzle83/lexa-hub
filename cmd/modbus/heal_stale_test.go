package main

import (
	"encoding/json"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/reconcile"
)

// ---------------------------------------------------------------------
// WS-4.5: heal a retained NonConvergedBegin left over from a previous
// process instance — see healStaleRetainedReport's doc in reconcile_shell.go.
// ---------------------------------------------------------------------

// TestDecideStaleHeal is the pure decision core's table test: no broker, no
// fake mqtt.Client, just the possibly-nil retained report in and the
// possibly-nil corrective End out.
func TestDecideStaleHeal(t *testing.T) {
	now := time.Unix(1700000000, 0)

	cases := []struct {
		name     string
		retained *bus.ReconcileReport
		wantNil  bool
		wantMRID string
		wantEp   uint64
	}{
		{
			name:     "nil retained (nothing on the topic) needs no healing",
			retained: nil,
			wantNil:  true,
		},
		{
			name: "retained NonConvergedEnd (already ended cleanly) needs no healing",
			retained: &bus.ReconcileReport{
				Kind: reconcile.ReportNonConvergedEnd.String(), MRID: "m-end",
			},
			wantNil: true,
		},
		{
			name: "retained NonConvergedBegin heals",
			retained: &bus.ReconcileReport{
				Kind: reconcile.ReportNonConvergedBegin.String(), MRID: "m-begin", Episode: 7,
			},
			wantNil:  false,
			wantMRID: "m-begin",
			wantEp:   7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideStaleHeal(c.retained, "battery", "bat-0", now)
			if c.wantNil {
				if got != nil {
					t.Fatalf("decideStaleHeal() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("decideStaleHeal() = nil, want a corrective End report")
			}
			if got.Kind != reconcile.ReportNonConvergedEnd.String() {
				t.Errorf("Kind = %q, want NonConvergedEnd", got.Kind)
			}
			if got.DeviceClass != "battery" || got.DeviceID != "bat-0" {
				t.Errorf("DeviceClass/DeviceID = %q/%q, want battery/bat-0", got.DeviceClass, got.DeviceID)
			}
			if got.MRID != c.wantMRID {
				t.Errorf("MRID = %q, want %q (must carry the stale Begin's MRID forward)", got.MRID, c.wantMRID)
			}
			if got.Episode != c.wantEp {
				t.Errorf("Episode = %d, want %d (must carry the stale Begin's episode forward)", got.Episode, c.wantEp)
			}
			if got.IssuedAt != now.Unix() || got.Ts != now.Unix() {
				t.Errorf("IssuedAt/Ts = %d/%d, want both %d (this instance's own clock)", got.IssuedAt, got.Ts, now.Unix())
			}
		})
	}
}

// ── fake mqtt.Client for the Subscribe/Unsubscribe/Publish glue ───────────

// fakeHealMQTTClient is a minimal mqtt.Client double for
// healStaleRetainedReportT. Subscribe optionally delivers a canned message
// SYNCHRONOUSLY, standing in for a retained message arriving right after the
// broker acks the SUBSCRIBE — real paho delivers retained messages async,
// but delivering synchronously here lets a test observe the outcome without
// depending on goroutine scheduling. Leaving retainedPayload nil simulates
// "nothing was retained on this topic" (the handler is never invoked, so
// healStaleRetainedReportT falls through to its retainedWait timeout — kept
// short by the test's injected timeouts). Every other method panics if
// called, so a test accidentally depending on unimplemented behavior fails
// loudly (mirrors internal/mqttutil's and cmd/hub's own fake-client pattern).
type fakeHealMQTTClient struct {
	retainedPayload []byte

	subscribedTopic string
	unsubscribed    []string
	publishes       []fakeHealPublish
}

type fakeHealPublish struct {
	topic    string
	retained bool
	payload  []byte
}

func (f *fakeHealMQTTClient) IsConnected() bool      { return true }
func (f *fakeHealMQTTClient) IsConnectionOpen() bool { return true }
func (f *fakeHealMQTTClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeHealMQTTClient) Disconnect(uint)        {}

func (f *fakeHealMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.publishes = append(f.publishes, fakeHealPublish{topic: topic, retained: retained, payload: b})
	return fakeHealDoneToken{}
}

func (f *fakeHealMQTTClient) Subscribe(topic string, qos byte, handler mqtt.MessageHandler) mqtt.Token {
	f.subscribedTopic = topic
	if f.retainedPayload != nil {
		handler(f, &fakeHealMessage{topic: topic, payload: f.retainedPayload, retained: true})
	}
	return fakeHealDoneToken{}
}

func (f *fakeHealMQTTClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}

func (f *fakeHealMQTTClient) Unsubscribe(topics ...string) mqtt.Token {
	f.unsubscribed = append(f.unsubscribed, topics...)
	return fakeHealDoneToken{}
}

func (f *fakeHealMQTTClient) AddRoute(string, mqtt.MessageHandler) {
	panic("not implemented")
}

func (f *fakeHealMQTTClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

// fakeHealDoneToken is an already-completed mqtt.Token, standing in for a
// broker that acked immediately.
type fakeHealDoneToken struct{}

func (fakeHealDoneToken) Wait() bool                     { return true }
func (fakeHealDoneToken) WaitTimeout(time.Duration) bool { return true }
func (fakeHealDoneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (fakeHealDoneToken) Error() error                   { return nil }

// fakeHealMessage is a minimal mqtt.Message double carrying just what
// healStaleRetainedReportT's handler needs: topic, payload, and Retained().
type fakeHealMessage struct {
	topic    string
	payload  []byte
	retained bool
}

func (m *fakeHealMessage) Duplicate() bool   { return false }
func (m *fakeHealMessage) Qos() byte         { return 1 }
func (m *fakeHealMessage) Retained() bool    { return m.retained }
func (m *fakeHealMessage) Topic() string     { return m.topic }
func (m *fakeHealMessage) MessageID() uint16 { return 0 }
func (m *fakeHealMessage) Payload() []byte   { return m.payload }
func (m *fakeHealMessage) Ack()              {}

// TestHealStaleRetainedReportT_HealsStaleBegin drives the full glue: a
// retained NonConvergedBegin comes back on Subscribe, so
// healStaleRetainedReportT must publish a retained corrective End (carrying
// the stale report's MRID/Episode) and unsubscribe afterward.
func TestHealStaleRetainedReportT_HealsStaleBegin(t *testing.T) {
	stale := bus.ReconcileReport{
		Envelope:    bus.Envelope{V: bus.ReconcileReportV},
		Kind:        reconcile.ReportNonConvergedBegin.String(),
		DeviceClass: "battery",
		DeviceID:    "bat-0",
		MRID:        "mrid-1",
		Episode:     3,
	}
	payload, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeHealMQTTClient{retainedPayload: payload}

	now := time.Unix(1700000100, 0)
	wantTopic := bus.ReconcileReportTopic("battery", "bat-0")
	healStaleRetainedReportT(fc, "battery", "bat-0", now, time.Second, time.Second)

	if fc.subscribedTopic != wantTopic {
		t.Fatalf("subscribed topic = %q, want %q", fc.subscribedTopic, wantTopic)
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1 (the corrective End)", len(fc.publishes))
	}
	p := fc.publishes[0]
	if p.topic != wantTopic {
		t.Errorf("published topic = %q, want %q", p.topic, wantTopic)
	}
	if !p.retained {
		t.Error("corrective End must be published retained (AD-013/WS-4.5)")
	}
	var end bus.ReconcileReport
	if err := json.Unmarshal(p.payload, &end); err != nil {
		t.Fatalf("unmarshal published End: %v", err)
	}
	if end.Kind != reconcile.ReportNonConvergedEnd.String() {
		t.Errorf("Kind = %q, want NonConvergedEnd", end.Kind)
	}
	if end.MRID != "mrid-1" {
		t.Errorf("MRID = %q, want mrid-1", end.MRID)
	}
	if end.Episode != 3 {
		t.Errorf("Episode = %d, want 3 (carried from the stale Begin)", end.Episode)
	}
	if len(fc.unsubscribed) != 1 || fc.unsubscribed[0] != wantTopic {
		t.Errorf("unsubscribed = %v, want exactly [%s]", fc.unsubscribed, wantTopic)
	}
}

// TestHealStaleRetainedReportT_NothingRetainedIsANoop covers the common
// case: no retained message on the topic at all (a healthy device with no
// history of a crash mid-episode). No publish, but still an unsubscribe (the
// one-shot probe always cleans up after itself).
func TestHealStaleRetainedReportT_NothingRetainedIsANoop(t *testing.T) {
	fc := &fakeHealMQTTClient{} // retainedPayload left nil: handler never fires

	healStaleRetainedReportT(fc, "battery", "bat-1", time.Now(), 20*time.Millisecond, 20*time.Millisecond)

	if len(fc.publishes) != 0 {
		t.Fatalf("got %d publishes, want 0 (nothing retained, nothing to heal)", len(fc.publishes))
	}
	if len(fc.unsubscribed) != 1 {
		t.Fatalf("unsubscribed = %v, want exactly one call even on the no-op path", fc.unsubscribed)
	}
}

// TestHealStaleRetainedReportT_AlreadyEndedIsANoop covers the retained
// report existing but already being a NonConvergedEnd (a clean prior
// shutdown, or a Begin that already converged before the crash) — must not
// publish a redundant/incorrect End.
func TestHealStaleRetainedReportT_AlreadyEndedIsANoop(t *testing.T) {
	already := bus.ReconcileReport{
		Kind: reconcile.ReportNonConvergedEnd.String(), DeviceClass: "solar", DeviceID: "pv-0", MRID: "mrid-2",
	}
	payload, err := json.Marshal(already)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeHealMQTTClient{retainedPayload: payload}

	healStaleRetainedReportT(fc, "solar", "pv-0", time.Now(), time.Second, time.Second)

	if len(fc.publishes) != 0 {
		t.Fatalf("got %d publishes, want 0 (already-ended retained report needs no healing)", len(fc.publishes))
	}
}
