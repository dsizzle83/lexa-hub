package main

// WP-6 (standards-buildout A4, BASIC-027/G31/G32): the hub-side alarm-edge
// detector. It watches bus.Measurement.AlarmBits (the raw SunSpec model 701
// Alrm bitfield, WP-2) for per-device bit TRANSITIONS and the breach-episode
// component's edge alerts, maps each defensible transition onto a CSIP
// Table 14 DER alarm/RTN LogEvent code, and hands bus.LogEventMsg edges to a
// logEventPublisher for async publish on bus.TopicHubLogEvent (edge, not
// retained, QoS 1) — lexa-northbound's logevent poster POSTs them to the
// EndDevice's LogEventListLink.
//
// Style mirror of cmd/hub/breach.go: the detector is pure state + edge
// arbitration (OnMeasurement/OnBreachAlert return the events to publish;
// tests exercise them without a broker), and the publisher owns the MQTT
// side using the TASK-046 async fire-then-harvest pattern so neither the
// measurement subscription goroutine nor the engine tick path ever blocks on
// a PUBACK.

import (
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
)

// SunSpec model 701 Alrm bitfield positions (model_701.json "Alrm" symbols).
// The vendored lexa-proto/sunspec package decodes Alrm as a raw bitfield32
// and deliberately defines no bit vocabulary (parse layer, not semantics);
// the semantic mapping lives here, hub-side, exactly as
// bus.Measurement.AlarmBits' doc comment promises.
const (
	alrm701GroundFault     uint32 = 1 << 0
	alrm701DCOverVolt      uint32 = 1 << 1
	alrm701ACDisconnect    uint32 = 1 << 2
	alrm701DCDisconnect    uint32 = 1 << 3
	alrm701GridDisconnect  uint32 = 1 << 4
	alrm701CabinetOpen     uint32 = 1 << 5
	alrm701ManualShutdown  uint32 = 1 << 6
	alrm701OverTemp        uint32 = 1 << 7
	alrm701OverFrequency   uint32 = 1 << 8
	alrm701UnderFrequency  uint32 = 1 << 9
	alrm701ACOverVolt      uint32 = 1 << 10
	alrm701ACUnderVolt     uint32 = 1 << 11
	alrm701BlownStringFuse uint32 = 1 << 12
	alrm701UnderTemp       uint32 = 1 << 13
	alrm701MemoryLoss      uint32 = 1 << 14
	alrm701HWTestFailure   uint32 = 1 << 15
	alrm701ManufacturerAlm uint32 = 1 << 16
)

// alrm701ToTable14 maps 701 Alrm bit masks to the CSIP Table 14 ALARM (even)
// code the onset posts; the clear posts LogEventRTN(code). Only defensible
// mappings are present — a bit whose Table 14 semantic is not a clean match
// gets NO LogEvent at all rather than a guessed one:
//
//	OVER_FREQUENCY  → OVER_FREQUENCY(6)   — direct (grid-interface frequency)
//	UNDER_FREQUENCY → UNDER_FREQUENCY(8)  — direct
//	AC_OVER_VOLT    → OVER_VOLTAGE(2)     — direct (AC/grid-interface voltage)
//	AC_UNDER_VOLT   → UNDER_VOLTAGE(4)    — direct
//	MANUAL_SHUTDOWN → EMERGENCY_LOCAL(14) — a locally initiated emergency
//	                  stop is exactly what Table 14's EMERGENCY_LOCAL names.
//
// Deliberately UNMAPPED (no LogEvent), with reasons:
//   - GROUND_FAULT, DC_OVER_VOLT, BLOWN_STRING_FUSE, OVER_TEMP, UNDER_TEMP,
//     CABINET_OPEN, MEMORY_LOSS, HW_TEST_FAILURE: low-level equipment health,
//     which CSIP explicitly scopes OUT of the alarm function set (§4.6.3
//     ¶379-380 — see the digest's Alarms section). DC-side conditions are not
//     grid-interface conditions.
//   - AC_DISCONNECT, DC_DISCONNECT, GRID_DISCONNECT: connection STATE, which
//     the standard reports via DERStatus genConnectStatus (Table 13, WP-4),
//     not as Table 14 alarms.
//   - MANUFACTURER_ALRM: vendor-defined semantics — unknowable here.
//   - Table 14 codes with no 701 Alrm source at all (OVER_CURRENT,
//     VOLTAGE/CURRENT_IMBALANCE, LOW_POWER_INPUT, PHASE_ROTATION) simply have
//     no producer in this repo yet; EMERGENCY_REMOTE's producer is the breach
//     episode path below, not an Alrm bit.
var alrm701ToTable14 = map[uint32]uint8{
	alrm701OverFrequency:  bus.LogEventDEROverFrequency,
	alrm701UnderFrequency: bus.LogEventDERUnderFrequency,
	alrm701ACOverVolt:     bus.LogEventDEROverVoltage,
	alrm701ACUnderVolt:    bus.LogEventDERUnderVoltage,
	alrm701ManualShutdown: bus.LogEventDEREmergencyLocal,
}

