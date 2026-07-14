package main

import (
	"encoding/xml"
	"math"
	"strings"
	"testing"

	"lexa-hub/internal/southbound/device"
	model "lexa-proto/csipmodel"
)

// fullMeasurements returns a snapshot where every quantity buildMMRs can
// encode is available. Values are chosen so scaling/rounding is observable
// (fractional V/Hz, negative VAr, fractional Wh registers).
func fullMeasurements() device.Measurements {
	return device.Measurements{
		W:          1500,
		V:          240.42,
		Hz:         60.018,
		Var:        -350.6,
		WhImpTotal: 1234567.4,
		WhExpTotal: 7654321.6,
	}
}

// nanMeasurements returns a snapshot with every encodable quantity
// unavailable — the same state main() initialises the latest map to before
// the first bus message arrives.
func nanMeasurements() device.Measurements {
	return device.Measurements{
		W: math.NaN(), V: math.NaN(), Hz: math.NaN(),
		Var: math.NaN(), WhImpTotal: math.NaN(), WhExpTotal: math.NaN(),
	}
}

// TestBuildMMRs_QuantityTable pins the full per-quantity encoding table
// (WP-5): the legacy W/V/Hz rows exactly as before, plus the VAr row
// (uom 63, kind power, multiplier 0 — BASIC-029) and the two lifetime Wh
// rows (uom 72, kind energy, flowDirection split, cumulative register
// representation: accumulationBehaviour=3, no dataQualifier/intervalLength,
// timestamped at the read instant with duration 0).
func TestBuildMMRs_QuantityTable(t *testing.T) {
	const now = int64(1_800_000_000)
	const intervalS = 60

	got := buildMMRs("inverter-0", fullMeasurements(), now, intervalS, true, true)

	want := []struct {
		mrid      string
		value     int64
		uom       uint8
		kind      uint8
		mult      int8
		qualifier uint8
		interval  uint32
		flow      uint8
		accum     uint8
		start     int64
		dur       uint32
	}{
		{"inverter-0-W", 1500, model.UomWatts, model.KindPower, 0, model.DataQualifierAverage, 60, 0, 0, now - 60, 60},
		{"inverter-0-V", 24042, model.UomVolts, model.KindVoltage, -2, model.DataQualifierAverage, 60, 0, 0, now - 60, 60},
		{"inverter-0-Hz", 6002, model.UomHertz, model.KindFreq, -2, model.DataQualifierAverage, 60, 0, 0, now - 60, 60},
		{"inverter-0-VAr", -351, uomVAr, model.KindPower, 0, model.DataQualifierAverage, 60, 0, 0, now - 60, 60},
		{"inverter-0-Wh-imp", 1234567, uomWh, kindEnergy, 0, 0, 0, flowDirectionForward, accumulationCumulative, now, 0},
		{"inverter-0-Wh-exp", 7654322, uomWh, kindEnergy, 0, 0, 0, flowDirectionReverse, accumulationCumulative, now, 0},
	}

	if len(got) != len(want) {
		t.Fatalf("buildMMRs returned %d MMRs, want %d", len(got), len(want))
	}
	for i, w := range want {
		mmr := got[i]
		if mmr.MRID != w.mrid {
			t.Errorf("[%d] MRID = %q, want %q", i, mmr.MRID, w.mrid)
		}
		rt := mmr.ReadingType
		if rt == nil {
			t.Fatalf("[%s] ReadingType is nil — every MMR must be self-describing (audit finding S-2)", w.mrid)
		}
		if rt.Uom != w.uom {
			t.Errorf("[%s] uom = %d, want %d", w.mrid, rt.Uom, w.uom)
		}
		if rt.Kind != w.kind {
			t.Errorf("[%s] kind = %d, want %d", w.mrid, rt.Kind, w.kind)
		}
		if rt.PowerOfTenMultiplier != w.mult {
			t.Errorf("[%s] powerOfTenMultiplier = %d, want %d", w.mrid, rt.PowerOfTenMultiplier, w.mult)
		}
		if rt.DataQualifier != w.qualifier {
			t.Errorf("[%s] dataQualifier = %d, want %d", w.mrid, rt.DataQualifier, w.qualifier)
		}
		if rt.IntervalLength != w.interval {
			t.Errorf("[%s] intervalLength = %d, want %d", w.mrid, rt.IntervalLength, w.interval)
		}
		if rt.FlowDirection != w.flow {
			t.Errorf("[%s] flowDirection = %d, want %d", w.mrid, rt.FlowDirection, w.flow)
		}
		if rt.AccumulationBehaviour != w.accum {
			t.Errorf("[%s] accumulationBehaviour = %d, want %d", w.mrid, rt.AccumulationBehaviour, w.accum)
		}
		if len(mmr.MirrorReadingSet) != 1 || len(mmr.MirrorReadingSet[0].Reading) != 1 {
			t.Fatalf("[%s] want exactly 1 MirrorReadingSet with 1 Reading", w.mrid)
		}
		rs := mmr.MirrorReadingSet[0]
		if rs.StartTime != w.start || rs.Duration != w.dur {
			t.Errorf("[%s] reading set time = (%d,%d), want (%d,%d)", w.mrid, rs.StartTime, rs.Duration, w.start, w.dur)
		}
		r := rs.Reading[0]
		if r.Value != w.value {
			t.Errorf("[%s] value = %d, want %d", w.mrid, r.Value, w.value)
		}
		if r.TimePeriod == nil {
			t.Fatalf("[%s] Reading.TimePeriod is nil — every measurement SHALL carry a timestamp (G25)", w.mrid)
		}
		if r.TimePeriod.Start != w.start || r.TimePeriod.Duration != w.dur {
			t.Errorf("[%s] reading time = (%d,%d), want (%d,%d)", w.mrid, r.TimePeriod.Start, r.TimePeriod.Duration, w.start, w.dur)
		}
	}
}

