package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
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

	// nextToken, when set, is returned by the NEXT Publish call instead of
	// the failNext/fakePublishToken default, then cleared — lets a test hand
	// back a controllable token (e.g. latentToken) for exactly one publish,
	// to exercise the genuine "still in flight, resolved only by a LATER
	// call's harvest" path (TASK-046), distinct from failNext's
	// already-resolved failedToken.
	nextToken mqtt.Token
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
	if f.nextToken != nil {
		tok := f.nextToken
		f.nextToken = nil
		return tok
	}
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

// failedToken stands in for a publish that already errored by the time the
// caller checks (Done() closed immediately, Error() non-nil) — the
// synchronous-failure fake the ORIGINAL (pre-TASK-046) tests used, still
// useful for exercising the actuators' OPPORTUNISTIC immediate post-fire
// check (see desired.go's ApplyBatteryCommand: firing calls harvestPending
// once right away, cheaply, in case the ack — or, as here, the error — is
// already sitting there; a genuinely in-flight publish just leaves it
// pending for the next call, which is what latentToken below exercises).
type failedToken struct{}

func (failedToken) Wait() bool                     { return true }
func (failedToken) WaitTimeout(time.Duration) bool { return true }
func (failedToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (failedToken) Error() error                   { return errors.New("no ack") }

// latentToken is a mqtt.Token that stays incomplete until resolve is called
// — standing in for a real broker round trip that hasn't come back yet, so a
// test can drive the genuine "still pending now, harvested on some LATER
// call" path (TASK-046) instead of failedToken's instantly-resolved one.
// Not safe for concurrent use (matches every actuator's single-goroutine
// assumption — see desiredPublishingBatteryActuator.pending's doc).
type latentToken struct {
	done chan struct{}
	err  error
}

func newLatentToken() *latentToken { return &latentToken{done: make(chan struct{})} }

func (t *latentToken) Wait() bool { <-t.done; return true }
func (t *latentToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}
func (t *latentToken) Done() <-chan struct{} { return t.done }
func (t *latentToken) Error() error          { return t.err }

// resolve completes t as if the broker had just responded (err == nil for a
// PUBACK, non-nil for a broker-reported failure).
func (t *latentToken) resolve(err error) {
	t.err = err
	close(t.done)
}

func ptr[T any](v T) *T { return &v }

// TestDesiredPublishingBatteryActuator_ContentChangeDedupe verifies the
// retained doc is republished only when the standing intent's CONTENT (not
// Seq/IssuedAt, which change every call) differs from the last publish — an
// identical repeat tick must publish nothing (the retained doc is standing
// intent, not a tick stream; a per-tick publish would defeat the point of
// "retained").
func TestDesiredPublishingBatteryActuator_ContentChangeDedupe(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

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
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

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
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

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

	fresh := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)
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
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

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
// publish does NOT update the dedupe baseline (TASK-046: the async harvest's
// rollback reproduces the old synchronous contract), so the identical
// content is retried on the very next call. It uses failedToken, which
// resolves to an error IMMEDIATELY — this actuator's opportunistic
// post-fire check (see harvestPending's doc) therefore catches the failure
// within the SAME ApplyBatteryCommand call that fired it, which is why this
// call returns nil rather than an error: with an async publish there is no
// synchronous broker round trip left to surface as a return value at all
// (the return is reserved for a marshal failure, see the doc on
// ApplyBatteryCommand/harvestPending) — only what got harvested, when.
func TestDesiredPublishingBatteryActuator_FailedPublishRetries(t *testing.T) {
	mc := &fakeHubMQTTClient{failNext: true}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatalf("an async publish failure must never surface as a return error: %v", err)
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

// TestDesiredPublishingBatteryActuator_PendingHarvestedNextCall_FailureRetries
// is FailedPublishRetries's twin using latentToken instead of failedToken: the
// publish stays genuinely IN FLIGHT (not yet resolved) across one whole extra
// call — the real motivating scenario (a slow-but-alive broker) — before
// resolving to a failure, exercising the "harvested at the START of the NEXT
// call" path the opportunistic immediate check above never gets to touch.
// This is also the mutation-test case from the task: comment out
// harvestPending's rollback (`a.lastPublished = a.pendingPrev`) and this test
// must fail, because the second identical-content call would then wrongly
// dedupe-suppress instead of retrying.
func TestDesiredPublishingBatteryActuator_PendingHarvestedNextCall_FailureRetries(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	lt := newLatentToken()
	mc.nextToken = lt
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}

	// Still pending: a repeat identical command must not publish again — the
	// one-slot-pending rule suppresses a duplicate in-flight send for
	// unchanged content (TASK-046).
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes while the first is still pending with identical content, want still 1 (no duplicate in-flight)", len(mc.publishes))
	}

	// The broker now reports failure. The NEXT call harvests it (this is the
	// genuine one-tick-later path) and, since the intent is unchanged, retries.
	lt.resolve(errors.New("no ack"))
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publishes after the pending publish resolved to a failure, want 2 (retry)", len(mc.publishes))
	}
}

