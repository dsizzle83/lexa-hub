package main

// WP-10 tests: the advanced-DER reconciler shell, table/fake-driver style
// mirroring reconcile_shell_test.go. The fake driver is a faithful (or
// deliberately unfaithful) register store: writes land in its curve fields,
// reads return them, and every call is appended to an op log so tests assert
// WHAT was touched and in what order.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/reconcile"
	"lexa-hub/internal/southbound/device"
	"lexa-proto/sunspec"
)

// ── Fake driver ──────────────────────────────────────────────────────────────

type fakeAdvDriver struct {
	caps    advCaps
	capsErr error

	vv   sunspec.VoltVarCurve
	vw   sunspec.VoltWattCurve
	wv   sunspec.WattVarCurve
	vtLV sunspec.VoltageTripSet
	vtHV sunspec.VoltageTripSet
	ftLF sunspec.FreqTripSet
	ftHF sunspec.FreqTripSet
	dr   sunspec.FreqDroopCtl

	// unfaithful: writes are ACKed but the stored state is left unchanged
	// (the accept-but-ignore device class).
	unfaithful bool
	// wErr, when non-nil, fails every curve/PF/var/ES write.
	wErr error

	enabled map[string]bool // per-axis function enable (7xx)
	ena12x  map[string]bool

	ops []string
}

func newFakeAdvDriver(caps advCaps) *fakeAdvDriver {
	return &fakeAdvDriver{caps: caps, enabled: map[string]bool{}, ena12x: map[string]bool{}}
}

func (d *fakeAdvDriver) op(format string, args ...any) {
	d.ops = append(d.ops, fmt.Sprintf(format, args...))
}

