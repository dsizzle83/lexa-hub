package main

// WP-6 alarm-edge detector tests: transition tables (bit set → alarm code,
// clear → RTN), mapping-pair completeness, multi-bit simultaneous
// transitions, the logevent_min_interval_s rate floor, breach-episode
// EMERGENCY_REMOTE pairing, and — pinned deliberately — the
// baseline-after-restart semantics: measurements are QoS 0 / non-retained,
// so a detector's FIRST observation per device is current state, never a
// transition (no events), and only transitions after that baseline emit.

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

func measWithAlarms(device string, bits uint32) bus.Measurement {
	b := bits
	return bus.Measurement{Device: device, AlarmBits: &b, Ts: time.Now().Unix()}
}

func codesOf(evs []bus.LogEventMsg) []uint8 {
	out := make([]uint8, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.LogEventCode)
	}
	return out
}

// TestLogEventDetector_SetThenClear is the core transition table: from a
// zero baseline, a mapped bit setting emits the even alarm code and its
// clearing emits the paired odd RTN.
func TestLogEventDetector_SetThenClear(t *testing.T) {
	d := newLogEventDetector(10 * time.Second)
	t0 := time.Unix(1750000000, 0)

	if evs := d.OnMeasurement(measWithAlarms("inv0", 0), t0); len(evs) != 0 {
		t.Fatalf("baseline observation emitted %v", evs)
	}

	evs := d.OnMeasurement(measWithAlarms("inv0", alrm701OverFrequency), t0.Add(time.Second))
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventDEROverFrequency || !evs[0].Alarm {
		t.Fatalf("set OVER_FREQUENCY: got %+v, want one alarm code %d", evs, bus.LogEventDEROverFrequency)
	}
	if evs[0].Device != "inv0" || evs[0].FunctionSet != bus.LogEventFunctionSetDER ||
		evs[0].V != bus.LogEventV || evs[0].DedupeKey == "" {
		t.Fatalf("event shape wrong: %+v", evs[0])
	}

	evs = d.OnMeasurement(measWithAlarms("inv0", 0), t0.Add(20*time.Second))
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventRTN(bus.LogEventDEROverFrequency) || evs[0].Alarm {
		t.Fatalf("clear OVER_FREQUENCY: got %+v, want one RTN code %d", evs, bus.LogEventRTN(bus.LogEventDEROverFrequency))
	}
}

// TestLogEventDetector_TransitionTable sweeps every mapped bit through
// set→clear and checks the exact Table 14 pair.
func TestLogEventDetector_TransitionTable(t *testing.T) {
	cases := []struct {
		name  string
		bit   uint32
		alarm uint8
	}{
		{"MANUAL_SHUTDOWN→EMERGENCY_LOCAL", alrm701ManualShutdown, bus.LogEventDEREmergencyLocal},
		{"OVER_FREQUENCY", alrm701OverFrequency, bus.LogEventDEROverFrequency},
		{"UNDER_FREQUENCY", alrm701UnderFrequency, bus.LogEventDERUnderFrequency},
		{"AC_OVER_VOLT→OVER_VOLTAGE", alrm701ACOverVolt, bus.LogEventDEROverVoltage},
		{"AC_UNDER_VOLT→UNDER_VOLTAGE", alrm701ACUnderVolt, bus.LogEventDERUnderVoltage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newLogEventDetector(time.Second)
			t0 := time.Unix(1750000000, 0)
			d.OnMeasurement(measWithAlarms("dev", 0), t0)

			evs := d.OnMeasurement(measWithAlarms("dev", tc.bit), t0.Add(2*time.Second))
			if len(evs) != 1 || evs[0].LogEventCode != tc.alarm || !evs[0].Alarm {
				t.Fatalf("set: got %+v, want alarm %d", evs, tc.alarm)
			}
			evs = d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(4*time.Second))
			if len(evs) != 1 || evs[0].LogEventCode != tc.alarm+1 || evs[0].Alarm {
				t.Fatalf("clear: got %+v, want RTN %d", evs, tc.alarm+1)
			}
		})
	}
}