// TestBuildMMRs_MRIDDiscipline pins mRID stability: the same device+quantity
// must yield the identical mRID on every tick (so the server updates one
// reading series instead of accumulating duplicates), and different devices
// or quantities must never collide.
func TestBuildMMRs_MRIDDiscipline(t *testing.T) {
	first := buildMMRs("battery-0", fullMeasurements(), 1_800_000_000, 60, true, true)
	m2 := fullMeasurements()
	m2.W, m2.Var, m2.WhImpTotal = 42, 17, 9_999_999 // values change; identity must not
	second := buildMMRs("battery-0", m2, 1_800_000_300, 60, true, true)

	if len(first) != len(second) {
		t.Fatalf("row count changed across ticks: %d vs %d", len(first), len(second))
	}
	seen := make(map[string]bool)
	for i := range first {
		if first[i].MRID != second[i].MRID {
			t.Errorf("mRID not stable across ticks: %q vs %q", first[i].MRID, second[i].MRID)
		}
		if seen[first[i].MRID] {
			t.Errorf("duplicate mRID within one tick: %q", first[i].MRID)
		}
		seen[first[i].MRID] = true
	}

	other := buildMMRs("meter-0", fullMeasurements(), 1_800_000_000, 60, true, true)
	for i := range other {
		if seen[other[i].MRID] {
			t.Errorf("mRID %q collides across devices", other[i].MRID)
		}
	}
}

// TestBuildMMRs_NaNSkip pins G27 ("never fabricate unavailable data") for the
// new rows: a quantity the device has not reported (NaN in the snapshot —
// nil on the bus never overwrites the NaN init) is omitted entirely, and each
// Wh register skips independently.
func TestBuildMMRs_NaNSkip(t *testing.T) {
	t.Run("all unavailable posts nothing", func(t *testing.T) {
		if got := buildMMRs("inverter-0", nanMeasurements(), 1_800_000_000, 60, true, true); len(got) != 0 {
			t.Fatalf("got %d MMRs from an all-NaN snapshot, want 0", len(got))
		}
	})

	t.Run("NaN VAr skips only the VAr row", func(t *testing.T) {
		m := fullMeasurements()
		m.Var = math.NaN()
		got := buildMMRs("inverter-0", m, 1_800_000_000, 60, true, true)
		if len(got) != 5 {
			t.Fatalf("got %d MMRs, want 5", len(got))
		}
		for _, mmr := range got {
			if mmr.MRID == "inverter-0-VAr" {
				t.Fatal("VAr row present despite NaN reactive power (G27 violation)")
			}
		}
	})

	t.Run("Wh registers skip independently", func(t *testing.T) {
		m := fullMeasurements()
		m.WhImpTotal = math.NaN()
		got := buildMMRs("meter-0", m, 1_800_000_000, 60, true, true)
		var haveImp, haveExp bool
		for _, mmr := range got {
			switch mmr.MRID {
			case "meter-0-Wh-imp":
				haveImp = true
			case "meter-0-Wh-exp":
				haveExp = true
			}
		}
		if haveImp {
			t.Error("Wh-imp row present despite NaN import register (G27 violation)")
		}
		if !haveExp {
			t.Error("Wh-exp row missing — one NaN register must not suppress the other")
		}
	})
}