func (d *fakeAdvDriver) countOps(prefix string) int {
	n := 0
	for _, o := range d.ops {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

func (d *fakeAdvDriver) Caps() (advCaps, error) { d.op("Caps"); return d.caps, d.capsErr }

func (d *fakeAdvDriver) ReadVoltVar() (sunspec.VoltVarCurve, error) {
	d.op("ReadVoltVar")
	return d.vv, nil
}
func (d *fakeAdvDriver) WriteVoltVar(c sunspec.VoltVarCurve) error {
	d.op("WriteVoltVar")
	if d.wErr != nil {
		return d.wErr
	}
	if !d.unfaithful {
		d.vv = c
		d.enabled[bus.AdvAxisVoltVar] = true
	}
	return nil
}
func (d *fakeAdvDriver) ReadVoltWatt() (sunspec.VoltWattCurve, error) {
	d.op("ReadVoltWatt")
	return d.vw, nil
}
func (d *fakeAdvDriver) WriteVoltWatt(c sunspec.VoltWattCurve) error {
	d.op("WriteVoltWatt")
	if d.wErr != nil {
		return d.wErr
	}
	if !d.unfaithful {
		d.vw = c
		d.enabled[bus.AdvAxisVoltWatt] = true
	}
	return nil
}
func (d *fakeAdvDriver) ReadWattVar() (sunspec.WattVarCurve, error) {
	d.op("ReadWattVar")
	return d.wv, nil
}
func (d *fakeAdvDriver) WriteWattVar(c sunspec.WattVarCurve) error {
	d.op("WriteWattVar")
	if d.wErr != nil {
		return d.wErr
	}
	if !d.unfaithful {
		d.wv = c
		d.enabled[bus.AdvAxisWattVar] = true
	}
	return nil
}
func (d *fakeAdvDriver) ReadVoltageTrip(hv bool) (sunspec.VoltageTripSet, error) {
	d.op("ReadVoltageTrip(hv=%t)", hv)
	if hv {
		return d.vtHV, nil
	}
	return d.vtLV, nil
}
func (d *fakeAdvDriver) WriteVoltageTrip(hv bool, s sunspec.VoltageTripSet) error {
	d.op("WriteVoltageTrip(hv=%t)", hv)
	if d.wErr != nil {
		return d.wErr
	}
	if !d.unfaithful {
		if hv {
			d.vtHV = s
		} else {
			d.vtLV = s
		}
	}
	return nil
}
func (d *fakeAdvDriver) ReadFreqTrip(hf bool) (sunspec.FreqTripSet, error) {
	d.op("ReadFreqTrip(hf=%t)", hf)
	if hf {
		return d.ftHF, nil
	}
	return d.ftLF, nil
}
func (d *fakeAdvDriver) WriteFreqTrip(hf bool, s sunspec.FreqTripSet) error {
	d.op("WriteFreqTrip(hf=%t)", hf)
	if d.wErr != nil {
		return d.wErr
	}
	if !d.unfaithful {
		if hf {
			d.ftHF = s
		} else {
			d.ftLF = s
		}
	}
	return nil
}
func (d *fakeAdvDriver) ReadFreqDroop() (sunspec.FreqDroopCtl, error) {
	d.op("ReadFreqDroop")
	return d.dr, nil
}
func (d *fakeAdvDriver) WriteFreqDroop(c sunspec.FreqDroopCtl) error {
	d.op("WriteFreqDroop")
	if d.wErr != nil {
		return d.wErr
	}
	if !d.unfaithful {
		d.dr = c
	}
	return nil
}
func (d *fakeAdvDriver) SetFixedPF(rvrtTms uint32, pf float64, overExcited bool) error {
	d.op("SetFixedPF(rvrt=%d,pf=%.3f,over=%t)", rvrtTms, pf, overExcited)
	if d.wErr != nil {
		return d.wErr
	}
	d.enabled[bus.AdvAxisFixedPF] = true
	return nil
}
func (d *fakeAdvDriver) SetFixedVarPct(rvrtTms uint32, pct float64) error {
	d.op("SetFixedVarPct(rvrt=%d,pct=%g)", rvrtTms, pct)
	if d.wErr != nil {
		return d.wErr
	}
	d.enabled[bus.AdvAxisFixedVar] = true
	return nil
}
func (d *fakeAdvDriver) SetEnergize(on bool) error {
	d.op("SetEnergize(%t)", on)
	return d.wErr
}
func (d *fakeAdvDriver) ReadFuncEnabled(axis string) (bool, error) {
	d.op("ReadFuncEnabled(%s)", axis)
	return d.enabled[axis], nil
}
func (d *fakeAdvDriver) DisableFunc(axis string) error {
	d.op("DisableFunc(%s)", axis)
	d.enabled[axis] = false
	return nil
}
func (d *fakeAdvDriver) SetFixedPF12x(pf float64, overExcited bool) error {
	d.op("SetFixedPF12x(pf=%.3f,over=%t)", pf, overExcited)
	if d.wErr != nil {
		return d.wErr
	}
	d.ena12x[bus.AdvAxisFixedPF] = true
	return nil
}
func (d *fakeAdvDriver) SetFixedVarPct12x(pct float64) error {
	d.op("SetFixedVarPct12x(pct=%g)", pct)
	if d.wErr != nil {
		return d.wErr
	}
	d.ena12x[bus.AdvAxisFixedVar] = true
	return nil
}
func (d *fakeAdvDriver) Read12xEnabled(axis string) (bool, error) {
	d.op("Read12xEnabled(%s)", axis)
	return d.ena12x[axis], nil
}
func (d *fakeAdvDriver) Disable12x(axis string) error {
	d.op("Disable12x(%s)", axis)
	d.ena12x[axis] = false
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func fullCaps() advCaps {
	return advCaps{
		Has703: true, Has704: true, Has705: true, Has706: true,
		Has707: true, Has708: true, Has709: true, Has710: true,
		Has711: true, Has712: true, Has123: true,
		VarRatingVar: 5000,
	}
}

// mkCurve builds an AdvCurve with the canonical hash the hub author stamps
// (CurveSetContentHash over the single entry) — the exact recomputation the
// shell must reproduce from a readback.
func mkCurve(mode string, curveType uint16, xm, ym int8, pts []bus.CurvePoint) *bus.AdvCurve {
	e := bus.CurveSetEntry{Mode: mode, CurveType: curveType, XMult: xm, YMult: ym, Points: pts}
	return &bus.AdvCurve{
		CurveType: curveType, XMult: xm, YMult: ym, Points: pts,
		Hash: bus.CurveSetContentHash([]bus.CurveSetEntry{e}),
	}
}

func advTestDoc(seq uint64, at time.Time, mut func(*bus.DesiredAdvanced)) bus.DesiredAdvanced {
	doc := bus.DesiredAdvanced{
		Envelope:    bus.Envelope{V: bus.DesiredAdvancedV},
		DeviceClass: bus.DesiredClassSolar,
		DeviceID:    "inv-0",
		Source:      "csip-event",
		MRID:        "mrid-1",
		IssuedAt:    at.Unix(),
		Seq:         seq,
	}
	if mut != nil {
		mut(&doc)
	}
	return doc
}

// testShell builds an active shell with a report recorder attached.
func testShell(t *testing.T, derGen string, drv advDriver, gate interlockGate) (*advShell, *metrics.Registry, *[]bus.ReconcileReport) {
	t.Helper()
	mreg := metrics.New()
	cfg := reconcile.Config{RetryBackoff: []time.Duration{15 * time.Second, 30 * time.Second}}
	s := newAdvShell("inv-0", derGen, cfg, mreg, modeActive, drv, gate)
	var reports []bus.ReconcileReport
	s.pub = func(r bus.ReconcileReport) { reports = append(reports, r) }
	return s, mreg, &reports
}

func lastReportFor(reports []bus.ReconcileReport, axis string) (bus.ReconcileReport, bool) {
	for i := len(reports) - 1; i >= 0; i-- {
		if reports[i].Axis == axis {
			return reports[i], true
		}
	}
	return bus.ReconcileReport{}, false
}

func meas(mut func(*device.Measurements)) device.Measurements {
	m := device.Measurements{
		W: math.NaN(), VA: math.NaN(), Var: math.NaN(), V: math.NaN(), Hz: math.NaN(),
		PF: math.NaN(), DCV: math.NaN(), DCW: math.NaN(), TmpCab: math.NaN(), SOC: math.NaN(),
		WhImpTotal: math.NaN(), WhExpTotal: math.NaN(),
	}
	if mut != nil {
		mut(&m)
	}
	return m
}

// ── Curve axes ───────────────────────────────────────────────────────────────

func TestAdvShell_VoltVarAdoptSuccess(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, mreg, reports := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}, {X: 12500, Y: -2500}})
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)

	if got := drv.countOps("WriteVoltVar"); got != 1 {
		t.Fatalf("expected exactly 1 WriteVoltVar (adopt handshake), got %d ops=%v", got, drv.ops)
	}
	// Re-read + compare: at least one ReadVoltVar after the write.
	if got := drv.countOps("ReadVoltVar"); got < 2 {
		t.Fatalf("expected pre-check + post-adopt readback reads, got %d", got)
	}
	rep, ok := lastReportFor(*reports, bus.AdvAxisVoltVar)
	if !ok {
		t.Fatal("no report published for volt_var")
	}
	if rep.AdoptState != bus.AdoptStateAdopted {
		t.Fatalf("adopt_state = %q, want adopted (reports %+v)", rep.AdoptState, *reports)
	}
	if rep.CurveHash != curve.Hash {
		t.Errorf("curve_hash = %q, want the readback-verified doc hash %q", rep.CurveHash, curve.Hash)
	}
	if rep.DeviceClass != "adv" || rep.MRID != "mrid-1" {
		t.Errorf("report attribution wrong: %+v", rep)
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_adv_adopts_total 1") {
		t.Errorf("expected 1 adopt, got:\n%s", mreg.Format())
	}
}