// TestLogEventDetector_PairCompleteness pins the mapping table's internal
// consistency: every mapped code is a valid even Table 14 alarm whose RTN is
// also valid, and the ordered iteration list covers exactly the map's keys.
func TestLogEventDetector_PairCompleteness(t *testing.T) {
	if len(alrm701MappedBits) != len(alrm701ToTable14) {
		t.Fatalf("ordered bit list (%d) and map (%d) disagree", len(alrm701MappedBits), len(alrm701ToTable14))
	}
	for _, mask := range alrm701MappedBits {
		code, ok := alrm701ToTable14[mask]
		if !ok {
			t.Fatalf("bit %#x in ordered list but not the map", mask)
		}
		if code%2 != 0 {
			t.Errorf("bit %#x maps to odd code %d (alarm codes are even)", mask, code)
		}
		if !bus.LogEventCodeValid(code) || !bus.LogEventCodeValid(bus.LogEventRTN(code)) {
			t.Errorf("bit %#x maps outside Table 14: %d/%d", mask, code, bus.LogEventRTN(code))
		}
		if alrm701MappedMask&mask == 0 {
			t.Errorf("bit %#x missing from alrm701MappedMask", mask)
		}
	}
}

// TestLogEventDetector_MultiBitSimultaneous: two mapped bits setting in the
// same measurement emit two events, in deterministic ascending-bit order.
func TestLogEventDetector_MultiBitSimultaneous(t *testing.T) {
	d := newLogEventDetector(time.Second)
	t0 := time.Unix(1750000000, 0)
	d.OnMeasurement(measWithAlarms("dev", 0), t0)

	evs := d.OnMeasurement(measWithAlarms("dev", alrm701ManualShutdown|alrm701ACUnderVolt), t0.Add(2*time.Second))
	want := []uint8{bus.LogEventDEREmergencyLocal, bus.LogEventDERUnderVoltage}
	got := codesOf(evs)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("simultaneous set: got %v, want %v", got, want)
	}
	if evs[0].LogEventID == evs[1].LogEventID {
		t.Fatalf("LogEventID not disambiguated: %d == %d", evs[0].LogEventID, evs[1].LogEventID)
	}
	if evs[0].DedupeKey == evs[1].DedupeKey {
		t.Fatalf("dedupe keys collide: %q", evs[0].DedupeKey)
	}

	// Both clearing together emits both RTNs.
	evs = d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(4*time.Second))
	want = []uint8{bus.LogEventRTN(bus.LogEventDEREmergencyLocal), bus.LogEventRTN(bus.LogEventDERUnderVoltage)}
	got = codesOf(evs)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("simultaneous clear: got %v, want %v", got, want)
	}
}

// TestLogEventDetector_UnmappedBitsEmitNothing: the deliberately-unmapped
// 701 bits (equipment health, connection state, vendor-defined) never
// produce a LogEvent, in either direction.
func TestLogEventDetector_UnmappedBitsEmitNothing(t *testing.T) {
	unmapped := alrm701GroundFault | alrm701DCOverVolt | alrm701ACDisconnect |
		alrm701DCDisconnect | alrm701GridDisconnect | alrm701CabinetOpen |
		alrm701OverTemp | alrm701BlownStringFuse | alrm701UnderTemp |
		alrm701MemoryLoss | alrm701HWTestFailure | alrm701ManufacturerAlm

	d := newLogEventDetector(time.Second)
	t0 := time.Unix(1750000000, 0)
	d.OnMeasurement(measWithAlarms("dev", 0), t0)
	if evs := d.OnMeasurement(measWithAlarms("dev", unmapped), t0.Add(2*time.Second)); len(evs) != 0 {
		t.Fatalf("unmapped bits set emitted %v", evs)
	}
	if evs := d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(4*time.Second)); len(evs) != 0 {
		t.Fatalf("unmapped bits clear emitted %v", evs)
	}
}

// TestLogEventDetector_NilAlarmBitsIgnored: a measurement without the 701
// Alrm field (meter, legacy 10x) neither emits nor seeds a baseline.
func TestLogEventDetector_NilAlarmBitsIgnored(t *testing.T) {
	d := newLogEventDetector(time.Second)
	t0 := time.Unix(1750000000, 0)
	if evs := d.OnMeasurement(bus.Measurement{Device: "meter"}, t0); len(evs) != 0 {
		t.Fatalf("nil alarm_bits emitted %v", evs)
	}
	// The device was NOT seeded by the nil-bits message: this next
	// observation is the baseline (no events), even with a bit set.
	if evs := d.OnMeasurement(measWithAlarms("meter", alrm701ACOverVolt), t0.Add(time.Second)); len(evs) != 0 {
		t.Fatalf("first real observation emitted %v, want baseline seed", evs)
	}
}