// TestDesiredPublishingBatteryActuator_DifferingCommandsPublishInOrderWhilePending
// verifies TASK-046's ordering argument directly: a genuinely different
// command must still publish even while an earlier one for the same device
// is still pending (paho preserves per-client publish order regardless of
// ack timing), and the two publishes must appear on the wire in call order.
func TestDesiredPublishingBatteryActuator_DifferingCommandsPublishInOrderWhilePending(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	mc.nextToken = newLatentToken() // first publish never resolves during this test
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -100, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}

	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -900, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publishes for a content-different command while the first is still pending, want 2", len(mc.publishes))
	}

	var first, second bus.DesiredState
	if err := json.Unmarshal(mc.publishes[0].payload, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mc.publishes[1].payload, &second); err != nil {
		t.Fatal(err)
	}
	if first.SetpointW == nil || *first.SetpointW != -100 {
		t.Fatalf("first publish SetpointW = %v, want -100", first.SetpointW)
	}
	if second.SetpointW == nil || *second.SetpointW != -900 {
		t.Fatalf("second publish SetpointW = %v, want -900 (order preserved)", second.SetpointW)
	}
}

// TestTickTiming_AccumulatesAcrossActuators verifies the TASK-046 tick-budget
// accumulator: every actuator kind's Apply call adds its own wall time into
// the SAME shared *tickTiming, and takeReset both returns the total and
// zeroes it for the next pass.
func TestTickTiming_AccumulatesAcrossActuators(t *testing.T) {
	tt := &tickTiming{}
	mc := &fakeHubMQTTClient{}
	batt := newDesiredPublishingBatteryActuator(mc, "b0", nil, nil, tt, nil)
	sol := newDesiredPublishingSolarActuator(mc, "inv0", nil, nil, tt, nil)

	if err := batt.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "b0", SetpointW: -100, Connect: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := sol.ApplySolarCommand(orchestrator.SolarCommand{Name: "inv0", CurtailToW: 500}); err != nil {
		t.Fatal(err)
	}

	total := tt.takeReset()
	if total <= 0 {
		t.Fatal("expected accumulated actuator time to be > 0 after two Apply calls")
	}
	if again := tt.takeReset(); again != 0 {
		t.Fatalf("takeReset must zero the accumulator, got %s on the second read", again)
	}
}

// TestTickTiming_OverrunDecision pins the arithmetic planObserver uses in
// main.go to decide lexa_hub_tick_overruns_total (total > tickBudget):
// tickTiming itself is unit-testable in isolation, but the counter increment
// lives inline in main()'s planObserver closure and is exercised at the
// bench gate (mqtt-broker-latency scenario, metric curl evidence) rather
// than here.
func TestTickTiming_OverrunDecision(t *testing.T) {
	tt := &tickTiming{}
	tt.add(2 * time.Second)
	budget := 1 * time.Second
	total := tt.takeReset()
	if total <= budget {
		t.Fatalf("2s accumulated against a 1s budget must count as an overrun, got total=%s budget=%s", total, budget)
	}
}