func TestAdvShell_VoltVarNoOpReAdoptSkipped(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})
	// The live curve ALREADY carries the desired content (e.g. adopted by a
	// previous process instance — the D6 no-op re-adoption case).
	drv.vv = vvFromDoc(sunspec.VoltVarCurve{}, *curve)

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)

	if got := drv.countOps("WriteVoltVar"); got != 0 {
		t.Fatalf("matching live curve must be a no-op re-adoption (0 writes), got %d", got)
	}
	if s.runners[bus.AdvAxisVoltVar].adoptState != bus.AdoptStateAdopted {
		t.Fatalf("adopt_state = %q, want adopted", s.runners[bus.AdvAxisVoltVar].adoptState)
	}
}

func TestAdvShell_VoltVarAdoptFAILED(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	drv.wErr = errors.New("inverter: model 705 adopt-curve FAILED")
	s, mreg, reports := testShell(t, "7xx", drv, nil)
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)

	rep, _ := lastReportFor(*reports, bus.AdvAxisVoltVar)
	if rep.AdoptState != bus.AdoptStateFailed {
		t.Fatalf("adopt_state = %q, want failed", rep.AdoptState)
	}
	if rep.CurveHash != "" {
		t.Errorf("failed report must not carry a verified curve_hash, got %q", rep.CurveHash)
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_adv_failed_total 1") {
		t.Errorf("expected 1 failure, got:\n%s", mreg.Format())
	}

	// Corrective re-adopt: backoff ≥ readback interval. A tick inside the
	// window must not retry; one past it must.
	writesBefore := drv.countOps("WriteVoltVar")
	s.tick(t0.Add(5 * time.Second))
	if drv.countOps("WriteVoltVar") != writesBefore {
		t.Fatalf("retry inside the 15s backoff window must be held")
	}
	drv.wErr = nil // device recovers
	s.tick(t0.Add(16 * time.Second))
	if drv.countOps("WriteVoltVar") != writesBefore+1 {
		t.Fatalf("expected a re-adopt after backoff, ops=%v", drv.ops)
	}
	if s.runners[bus.AdvAxisVoltVar].adoptState != bus.AdoptStateAdopted {
		t.Fatalf("recovered adopt should verify adopted, got %q", s.runners[bus.AdvAxisVoltVar].adoptState)
	}
}

func TestAdvShell_VoltVarReadbackMismatchDiverges(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	drv.unfaithful = true // ACKs the write, live curve never changes
	s, _, reports := testShell(t, "7xx", drv, nil)
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)

	rep, _ := lastReportFor(*reports, bus.AdvAxisVoltVar)
	if rep.AdoptState != bus.AdoptStateDiverged {
		t.Fatalf("accept-but-ignore device must read back DIVERGED, got %q", rep.AdoptState)
	}
}