// alrm701MappedBits is alrm701ToTable14's key set in ascending bit order —
// observeBits iterates THIS, not the map, so multi-bit simultaneous
// transitions emit in a deterministic order (and LogEventID assignment is
// reproducible in tests).
var alrm701MappedBits = []uint32{
	alrm701ManualShutdown,
	alrm701OverFrequency,
	alrm701UnderFrequency,
	alrm701ACOverVolt,
	alrm701ACUnderVolt,
}

// logEventSiteDevice is the LogEventMsg.Device value for site-level events
// that have no single source device (the breach-episode source).
const logEventSiteDevice = "site"

// devAlarmState is one device's alarm-bit tracking state.
type devAlarmState struct {
	// reported is the alarm-bit state the detector has REPORTED (or adopted
	// as its restart baseline) — not necessarily the last OBSERVED state: a
	// transition suppressed by the rate floor leaves its bit un-flipped here,
	// so the next measurement after the floor re-detects the (still-present)
	// change and the pair discipline never loses a transition, only delays
	// it. A flap that reverts inside the floor nets to nothing.
	reported uint32
	// lastEmit is when each mapped bit last emitted an event, for the
	// per-device-per-bit rate floor.
	lastEmit map[uint32]time.Time
}

// logEventDetector turns alarm-bit transitions and breach-episode edges into
// bus.LogEventMsg edges. Concurrency mirrors breachEpisodes: OnMeasurement
// runs on the MQTT measurement-subscription goroutine, OnBreachAlert on
// whichever goroutine drives emitAlerts (engine control loop or the
// reconcile-report subscription); mu serializes them, and callers publish the
// returned events outside the lock.
type logEventDetector struct {
	mu sync.Mutex

	// minInterval is the logevent_min_interval_s rate floor (flash budget /
	// journal budget: a chattering device must not turn every poll into a
	// LogEvent + journal line). Applied per device per bit, to the
	// MEASUREMENT path only — breach-episode edges are already rare and
	// edge-triggered upstream (breachEpisodes is the debounce), and unlike
	// measurements they have no periodic re-check that would eventually flush
	// a suppressed RTN, so suppressing them could orphan an alarm/RTN pair.
	minInterval time.Duration

	devices map[string]*devAlarmState

	// breachReported latches whether an EMERGENCY_REMOTE alarm is
	// outstanding, so a mRID-switch re-alert (breachEpisodes emits a second
	// Active edge with no intervening clear) does not post a second unpaired
	// alarm — the site-level "cannot honor the remote directive" condition
	// was continuous. The RTN on a clear edge is emitted even when this is
	// false: a hub restart mid-episode loses this latch while the alarm POST
	// from the previous incarnation stands, and the clear is that alarm's
	// only chance at a pair (same reasoning as the seeded-baseline RTN in
	// observeBits below).
	breachReported bool

	// seq feeds LogEventMsg.LogEventID (2030.5's same-second disambiguator);
	// wraps at uint16 by construction.
	seq uint16
}

// newLogEventDetector builds the detector with the configured rate floor.
func newLogEventDetector(minInterval time.Duration) *logEventDetector {
	return &logEventDetector{
		minInterval: minInterval,
		devices:     make(map[string]*devAlarmState),
	}
}