// TestDesiredPublishingSolarActuator_RestoreIsExplicitCeiling is the core
// TASK-029 mapping: restore (CurtailToW == NaN) must publish an EXPLICIT large
// CeilingW (bus.RestoreCeilingW), never an absent field — the whole Mode-A/B
// class exists because restore must be a positive opinion on the wire.
func TestDesiredPublishingSolarActuator_RestoreIsExplicitCeiling(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil, nil, nil, nil)

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

// TestDesiredPublishingSolarActuator_AsyncFailureRetries: a failed publish
// (opportunistically caught the same call, see the battery twin's doc) never
// surfaces as a return error (TASK-046: nothing left to synchronously
// return), does not update the dedupe baseline, and is retried on the very
// next call with identical content.
func TestDesiredPublishingSolarActuator_AsyncFailureRetries(t *testing.T) {
	mc := &fakeHubMQTTClient{failNext: true}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil, nil, nil, nil)
	cmd := orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: 1000}

	if err := a.ApplySolarCommand(cmd); err != nil {
		t.Fatalf("an async publish failure must never surface as a return error: %v", err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publish attempts, want 1", len(mc.publishes))
	}

	if err := a.ApplySolarCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publish attempts after a failed first publish, want 2 (retry)", len(mc.publishes))
	}
}

// TestDesiredPublishingSolarActuator_ContentDedupe: an unchanged ceiling
// publishes once (the retained doc is standing intent, not a tick stream).
func TestDesiredPublishingSolarActuator_ContentDedupe(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil, nil, nil, nil)
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
	a := newDesiredPublishingEVSEActuator(mc, "cs-001", nil, nil, nil, nil)

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

// ---------------------------------------------------------------------
// WS-4.3: real MRID plumbed from the command into the published doc
// ---------------------------------------------------------------------

// TestDesiredPublishingBatteryActuator_MRIDPassthrough verifies the
// published bus.DesiredState.MRID matches orchestrator.BatteryCommand.MRID —
// the actuator-side half of WS-4.3 (the optimizer-side half is covered by
// TestOptimizer_CommandsStampedWithActiveControlMRID/
// TestOptimizer_CommandsUnstampedWithNoActiveControl in
// internal/orchestrator/optimizer_test.go). Both a populated and an empty
// MRID are exercised so this doesn't just prove "some string passes
// through" — an empty MRID (no active CSIP control) must publish as
// omitted (bus.DesiredState.MRID has `omitempty`), not a literal "".
func TestDesiredPublishingBatteryActuator_MRIDPassthrough(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)

	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, MRID: "mrid-batt-1"}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}
	var doc bus.DesiredState
	if err := json.Unmarshal(mc.publishes[0].payload, &doc); err != nil {
		t.Fatalf("unmarshal published doc: %v", err)
	}
	if doc.MRID != "mrid-batt-1" {
		t.Fatalf("published MRID = %q, want mrid-batt-1", doc.MRID)
	}

	// A content-differing follow-up command with MRID == "" (control cleared)
	// must publish MRID omitted from the wire doc, not a literal "mrid".
	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -600, MRID: ""}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publishes after a content-changing follow-up, want 2", len(mc.publishes))
	}
	if strings.Contains(string(mc.publishes[1].payload), `"mrid"`) {
		t.Errorf("published payload = %s, want mrid key omitted (empty MRID, omitempty)", mc.publishes[1].payload)
	}
}

// TestDesiredPublishingSolarActuator_MRIDPassthrough is
// TestDesiredPublishingBatteryActuator_MRIDPassthrough's solar counterpart.
func TestDesiredPublishingSolarActuator_MRIDPassthrough(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "pv-0", nil, nil, nil, nil)

	if err := a.ApplySolarCommand(orchestrator.SolarCommand{Name: "pv-0", CurtailToW: 2000, MRID: "mrid-solar-1"}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}
	var doc bus.DesiredState
	if err := json.Unmarshal(mc.publishes[0].payload, &doc); err != nil {
		t.Fatalf("unmarshal published doc: %v", err)
	}
	if doc.MRID != "mrid-solar-1" {
		t.Fatalf("published MRID = %q, want mrid-solar-1", doc.MRID)
	}
}

