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

// fakeBatteryActuator is a legacy-actuator double recording every call, so
// tests can assert the wrapper delegates FIRST and unconditionally.
type fakeBatteryActuator struct {
	calls []orchestrator.BatteryCommand
	err   error
}

func (f *fakeBatteryActuator) ApplyBatteryCommand(cmd orchestrator.BatteryCommand) error {
	f.calls = append(f.calls, cmd)
	return f.err
}

func ptr[T any](v T) *T { return &v }

// TestDesiredPublishingBatteryActuator_DelegatesFirst verifies the legacy
// actuator is invoked (and its error/return value is what the caller sees)
// regardless of the desired-doc publish outcome — the whole point of
// "additive, zero-risk" shadow mode (ledger L1–L4 stay byte-for-byte legacy).
func TestDesiredPublishingBatteryActuator_DelegatesFirst(t *testing.T) {
	inner := &fakeBatteryActuator{}
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)

	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inner.calls) != 1 || inner.calls[0] != cmd {
		t.Fatalf("legacy actuator not invoked with the exact command: %+v", inner.calls)
	}

	inner.err = errors.New("legacy publish failed")
	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -600, Connect: ptr(true)}); err == nil {
		t.Fatal("expected the legacy actuator's error to propagate")
	}
}

// TestDesiredPublishingBatteryActuator_ContentChangeDedupe verifies the
// retained doc is republished only when the standing intent's CONTENT (not
// Seq/IssuedAt, which change every call) differs from the last publish — an
// identical repeat tick must publish nothing (the retained doc is standing
// intent, not a tick stream; a per-tick publish would defeat the point of
// "retained").
func TestDesiredPublishingBatteryActuator_ContentChangeDedupe(t *testing.T) {
	inner := &fakeBatteryActuator{}
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)

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
	inner := &fakeBatteryActuator{}
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)

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
	inner := &fakeBatteryActuator{}
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)

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

	fresh := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)
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
	inner := &fakeBatteryActuator{}
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)

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

// TestDesiredPublishingBatteryActuator_FailedPublishRetries verifies a
// failed publish does not update the dedupe baseline, so the identical
// content is retried on the very next tick (mirrors cmdDeduper's own
// "not delivered; retry next tick" convention).
func TestDesiredPublishingBatteryActuator_FailedPublishRetries(t *testing.T) {
	inner := &fakeBatteryActuator{}
	mc := &fakeHubMQTTClient{failNext: true}
	a := newDesiredPublishingBatteryActuator(inner, mc, "battery-0", nil)

	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err) // legacy actuator itself did not fail
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
