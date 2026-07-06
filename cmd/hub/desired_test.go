package main

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
)

// fakePublishToken is an already-completed mqtt.Token, standing in for a
// broker that acked immediately.
type fakePublishToken struct{}

func (fakePublishToken) Wait() bool                     { return true }
func (fakePublishToken) WaitTimeout(time.Duration) bool { return true }
func (fakePublishToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (fakePublishToken) Error() error                   { return nil }

// fakeHubMQTTClient is a minimal mqtt.Client double: only Publish is
// exercised by desiredPublishingBatteryActuator/mqttutil.PublishJSONRetained.
// Every other method panics if called, so a test accidentally depending on
// unimplemented behavior fails loudly (mirrors internal/mqttutil's own
// fakeClient/fakeAckingClient pattern).
type fakeHubMQTTClient struct {
	publishes []fakeHubPublish
	failNext  bool
}

type fakeHubPublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

func (f *fakeHubMQTTClient) IsConnected() bool      { return true }
func (f *fakeHubMQTTClient) IsConnectionOpen() bool { return true }
func (f *fakeHubMQTTClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeHubMQTTClient) Disconnect(uint)        {}
func (f *fakeHubMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.publishes = append(f.publishes, fakeHubPublish{topic: topic, qos: qos, retained: retained, payload: b})
	if f.failNext {
		f.failNext = false
		return &failedToken{}
	}
	return fakePublishToken{}
}
func (f *fakeHubMQTTClient) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeHubMQTTClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeHubMQTTClient) Unsubscribe(...string) mqtt.Token { panic("not implemented") }
func (f *fakeHubMQTTClient) AddRoute(string, mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeHubMQTTClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

// failedToken stands in for a publish that never got a PUBACK / errored.
type failedToken struct{}

func (failedToken) Wait() bool                     { return true }
func (failedToken) WaitTimeout(time.Duration) bool { return true }
func (failedToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (failedToken) Error() error                   { return errors.New("no ack") }

func ptr[T any](v T) *T { return &v }

// TestDesiredPublishingBatteryActuator_ContentChangeDedupe verifies the
// retained doc is republished only when the standing intent's CONTENT (not
// Seq/IssuedAt, which change every call) differs from the last publish — an
// identical repeat tick must publish nothing (the retained doc is standing
// intent, not a tick stream; a per-tick publish would defeat the point of
// "retained").
func TestDesiredPublishingBatteryActuator_ContentChangeDedupe(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil)

	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("first command: got %d publishes, want 1", len(mc.publishes))
	}

	// Identical content, repeated (as a restore-rule tick would): no new publish.
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("repeat identical command: got %d publishes, want still 1 (no republish)", len(mc.publishes))
	}

	// Changed content: republishes.
	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -700, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("changed command: got %d publishes, want 2", len(mc.publishes))
	}
}

// TestDesiredPublishingBatteryActuator_LeaveUnchanged verifies BatteryCommand's
// "leave unchanged" convention (SetpointW==NaN, Connect==nil) carries the
// PREVIOUS standing value forward on the wire rather than going absent — a nil
// field on the wire means "no opinion" (AD-013), which is a different thing
// from "same opinion as last time".
func TestDesiredPublishingBatteryActuator_LeaveUnchanged(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil)

	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	// Connect-only command (SetpointW left at struct zero value 0, a real
	// idle command — Connect toggled to false): SetpointW is a concrete 0,
	// not NaN, but exercise the true "leave unchanged" path directly instead.
	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: math.NaN(), Connect: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publishes, want 2 (Connect-only change still republishes)", len(mc.publishes))
	}
	var doc bus.DesiredState
	if err := json.Unmarshal(mc.publishes[1].payload, &doc); err != nil {
		t.Fatalf("unmarshal published doc: %v", err)
	}
	if doc.SetpointW == nil || *doc.SetpointW != -500 {
		t.Fatalf("SetpointW = %v, want carried-forward -500 (NaN means leave-unchanged, not absent)", doc.SetpointW)
	}
	if doc.Connect == nil || *doc.Connect != false {
		t.Fatalf("Connect = %v, want false", doc.Connect)
	}
}