// TestDesiredPublishingEVSEActuator_MRIDPassthrough is
// TestDesiredPublishingBatteryActuator_MRIDPassthrough's EVSE counterpart.
func TestDesiredPublishingEVSEActuator_MRIDPassthrough(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingEVSEActuator(mc, "cs-001", nil, nil, nil, nil)

	if err := a.ApplyEVSECommand(orchestrator.EVSECommand{StationID: "cs-001", ConnectorID: 1, MaxCurrentA: 16, MRID: "mrid-evse-1"}); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}
	var doc bus.DesiredState
	if err := json.Unmarshal(mc.publishes[0].payload, &doc); err != nil {
		t.Fatalf("unmarshal published doc: %v", err)
	}
	if doc.MRID != "mrid-evse-1" {
		t.Fatalf("published MRID = %q, want mrid-evse-1", doc.MRID)
	}
}

// ---------------------------------------------------------------------
// WS-2 fix 1: heartbeat re-stamp (desiredHeartbeatInterval)
// ---------------------------------------------------------------------

// TestDesiredPublishingBatteryActuator_HeartbeatRestampsWithoutContentChange
// is WS-2 fix 1's core acceptance criterion: an actuator whose standing
// intent has not changed content must still re-publish, with a fresh
// IssuedAt/Seq, once desiredHeartbeatInterval has elapsed since the last
// publish — otherwise the retained doc's IssuedAt ages past the reconciler's
// StaleAfter bound and a restarting consumer rejects it outright (the WS-2
// fail-open).
func TestDesiredPublishingBatteryActuator_HeartbeatRestampsWithoutContentChange(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, nil)
	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}

	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("first command: got %d publishes, want 1", len(mc.publishes))
	}
	firstIssuedAt := a.lastPublished.IssuedAt

	// Well within the cadence: identical content must not republish (this is
	// the content-dedupe path staying intact — see the sibling test below).
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("repeat within desiredHeartbeatInterval: got %d publishes, want still 1", len(mc.publishes))
	}

	// Back-date the believed-live doc past the heartbeat cadence, simulating
	// a converged optimizer that hasn't changed content in that long — the
	// STOCK-realistic quiescence scenario WS-2 exploits.
	backdatedIssuedAt := firstIssuedAt - int64(desiredHeartbeatInterval/time.Second)
	a.lastPublished.IssuedAt = backdatedIssuedAt

	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("after desiredHeartbeatInterval elapsed: got %d publishes, want 2 (heartbeat re-stamp)", len(mc.publishes))
	}

	var doc bus.DesiredState
	if err := json.Unmarshal(mc.publishes[1].payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SetpointW == nil || *doc.SetpointW != -500 || doc.Connect == nil || !*doc.Connect {
		t.Fatalf("heartbeat re-stamp must carry the SAME content, got %+v", doc)
	}
	// IssuedAt has 1-second resolution and this test runs well within a
	// single wall-clock second, so the re-stamp can legitimately equal
	// firstIssuedAt — assert it moved forward from the artificially
	// backdated value (proving a genuine re-stamp happened) rather than
	// requiring strict inequality against firstIssuedAt.
	if doc.IssuedAt <= backdatedIssuedAt {
		t.Fatalf("heartbeat re-stamp must carry a FRESH IssuedAt, got %d, backdated was %d", doc.IssuedAt, backdatedIssuedAt)
	}
	if doc.Seq != 1 {
		t.Fatalf("heartbeat re-stamp must still consume the next Seq (async path, not bypassed), got %d", doc.Seq)
	}
}