// TestAdvShell_NonConvergedFeedsBreachPath: sustained curve divergence opens
// a NonConvergedBegin (the breach-episode evidence), and recovery closes it.
func TestAdvShell_NonConvergedBeginEnd(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	drv.unfaithful = true
	mreg := metrics.New()
	cfg := reconcile.Config{
		RetryBackoff:    []time.Duration{15 * time.Second, 15 * time.Second},
		ConvergeTimeout: 20 * time.Second,
	}
	s := newAdvShell("inv-0", "7xx", cfg, mreg, modeActive, drv, nil)
	var reports []bus.ReconcileReport
	s.pub = func(r bus.ReconcileReport) { reports = append(reports, r) }

	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)

	s.tick(t0.Add(25 * time.Second)) // past ConvergeTimeout: episode opens (and one retry fires)
	var begin *bus.ReconcileReport
	for i := range reports {
		if reports[i].Kind == "NonConvergedBegin" {
			begin = &reports[i]
		}
	}
	if begin == nil {
		t.Fatalf("sustained divergence must publish NonConvergedBegin, reports=%+v", reports)
	}
	if begin.Axis != bus.AdvAxisVoltVar || begin.MRID != "mrid-1" || begin.DeviceClass != "adv" {
		t.Errorf("Begin attribution wrong: %+v", *begin)
	}

	drv.unfaithful = false // device starts honoring writes
	s.tick(t0.Add(45 * time.Second))
	foundEnd := false
	for _, r := range reports {
		if r.Kind == "NonConvergedEnd" && r.Axis == bus.AdvAxisVoltVar {
			foundEnd = true
		}
	}
	if !foundEnd {
		t.Fatalf("recovery must publish NonConvergedEnd, reports=%+v", reports)
	}
}

// ── Trips + droop ────────────────────────────────────────────────────────────

func TestAdvShell_TripSetAdoptAndUncommandedSubcurve(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	must := mkCurve(bus.CurveModeLVRTMustTrip, 3, -2, 0, []bus.CurvePoint{{X: 100, Y: 50}, {X: 2000, Y: 70}})
	may := mkCurve(bus.CurveModeLVRTMayTrip, 4, -2, 0, []bus.CurvePoint{{X: 100, Y: 45}})
	setEntries := []bus.CurveSetEntry{
		{Mode: bus.CurveModeLVRTMustTrip, CurveType: 3, XMult: -2, YMult: 0, Points: must.Points},
		{Mode: bus.CurveModeLVRTMayTrip, CurveType: 4, XMult: -2, YMult: 0, Points: may.Points},
	}
	set := &bus.AdvTripSet{
		Curves: []bus.AdvTripCurve{
			{Kind: bus.AdvTripMustTrip, Curve: *must},
			{Kind: bus.AdvTripMayTrip, Curve: *may},
		},
		Hash: bus.CurveSetContentHash(setEntries),
	}
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.Trips = &bus.AdvTrips{LV: set}
	}), t0)

	if s.runners[bus.AdvAxisTripLV].adoptState != bus.AdoptStateAdopted {
		t.Fatalf("trip_lv adopt_state = %q, want adopted (ops=%v)", s.runners[bus.AdvAxisTripLV].adoptState, drv.ops)
	}
	if drv.countOps("WriteVoltageTrip(hv=false)") != 1 {
		t.Fatalf("expected one LV trip write, ops=%v", drv.ops)
	}

	// A leftover UNCOMMANDED momentary-cessation curve on the device is a
	// provisioning mismatch, not a match: the reconnect re-verify must NOT
	// no-op-skip, and (with the device now ignoring writes) must land
	// diverged rather than adopted.
	drv.unfaithful = true
	drv.vtLV.MomCess = []sunspec.TripVPoint{{V: 40, Tms: 1}}
	writesBefore := drv.countOps("WriteVoltageTrip(hv=false)")
	s.markReconnected()
	s.observe(meas(nil), true, t0.Add(30*time.Second))
	if drv.countOps("WriteVoltageTrip(hv=false)") != writesBefore+1 {
		t.Fatalf("uncommanded sub-curve must defeat the no-op skip and re-write, ops=%v", drv.ops)
	}
	if got := s.runners[bus.AdvAxisTripLV].adoptState; got != bus.AdoptStateDiverged {
		t.Fatalf("uncommanded sub-curve on readback must not verify as adopted, got %q", got)
	}
}

func TestAdvShell_FreqDroopAdopt(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.FreqDroop = &bus.AdvFreqDroop{DbOfHz: 0.036, DbUfHz: 0.036, KOf: 0.05, KUf: 0.05, OlrtS: 5}
	}), t0)

	if s.runners[bus.AdvAxisFreqDroop].adoptState != bus.AdoptStateAdopted {
		t.Fatalf("droop adopt_state = %q, want adopted (ops=%v)", s.runners[bus.AdvAxisFreqDroop].adoptState, drv.ops)
	}
	if drv.countOps("WriteFreqDroop") != 1 {
		t.Fatalf("expected one droop write, ops=%v", drv.ops)
	}
}

// ── Unsupported paths ────────────────────────────────────────────────────────