// TestLogEventDetector_RateFloor: a transition inside logevent_min_interval_s
// of that bit's last emission is suppressed WITHOUT losing it — the reported
// state stays un-flipped, so the first measurement past the floor emits the
// (still-pending) transition and the alarm/RTN pair completes late, never
// gets lost.
func TestLogEventDetector_RateFloor(t *testing.T) {
	d := newLogEventDetector(10 * time.Second)
	t0 := time.Unix(1750000000, 0)
	d.OnMeasurement(measWithAlarms("dev", 0), t0)

	if evs := d.OnMeasurement(measWithAlarms("dev", alrm701ACOverVolt), t0.Add(time.Second)); len(evs) != 1 {
		t.Fatalf("initial set: got %v", evs)
	}
	// Clear 3s later — inside the floor: suppressed.
	if evs := d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(4*time.Second)); len(evs) != 0 {
		t.Fatalf("clear inside floor emitted %v", evs)
	}
	// Still clear 5s later — still inside the floor: still suppressed.
	if evs := d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(6*time.Second)); len(evs) != 0 {
		t.Fatalf("clear still inside floor emitted %v", evs)
	}
	// First measurement past the floor: the pending RTN emits.
	evs := d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(12*time.Second))
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventRTN(bus.LogEventDEROverVoltage) {
		t.Fatalf("clear past floor: got %v, want RTN %d", evs, bus.LogEventRTN(bus.LogEventDEROverVoltage))
	}
}

// TestLogEventDetector_FlapInsideFloorNetsToNothing: set→clear→set inside
// the floor produces exactly the one initial alarm — the flap is absorbed
// and no orphan RTN or duplicate alarm ever emits.
func TestLogEventDetector_FlapInsideFloorNetsToNothing(t *testing.T) {
	d := newLogEventDetector(10 * time.Second)
	t0 := time.Unix(1750000000, 0)
	d.OnMeasurement(measWithAlarms("dev", 0), t0)

	if evs := d.OnMeasurement(measWithAlarms("dev", alrm701UnderFrequency), t0.Add(time.Second)); len(evs) != 1 {
		t.Fatalf("initial set: got %v", evs)
	}
	if evs := d.OnMeasurement(measWithAlarms("dev", 0), t0.Add(3*time.Second)); len(evs) != 0 {
		t.Fatalf("flap clear emitted %v", evs)
	}
	if evs := d.OnMeasurement(measWithAlarms("dev", alrm701UnderFrequency), t0.Add(5*time.Second)); len(evs) != 0 {
		t.Fatalf("flap re-set emitted %v", evs)
	}
	// Past the floor, observed == reported (both set): nothing pending.
	if evs := d.OnMeasurement(measWithAlarms("dev", alrm701UnderFrequency), t0.Add(15*time.Second)); len(evs) != 0 {
		t.Fatalf("post-flap steady state emitted %v", evs)
	}
}

// TestLogEventDetector_RestartReseed pins the restart contract: measurements
// are QoS 0 / non-retained, so a fresh detector (hub restart) adopts the
// device's CURRENT alarm state silently — no events — and a bit that was SET
// at seed and later clears emits the RTN (the previous incarnation's posted
// alarm's only chance at its pair).
func TestLogEventDetector_RestartReseed(t *testing.T) {
	// Previous incarnation: alarm emitted.
	d1 := newLogEventDetector(time.Second)
	t0 := time.Unix(1750000000, 0)
	d1.OnMeasurement(measWithAlarms("dev", 0), t0)
	if evs := d1.OnMeasurement(measWithAlarms("dev", alrm701ACUnderVolt), t0.Add(2*time.Second)); len(evs) != 1 {
		t.Fatalf("pre-restart set: got %v", evs)
	}

	// "Restart": a brand-new detector sees the still-set bit first.
	d2 := newLogEventDetector(time.Second)
	if evs := d2.OnMeasurement(measWithAlarms("dev", alrm701ACUnderVolt), t0.Add(10*time.Second)); len(evs) != 0 {
		t.Fatalf("first observation after restart emitted %v, want baseline (no events)", evs)
	}
	// The clear after the re-seed IS a real observed transition: RTN emits.
	evs := d2.OnMeasurement(measWithAlarms("dev", 0), t0.Add(20*time.Second))
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventRTN(bus.LogEventDERUnderVoltage) {
		t.Fatalf("clear after re-seed: got %v, want RTN %d", evs, bus.LogEventRTN(bus.LogEventDERUnderVoltage))
	}
}