// TestDesiredPublishingSolarActuator_NoHeartbeatBeforeCadence pins the other
// side of the boundary: a republish must NOT fire before
// desiredHeartbeatInterval has elapsed, even down to one second short — the
// heartbeat must not degrade into a per-tick publish (which would defeat the
// "retained doc is standing intent, not a tick stream" contract TASK-046
// established).
func TestDesiredPublishingSolarActuator_NoHeartbeatBeforeCadence(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil, nil, nil, nil)
	cmd := orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: 2000}

	if err := a.ApplySolarCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(mc.publishes))
	}

	// One second short of the cadence: must not republish.
	a.lastPublished.IssuedAt -= int64(desiredHeartbeatInterval/time.Second) - 1
	if err := a.ApplySolarCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 1 {
		t.Fatalf("1s short of desiredHeartbeatInterval: got %d publishes, want still 1", len(mc.publishes))
	}

	// Exactly at the cadence: must republish.
	a.lastPublished.IssuedAt -= 1
	if err := a.ApplySolarCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("at desiredHeartbeatInterval: got %d publishes, want 2", len(mc.publishes))
	}
}

// TestDesiredPublishingBatteryActuator_HeartbeatDoesNotJournalDispatch is
// WS-2 fix 1's "keep desiredContentEqual for the write-to-hardware dedupe"
// requirement made concrete on the hub side: a heartbeat re-stamp flows
// through the same async publish path (it must, so its failures are counted
// the same way — see the publishes/asyncFailures assertions in the sibling
// tests), but it is NOT a new command, so it must not journal a TASK-040
// dispatch event — that log is the audit trail of actual commands, and its
// "post-dedupe only" contract (TestDesiredPublishingBatteryActuator_
// JournalsDispatchPostDedupeOnly) must hold for heartbeats too.
func TestDesiredPublishingBatteryActuator_HeartbeatDoesNotJournalDispatch(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, jw)
	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}

	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	a.lastPublished.IssuedAt -= int64(desiredHeartbeatInterval / time.Second)
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if len(mc.publishes) != 2 {
		t.Fatalf("got %d publishes, want 2 (initial + heartbeat)", len(mc.publishes))
	}
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	if got := len(events[journal.TypeDispatch]); got != 1 {
		t.Fatalf("dispatch events = %d, want 1 (heartbeat re-stamp must not journal a second dispatch)", got)
	}
}

// TestDesiredPublishingBatteryActuator_HeartbeatCountsPublishesAndFailures
// verifies the heartbeat re-stamp flows through the SAME metrics the
// TASK-046 async path already counts (publishes.Inc() on fire,
// asyncFailures.Inc() on a harvested failure) rather than a bypass — a
// silently-uncounted heartbeat failure would hide exactly the "publisher
// wedged" condition WS-2 fix 1 exists to surface.
func TestDesiredPublishingBatteryActuator_HeartbeatCountsPublishesAndFailures(t *testing.T) {
	reg := metrics.New()
	publishes := reg.Counter("test_desired_publishes_total")
	asyncFailures := reg.Counter("test_desired_publish_failures_total")
	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", publishes, asyncFailures, nil, nil)
	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}

	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	a.lastPublished.IssuedAt -= int64(desiredHeartbeatInterval / time.Second)
	mc.failNext = true
	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	out := reg.Format()
	if !strings.Contains(out, "test_desired_publishes_total 1\n") {
		t.Fatalf("publishes counter: want 1 (only the first fire succeeded; the heartbeat failed), got:\n%s", out)
	}
	if !strings.Contains(out, "test_desired_publish_failures_total 1\n") {
		t.Fatalf("asyncFailures counter: want 1 (heartbeat failure must be counted, not bypassed), got:\n%s", out)
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

// ---------------------------------------------------------------------
// TASK-040: journal wiring (dispatch, post-dedupe only)
// ---------------------------------------------------------------------

// TestDesiredPublishingBatteryActuator_JournalsDispatchPostDedupeOnly is the
// "post-dedupe" acceptance criterion: a dispatch is journaled only when the
// desired-doc publish actually happens (content changed), never on a
// dedupe-suppressed repeat tick with unchanged content.
func TestDesiredPublishingBatteryActuator_JournalsDispatchPostDedupeOnly(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, jw)
	cmd := orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}

	if err := a.ApplyBatteryCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyBatteryCommand(cmd); err != nil { // dedupe-suppressed repeat
		t.Fatal(err)
	}
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	d := events[journal.TypeDispatch]
	if len(d) != 1 {
		t.Fatalf("dispatch events = %d, want 1 (dedupe-suppressed repeat must not journal)", len(d))
	}
	var payload journal.Dispatch
	if err := json.Unmarshal(d[0].Data, &payload); err != nil {
		t.Fatalf("unmarshal Dispatch: %v", err)
	}
	if payload.Kind != journal.KindBattery || payload.Device != "battery-0" ||
		payload.SetpointW == nil || *payload.SetpointW != -500 {
		t.Fatalf("Dispatch payload = %+v, want kind=battery device=battery-0 setpoint_w=-500", payload)
	}
}

