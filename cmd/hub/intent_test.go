package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/orchestrator"
)

// fakeHubEngine records every hubEngine setter call instead of driving a real
// orchestrator.Engine control loop — the adopter's job (Unit 3.3) is calling
// the right setter with the right value, which internal/orchestrator's own
// tests (engine_intents_test.go) already verify end-to-end; this package only
// needs to observe the call.
type fakeHubEngine struct {
	mu             sync.Mutex
	evGoals        []orchestrator.EVGoal
	reservePcts    []float64
	solarForecasts []orchestrator.ExternalForecast
	loadProfiles   [][]float64
	fallbackTOUs   []*orchestrator.TOUCostModel
}

func (f *fakeHubEngine) SetEVGoal(g orchestrator.EVGoal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evGoals = append(f.evGoals, g)
}
func (f *fakeHubEngine) SetBackupReserve(pct float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reservePcts = append(f.reservePcts, pct)
}
func (f *fakeHubEngine) SetSolarForecast(fc orchestrator.ExternalForecast) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.solarForecasts = append(f.solarForecasts, fc)
}
func (f *fakeHubEngine) SetLoadProfile(stepKw []float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadProfiles = append(f.loadProfiles, stepKw)
}
func (f *fakeHubEngine) SetFallbackTOU(m *orchestrator.TOUCostModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallbackTOUs = append(f.fallbackTOUs, m)
}

func (f *fakeHubEngine) lastEVGoal() orchestrator.EVGoal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evGoals[len(f.evGoals)-1]
}
func (f *fakeHubEngine) evGoalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.evGoals)
}

// fakeCostSwapper is costModelSwapper's test double, standing in for
// *orchestrator.DefaultOptimizer.
type fakeCostSwapper struct {
	mu     sync.Mutex
	models []*orchestrator.TOUCostModel
}

func (f *fakeCostSwapper) SwapCostModel(m *orchestrator.TOUCostModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append(f.models, m)
}

// testAdopterFixture bundles an intentAdopter with its fakes for assertions.
type testAdopterFixture struct {
	adopter *intentAdopter
	eng     *fakeHubEngine
	opt     *fakeCostSwapper
	mc      *fakeHubMQTTClient
}

// newTestAdopter builds an intentAdopter wired to fakes, with one inverter
// (5000W) and one battery (8000W) device — solarMaxWTotal=5000,
// allMaxWTotal=13000, so solarforecast's ceiling is 7.5kW and loadprofile's
// is 39kW, both used by the clamp tests below. jw may be nil (journal
// disabled, matching hub.json's default) or a real *journal.Writer opened on
// a t.TempDir() when a test wants to inspect journaled events.
func newTestAdopter(t *testing.T, jw *journal.Writer) *testAdopterFixture {
	t.Helper()
	cfg := &Config{
		Devices: []DeviceConfig{
			{Name: "inv-0", Role: "inverter", MaxW: 5000},
			{Name: "batt-0", Role: "battery", MaxW: 8000},
		},
	}
	eng := &fakeHubEngine{}
	opt := &fakeCostSwapper{}
	mc := &fakeHubMQTTClient{}
	a := newIntentAdopter(eng, opt, jw, mc, cfg, nil, nil)
	// Deterministic clock for expiry/staleness math.
	fixedNow := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return fixedNow }
	return &testAdopterFixture{adopter: a, eng: eng, opt: opt, mc: mc}
}

// callLocked invokes fn while holding the adopter's mutex — mirroring the
// invariant every apply* method relies on in production (adopt() always
// holds a.mu for the whole call, including any field write an apply* method
// makes). A test that calls an apply* method directly instead of through
// adopt() must preserve that invariant itself whenever the call can schedule
// (or race with) the chargeNowRevert timer's own goroutine — otherwise the
// unprotected write below and the timer's locked write are a genuine data
// race (caught by go test -race), not just a hypothetical one.
func (f *testAdopterFixture) callLocked(fn func() (string, string)) (string, string) {
	f.adopter.mu.Lock()
	defer f.adopter.mu.Unlock()
	return fn()
}