// OnMeasurement feeds one device measurement. Returns the LogEvent edges to
// publish (caller publishes outside the lock; may be empty).
//
// Baseline-after-restart semantics (pinned by logevent_test.go): measurements
// are QoS 0 and not retained, so after a hub restart the first measurement
// seen per device is CURRENT STATE, not a transition — it seeds the baseline
// and emits NO events (an alarm already active at seed time was, in the
// common case, already posted by the previous hub incarnation; re-posting it
// with a fresh timestamp would fabricate an occurrence, G32's timestamps are
// occurrence times). A bit that is SET at seed and later clears DOES emit the
// RTN: that clear is a real transition this process observed, and it is the
// previous incarnation's alarm's only chance at its paired RTN.
func (d *logEventDetector) OnMeasurement(msg bus.Measurement, now time.Time) []bus.LogEventMsg {
	if msg.AlarmBits == nil {
		return nil // device does not report a 701 Alrm bitfield (meter, legacy 10x)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.observeBits(msg.Device, *msg.AlarmBits, now)
}

// observeBits is OnMeasurement's core; mu held.
func (d *logEventDetector) observeBits(device string, bits uint32, now time.Time) []bus.LogEventMsg {
	ds, ok := d.devices[device]
	if !ok {
		// First observation since this process started: baseline, no events.
		d.devices[device] = &devAlarmState{reported: bits, lastEmit: make(map[uint32]time.Time)}
		if bits != 0 {
			// One edge line per device lifetime — an operator breadcrumb that
			// active alarm bits were adopted silently as the restart baseline.
			slog.Info("logevent: alarm bits active at baseline — no LogEvent emitted (restart re-seed)",
				"device", device, "alarm_bits", fmt.Sprintf("%#x", bits))
		}
		return nil
	}

	var out []bus.LogEventMsg
	for _, mask := range alrm701MappedBits {
		alarmCode := alrm701ToTable14[mask]
		cur := bits&mask != 0
		rep := ds.reported&mask != 0
		if cur == rep {
			continue
		}
		if last, seen := ds.lastEmit[mask]; seen && now.Sub(last) < d.minInterval {
			// Rate floor: suppress, and deliberately do NOT update reported —
			// the next measurement past the floor re-detects the change if it
			// persisted, so pairs complete late rather than never.
			continue
		}
		code := alarmCode
		if !cur {
			code = bus.LogEventRTN(alarmCode)
		}
		out = append(out, d.eventLocked(device, code, cur, now,
			fmt.Sprintf("%s/c%d@%d#%d", device, code, now.Unix(), d.seq+1)))
		ds.lastEmit[mask] = now
		ds.reported ^= mask
	}
	// Unmapped bits carry no emission duty; keep reported tracking reality
	// for them so the struct reads as "current state modulo suppressed
	// mapped transitions".
	ds.reported = (ds.reported & alrm701MappedMask) | (bits &^ alrm701MappedMask)
	return out
}

// alrm701MappedMask is the union of the mapped Alrm bits (derived once).
var alrm701MappedMask = func() uint32 {
	var m uint32
	for mask := range alrm701ToTable14 {
		m |= mask
	}
	return m
}()

// OnBreachAlert feeds one breach-episode edge alert (the same
// bus.ComplianceAlert emitAlerts is about to publish). Returns the LogEvent
// edges to publish.
//
// Mapping choice (justification): a CannotComply breach episode has no
// Table 14 code of its own — "cannot comply" is the Response function set's
// vocabulary (the CannotComply Response the responses.Tracker already
// POSTs), while LogEvent carries the site CONDITION. EMERGENCY_REMOTE
// (16/17) is the closest defensible Table 14 name for that condition: the
// site is in distress DEFINED RELATIVE TO the remotely commanded constraint
// (a remote-directive emergency), where EMERGENCY_LOCAL names a locally
// initiated stop and the physical-cause codes (LOW_POWER_INPUT etc.) assert
// a specific cause the hub cannot attest. Alarm at episode onset, RTN at
// episode clear — the breachEpisodes component already guarantees exactly
// one onset and one clear edge per episode (with the one mRID-switch
// exception breachReported absorbs; see its field doc).
func (d *logEventDetector) OnBreachAlert(alert bus.ComplianceAlert, now time.Time) []bus.LogEventMsg {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := alert.EpisodeID
	if key == "" {
		key = alert.MRID // pre-TASK-031 publisher shape; same fallback as the responses tracker
	}
	if alert.Active {
		if d.breachReported {
			return nil // mRID-switch re-alert: condition continuous, alarm already posted
		}
		d.breachReported = true
		return []bus.LogEventMsg{d.eventLocked(logEventSiteDevice,
			bus.LogEventDEREmergencyRemote, true, now, key+"/alarm")}
	}
	// Clear: emit the RTN even when breachReported is false — see the field
	// doc (restart-orphaned pair completion).
	d.breachReported = false
	return []bus.LogEventMsg{d.eventLocked(logEventSiteDevice,
		bus.LogEventRTN(bus.LogEventDEREmergencyRemote), false, now, key+"/rtn")}
}

// eventLocked mints one LogEventMsg (mu held) and logs the edge — the ONLY
// log this pipeline produces per event (edge logging, TASK-045 discipline;
// suppressed/baseline transitions log nothing or one Debug line).
func (d *logEventDetector) eventLocked(device string, code uint8, alarm bool, now time.Time, dedupeKey string) bus.LogEventMsg {
	d.seq++
	ev := bus.LogEventMsg{
		Envelope:     bus.Envelope{V: bus.LogEventV},
		Device:       device,
		FunctionSet:  bus.LogEventFunctionSetDER,
		LogEventCode: code,
		Alarm:        alarm,
		LogEventID:   d.seq,
		CreatedTs:    now.Unix(),
		DedupeKey:    dedupeKey,
	}
	slog.Info("logevent: DER alarm edge",
		"device", device, "code", code, "alarm", alarm, "key", dedupeKey)
	return ev
}

// maxPendingLogEvents bounds the publisher's harvest queue: LogEvents are
// rare edges, so more than this many unresolved publishes means the broker
// is sick — the oldest handle is dropped (the publish itself is not
// cancelled; paho may still deliver it, exactly like a timed-out actuator
// publish).
const maxPendingLogEvents = 32

// logEventPublisher owns the MQTT side: async QoS 1 publish (never retained
// — bus.TopicHubLogEvent is an edge) with the TASK-046 fire-then-harvest
// pattern, so no caller ever blocks on a PUBACK. Unlike the desired-doc
// actuators there is no dedupe baseline to roll back on a failed/timed-out
// publish and no re-issue: a LogEvent edge is an occurrence, not standing
// state — a drop is logged and counted (the publish-failure instrumentation
// hook inside PendingPub.Harvest feeds lexa_mqtt_publish_failures_total) and
// otherwise accepted, the same crash-only stance the northbound poster takes
// for a failed POST.
type logEventPublisher struct {
	mu        sync.Mutex
	mc        mqtt.Client
	pending   []*mqttutil.PendingPub
	published *metrics.Counter // lexa_hub_logevents_total; nil-safe
}

func newLogEventPublisher(mc mqtt.Client, published *metrics.Counter) *logEventPublisher {
	return &logEventPublisher{mc: mc, published: published}
}

// publish fires each event async and harvests prior publishes non-blocking.
// Safe from any goroutine (mu); a no-op on an empty slice.
func (p *logEventPublisher) publish(evs []bus.LogEventMsg) {
	if len(evs) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.harvestLocked()
	for i := range evs {
		// QoS 1 per bus.PubQoS(bus.TopicHubLogEvent) — PublishJSONAsync is
		// QoS 1, not retained, by construction.
		pp, err := mqttutil.PublishJSONAsync(p.mc, bus.TopicHubLogEvent, evs[i])
		if err != nil {
			log.Printf("lexa-hub: publish logevent: %v", err)
			continue
		}
		p.published.Inc()
		p.pending = append(p.pending, pp)
	}
	// One free opportunistic harvest (Harvest never blocks) so an
	// already-resolved ack/error is caught without waiting for the next call.
	p.harvestLocked()
	if n := len(p.pending) - maxPendingLogEvents; n > 0 {
		slog.Warn("lexa-hub: logevent pending queue overflow — dropping oldest handles",
			"dropped", n)
		p.pending = append(p.pending[:0], p.pending[n:]...)
	}
}

// harvestLocked resolves completed/timed-out pending publishes; mu held.
func (p *logEventPublisher) harvestLocked() {
	keep := p.pending[:0]
	for _, pp := range p.pending {
		done, timedOut, err := pp.Harvest(mqttutil.PublishTimeout)
		switch {
		case done && err != nil:
			log.Printf("lexa-hub: publish logevent: %v (async)", err)
		case !done && timedOut:
			log.Printf("lexa-hub: publish logevent: no ack after %s (async)", mqttutil.PublishTimeout)
		case !done:
			keep = append(keep, pp)
		}
	}
	p.pending = keep
}