// TestDesiredPublishingBatteryActuator_FailedPublishDoesNotJournal verifies a
// publish that the opportunistic post-fire check (harvestPending, called
// from ApplyBatteryCommand right after firing — see its doc) immediately
// finds failed never journals a dispatch — only a publish that, as far as
// this actuator can tell by the time it returns, actually took. TASK-046:
// the failure no longer surfaces as this call's return error (there is no
// synchronous broker round trip left to report it through), so this asserts
// nil error and zero journal entries instead of a non-nil error.
func TestDesiredPublishingBatteryActuator_FailedPublishDoesNotJournal(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	mc := &fakeHubMQTTClient{failNext: true}
	a := newDesiredPublishingBatteryActuator(mc, "battery-0", nil, nil, nil, jw)
	if err := a.ApplyBatteryCommand(orchestrator.BatteryCommand{Name: "battery-0", SetpointW: -500, Connect: ptr(true)}); err != nil {
		t.Fatalf("an async publish failure must never surface as a return error: %v", err)
	}
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	if got := len(events[journal.TypeDispatch]); got != 0 {
		t.Fatalf("dispatch events = %d, want 0 (a failed publish must not journal)", got)
	}
}

// TestDesiredPublishingSolarActuator_JournalsDispatch verifies the solar
// actuator's dispatch payload carries CeilingW (not SetpointW) and the solar
// Kind.
func TestDesiredPublishingSolarActuator_JournalsDispatch(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingSolarActuator(mc, "inverter-0", nil, nil, nil, jw)
	if err := a.ApplySolarCommand(orchestrator.SolarCommand{Name: "inverter-0", CurtailToW: 3000}); err != nil {
		t.Fatal(err)
	}
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	d := events[journal.TypeDispatch]
	if len(d) != 1 {
		t.Fatalf("dispatch events = %d, want 1", len(d))
	}
	var payload journal.Dispatch
	if err := json.Unmarshal(d[0].Data, &payload); err != nil {
		t.Fatalf("unmarshal Dispatch: %v", err)
	}
	if payload.Kind != journal.KindSolar || payload.CeilingW == nil || *payload.CeilingW != 3000 {
		t.Fatalf("Dispatch payload = %+v, want kind=solar ceiling_w=3000", payload)
	}
}

// TestDesiredPublishingEVSEActuator_JournalsDispatch verifies the EVSE
// actuator's dispatch payload carries MaxCurrentA and the evse Kind.
func TestDesiredPublishingEVSEActuator_JournalsDispatch(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	mc := &fakeHubMQTTClient{}
	a := newDesiredPublishingEVSEActuator(mc, "cs-001", nil, nil, nil, jw)
	if err := a.ApplyEVSECommand(orchestrator.EVSECommand{StationID: "cs-001", ConnectorID: 1, MaxCurrentA: 16}); err != nil {
		t.Fatal(err)
	}
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	d := events[journal.TypeDispatch]
	if len(d) != 1 {
		t.Fatalf("dispatch events = %d, want 1", len(d))
	}
	var payload journal.Dispatch
	if err := json.Unmarshal(d[0].Data, &payload); err != nil {
		t.Fatalf("unmarshal Dispatch: %v", err)
	}
	if payload.Kind != journal.KindEVSE || payload.Device != "cs-001" ||
		payload.MaxCurrentA == nil || *payload.MaxCurrentA != 16 {
		t.Fatalf("Dispatch payload = %+v, want kind=evse device=cs-001 max_current_a=16", payload)
	}
}
