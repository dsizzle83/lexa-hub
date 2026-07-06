package main

import (
	"log"
	"math"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

// desiredPublishingBatteryActuator wraps a BatteryActuator (TASK-027) so every
// battery command ALSO republishes the standing intent as a retained
// bus.DesiredState document on lexa/desired/battery/{device} (AD-013), for the
// lexa-modbus shadow reconciler to consume. This is purely additive: the
// legacy actuator is invoked FIRST, unchanged, and its return value is what
// this type returns — the desired-doc publish can fail without altering the
// legacy behavior or the caller-visible error, exactly as ledger L1–L4
// require while the reconciler is still shadow-only (03 Phase 2).
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
	inner  orchestrator.BatteryActuator
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

	// lastPublished is the last doc's content (excluding Seq/IssuedAt/V), so
	// repeat ticks with an unchanged standing intent publish nothing (the
	// retained doc is standing intent, not a tick stream — a per-tick publish
	// would recreate the QoS 1 storm cmdDeduper exists to prevent).
	lastPublished *bus.DesiredState
	seq           uint64

	// publishes counts every retained publish actually sent
	// (lexa_hub_desired_publishes_total, TASK-044-style); nil-safe.
	publishes *metrics.Counter
}

// newDesiredPublishingBatteryActuator builds the wrapper around inner (the
// legacy MQTTBatteryActuator) for device.
func newDesiredPublishingBatteryActuator(inner orchestrator.BatteryActuator, mc mqtt.Client, device string, publishes *metrics.Counter) *desiredPublishingBatteryActuator {
	return &desiredPublishingBatteryActuator{inner: inner, mc: mc, device: device, publishes: publishes}
}

// ApplyBatteryCommand delegates to the legacy actuator first (unchanged path,
// unchanged return value), then — only when the resulting standing intent's
// content differs from the last published doc — publishes a retained
// bus.DesiredState.
func (a *desiredPublishingBatteryActuator) ApplyBatteryCommand(cmd orchestrator.BatteryCommand) error {
	err := a.inner.ApplyBatteryCommand(cmd)

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
		return err
	}

	now := time.Now()
	doc.IssuedAt = now.Unix()
	doc.Seq = a.seq

	if pubErr := mqttutil.PublishJSONRetained(a.mc, bus.DesiredTopic(bus.DesiredClassBattery, a.device), doc); pubErr != nil {
		log.Printf("lexa-hub: publish desired battery %s: %v", a.device, pubErr)
		// Not delivered: leave lastPublished/seq alone so the identical
		// content is retried on the next tick, mirroring cmdDeduper's own
		// "not delivered; retry next tick" convention in actuators.go.
		return err
	}
	stored := doc
	a.lastPublished = &stored
	a.seq++
	a.publishes.Inc()
	return err
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

// desiredPublishingSolarActuator wraps a SolarActuator (TASK-029) so every
// solar command ALSO republishes the standing curtailment intent as a retained
// bus.DesiredState document on lexa/desired/solar/{device} (AD-013), for the
// lexa-modbus solar reconciler to consume. Purely additive, exactly like the
// battery wrapper: the legacy actuator is invoked FIRST, unchanged, and its
// return value is what this type returns — the desired-doc publish can fail
// without altering the legacy behavior.
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
	inner  orchestrator.SolarActuator
	mc     mqtt.Client
	device string

	lastPublished *bus.DesiredState
	seq           uint64
	publishes     *metrics.Counter
}

func newDesiredPublishingSolarActuator(inner orchestrator.SolarActuator, mc mqtt.Client, device string, publishes *metrics.Counter) *desiredPublishingSolarActuator {
	return &desiredPublishingSolarActuator{inner: inner, mc: mc, device: device, publishes: publishes}
}

// ApplySolarCommand delegates to the legacy actuator first (unchanged path,
// unchanged return value), then — only when the derived ceiling differs from
// the last published doc — publishes a retained bus.DesiredState carrying an
// explicit CeilingW. Unlike the battery wrapper there is no "carry the last
// value forward" bookkeeping: a SolarCommand always expresses a full ceiling
// opinion (NaN CurtailToW is restore, a real value is the cap), so every call
// yields a complete CeilingW.
func (a *desiredPublishingSolarActuator) ApplySolarCommand(cmd orchestrator.SolarCommand) error {
	err := a.inner.ApplySolarCommand(cmd)

	ceiling := bus.RestoreCeilingW // NaN CurtailToW ⇒ restore is an explicit large ceiling
	if !math.IsNaN(cmd.CurtailToW) {
		ceiling = math.Max(0, cmd.CurtailToW)
	}

	doc := bus.DesiredState{
		Envelope:    bus.Envelope{V: bus.DesiredStateV},
		DeviceClass: bus.DesiredClassSolar,
		DeviceID:    a.device,
		CeilingW:    &ceiling,
		Source:      "economic",
	}

	if desiredContentEqual(a.lastPublished, doc) {
		return err
	}

	now := time.Now()
	doc.IssuedAt = now.Unix()
	doc.Seq = a.seq

	if pubErr := mqttutil.PublishJSONRetained(a.mc, bus.DesiredTopic(bus.DesiredClassSolar, a.device), doc); pubErr != nil {
		log.Printf("lexa-hub: publish desired solar %s: %v", a.device, pubErr)
		return err
	}
	stored := doc
	a.lastPublished = &stored
	a.seq++
	a.publishes.Inc()
	return err
}

// desiredPublishingEVSEActuator wraps an EVSEActuator (TASK-030) so every EVSE
// command ALSO republishes the standing current-limit intent as a retained
// bus.DesiredState document on lexa/desired/evse/{station} (AD-013), for the
// lexa-ocpp reconciler to consume. Additive, same pattern.
//
// orchestrator.EVSECommand.MaxCurrentA == 0 is an explicit suspend (not "no
// opinion"), so it is published as MaxCurrentA == &0 — the reconciler maps that
// to a 0 A SetChargingProfile. ConnectorID rides inside the document (the EVSE
// keeps one retained doc per station, topic device == stationID). Connect is
// reserved for future disconnect semantics and always published true (TASK-030
// blast-radius note: connect=false is a follow-up, not this task).
type desiredPublishingEVSEActuator struct {
	inner     orchestrator.EVSEActuator
	mc        mqtt.Client
	stationID string

	lastPublished *bus.DesiredState
	seq           uint64
	publishes     *metrics.Counter
}

func newDesiredPublishingEVSEActuator(inner orchestrator.EVSEActuator, mc mqtt.Client, stationID string, publishes *metrics.Counter) *desiredPublishingEVSEActuator {
	return &desiredPublishingEVSEActuator{inner: inner, mc: mc, stationID: stationID, publishes: publishes}
}

// ApplyEVSECommand delegates to the legacy actuator first, then publishes a
// retained desired doc when the current-limit intent's content changes.
func (a *desiredPublishingEVSEActuator) ApplyEVSECommand(cmd orchestrator.EVSECommand) error {
	err := a.inner.ApplyEVSECommand(cmd)

	maxA := cmd.MaxCurrentA
	connect := true
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
		return err
	}

	now := time.Now()
	doc.IssuedAt = now.Unix()
	doc.Seq = a.seq

	if pubErr := mqttutil.PublishJSONRetained(a.mc, bus.DesiredTopic(bus.DesiredClassEVSE, a.stationID), doc); pubErr != nil {
		log.Printf("lexa-hub: publish desired evse %s: %v", a.stationID, pubErr)
		return err
	}
	stored := doc
	a.lastPublished = &stored
	a.seq++
	a.publishes.Inc()
	return err
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