// TestDesiredPublishingBatteryActuator_SeqMonotonic verifies Seq increases by
// one per actual publish (not per call) and restarts at 0 for a fresh
// actuator instance — the AD-013-accepted "seq resets on publisher restart"
// case, disambiguated on the consumer side by a strictly-newer IssuedAt
// (reconcile's SeqReset path, covered in internal/reconcile's own tests).
func TestDesiredPublishingBatteryActuator_SeqMonotonic(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil)

	for i, w := range []float64{-100, -200, -300} {
		if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: w, Connect: ptr(true)}); err != nil {
			t.Fatal(err)
		}
		var doc bus.DesiredState
		if err := json.Unmarshal(mc.publishes[i].payload, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Seq != uint64(i) {
			t.Fatalf("publish %d: Seq = %d, want %d", i, doc.Seq, i)
		}
		if doc.V != bus.DesiredStateV {
			t.Fatalf("publish %d: V = %d, want %d", i, doc.V, bus.DesiredStateV)
		}
	}

	fresh := newDesiredPublishingBatteryActuator(mc, "battery-0", nil)
	if err := fresh.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -400, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	var doc bus.DesiredState
	if err := json.Unmarshal(mc.publishes[len(mc.publishes)-1].payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Seq != 0 {
		t.Fatalf("fresh actuator (simulating a hub restart): Seq = %d, want 0", doc.Seq)
	}
}

// TestDesiredPublishingBatteryActuator_Retained verifies the publish is
// retained and on the AD-013 topic lexa/desired/battery/{device}.
func TestDesiredPublishingBatteryActuator_Retained(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil)

	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}
	p := mc.publishes[0]
	if p.topic != bus.DesiredTopic(bus.DesiredClassBattery, "battery-0") {
		t.Fatalf("topic = %q, want %q", p.topic, bus.DesiredTopic(bus.DesiredClassBattery, "battery-0"))
	}
	if !p.retained {
		t.Fatal("desired doc must be published retained (AD-013)")
	}
	if p.qos != 1 {
		t.Fatalf("qos = %d, want 1 (control-plane QoS 1 per bus.PubQoS)", p.qos)
	}
}

// TestDesiredPublishingBatteryActuator_FailedPublishRetries verifies a failed
// publish surfaces as the actuator error (the desired-doc publish IS the command
// now, TASK-032) AND does not update the dedupe baseline, so the identical
// content is retried on the very next tick.
func TestDesiredPublishingBatteryActuator_FailedPublishRetries(t *testing.T) {
	mc := &fakeHubMQTTClient{failNext: true}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil)

	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}
	if err := a.ApplyBatteryCommand(cmd); err == nil {
		t.Fatal("a failed desired publish must surface as the actuator error")
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publish attempts, want 1", len(mc.publishes))
	}

	// Retry with identical content: must publish again (first one never took).
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publish attempts after a failed first publish, want 2 (retry)", len(mc.publishes))
	}
}

// TestDesiredPublishingSolarActuator_RestoreIsExplicitCeiling is the core
// TASK-029 mapping: restore (CurtailToW == NaN) must publish an EXPLICIT large
// CeilingW (bus.RestoreCeilingW), never an absent field — the whole Mode-A/B
// class exists because restore must be a positive opinion on the wire.
func TestDesiredPublishingSolarActuator_RestoreIsExplicitCeiling(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil)

	// A cap first, then restore.
	if err := a.ApplySolarCommand(orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplySolarCommand(orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: math.NaN()}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("cap then restore must be two publishes, got %d", len(mc.publishes))
	}
	var cap, restore bus.DesiredState
	if err := json.Unmarshal(mc.publishes[0].payload, &cap); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mc.publishes[1].payload, &restore); err != nil {
		t.Fatal(err)
	}
	if cap.CeilingW == nil || *cap.CeilingW != 3000 {
		t.Fatalf("cap CeilingW = %v, want 3000", cap.CeilingW)
	}
	if restore.CeilingW == nil || *restore.CeilingW != bus.RestoreCeilingW {
		t.Fatalf("restore CeilingW = %v, want explicit RestoreCeilingW (never absent)", restore.CeilingW)
	}
	if restore.DeviceClass != bus.DesiredClassSolar {
		t.Fatalf("class = %q, want solar", restore.DeviceClass)
	}
	if mc.publishes[1].topic != bus.DesiredTopic(bus.DesiredClassSolar, "inverter-0") || !mc.publishes[1].retained {
		t.Fatalf("solar doc must be retained on the solar topic")
	}
}