// lastResult decodes the most recently published bus.IntentResult from the
// fake MQTT client.
func lastResult(t *testing.T, mc *fakeHubMQTTClient) bus.IntentResult {
	t.Helper()
	if len(mc.publishes) == 0 {
		t.Fatal("no IntentResult published")
	}
	last := mc.publishes[len(mc.publishes)-1]
	if last.topic != bus.TopicIntentResult {
		t.Fatalf("published topic = %q, want %q", last.topic, bus.TopicIntentResult)
	}
	if last.retained {
		t.Fatalf("IntentResult must not be retained")
	}
	var res bus.IntentResult
	if err := json.Unmarshal(last.payload, &res); err != nil {
		t.Fatalf("unmarshal IntentResult: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------
// Result publish shape
// ---------------------------------------------------------------------

func TestIntentAdopter_ResultPublish_Shape(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.EVGoalIntent{
		IntentMeta:    bus.IntentMeta{ID: "req-1", Origin: "app"},
		TargetSocKwh:  ptr(40.0),
		DepartureUnix: f.adopter.now().Add(6 * time.Hour).Unix(),
	}
	f.adopter.adopt("evgoal", msg.IntentMeta, func() (string, string) { return f.adopter.applyEVGoal(msg) })

	res := lastResult(t, f.mc)
	if res.V != bus.IntentResultV {
		t.Errorf("v = %d, want %d", res.V, bus.IntentResultV)
	}
	if res.ID != "req-1" || res.Kind != "evgoal" || res.Outcome != "applied" {
		t.Errorf("result = %+v, want id=req-1 kind=evgoal outcome=applied", res)
	}
	if res.Ts == 0 {
		t.Errorf("ts must be stamped")
	}
}

// ---------------------------------------------------------------------
// evgoal
// ---------------------------------------------------------------------

func TestIntentAdopter_EVGoal_Applied(t *testing.T) {
	f := newTestAdopter(t, nil)
	dep := f.adopter.now().Add(6 * time.Hour).Unix()
	msg := bus.EVGoalIntent{
		IntentMeta:    bus.IntentMeta{ID: "e1"},
		TargetSocKwh:  ptr(40.0),
		DepartureUnix: dep,
		InitialSocKwh: ptr(10.0),
	}
	f.adopter.adopt("evgoal", msg.IntentMeta, func() (string, string) { return f.adopter.applyEVGoal(msg) })

	if got := lastResult(t, f.mc); got.Outcome != "applied" {
		t.Fatalf("outcome = %q, want applied", got.Outcome)
	}
	got := f.eng.lastEVGoal()
	want := orchestrator.EVGoal{TargetSocKwh: 40, DepartureUnix: dep, InitialSocKwh: 10}
	if got != want {
		t.Errorf("SetEVGoal called with %+v, want %+v", got, want)
	}
	if f.adopter.standingEVGoal == nil || *f.adopter.standingEVGoal != want {
		t.Errorf("standingEVGoal = %+v, want %+v", f.adopter.standingEVGoal, want)
	}
}

func TestIntentAdopter_EVGoal_RejectNilTarget(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.EVGoalIntent{DepartureUnix: f.adopter.now().Add(time.Hour).Unix()}
	outcome, detail := f.adopter.applyEVGoal(msg)
	if outcome != "rejected" || detail == "" {
		t.Errorf("outcome/detail = %q/%q, want rejected/non-empty", outcome, detail)
	}
	if f.eng.evGoalCount() != 0 {
		t.Errorf("SetEVGoal must not be called on rejection")
	}
}

func TestIntentAdopter_EVGoal_RejectNegativeTarget(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.EVGoalIntent{TargetSocKwh: ptr(-1.0), DepartureUnix: f.adopter.now().Add(time.Hour).Unix()}
	outcome, _ := f.adopter.applyEVGoal(msg)
	if outcome != "rejected" {
		t.Errorf("outcome = %q, want rejected", outcome)
	}
}

func TestIntentAdopter_EVGoal_RejectCapacityExceeded(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.EVGoalIntent{
		TargetSocKwh:  ptr(80.0),
		CapacityKwh:   ptr(60.0),
		DepartureUnix: f.adopter.now().Add(time.Hour).Unix(),
	}
	outcome, detail := f.adopter.applyEVGoal(msg)
	if outcome != "rejected" || detail != "target exceeds stated capacity" {
		t.Errorf("outcome/detail = %q/%q, want rejected/%q", outcome, detail, "target exceeds stated capacity")
	}
}

func TestIntentAdopter_EVGoal_CapacityWithinBoundsApplies(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.EVGoalIntent{
		TargetSocKwh:  ptr(40.0),
		CapacityKwh:   ptr(60.0),
		DepartureUnix: f.adopter.now().Add(time.Hour).Unix(),
	}
	outcome, _ := f.adopter.applyEVGoal(msg)
	if outcome != "applied" {
		t.Errorf("outcome = %q, want applied (target <= stated capacity)", outcome)
	}
}

func TestIntentAdopter_EVGoal_Expired(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.EVGoalIntent{
		TargetSocKwh:  ptr(40.0),
		DepartureUnix: f.adopter.now().Add(-time.Hour).Unix(), // in the past
	}
	outcome, _ := f.adopter.applyEVGoal(msg)
	if outcome != "expired" {
		t.Errorf("outcome = %q, want expired", outcome)
	}
	if f.eng.evGoalCount() != 0 {
		t.Errorf("SetEVGoal must not be called on an expired goal")
	}
}

// ---------------------------------------------------------------------
// reserve
// ---------------------------------------------------------------------

func TestIntentAdopter_Reserve_Applied(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.BackupReserveIntent{ReservePct: ptr(50.0)}
	outcome, _ := f.adopter.applyReserve(msg)
	if outcome != "applied" {
		t.Errorf("outcome = %q, want applied", outcome)
	}
	f.eng.mu.Lock()
	defer f.eng.mu.Unlock()
	if len(f.eng.reservePcts) != 1 || f.eng.reservePcts[0] != 50 {
		t.Errorf("reservePcts = %v, want [50]", f.eng.reservePcts)
	}
}

func TestIntentAdopter_Reserve_RejectNil(t *testing.T) {
	f := newTestAdopter(t, nil)
	outcome, _ := f.adopter.applyReserve(bus.BackupReserveIntent{})
	if outcome != "rejected" {
		t.Errorf("outcome = %q, want rejected", outcome)
	}
}

func TestIntentAdopter_Reserve_RejectOutOfRange(t *testing.T) {
	f := newTestAdopter(t, nil)
	for _, pct := range []float64{-1, 101} {
		outcome, _ := f.adopter.applyReserve(bus.BackupReserveIntent{ReservePct: ptr(pct)})
		if outcome != "rejected" {
			t.Errorf("pct=%v outcome = %q, want rejected", pct, outcome)
		}
	}
}

// ---------------------------------------------------------------------
// tariff
// ---------------------------------------------------------------------

func TestIntentAdopter_Tariff_Applied(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.TariffIntent{Tariff: validTOUSpec()}
	outcome, detail := f.adopter.applyTariff(msg)
	if outcome != "applied" || detail != "" {
		t.Fatalf("outcome/detail = %q/%q, want applied/\"\"", outcome, detail)
	}
	f.eng.mu.Lock()
	gotEng := len(f.eng.fallbackTOUs)
	f.eng.mu.Unlock()
	if gotEng != 1 {
		t.Errorf("SetFallbackTOU calls = %d, want 1", gotEng)
	}
	f.opt.mu.Lock()
	gotOpt := len(f.opt.models)
	f.opt.mu.Unlock()
	if gotOpt != 1 {
		t.Errorf("SwapCostModel calls = %d, want 1", gotOpt)
	}
}

func TestIntentAdopter_Tariff_CompileErrorPassthrough(t *testing.T) {
	f := newTestAdopter(t, nil)
	bad := bus.TariffSpec{Currency: "EUR"} // compileTariff rejects non-USD
	wantErr := "tariff: currency"

	outcome, detail := f.adopter.applyTariff(bus.TariffIntent{Tariff: bad})
	if outcome != "rejected" {
		t.Fatalf("outcome = %q, want rejected", outcome)
	}
	_, compileErr := compileTariff(bad)
	if compileErr == nil {
		t.Fatal("compileTariff must reject a non-USD spec")
	}
	if detail != compileErr.Error() {
		t.Errorf("detail = %q, want the compiler's own message %q", detail, compileErr.Error())
	}
	if len(detail) < len(wantErr) || detail[:len(wantErr)] != wantErr {
		t.Errorf("detail = %q, want it to start with %q", detail, wantErr)
	}
}

// ---------------------------------------------------------------------
// solarforecast
// ---------------------------------------------------------------------

func TestIntentAdopter_SolarForecast_ClampCounting(t *testing.T) {
	f := newTestAdopter(t, nil)
	// solarMaxWTotal = 5000W -> ceiling = 1.5*5000/1000 = 7.5 kW.
	msg := bus.SolarForecastIntent{
		WindowStart: 1751500800, // must be 5-min aligned; picked arbitrarily below
		StepKw:      []float64{-1, 3, 7.5, 10, 100},
		SourceTs:    f.adopter.now().Unix(), // fresh
	}
	msg.WindowStart -= msg.WindowStart % intentPlanStepSec // ensure alignment regardless of the literal above
	outcome, detail := f.adopter.applySolarForecast(msg)
	if outcome != "clamped" || detail == "" {
		t.Fatalf("outcome/detail = %q/%q, want clamped/non-empty", outcome, detail)
	}
	f.eng.mu.Lock()
	got := f.eng.solarForecasts[len(f.eng.solarForecasts)-1].StepKw
	f.eng.mu.Unlock()
	want := []float64{0, 3, 7.5, 7.5, 7.5}
	if len(got) != len(want) {
		t.Fatalf("StepKw = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("StepKw[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestIntentAdopter_SolarForecast_AlignmentReject(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.SolarForecastIntent{WindowStart: 1751500801, StepKw: []float64{1, 2}} // not 5-min aligned
	outcome, _ := f.adopter.applySolarForecast(msg)
	if outcome != "rejected" {
		t.Errorf("outcome = %q, want rejected", outcome)
	}
}

func TestIntentAdopter_SolarForecast_LenReject(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.SolarForecastIntent{StepKw: make([]float64, 289)}
	outcome, _ := f.adopter.applySolarForecast(msg)
	if outcome != "rejected" {
		t.Errorf("outcome = %q, want rejected", outcome)
	}
}

func TestIntentAdopter_SolarForecast_StaleButApplied(t *testing.T) {
	f := newTestAdopter(t, nil)
	staleSourceTs := f.adopter.now().Add(-13 * time.Hour).Unix()
	msg := bus.SolarForecastIntent{
		WindowStart: 0,
		StepKw:      []float64{1, 2, 3}, // within ceiling, no clamping
		SourceTs:    staleSourceTs,
	}
	outcome, detail := f.adopter.applySolarForecast(msg)
	if outcome != "applied" {
		t.Fatalf("outcome = %q, want applied (stale is still adopted, per §3.1)", outcome)
	}
	if detail == "" {
		t.Errorf("detail must note the staleness")
	}
	// The forecast must still reach the engine despite being stale.
	f.eng.mu.Lock()
	n := len(f.eng.solarForecasts)
	f.eng.mu.Unlock()
	if n != 1 {
		t.Errorf("SetSolarForecast calls = %d, want 1", n)
	}
	if f.adopter.lastForecastStaleAlarm.IsZero() {
		t.Errorf("stale alarm rate-limit timestamp was not recorded")
	}
}

func TestIntentAdopter_SolarForecast_FreshIsApplied(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.SolarForecastIntent{StepKw: []float64{1, 2}, SourceTs: f.adopter.now().Unix()}
	outcome, detail := f.adopter.applySolarForecast(msg)
	if outcome != "applied" || detail != "" {
		t.Errorf("outcome/detail = %q/%q, want applied/\"\"", outcome, detail)
	}
}

// ---------------------------------------------------------------------
// loadprofile
// ---------------------------------------------------------------------

func TestIntentAdopter_LoadProfile_ClampCounting(t *testing.T) {
	f := newTestAdopter(t, nil)
	// allMaxWTotal = 13000W -> ceiling = 3*13000/1000 = 39 kW.
	msg := bus.LoadProfileIntent{StepKw: []float64{-5, 10, 39, 50}}
	outcome, detail := f.adopter.applyLoadProfile(msg)
	if outcome != "clamped" || detail == "" {
		t.Fatalf("outcome/detail = %q/%q, want clamped/non-empty", outcome, detail)
	}
	f.eng.mu.Lock()
	got := f.eng.loadProfiles[len(f.eng.loadProfiles)-1]
	f.eng.mu.Unlock()
	want := []float64{0, 10, 39, 39}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("StepKw[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestIntentAdopter_LoadProfile_LenReject(t *testing.T) {
	f := newTestAdopter(t, nil)
	outcome, _ := f.adopter.applyLoadProfile(bus.LoadProfileIntent{StepKw: make([]float64, 300)})
	if outcome != "rejected" {
		t.Errorf("outcome = %q, want rejected", outcome)
	}
}

// ---------------------------------------------------------------------
// chargenow
// ---------------------------------------------------------------------

func TestIntentAdopter_ChargeNow_TTLZeroRejected(t *testing.T) {
	f := newTestAdopter(t, nil)
	outcome, _ := f.adopter.applyChargeNow(bus.ChargeNowIntent{})
	if outcome != "rejected" {
		t.Errorf("outcome = %q, want rejected", outcome)
	}
}

func TestIntentAdopter_ChargeNow_Expired(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.ChargeNowIntent{
		IntentMeta: bus.IntentMeta{IssuedAt: f.adopter.now().Add(-time.Hour).Unix(), TTLS: 60},
	}
	outcome, _ := f.adopter.applyChargeNow(msg)
	if outcome != "expired" {
		t.Errorf("outcome = %q, want expired", outcome)
	}
}

func TestIntentAdopter_ChargeNow_NoStandingGoalRejected(t *testing.T) {
	f := newTestAdopter(t, nil)
	msg := bus.ChargeNowIntent{IntentMeta: bus.IntentMeta{IssuedAt: f.adopter.now().Unix(), TTLS: 60}}
	outcome, detail := f.adopter.applyChargeNow(msg)
	if outcome != "rejected" {
		t.Fatalf("outcome = %q, want rejected", outcome)
	}
	if detail != "no EV goal to accelerate — set a target first" {
		t.Errorf("detail = %q, unexpected", detail)
	}
}

func TestIntentAdopter_ChargeNow_AppliedAcceleratesStandingGoal(t *testing.T) {
	f := newTestAdopter(t, nil)
	standing := orchestrator.EVGoal{TargetSocKwh: 40, DepartureUnix: f.adopter.now().Add(8 * time.Hour).Unix(), InitialSocKwh: 5}
	f.adopter.standingEVGoal = &standing

	msg := bus.ChargeNowIntent{IntentMeta: bus.IntentMeta{IssuedAt: f.adopter.now().Unix(), TTLS: 60}}
	outcome, _ := f.callLocked(func() (string, string) { return f.adopter.applyChargeNow(msg) })
	if outcome != "applied" {
		t.Fatalf("outcome = %q, want applied", outcome)
	}
	got := f.eng.lastEVGoal()
	wantDeparture := f.adopter.now().Add(60 * time.Second).Unix()
	if got.TargetSocKwh != 40 || got.InitialSocKwh != 5 || got.DepartureUnix != wantDeparture {
		t.Errorf("accelerated goal = %+v, want target=40 initial=5 departure=%d", got, wantDeparture)
	}
	if f.adopter.chargeNowRevert == nil {
		t.Errorf("chargeNowRevert timer must be scheduled")
	}
}

// TestIntentAdopter_ChargeNow_RevertRestoresStandingGoal drives the revert
// path directly (the "call the revert func directly" option the unit brief
// names) rather than sleeping out a real TTL — deterministic and fast.
func TestIntentAdopter_ChargeNow_RevertRestoresStandingGoal(t *testing.T) {
	f := newTestAdopter(t, nil)
	standing := orchestrator.EVGoal{TargetSocKwh: 40, DepartureUnix: f.adopter.now().Add(8 * time.Hour).Unix()}
	f.adopter.standingEVGoal = &standing

	msg := bus.ChargeNowIntent{IntentMeta: bus.IntentMeta{IssuedAt: f.adopter.now().Unix(), TTLS: 60}}
	f.callLocked(func() (string, string) { return f.adopter.applyChargeNow(msg) })
	if f.eng.evGoalCount() != 1 {
		t.Fatalf("evGoalCount = %d, want 1 after apply", f.eng.evGoalCount())
	}

	// Simulate the AfterFunc firing: same gen, same goal it was scheduled with.
	f.adopter.revertChargeNow(f.adopter.chargeNowGen, standing)

	if f.eng.evGoalCount() != 2 {
		t.Fatalf("evGoalCount = %d, want 2 after revert", f.eng.evGoalCount())
	}
	if got := f.eng.lastEVGoal(); got != standing {
		t.Errorf("reverted goal = %+v, want standing goal %+v", got, standing)
	}
	if f.adopter.chargeNowRevert != nil {
		t.Errorf("chargeNowRevert must be cleared after firing")
	}
}

// TestIntentAdopter_ChargeNow_RevertSupersededByNewEVGoalIsNoop proves the
// generation-guard: a revert that fires (or is invoked) AFTER a fresh evgoal
// intent has already replaced the standing goal must not clobber it.
func TestIntentAdopter_ChargeNow_RevertSupersededByNewEVGoalIsNoop(t *testing.T) {
	f := newTestAdopter(t, nil)
	standing := orchestrator.EVGoal{TargetSocKwh: 40, DepartureUnix: f.adopter.now().Add(8 * time.Hour).Unix()}
	f.adopter.standingEVGoal = &standing

	msg := bus.ChargeNowIntent{IntentMeta: bus.IntentMeta{IssuedAt: f.adopter.now().Unix(), TTLS: 60}}
	f.callLocked(func() (string, string) { return f.adopter.applyChargeNow(msg) })
	staleGen := f.adopter.chargeNowGen

	// A fresh evgoal lands before the revert fires: bumps chargeNowGen,
	// stops the pending timer, becomes the new standing goal.
	newGoalMsg := bus.EVGoalIntent{TargetSocKwh: ptr(55.0), DepartureUnix: f.adopter.now().Add(2 * time.Hour).Unix()}
	f.callLocked(func() (string, string) { return f.adopter.applyEVGoal(newGoalMsg) })
	if f.eng.evGoalCount() != 2 {
		t.Fatalf("evGoalCount = %d, want 2 (chargenow + new evgoal)", f.eng.evGoalCount())
	}

	// The OLD chargenow's revert now fires late, with the stale generation.
	f.adopter.revertChargeNow(staleGen, standing)

	if f.eng.evGoalCount() != 2 {
		t.Errorf("evGoalCount = %d, want still 2 — a superseded revert must be a no-op", f.eng.evGoalCount())
	}
	if got := f.eng.lastEVGoal(); got.TargetSocKwh != 55 {
		t.Errorf("last goal = %+v, the new evgoal (target=55) must still be in effect", got)
	}
}

// TestIntentAdopter_ChargeNow_RealTimerFires exercises the actual
// time.AfterFunc path end-to-end with a 1s TTL (the shortest legal ttl_s,
// which is defined in whole seconds) rather than invoking revertChargeNow
// directly, so the wiring itself — not just the guard logic — is pinned.
func TestIntentAdopter_ChargeNow_RealTimerFires(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skips the 1s real-timer wait")
	}
	f := newTestAdopter(t, nil)
	standing := orchestrator.EVGoal{TargetSocKwh: 40, DepartureUnix: f.adopter.now().Add(8 * time.Hour).Unix()}
	f.adopter.standingEVGoal = &standing

	msg := bus.ChargeNowIntent{IntentMeta: bus.IntentMeta{IssuedAt: f.adopter.now().Unix(), TTLS: 1}}
	f.callLocked(func() (string, string) { return f.adopter.applyChargeNow(msg) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.eng.evGoalCount() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f.eng.evGoalCount() < 2 {
		t.Fatalf("evGoalCount = %d, want >= 2 (revert never fired)", f.eng.evGoalCount())
	}
	if got := f.eng.lastEVGoal(); got != standing {
		t.Errorf("reverted goal = %+v, want standing goal %+v", got, standing)
	}
}

// ---------------------------------------------------------------------
// dedupe + journal
// ---------------------------------------------------------------------

func TestIntentAdopter_Dedupe_SecondDeliveryIsDuplicateNoJournal(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	f := newTestAdopter(t, jw)
	msg := bus.EVGoalIntent{
		IntentMeta:    bus.IntentMeta{ID: "dup-1"},
		TargetSocKwh:  ptr(40.0),
		DepartureUnix: f.adopter.now().Add(time.Hour).Unix(),
	}
	apply := func() (string, string) { return f.adopter.applyEVGoal(msg) }

	f.adopter.adopt("evgoal", msg.IntentMeta, apply)
	if got := lastResult(t, f.mc); got.Outcome != "applied" {
		t.Fatalf("first delivery outcome = %q, want applied", got.Outcome)
	}

	f.adopter.adopt("evgoal", msg.IntentMeta, apply)
	if got := lastResult(t, f.mc); got.Outcome != "duplicate" {
		t.Fatalf("second delivery (same id) outcome = %q, want duplicate", got.Outcome)
	}

	if err := jw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	events := journalEventsByType(t, dir)
	if got := len(events[journal.TypeIntentApplied]); got != 1 {
		t.Fatalf("intent_applied events = %d, want 1 (duplicate must not re-journal)", got)
	}

	// SetEVGoal must only have been called once too (the duplicate never
	// reaches apply()).
	if f.eng.evGoalCount() != 1 {
		t.Errorf("evGoalCount = %d, want 1 (duplicate must not re-apply)", f.eng.evGoalCount())
	}
}

func TestIntentAdopter_Dedupe_DifferentIDsBothApply(t *testing.T) {
	f := newTestAdopter(t, nil)
	base := bus.EVGoalIntent{TargetSocKwh: ptr(40.0), DepartureUnix: f.adopter.now().Add(time.Hour).Unix()}

	m1 := base
	m1.ID = "a"
	f.adopter.adopt("evgoal", m1.IntentMeta, func() (string, string) { return f.adopter.applyEVGoal(m1) })

	m2 := base
	m2.ID = "b"
	f.adopter.adopt("evgoal", m2.IntentMeta, func() (string, string) { return f.adopter.applyEVGoal(m2) })

	if f.eng.evGoalCount() != 2 {
		t.Errorf("evGoalCount = %d, want 2 (different ids must both apply)", f.eng.evGoalCount())
	}
}

func TestIntentAdopter_Journal_RejectedGoesToIntentRejected(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	f := newTestAdopter(t, jw)
	msg := bus.BackupReserveIntent{IntentMeta: bus.IntentMeta{ID: "r1"}} // nil ReservePct -> rejected
	f.adopter.adopt("reserve", msg.IntentMeta, func() (string, string) { return f.adopter.applyReserve(msg) })

	if err := jw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	events := journalEventsByType(t, dir)
	if got := len(events[journal.TypeIntentRejected]); got != 1 {
		t.Fatalf("intent_rejected events = %d, want 1", got)
	}
	if got := len(events[journal.TypeIntentApplied]); got != 0 {
		t.Fatalf("intent_applied events = %d, want 0", got)
	}
	var p journal.IntentRejected
	if err := json.Unmarshal(events[journal.TypeIntentRejected][0].Data, &p); err != nil {
		t.Fatalf("unmarshal IntentRejected: %v", err)
	}
	if p.Kind != "reserve" || p.ID != "r1" || p.Outcome != "rejected" {
		t.Errorf("payload = %+v, want kind=reserve id=r1 outcome=rejected", p)
	}
}

func TestIntentAdopter_NilJournalIsNoop(t *testing.T) {
	f := newTestAdopter(t, nil) // jw == nil
	msg := bus.BackupReserveIntent{ReservePct: ptr(50.0)}
	// Must not panic with a nil journal.
	f.adopter.adopt("reserve", msg.IntentMeta, func() (string, string) { return f.adopter.applyReserve(msg) })
	if got := lastResult(t, f.mc); got.Outcome != "applied" {
		t.Errorf("outcome = %q, want applied", got.Outcome)
	}
}

// ---------------------------------------------------------------------
// mode (Unit 3.4): the seventh kind, routed through adopt → applyMode →
// modeManager.request. These pin the routing/validation surface intent.go
// owns; modeManager's own behavior is covered in mode_test.go.
// ---------------------------------------------------------------------

// wireMode attaches a modeManager (sharing the fixture's fake MQTT client and
// clock) so applyMode can reach it, mirroring main.go's adopter.modes wiring.
func (f *testAdopterFixture) wireMode(t *testing.T, initialMode string) *modeManager {
	t.Helper()
	mm := newModeManager(initialMode,
		&fakeOptimizer{marker: "opt"}, &fakeOptimizer{marker: "gw"}, &fakeSafety{marker: "safe"},
		nil, f.mc, nil)
	mm.setEngine(&fakeWaker{})
	mm.now = f.adopter.now
	f.adopter.modes = mm
	return mm
}

func TestIntentAdopter_Mode_AppliedRoutesThroughRequest(t *testing.T) {
	f := newTestAdopter(t, nil)
	mm := f.wireMode(t, "optimizer")

	msg := bus.ModeIntent{IntentMeta: bus.IntentMeta{ID: "m1", Actor: "a@b", Origin: "app"}, Mode: "gateway"}
	f.adopter.adopt("mode", msg.IntentMeta, func() (string, string) { return f.adopter.applyMode(msg) })

	if mm.Mode() != "gateway" {
		t.Errorf("mode = %q, want gateway (routed through request)", mm.Mode())
	}
	res := lastResult(t, f.mc) // IntentResult is the LAST publish (after the retained ModeStatus)
	if res.Kind != "mode" || res.ID != "m1" || res.Outcome != "applied" {
		t.Errorf("result = %+v, want kind=mode id=m1 outcome=applied", res)
	}
}

func TestIntentAdopter_Mode_InvalidValueRejectedBeforeManager(t *testing.T) {
	f := newTestAdopter(t, nil)
	mm := f.wireMode(t, "optimizer")

	outcome, detail := f.adopter.applyMode(bus.ModeIntent{Mode: "turbo"})
	if outcome != "rejected" || detail == "" {
		t.Errorf("outcome/detail = %q/%q, want rejected/non-empty", outcome, detail)
	}
	if mm.Mode() != "optimizer" {
		t.Errorf("mode = %q, want unchanged optimizer (invalid value must not reach the manager)", mm.Mode())
	}
}

func TestIntentAdopter_Mode_SameModeIsDuplicate(t *testing.T) {
	f := newTestAdopter(t, nil)
	f.wireMode(t, "gateway")

	outcome, _ := f.adopter.applyMode(bus.ModeIntent{IntentMeta: bus.IntentMeta{ID: "m2"}, Mode: "gateway"})
	if outcome != "duplicate" {
		t.Errorf("outcome = %q, want duplicate (already in gateway)", outcome)
	}
}

func TestIntentAdopter_Mode_NilManagerRejects(t *testing.T) {
	f := newTestAdopter(t, nil) // modes left nil
	outcome, detail := f.adopter.applyMode(bus.ModeIntent{Mode: "gateway"})
	if outcome != "rejected" || detail != "mode manager not wired" {
		t.Errorf("outcome/detail = %q/%q, want rejected/\"mode manager not wired\"", outcome, detail)
	}
}
