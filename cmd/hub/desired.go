package main

import (
	"log"
	"math"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

// tickTiming accumulates the wall-clock cost of one engine pass's actuator
// Apply*Command calls (TASK-046 tick budget). Every desired-doc actuator adds
// its own call duration into the SAME *tickTiming (one per hub process, built
// in main.go and handed to every actuator constructor below), so the
// accumulator's value after a pass is the total time ALL devices' Apply calls
// took that pass — the synchronous cost §11 flagged, now that the publishes
// themselves are async and should be close to zero.
//
// Engine.tick()/safetyTick() call PlanObserver BEFORE executePlan (the
// function that invokes Apply*Command) — see internal/orchestrator/engine.go
// tick()/executePlan — so this pass's actuator time is only fully known by
// the time PlanObserver runs for the NEXT pass. main.go's planObserver reads
// (and resets) this accumulator at its own entry, one pass lagged from the
// executePlan it's reporting on; see its comment for why that lag is
// acceptable for an overrun counter.
type tickTiming struct {
	mu      sync.Mutex
	elapsed time.Duration
}

func (t *tickTiming) add(d time.Duration) {
	t.mu.Lock()
	t.elapsed += d
	t.mu.Unlock()
}

// takeReset returns the accumulated duration and resets it to zero — read
// exactly once per pass by planObserver.
func (t *tickTiming) takeReset() time.Duration {
	t.mu.Lock()
	d := t.elapsed
	t.elapsed = 0
	t.mu.Unlock()
	return d
}

// desiredPublishingBatteryActuator is the battery actuator: every battery
// command publishes the standing intent as a retained bus.DesiredState document
// on lexa/desired/battery/{device} (AD-013), which the lexa-modbus reconciler
// consumes as the authoritative desired state. TASK-032 deleted the legacy
// lexa/control/battery command path; this desired-doc publisher is now the ONLY
// battery actuator implementation (it no longer wraps a legacy publisher).
//
// TASK-046: the publish itself is now async (fire-with-timeout, harvested at
// the start of the NEXT Apply call) instead of a synchronous PUBACK wait —
// see harvestPending's doc for the full contract. ApplyBatteryCommand's error
// return is therefore no longer how a failed/late publish surfaces (that is
// now a harvested, logged, counted event with no caller-visible error) — it
// is reserved for the one thing that IS still synchronous, a JSON marshal
// failure, which cannot happen for this hand-built doc in practice. Kept as
// an error return anyway so the interface (and engine.go's existing
// log-on-error call site) needs no change.
//
// Source/MRID: BatteryCommand carries neither today (internal/orchestrator's
// SystemState is where the active CSIP control's mRID lives — optimizer.go's
// plan.Breach.MRID stamping shows the only place it currently escapes the
// optimizer). Plumbing it through the actuator interface is a change to
// internal/orchestrator, which this task does not touch (radioactive zone,
// 05 §12). Every document below is stamped Source: "economic", MRID: "" —
// TASK-031 (CannotComply attribution end-to-end) is the follow-up that wires
// the real mRID through.
type desiredPublishingBatteryActuator struct {
	mc     mqtt.Client
	device string

	// Standing merged intent: BatteryCommand.SetpointW == NaN and Connect ==
	// nil both mean "leave unchanged" (see orchestrator.BatteryCommand's doc),
	// so the wrapper — the only thing that ever builds this document — must
	// carry the last real value forward rather than let it go absent, per
	// AD-013's field-absence rule ("nil" on the WIRE means no opinion; an
	// unchanged tick is not "no opinion", it is "same opinion as before").
	haveSetpoint bool
	setpointW    float64
	connect      *bool

	// lastPublished is the content (excluding Seq/IssuedAt/V) of the doc we
	// currently BELIEVE is live on the broker — optimistically updated the
	// instant an async publish is fired (see ApplyBatteryCommand), then
	// either left alone (harvest confirms delivery) or rolled back to
	// pendingPrev/pendingSeq (harvest finds a failure or timeout) by
	// harvestPending. Repeat ticks with unchanged standing intent compare
	// equal against it and publish nothing — the retained doc is standing
	// intent, not a tick stream — and, same mechanism, an identical retry
	// while a publish is still in flight is suppressed too (TASK-046's
	// one-slot-pending rule: no duplicate in-flight for one topic).
	lastPublished *bus.DesiredState
	seq           uint64

	// pending is the most recently fired async publish, if its outcome
	// hasn't been harvested yet; nil once harvested (success or failure) or
	// before the first publish. Only ever one at a time (TASK-046 "one-slot
	// pending"): if a NEW, content-different command needs to go out while an
	// older one is still in flight, it is still published — paho preserves
	// per-client publish order regardless of ack timing — but this actuator
	// stops tracking the older one's individual completion in favor of the
	// newer one; a stale, later-arriving ack for the abandoned one is
	// harmless (nothing depends on it) and is not double-counted as a
	// failure. pendingPrev/pendingSeq are what harvestPending rolls back to
	// if pending's publish turns out to have failed or timed out.
	pending     *mqttutil.PendingPub
	pendingPrev *bus.DesiredState
	pendingSeq  uint64

	// publishes counts every retained publish actually fired
	// (lexa_hub_desired_publishes_total, TASK-044-style); nil-safe.
	publishes *metrics.Counter
	// asyncFailures counts every harvested publish failure/timeout across all
	// desired-doc actuators (lexa_hub_desired_publish_failures_total); the
	// async-era equivalent of the synchronous error engine.go used to log
	// straight from ApplyBatteryCommand's return. nil-safe.
	asyncFailures *metrics.Counter

	// timing accumulates this actuator's Apply call durations into the
	// shared per-process tick budget (TASK-046); nil-safe (a nil timing
	// simply means nothing is measured — only used by tests that don't care).
	timing *tickTiming

	// jw is the optional TASK-040 event journal; nil disables the dispatch
	// emit below. Guarded fire-and-forget, same discipline as every other
	// journal call site in this package.
	jw *journal.Writer
}

// newDesiredPublishingBatteryActuator builds the battery actuator for device.
func newDesiredPublishingBatteryActuator(mc mqtt.Client, device string, publishes, asyncFailures *metrics.Counter, timing *tickTiming, jw *journal.Writer) *desiredPublishingBatteryActuator {
	return &desiredPublishingBatteryActuator{mc: mc, device: device, publishes: publishes, asyncFailures: asyncFailures, timing: timing, jw: jw}
}

// harvestPending checks on the previous async publish, if one is still
// outstanding, WITHOUT blocking (mqttutil.PendingPub.Harvest never waits):
//
//   - still in flight, within mqttutil.PublishTimeout of when it was sent:
//     leave it — try again next call.
//   - delivered (ack, no error): leave the optimistic lastPublished/seq
//     exactly as they are; they were already correct.
//   - failed (broker error) OR timed out with no verdict yet: roll
//     lastPublished/seq back to what they were BEFORE this publish was fired
//     (pendingPrev/pendingSeq) and count it. This reproduces, one tick later,
//     exactly what the old synchronous branch did on a publish error — "leave
//     the dedupe baseline alone so the identical content is retried on the
//     next tick" — because the next ApplyBatteryCommand call's content
//     comparison now runs against the ROLLED-BACK (i.e. stale) baseline
//     again, so an unchanged standing intent no longer compares equal and
//     re-publishes. A timeout with no later-arriving success is treated the
//     same as an outright failure: the publish is not cancelled (paho may
//     still deliver it), but this actuator stops waiting to find out.
//
// harvestPending is called twice per ApplyBatteryCommand: once at entry, for
// whatever publish is still outstanding from a PREVIOUS call, and once
// (opportunistically) immediately after firing a new one this call. The
// second call is what it sounds like it shouldn't be — free: Harvest never
// blocks, so checking right away costs nothing, and it catches the case
// where the ack (or, in a unit test with an already-resolved fake token, the
// error) is already sitting there — a fast/local broker, or a test —
// without waiting a whole extra tick to find out. A genuinely in-flight
// publish (the real motivating case, and the only one that matters against
// an actual sick broker) is untouched by this and is left for the NEXT
// call's entry harvest, exactly as documented above. The return value tells
// the caller whether THIS call's own just-fired publish was rolled back
// by that immediate check.
func (a *desiredPublishingBatteryActuator) harvestPending() (rolledBack bool) {
	if a.pending == nil {
		return false
	}
	done, timedOut, err := a.pending.Harvest(mqttutil.PublishTimeout)
	if !done && !timedOut {
		return false // still in flight, within bound — check again next call
	}
	if done && err == nil {
		a.pending = nil
		return false // confirmed delivered; optimistic state was already correct
	}
	if err != nil {
		log.Printf("lexa-hub: publish desired battery %s: %v (async)", a.device, err)
	} else {
		log.Printf("lexa-hub: publish desired battery %s: no ack after %s (async)", a.device, mqttutil.PublishTimeout)
	}
	a.lastPublished = a.pendingPrev
	a.seq = a.pendingSeq
	a.pending = nil
	a.asyncFailures.Inc()
	return true
}

// ApplyBatteryCommand folds the command into the standing intent, harvests
// the previous publish's outcome, then — only when the merged intent's
// content differs from what this actuator currently believes is live —
// fires a new retained bus.DesiredState publish asynchronously (TASK-046: no
// PUBACK wait on the tick path). See harvestPending and the lastPublished/
// pending field docs above for the full contract.
func (a *desiredPublishingBatteryActuator) ApplyBatteryCommand(cmd orchestrator.BatteryCommand) error {
	start := time.Now()
	defer func() {
		if a.timing != nil {
			a.timing.add(time.Since(start))
		}
	}()

	a.harvestPending()

	if !math.IsNaN(cmd.SetpointW) {
		a.setpointW = cmd.SetpointW
		a.haveSetpoint = true
	}
	if cmd.Connect != nil {
		c := *cmd.Connect
		a.connect = &c
	}

	doc := bus.DesiredState{
		Envelope:    bus.Envelope{V: bus.DesiredStateV},
		DeviceClass: bus.DesiredClassBattery,
		DeviceID:    a.device,
		Source:      "economic",
	}
	if a.haveSetpoint {
		w := a.setpointW
		doc.SetpointW = &w
	}
	if a.connect != nil {
		c := *a.connect
		doc.Connect = &c
	}

	if desiredContentEqual(a.lastPublished, doc) {
		return nil
	}

	now := time.Now()
	doc.IssuedAt = now.Unix()
	doc.Seq = a.seq

	pp, pubErr := mqttutil.PublishJSONRetainedAsync(a.mc, bus.DesiredTopic(bus.DesiredClassBattery, a.device), doc)
	if pubErr != nil {
		// Marshal error only (PublishJSONRetainedAsync's doc) — nothing was
		// queued, so there is nothing to harvest and no optimistic state to set.
		log.Printf("lexa-hub: publish desired battery %s: %v", a.device, pubErr)
		return pubErr
	}

	a.pendingPrev = a.lastPublished
	a.pendingSeq = a.seq
	stored := doc
	a.lastPublished = &stored
	a.seq++
	a.pending = pp

	// Opportunistic immediate check (see harvestPending's doc): if this just
	// happened to resolve to a failure already, don't count/journal a
	// dispatch that we already know didn't stick — the rolled-back baseline
	// means the very next call sees this content as still-needed and retries.
	if a.harvestPending() {
		return nil
	}

	a.publishes.Inc()
	// TASK-040: dispatch is journaled post-dedupe (this line only runs on an
	// actual content-changed publish that the opportunistic check above did
	// not immediately roll back), so write volume is bounded by real command
	// changes, not the tick rate. Journaled at (successful-so-far) FIRE time,
	// not confirmed-delivery time: "dispatch" records that the hub told the
	// device this, and TASK-032's contract already tolerates a late/dropped
	// delivery (the reconciler re-asserts); waiting for a later harvest to
	// journal would silently lose entries for any command whose actuator
	// instance never gets a "next call" (e.g. process exit before the
	// following tick).
	if a.jw != nil {
		if ev, err := journal.NewDispatchEvent("hub", journal.NewDispatch(a.device, journal.KindBattery, doc.SetpointW, nil, nil, doc.Connect)); err == nil {
			_ = a.jw.Append(ev)
		}
	}
	return nil
}

// desiredContentEqual reports whether cand's opinion content matches last's —
// ignoring Envelope.V (constant), IssuedAt, and Seq, which change on every
// publish regardless of content and must not themselves trigger a republish.
// last == nil (nothing published yet) is never equal to any content.
func desiredContentEqual(last *bus.DesiredState, cand bus.DesiredState) bool {
	if last == nil {
		return false
	}
	return last.DeviceClass == cand.DeviceClass &&
		last.DeviceID == cand.DeviceID &&
		last.Source == cand.Source &&
		last.MRID == cand.MRID &&
		last.ConnectorID == cand.ConnectorID &&
		floatPtrEqual(last.SetpointW, cand.SetpointW) &&
		floatPtrEqual(last.CeilingW, cand.CeilingW) &&
		floatPtrEqual(last.MaxCurrentA, cand.MaxCurrentA) &&
		boolPtrEqual(last.Connect, cand.Connect)
}

// desiredPublishingSolarActuator is the solar actuator: every solar command
// publishes the standing curtailment intent as a retained bus.DesiredState
// document on lexa/desired/solar/{device} (AD-013), which the lexa-modbus solar
// reconciler consumes as the authoritative desired state. TASK-032 deleted the
// legacy lexa/control/solar command path; this desired-doc publisher is now the
// ONLY solar actuator implementation. TASK-046: async publish, harvested at the
// start of the next call — see desiredPublishingBatteryActuator's doc for the
// full contract (identical shape, different payload).
//
// The critical solar-specific mapping (ledger L1/L7): restore is a WRITE, not an
// absence. orchestrator.SolarCommand encodes restore as CurtailToW == NaN; this
// wrapper translates that to an EXPLICIT CeilingW = bus.RestoreCeilingW (the
// device clamps it to WMax → 100% output). A real cap value maps to CeilingW =
// that value. The doc NEVER encodes restore as an absent CeilingW — the whole
// Mode-A/B class exists because restore must be explicit and connectivity-
// independent (the retained doc keeps the cap value until the optimizer
// releases it; the reconciler reasserts it on reconnect regardless of whether
// the inverter was dark, reproducing the solarCapActive dark-inverter gate
// without a publisher equivalent).
type desiredPublishingSolarActuator struct {
	mc     mqtt.Client
	device string

	// connect is the standing connect intent (Unit 3.6 gateway fan-out). A nil
	// SolarCommand.Connect means "no opinion — leave unchanged"; the last
	// non-nil value is carried forward exactly like the battery actuator's
	// connect fold. It stays nil in optimizer mode (DefaultOptimizer never sets
	// SolarCommand.Connect), so the published doc omits Connect entirely and is
	// byte-identical to pre-3.6 — the upgrade-storm guard (a nil Connect must
	// never change existing doc content and trigger a spurious republish).
	connect *bool

	lastPublished *bus.DesiredState
	seq           uint64

	pending     *mqttutil.PendingPub
	pendingPrev *bus.DesiredState
	pendingSeq  uint64

	publishes     *metrics.Counter
	asyncFailures *metrics.Counter
	timing        *tickTiming
	jw            *journal.Writer
}

func newDesiredPublishingSolarActuator(mc mqtt.Client, device string, publishes, asyncFailures *metrics.Counter, timing *tickTiming, jw *journal.Writer) *desiredPublishingSolarActuator {
	return &desiredPublishingSolarActuator{mc: mc, device: device, publishes: publishes, asyncFailures: asyncFailures, timing: timing, jw: jw}
}

// harvestPending is desiredPublishingBatteryActuator.harvestPending's twin —
// see that doc for the full contract, including why it is called twice per
// Apply call and what its return value means.
func (a *desiredPublishingSolarActuator) harvestPending() (rolledBack bool) {
	if a.pending == nil {
		return false
	}
	done, timedOut, err := a.pending.Harvest(mqttutil.PublishTimeout)
	if !done && !timedOut {
		return false
	}
	if done && err == nil {
		a.pending = nil
		return false
	}
	if err != nil {
		log.Printf("lexa-hub: publish desired solar %s: %v (async)", a.device, err)
	} else {
		log.Printf("lexa-hub: publish desired solar %s: no ack after %s (async)", a.device, mqttutil.PublishTimeout)
	}
	a.lastPublished = a.pendingPrev
	a.seq = a.pendingSeq
	a.pending = nil
	a.asyncFailures.Inc()
	return true
}

// ApplySolarCommand publishes — only when the derived ceiling differs from
// what this actuator currently believes is live — a retained bus.DesiredState
// carrying an explicit CeilingW, fired asynchronously (TASK-046). A
// SolarCommand always expresses a full ceiling opinion (NaN CurtailToW is
// restore, a real value is the cap), so every call yields a complete
// CeilingW.
func (a *desiredPublishingSolarActuator) ApplySolarCommand(cmd orchestrator.SolarCommand) error {
	start := time.Now()
	defer func() {
		if a.timing != nil {
			a.timing.add(time.Since(start))
		}
	}()

	a.harvestPending()

	ceiling := bus.RestoreCeilingW // NaN CurtailToW ⇒ restore is an explicit large ceiling
	if !math.IsNaN(cmd.CurtailToW) {
		ceiling = math.Max(0, cmd.CurtailToW)
	}

	// Fold the gateway connect intent (Unit 3.6): carry a non-nil opinion
	// forward; a nil Connect leaves the standing value untouched (nil stays nil
	// in optimizer mode ⇒ Connect omitted from the doc, byte-stable on upgrade).
	if cmd.Connect != nil {
		c := *cmd.Connect
		a.connect = &c
	}

	doc := bus.DesiredState{
		Envelope:    bus.Envelope{V: bus.DesiredStateV},
		DeviceClass: bus.DesiredClassSolar,
		DeviceID:    a.device,
		CeilingW:    &ceiling,
		Source:      "economic",
	}
	if a.connect != nil {
		c := *a.connect
		doc.Connect = &c
	}

	if desiredContentEqual(a.lastPublished, doc) {
		return nil
	}

	now := time.Now()
	doc.IssuedAt = now.Unix()
	doc.Seq = a.seq

	pp, pubErr := mqttutil.PublishJSONRetainedAsync(a.mc, bus.DesiredTopic(bus.DesiredClassSolar, a.device), doc)
	if pubErr != nil {
		log.Printf("lexa-hub: publish desired solar %s: %v", a.device, pubErr)
		return pubErr
	}

	a.pendingPrev = a.lastPublished
	a.pendingSeq = a.seq
	stored := doc
	a.lastPublished = &stored
	a.seq++
	a.pending = pp

	if a.harvestPending() {
		return nil
	}

	a.publishes.Inc()
	if a.jw != nil {
		if ev, err := journal.NewDispatchEvent("hub", journal.NewDispatch(a.device, journal.KindSolar, nil, doc.CeilingW, nil, doc.Connect)); err == nil {
			_ = a.jw.Append(ev)
		}
	}
	return nil
}

// desiredPublishingEVSEActuator is the EVSE actuator: every EVSE command
// publishes the standing current-limit intent as a retained bus.DesiredState
// document on lexa/desired/evse/{station} (AD-013), which the lexa-ocpp
// reconciler consumes as the authoritative desired state. TASK-032 deleted the
// legacy lexa/evse/{station}/command path; this is now the ONLY EVSE actuator
// implementation. TASK-046: async publish, harvested at the start of the next
// call — see desiredPublishingBatteryActuator's doc for the full contract.
//
// orchestrator.EVSECommand.MaxCurrentA == 0 is an explicit suspend (not "no
// opinion"), so it is published as MaxCurrentA == &0 — the reconciler maps that
// to a 0 A SetChargingProfile. ConnectorID rides inside the document (the EVSE
// keeps one retained doc per station, topic device == stationID). Connect: the
// EVSE doc has ALWAYS asserted connect (TASK-030 hardcoded true); Unit 3.6's
// gateway fan-out lets a non-nil EVSECommand.Connect (a CSIP OpModConnect
// cease-to-energize) override it — carried forward like the battery actuator's
// fold, defaulting to true when no command has expressed an opinion (so an
// upgrade with a nil Connect keeps the doc byte-stable).
type desiredPublishingEVSEActuator struct {
	mc        mqtt.Client
	stationID string

	// connect is the standing connect intent. It defaults to true (see the type
	// doc) — set lazily on first Apply — so optimizer-mode docs (nil
	// EVSECommand.Connect) publish Connect=true exactly as they always have; a
	// non-nil command Connect overrides and is carried forward.
	connect *bool

	lastPublished *bus.DesiredState
	seq           uint64

	pending     *mqttutil.PendingPub
	pendingPrev *bus.DesiredState
	pendingSeq  uint64

	publishes     *metrics.Counter
	asyncFailures *metrics.Counter
	timing        *tickTiming
	jw            *journal.Writer
}

func newDesiredPublishingEVSEActuator(mc mqtt.Client, stationID string, publishes, asyncFailures *metrics.Counter, timing *tickTiming, jw *journal.Writer) *desiredPublishingEVSEActuator {
	return &desiredPublishingEVSEActuator{mc: mc, stationID: stationID, publishes: publishes, asyncFailures: asyncFailures, timing: timing, jw: jw}
}

// harvestPending is desiredPublishingBatteryActuator.harvestPending's twin —
// see that doc for the full contract, including why it is called twice per
// Apply call and what its return value means.
func (a *desiredPublishingEVSEActuator) harvestPending() (rolledBack bool) {
	if a.pending == nil {
		return false
	}
	done, timedOut, err := a.pending.Harvest(mqttutil.PublishTimeout)
	if !done && !timedOut {
		return false
	}
	if done && err == nil {
		a.pending = nil
		return false
	}
	if err != nil {
		log.Printf("lexa-hub: publish desired evse %s: %v (async)", a.stationID, err)
	} else {
		log.Printf("lexa-hub: publish desired evse %s: no ack after %s (async)", a.stationID, mqttutil.PublishTimeout)
	}
	a.lastPublished = a.pendingPrev
	a.seq = a.pendingSeq
	a.pending = nil
	a.asyncFailures.Inc()
	return true
}

// ApplyEVSECommand publishes a retained desired doc, asynchronously
// (TASK-046), when the current-limit intent's content differs from what this
// actuator currently believes is live.
func (a *desiredPublishingEVSEActuator) ApplyEVSECommand(cmd orchestrator.EVSECommand) error {
	start := time.Now()
	defer func() {
		if a.timing != nil {
			a.timing.add(time.Since(start))
		}
	}()

	a.harvestPending()

	// Fold the gateway connect intent (Unit 3.6). Default to true — the EVSE doc
	// has always asserted connect — so a nil cmd.Connect (optimizer mode) keeps
	// the doc byte-stable; a non-nil value overrides and is carried forward.
	if a.connect == nil {
		t := true
		a.connect = &t
	}
	if cmd.Connect != nil {
		c := *cmd.Connect
		a.connect = &c
	}
	maxA := cmd.MaxCurrentA
	connect := *a.connect
	doc := bus.DesiredState{
		Envelope:    bus.Envelope{V: bus.DesiredStateV},
		DeviceClass: bus.DesiredClassEVSE,
		DeviceID:    a.stationID,
		MaxCurrentA: &maxA,
		ConnectorID: cmd.ConnectorID,
		Connect:     &connect,
		Source:      "economic",
	}

	if desiredContentEqual(a.lastPublished, doc) {
		return nil
	}

	now := time.Now()
	doc.IssuedAt = now.Unix()
	doc.Seq = a.seq

	pp, pubErr := mqttutil.PublishJSONRetainedAsync(a.mc, bus.DesiredTopic(bus.DesiredClassEVSE, a.stationID), doc)
	if pubErr != nil {
		log.Printf("lexa-hub: publish desired evse %s: %v", a.stationID, pubErr)
		return pubErr
	}

	a.pendingPrev = a.lastPublished
	a.pendingSeq = a.seq
	stored := doc
	a.lastPublished = &stored
	a.seq++
	a.pending = pp

	if a.harvestPending() {
		return nil
	}

	a.publishes.Inc()
	if a.jw != nil {
		if ev, err := journal.NewDispatchEvent("hub", journal.NewDispatch(a.stationID, journal.KindEVSE, nil, nil, doc.MaxCurrentA, doc.Connect)); err == nil {
			_ = a.jw.Append(ev)
		}
	}
	return nil
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