// TestLogEventDetector_BreachPair: a breach-episode onset emits the
// EMERGENCY_REMOTE alarm on the "site" device, a mRID-switch re-alert (a
// second Active edge with no intervening clear) is absorbed, and the clear
// emits the paired RTN keyed to the episode.
func TestLogEventDetector_BreachPair(t *testing.T) {
	d := newLogEventDetector(10 * time.Second)
	t0 := time.Unix(1750000000, 0)

	onset := bus.ComplianceAlert{MRID: "M1", Active: true, EpisodeID: "M1@1#1"}
	evs := d.OnBreachAlert(onset, t0)
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventDEREmergencyRemote || !evs[0].Alarm {
		t.Fatalf("onset: got %+v, want EMERGENCY_REMOTE alarm", evs)
	}
	if evs[0].Device != logEventSiteDevice || evs[0].DedupeKey != "M1@1#1/alarm" {
		t.Fatalf("onset shape wrong: %+v", evs[0])
	}

	// mRID switch: new episode Active with no clear — condition continuous,
	// no second unpaired alarm.
	swi := bus.ComplianceAlert{MRID: "M2", Active: true, EpisodeID: "M2@2#2"}
	if evs := d.OnBreachAlert(swi, t0.Add(time.Second)); len(evs) != 0 {
		t.Fatalf("mRID-switch re-alert emitted %v", evs)
	}

	clear := bus.ComplianceAlert{MRID: "M2", Active: false, EpisodeID: "M2@2#2"}
	evs = d.OnBreachAlert(clear, t0.Add(2*time.Second))
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventRTN(bus.LogEventDEREmergencyRemote) || evs[0].Alarm {
		t.Fatalf("clear: got %+v, want EMERGENCY_REMOTE RTN", evs)
	}
	if evs[0].DedupeKey != "M2@2#2/rtn" {
		t.Fatalf("clear dedupe key = %q", evs[0].DedupeKey)
	}

	// Note the rate floor does NOT apply to episode edges (10 s floor, edges
	// 1–2 s apart above): breachEpisodes is already the debounce, and a
	// suppressed RTN here would have no re-check path to flush it.
}

// TestLogEventDetector_BreachClearWithoutOnsetEmitsRTN: a clear edge on a
// fresh detector (hub restarted mid-episode; the alarm was posted by the
// previous incarnation) still emits the RTN, completing the orphaned pair.
func TestLogEventDetector_BreachClearWithoutOnsetEmitsRTN(t *testing.T) {
	d := newLogEventDetector(time.Second)
	clear := bus.ComplianceAlert{MRID: "M1", Active: false, EpisodeID: "M1@1#1"}
	evs := d.OnBreachAlert(clear, time.Unix(1750000000, 0))
	if len(evs) != 1 || evs[0].LogEventCode != bus.LogEventRTN(bus.LogEventDEREmergencyRemote) {
		t.Fatalf("restart-orphan clear: got %v, want EMERGENCY_REMOTE RTN", evs)
	}
}

// TestLogEventDetector_BreachFallbackKeyUsesMRID: a pre-TASK-031 publisher
// shape (no episode ID) keys the dedupe on the mRID, same fallback as the
// responses tracker.
func TestLogEventDetector_BreachFallbackKeyUsesMRID(t *testing.T) {
	d := newLogEventDetector(time.Second)
	onset := bus.ComplianceAlert{MRID: "M9", Active: true}
	evs := d.OnBreachAlert(onset, time.Unix(1750000000, 0))
	if len(evs) != 1 || evs[0].DedupeKey != "M9/alarm" {
		t.Fatalf("fallback key: got %+v, want M9/alarm", evs)
	}
}