func TestAdvShell_UnsupportedAxes(t *testing.T) {
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})

	cases := []struct {
		name   string
		derGen string
		caps   advCaps
		mut    func(*bus.DesiredAdvanced)
		axis   string
	}{
		{"12x-curve", "12x", fullCaps(), func(d *bus.DesiredAdvanced) {
			d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
		}, bus.AdvAxisVoltVar},
		{"12x-energize", "12x", fullCaps(), func(d *bus.DesiredAdvanced) {
			d.Energize = ptrB(true)
		}, bus.AdvAxisEnergize},
		{"unknown-gen", "", fullCaps(), func(d *bus.DesiredAdvanced) {
			d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
		}, bus.AdvAxisVoltVar},
		{"7xx-without-705", "7xx", func() advCaps { c := fullCaps(); c.Has705 = false; return c }(), func(d *bus.DesiredAdvanced) {
			d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
		}, bus.AdvAxisVoltVar},
		{"freq-watt-never-executes", "7xx", fullCaps(), func(d *bus.DesiredAdvanced) {
			d.FreqWatt = curve
		}, bus.AdvAxisFreqWatt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drv := newFakeAdvDriver(tc.caps)
			s, mreg, reports := testShell(t, tc.derGen, drv, nil)
			s.setDesired(advTestDoc(1, t0, tc.mut), t0)
			rep, ok := lastReportFor(*reports, tc.axis)
			if !ok || rep.AdoptState != bus.AdoptStateUnsupported {
				t.Fatalf("axis %s adopt_state = %+v, want unsupported", tc.axis, rep)
			}
			// Never a hardware mutation for an unsupported axis.
			for _, prefix := range []string{"WriteVoltVar", "WriteVoltWatt", "WriteWattVar", "SetEnergize"} {
				if drv.countOps(prefix) != 0 {
					t.Errorf("unsupported axis must not write (%s), ops=%v", prefix, drv.ops)
				}
			}
			if !strings.Contains(mreg.Format(), "lexa_mb_adv_unsupported_total 1") {
				t.Errorf("expected unsupported counter 1, got:\n%s", mreg.Format())
			}
		})
	}
}

// ── Fixed PF / fixed var: measured convergence ───────────────────────────────

func TestAdvShell_FixedPFMeasuredConvergence(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, mreg, reports := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	rvrt := int64(3600)
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedPF, FixedPF: &bus.FixedPF{PF: 0.95, OverExcited: true}}
		d.RvrtTmsS = &rvrt
	}), t0)

	// The 704 write carries the C3 reversion window.
	if drv.countOps("SetFixedPF(rvrt=3600,pf=0.950,over=true)") != 1 {
		t.Fatalf("expected SetFixedPF with rvrt=3600, ops=%v", drv.ops)
	}
	rep, _ := lastReportFor(*reports, bus.AdvAxisFixedPF)
	if rep.AdoptState != bus.AdoptStatePending {
		t.Fatalf("write success is NOT convergence: adopt_state = %q, want pending", rep.AdoptState)
	}

	// Unassessable sample (|W| under the floor): hold — no verdict.
	s.observe(meas(func(m *device.Measurements) { m.W = 20; m.PF = 0.2 }), true, t0.Add(2*time.Second))
	if got := s.runners[bus.AdvAxisFixedPF].adoptState; got != bus.AdoptStatePending {
		t.Fatalf("under-W-floor sample must hold, got %q", got)
	}

	// One-sided: measured |PF| ABOVE commanded (closer to unity) is compliant.
	s.observe(meas(func(m *device.Measurements) { m.W = 2000; m.PF = 0.99 }), true, t0.Add(4*time.Second))
	if got := s.runners[bus.AdvAxisFixedPF].adoptState; got != bus.AdoptStateAdopted {
		t.Fatalf("above-commanded PF must converge (one-sided), got %q", got)
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_adv_matches_total 1") {
		t.Errorf("expected 1 match, got:\n%s", mreg.Format())
	}

	// Sustained shortfall: diverged + a corrective write (first of the episode
	// is immediate per the core's contract).
	before := drv.countOps("SetFixedPF(")
	s.observe(meas(func(m *device.Measurements) { m.W = 2000; m.PF = 0.7 }), true, t0.Add(6*time.Second))
	if got := s.runners[bus.AdvAxisFixedPF].adoptState; got != bus.AdoptStateDiverged {
		t.Fatalf("PF shortfall must diverge, got %q", got)
	}
	if drv.countOps("SetFixedPF(") != before+1 {
		t.Fatalf("diverged PF must trigger a corrective write, ops=%v", drv.ops)
	}
}