// TestBuildMMRs_ConfigFlags pins the post_var/post_wh gates: a disabled
// quantity is omitted even when the measurement is available, and disabling
// both restores the exact pre-WP-5 W/V/Hz row set.
func TestBuildMMRs_ConfigFlags(t *testing.T) {
	m := fullMeasurements()

	t.Run("post_var false omits VAr only", func(t *testing.T) {
		got := buildMMRs("inverter-0", m, 1_800_000_000, 60, false, true)
		if len(got) != 5 {
			t.Fatalf("got %d MMRs, want 5", len(got))
		}
		for _, mmr := range got {
			if mmr.MRID == "inverter-0-VAr" {
				t.Fatal("VAr row present with post_var=false")
			}
		}
	})

	t.Run("post_wh false omits both Wh rows", func(t *testing.T) {
		got := buildMMRs("inverter-0", m, 1_800_000_000, 60, true, false)
		if len(got) != 4 {
			t.Fatalf("got %d MMRs, want 4", len(got))
		}
		for _, mmr := range got {
			if strings.HasPrefix(mmr.MRID, "inverter-0-Wh") {
				t.Fatalf("%s row present with post_wh=false", mmr.MRID)
			}
		}
	})

	t.Run("both false is the legacy W/V/Hz set", func(t *testing.T) {
		got := buildMMRs("inverter-0", m, 1_800_000_000, 60, false, false)
		if len(got) != 3 {
			t.Fatalf("got %d MMRs, want 3", len(got))
		}
		for i, suffix := range []string{"W", "V", "Hz"} {
			if want := "inverter-0-" + suffix; got[i].MRID != want {
				t.Errorf("[%d] MRID = %q, want %q", i, got[i].MRID, want)
			}
		}
	})
}

// TestBuildMMRs_WireEncoding pins the serialized XML shape of one cumulative
// Wh row and one legacy instantaneous row: the cumulative ReadingType carries
// accumulationBehaviour/flowDirection and omits dataQualifier/intervalLength
// (0 ⇒ omitempty), with the reading stamped at the read instant (duration 0);
// the legacy row's wire shape is unchanged by WP-5.
func TestBuildMMRs_WireEncoding(t *testing.T) {
	const now = int64(1_800_000_000)
	got := buildMMRs("meter-0", fullMeasurements(), now, 60, true, true)

	byMRID := make(map[string]model.MirrorMeterReading, len(got))
	for _, mmr := range got {
		byMRID[mmr.MRID] = mmr
	}

	t.Run("cumulative Wh row", func(t *testing.T) {
		mmr, ok := byMRID["meter-0-Wh-imp"]
		if !ok {
			t.Fatal("Wh-imp row missing")
		}
		body, err := xml.Marshal(&mmr)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(body)
		for _, want := range []string{
			"<accumulationBehaviour>3</accumulationBehaviour>",
			"<flowDirection>1</flowDirection>",
			"<kind>12</kind>",
			"<uom>72</uom>",
			"<start>1800000000</start>",
			"<duration>0</duration>",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("Wh-imp XML missing %s\nxml: %s", want, s)
			}
		}
		for _, reject := range []string{"<dataQualifier>", "<intervalLength>"} {
			if strings.Contains(s, reject) {
				t.Errorf("Wh-imp XML contains %s — a register snapshot has no interval/qualifier\nxml: %s", reject, s)
			}
		}
	})

	t.Run("legacy W row unchanged", func(t *testing.T) {
		mmr, ok := byMRID["meter-0-W"]
		if !ok {
			t.Fatal("W row missing")
		}
		body, err := xml.Marshal(&mmr)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(body)
		for _, want := range []string{
			"<dataQualifier>2</dataQualifier>",
			"<intervalLength>60</intervalLength>",
			"<uom>38</uom>",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("W XML missing %s\nxml: %s", want, s)
			}
		}
		for _, reject := range []string{"<accumulationBehaviour>", "<flowDirection>"} {
			if strings.Contains(s, reject) {
				t.Errorf("W XML contains %s — WP-5 must not change the legacy rows\nxml: %s", reject, s)
			}
		}
	})
}