// TestDesiredPublishingSolarActuator_FailedPublishError: a failed publish
// surfaces as the actuator error (the desired-doc publish IS the command now).
func TestDesiredPublishingSolarActuator_FailedPublishError(t *testing.T) {
	mc := &fakeHubMQTTClient{failNext: true}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil)
	if err := a.ApplySolarCommand(orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: 1000}); err == nil {
		t.Fatal("a failed desired publish must surface as the actuator error")
	}
}

// TestDesiredPublishingSolarActuator_ContentDedupe: an unchanged ceiling
// publishes once (the retained doc is standing intent, not a tick stream).
func TestDesiredPublishingSolarActuator_ContentDedupe(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil)
	for i := 0; i < 3; i++ {
		if err := a.ApplySolarCommand(orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: 2000}); err != nil {
			t.Fatal(err)
		}
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("identical ceiling must publish once, got %d", len(mc.publishes))
	}
}

// TestDesiredPublishingEVSEActuator_Mapping: MaxCurrentA (incl. 0-suspend) and
// ConnectorID ride into the doc; the topic is the station's evse desired topic.
func TestDesiredPublishingEVSEActuator_Mapping(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingEVSEActuator(mc, "cs-001", nil)

	if err := a.ApplyEVSECommand(orchestrator.EVSECommand{StationID: "cs-001", ConnectorID: 1, MaxCurrentA: 16}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyEVSECommand(orchestrator.EVSECommand{StationID: "cs-001", ConnectorID: 1, MaxCurrentA: 0}); err != nil {
		t.Fatal(err) // 0 = explicit suspend, must republish
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("limit then suspend must be two publishes, got %d", len(mc.publishes))
	}
	var limit, suspend bus.DesiredState
	_ = json.Unmarshal(mc.publishes[0].payload, &limit)
	_ = json.Unmarshal(mc.publishes[1].payload, &suspend)
	if limit.MaxCurrentA == nil || *limit.MaxCurrentA != 16 || limit.ConnectorID != 1 {
		t.Fatalf("limit doc = %+v, want MaxCurrentA=16 connector=1", limit)
	}
	if suspend.MaxCurrentA == nil || *suspend.MaxCurrentA != 0 {
		t.Fatalf("suspend must publish MaxCurrentA=&0 (explicit), got %v", suspend.MaxCurrentA)
	}
	if suspend.DeviceClass != bus.DesiredClassEVSE {
		t.Fatalf("class = %q, want evse", suspend.DeviceClass)
	}
	if mc.publishes[0].topic != bus.DesiredTopic(bus.DesiredClassEVSE, "cs-001") || !mc.publishes[0].retained {
		t.Fatalf("evse doc must be retained on the evse topic")
	}
}

func TestDesiredContentEqual(t *testing.T) {
	base := bus.DesiredState{DeviceClass: "battery", DeviceID: "battery-0", Source: "economic", SetpointW: ptr(-500.0), Connect: ptr(true)}
	same := base
	same.Seq = 7
	same.IssuedAt = 123456
	if !desiredContentEqual(&base, same) {
		t.Fatal("differing only in Seq/IssuedAt must compare equal")
	}

	changed := base
	changed.SetpointW = ptr(-600.0)
	if desiredContentEqual(&base, changed) {
		t.Fatal("differing SetpointW must compare unequal")
	}

	if desiredContentEqual(nil, base) {
		t.Fatal("nil last must never compare equal")
	}
}