func TestAdvShell_FixedVarBandAndUnknownRating(t *testing.T) {
	t0 := time.Now()
	pct := 30.0

	// Known rating (5000 var): target 1500, band ±250.
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedVar, FixedVarPct: &pct}
	}), t0)
	if drv.countOps("SetFixedVarPct(rvrt=0,pct=30)") != 1 {
		t.Fatalf("expected SetFixedVarPct, ops=%v", drv.ops)
	}
	s.observe(meas(func(m *device.Measurements) { m.W = 2000; m.Var = 1450 }), true, t0.Add(2*time.Second))
	if got := s.runners[bus.AdvAxisFixedVar].adoptState; got != bus.AdoptStateAdopted {
		t.Fatalf("in-band var must converge, got %q", got)
	}
	s.observe(meas(func(m *device.Measurements) { m.W = 2000; m.Var = 100 }), true, t0.Add(4*time.Second))
	if got := s.runners[bus.AdvAxisFixedVar].adoptState; got != bus.AdoptStateDiverged {
		t.Fatalf("accept-but-ignore var (≈0 measured) must diverge, got %q", got)
	}

	// Unknown rating: assessment must HOLD (never a false verdict).
	caps := fullCaps()
	caps.VarRatingVar = math.NaN()
	drv2 := newFakeAdvDriver(caps)
	s2, _, _ := testShell(t, "7xx", drv2, nil)
	s2.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedVar, FixedVarPct: &pct}
	}), t0)
	s2.observe(meas(func(m *device.Measurements) { m.W = 2000; m.Var = 0 }), true, t0.Add(2*time.Second))
	if got := s2.runners[bus.AdvAxisFixedVar].adoptState; got != bus.AdoptStatePending {
		t.Fatalf("unknown var rating must hold pending, got %q", got)
	}
}

// ── Energize + interlock seniority ───────────────────────────────────────────

func TestAdvShell_EnergizeMeasuredCessation(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) { d.Energize = ptrB(false) }), t0)
	if drv.countOps("SetEnergize(false)") != 1 {
		t.Fatalf("cease-to-energize must write 703 ES, ops=%v", drv.ops)
	}
	// Still producing: diverged (measured truth beats the ACK).
	s.observe(meas(func(m *device.Measurements) { m.W = 3000 }), true, t0.Add(2*time.Second))
	if got := s.runners[bus.AdvAxisEnergize].adoptState; got != bus.AdoptStateDiverged {
		t.Fatalf("producing while ceased-commanded must diverge, got %q", got)
	}
	// Measured cessation converges.
	s.observe(meas(func(m *device.Measurements) { m.W = 10 }), true, t0.Add(4*time.Second))
	if got := s.runners[bus.AdvAxisEnergize].adoptState; got != bus.AdoptStateAdopted {
		t.Fatalf("measured cessation must converge, got %q", got)
	}
}

func TestAdvShell_EnergizeOneSidedWhenDark(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) { d.Energize = ptrB(true) }), t0)
	// Dark device (no production, no state points): one-sided — never
	// diverged (night / lawful enter-service delay is not a fault).
	s.observe(meas(func(m *device.Measurements) { m.W = 0 }), true, t0.Add(2*time.Second))
	if got := s.runners[bus.AdvAxisEnergize].adoptState; got != bus.AdoptStatePending {
		t.Fatalf("dark energize=true must hold pending, got %q", got)
	}
	// Positive evidence (ConnSt) converges.
	one := uint16(1)
	s.observe(meas(func(m *device.Measurements) { m.W = 0; m.ConnSt = &one }), true, t0.Add(4*time.Second))
	if got := s.runners[bus.AdvAxisEnergize].adoptState; got != bus.AdoptStateAdopted {
		t.Fatalf("ConnSt=1 must converge energize=true, got %q", got)
	}
}

func TestAdvShell_InterlockSuppressesEnergizeRestore(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	gate := &fakeGate{trip: map[string]bool{"inv-0": true}}
	s, mreg, _ := testShell(t, "7xx", drv, gate)
	t0 := time.Now()

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) { d.Energize = ptrB(true) }), t0)
	if drv.countOps("SetEnergize") != 0 {
		t.Fatalf("energize-restore must be suppressed while Tier-0 tripped, ops=%v", drv.ops)
	}
	if !strings.Contains(mreg.Format(), "lexa_mb_interlock_holds_total 1") {
		t.Errorf("expected 1 interlock hold, got:\n%s", mreg.Format())
	}

	// Cease-to-energize is NOT a restore: allowed through even while tripped.
	drv2 := newFakeAdvDriver(fullCaps())
	s2, _, _ := testShell(t, "7xx", drv2, gate)
	s2.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) { d.Energize = ptrB(false) }), t0)
	if drv2.countOps("SetEnergize(false)") != 1 {
		t.Fatalf("cease write must pass through a trip, ops=%v", drv2.ops)
	}

	// Trip clears → reconnect reassert executes the standing energize.
	gate.trip["inv-0"] = false
	s.markReconnected()
	s.observe(meas(nil), true, t0.Add(10*time.Second))
	if drv.countOps("SetEnergize(true)") != 1 {
		t.Fatalf("cleared trip + reconnect must reassert energize, ops=%v", drv.ops)
	}
}

// ── Legacy 12x ───────────────────────────────────────────────────────────────

// recordingModelRW is a fake register store for the M123 write-order tests.
type recordingModelRW struct {
	regs   []uint16
	writes []string // "offset=values" in call order
}

func (rw *recordingModelRW) ReadModel(modelID uint16) ([]uint16, error) {
	out := make([]uint16, len(rw.regs))
	copy(out, rw.regs)
	return out, nil
}
func (rw *recordingModelRW) WriteModel(modelID uint16, offset uint16, values []uint16) error {
	rw.writes = append(rw.writes, fmt.Sprintf("m%d@%d=%v", modelID, offset, values))
	return nil
}

func TestWriteM123FixedPF_EnableRewriteOrder(t *testing.T) {
	rw := &recordingModelRW{regs: make([]uint16, 26)}
	rw.regs[sunspec.M123_OutPFSet_SF] = uint16(0xFFFE) // SF = −2 → raw = pf×100

	if err := writeM123FixedPF(rw, 0.95, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("m123@%d=[%d]", sunspec.M123_OutPFSet, sunspec.RawFromScaleSigned(0.95, -2)),
		fmt.Sprintf("m123@%d=[1]", sunspec.M123_OutPFSet_Ena),
	}
	if !reflect.DeepEqual(rw.writes, want) {
		t.Fatalf("write order = %v, want %v (value FIRST, Ena rewrite LAST — Rule 21)", rw.writes, want)
	}

	// Under-excited: negative engineering value.
	rw.writes = nil
	if err := writeM123FixedPF(rw, 0.9, false); err != nil {
		t.Fatal(err)
	}
	if rw.writes[0] != fmt.Sprintf("m123@%d=[%d]", sunspec.M123_OutPFSet, sunspec.RawFromScaleSigned(-0.9, -2)) {
		t.Errorf("under-excited PF must encode negative, got %v", rw.writes)
	}
}

func TestWriteM123VarPct_EnableRewriteOrder(t *testing.T) {
	rw := &recordingModelRW{regs: make([]uint16, 26)}
	rw.regs[sunspec.M123_VArPct_SF] = 0 // SF = 0 → raw = pct

	if err := writeM123VarPct(rw, -25); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("m123@%d=[2]", sunspec.M123_VArPct_Mod), // % of VArMax
		fmt.Sprintf("m123@%d=[%d]", sunspec.M123_VArPct, sunspec.RawFromScaleSigned(-25, 0)),
		fmt.Sprintf("m123@%d=[1]", sunspec.M123_VArPct_Ena),
	}
	if !reflect.DeepEqual(rw.writes, want) {
		t.Fatalf("write order = %v, want %v (mode, value, Ena LAST)", rw.writes, want)
	}
}

func TestAdvShell_12xRoutesToLegacyWrites(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "12x", drv, nil)
	t0 := time.Now()

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedPF, FixedPF: &bus.FixedPF{PF: 0.9, OverExcited: false}}
	}), t0)
	if drv.countOps("SetFixedPF12x(pf=0.900,over=false)") != 1 {
		t.Fatalf("12x device must take the legacy M123 path, ops=%v", drv.ops)
	}
	if drv.countOps("SetFixedPF(") != 0 {
		t.Fatalf("12x device must never take the 704 path, ops=%v", drv.ops)
	}
}

// ── Release semantics ────────────────────────────────────────────────────────

func TestAdvShell_ReleaseDisablesFunction(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, reports := testShell(t, "7xx", drv, nil)
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})

	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)
	if !drv.enabled[bus.AdvAxisVoltVar] {
		t.Fatal("precondition: volt_var enabled after adopt")
	}

	// Release: the next doc's ReactiveMode is an explicit null.
	s.setDesired(advTestDoc(2, t0.Add(30*time.Second), nil), t0.Add(30*time.Second))
	if drv.countOps("DisableFunc(volt_var)") != 1 {
		t.Fatalf("release must disable the function, ops=%v", drv.ops)
	}
	if drv.enabled[bus.AdvAxisVoltVar] {
		t.Fatal("volt_var must be disabled after release")
	}
	rep, _ := lastReportFor(*reports, bus.AdvAxisVoltVar)
	if rep.AdoptState != "" {
		t.Fatalf("released axis adopt_state = %q, want \"\"", rep.AdoptState)
	}
}

func TestAdvShell_NullTripsAndDroopNeverStripped(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()

	// A doc with null trips/droop must never disable or write those models
	// (protective functions are not released by absence).
	s.setDesired(advTestDoc(1, t0, nil), t0)
	for _, prefix := range []string{"WriteVoltageTrip", "WriteFreqTrip", "WriteFreqDroop", "DisableFunc(freq_droop"} {
		if drv.countOps(prefix) != 0 {
			t.Errorf("null protective axis must not be touched (%s), ops=%v", prefix, drv.ops)
		}
	}
}

// ── Shadow mode ──────────────────────────────────────────────────────────────

func TestAdvShadow_ZeroWrites(t *testing.T) {
	mreg := metrics.New()
	s := newAdvShell("inv-0", "7xx", reconcile.Config{}, mreg, modeShadow, nil, nil)
	if s.driver != nil || s.pub != nil {
		t.Fatal("shadow shell must have no driver and no publisher")
	}
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})
	pct := 40.0
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveFixedVar, FixedVarPct: &pct}
		d.VoltWatt = curve
		d.Energize = ptrB(false)
	}), t0)
	// Diverging measurements: verdicts recorded, still zero writes.
	s.observe(meas(func(m *device.Measurements) { m.W = 3000; m.Var = 0 }), true, t0.Add(2*time.Second))
	s.tick(t0.Add(60 * time.Second))

	out := mreg.Format()
	if !strings.Contains(out, "lexa_mb_adv_writes_total 0") {
		t.Errorf("shadow must never write, got:\n%s", out)
	}
	if strings.Contains(out, "lexa_mb_adv_would_writes_total 0\n") {
		t.Errorf("shadow must still record would-writes, got:\n%s", out)
	}
	// The type itself: no driver field is set, so there is no path through
	// which a write could be routed — the same absence-guarantee the battery
	// shadow test pins.
}

// ── Reassert-on-reconnect + retained-doc seed ────────────────────────────────

func TestAdvShell_ReconnectReassertsProvisioning(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})

	// The retained doc IS the initial-desired seed: delivered on subscribe.
	s.setDesired(advTestDoc(1, t0, func(d *bus.DesiredAdvanced) {
		d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
	}), t0)
	if s.runners[bus.AdvAxisVoltVar].adoptState != bus.AdoptStateAdopted {
		t.Fatal("precondition: adopted")
	}

	// Device reboots to defaults (live curve wiped), transport reconnects.
	drv.vv = sunspec.VoltVarCurve{}
	writesBefore := drv.countOps("WriteVoltVar")
	s.markReconnected()
	s.observe(meas(nil), true, t0.Add(30*time.Second))
	if drv.countOps("WriteVoltVar") != writesBefore+1 {
		t.Fatalf("reconnect must re-adopt the standing curve, ops=%v", drv.ops)
	}
	if s.runners[bus.AdvAxisVoltVar].adoptState != bus.AdoptStateAdopted {
		t.Fatalf("re-adopt after reconnect should verify, got %q", s.runners[bus.AdvAxisVoltVar].adoptState)
	}

	// The reconnect signal is consumed: a second observe does not re-fire.
	writesAfter := drv.countOps("WriteVoltVar")
	s.observe(meas(nil), true, t0.Add(32*time.Second))
	if drv.countOps("WriteVoltVar") != writesAfter {
		t.Fatalf("reconnect signal must be one-shot, ops=%v", drv.ops)
	}
}

// TestAdvShell_HeartbeatDoesNotReExecute pins the D6 skip: an unchanged-
// content heartbeat republish (new seq/issuedAt, same axes) is a baseline
// refresh, not a re-adoption.
func TestAdvShell_HeartbeatDoesNotReExecute(t *testing.T) {
	drv := newFakeAdvDriver(fullCaps())
	s, _, _ := testShell(t, "7xx", drv, nil)
	t0 := time.Now()
	curve := mkCurve(bus.CurveModeVoltVar, 1, -2, 0, []bus.CurvePoint{{X: 11700, Y: 2500}})
	mk := func(seq uint64, at time.Time) bus.DesiredAdvanced {
		return advTestDoc(seq, at, func(d *bus.DesiredAdvanced) {
			d.ReactiveMode = &bus.AdvReactiveMode{Kind: bus.AdvReactiveVoltVar, Curve: curve}
		})
	}
	s.setDesired(mk(1, t0), t0)
	writes := drv.countOps("WriteVoltVar")
	s.setDesired(mk(2, t0.Add(60*time.Second)), t0.Add(60*time.Second))
	if drv.countOps("WriteVoltVar") != writes {
		t.Fatalf("heartbeat republish must not re-adopt, ops=%v", drv.ops)
	}
}

// ── Report wire round-trip ───────────────────────────────────────────────────

func TestAdvReconcileReport_RoundTrip(t *testing.T) {
	in := bus.ReconcileReport{
		Envelope:    bus.Envelope{V: bus.ReconcileReportV},
		Kind:        "AdoptState",
		DeviceClass: "adv",
		DeviceID:    "inv-0",
		MRID:        "mrid-1",
		Seq:         7,
		IssuedAt:    1700000000,
		Episode:     2,
		Ts:          1700000100,
		Axis:        bus.AdvAxisVoltVar,
		AdoptState:  bus.AdoptStateAdopted,
		CurveHash:   "abc123",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out bus.ReconcileReport
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}

	// A legacy scalar report (no adv fields) decodes with empty extension
	// fields — the additive contract.
	var legacy bus.ReconcileReport
	if err := json.Unmarshal([]byte(`{"v":1,"kind":"NonConvergedBegin","device_class":"battery","device_id":"b0","episode":1,"ts":5}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Axis != "" || legacy.AdoptState != "" || legacy.CurveHash != "" {
		t.Fatalf("legacy report must decode with empty adv fields: %+v", legacy)
	}
}
