package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// These pin CSIPPassthrough (Unit 3.5, DEVICE_ROADMAP §3.5) against its spec:
// it is a pure mapping from the active CSIP control (or its absence) plus the
// GatewayEVSEPolicy onto demands, with no economic opinion of its own and no
// diagnosis of non-convergence. Unit 3.5 is provably unwired — nothing but
// this file constructs a CSIPPassthrough.

// ── fixtures ─────────────────────────────────────────────────────────────────

// passthroughPlant is a fixed, bench-defaulted plant: 2 inverters, 1 battery,
// 1 EVSE. Plant.EVSEs is keyed by station only (cmd/hub/main.go's
// buildConstraintPlant), so "evse1" here stands for the whole station —
// stack.go's parseEVSEDevice treats a bare device key as connector 0.
func passthroughPlant() Plant {
	return Plant{
		Inverters: map[string]orchestrator.InverterPlant{
			"inv1": orchestrator.InverterPlant{}.WithDefaults(),
			"inv2": orchestrator.InverterPlant{}.WithDefaults(),
		},
		Batteries: map[string]orchestrator.BatteryPlant{
			"bat1": orchestrator.BatteryPlant{}.WithDefaults(),
		},
		EVSEs: map[string]orchestrator.EVSEPlant{
			"evse1": orchestrator.EVSEPlant{}.WithDefaults(),
		},
		Meter: orchestrator.MeterPlant{}.WithDefaults(),
	}
}

// passthroughDevices lists every device key in passthroughPlant, across all
// three classes — the set a connect demand (when emitted) must cover.
var passthroughDevices = []string{"inv1", "inv2", "bat1", "evse1"}

// passthroughInput builds an Input from a CSIP control (nil = none) and a
// timestamp, at the tuned tick. CSIPPassthrough never reads State.Solar/
// Batteries/EVSEs (it drives entirely off Plant + CSIPControl + Timestamp),
// so the fixture SystemState carries only what the constraint actually uses.
func passthroughInput(ctl *orchestrator.CSIPControlState, ts time.Time) Input {
	return Input{
		State:       orchestrator.SystemState{Timestamp: ts, CSIPControl: ctl},
		Plant:       passthroughPlant(),
		TickSeconds: tunedTickInterval.Seconds(),
	}
}

// newPassthroughSession returns a fresh base Session, mirroring newExportPair
// et al. — CSIPPassthrough does not use it (see the type doc) but the
// Constraint interface requires one.
func newPassthroughSession() *Session { return NewSession("csip-passthrough", 0) }

func maxLimControl(w int16) *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "maxlim-mrid",
		Base: model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: w, Multiplier: 0}},
	}
}

func fixedWControl(w int16) *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "fixedw-mrid",
		Base: model.DERControlBase{OpModFixedW: &model.ActivePower{Value: w, Multiplier: 0}},
	}
}

func connectOnlyControl(v bool) *orchestrator.CSIPControlState {
	return &orchestrator.CSIPControlState{
		Source: "event", MRID: "connect-mrid",
		Base: model.DERControlBase{OpModConnect: boolPtr(v)},
	}
}

// evCeilingOf returns the emitted EVSE current ceiling for a station (NaN if
// none) — the AxisEVSECurrentA twin of export_test.go's ceilingOf.
func evCeilingOf(demands []Demand, station string) float64 {
	for _, d := range demands {
		if d.Axis == AxisEVSECurrentA && d.Device == station {
			return d.Max
		}
	}
	return math.NaN()
}

// connectOf returns the emitted connect demand's value for a device, or nil
// if no AxisConnect demand was emitted for it.
func connectOf(demands []Demand, device string) *bool {
	for _, d := range demands {
		if d.Axis == AxisConnect && d.Device == device {
			return d.Connect
		}
	}
	return nil
}

// resolveSane feeds demands through the SAME arbiter Resolve stack.go's
// Optimize calls and asserts the result is well-formed: Resolve must not
// panic, every resolved interval must have Min<=Max whenever both sides are
// bounded, and no device key must be empty. This is the "arbiter-well-formed"
// smoke test the spec asks every table case to pass; AD-007's structural
// narrowing-only property itself is pinned exhaustively in arbiter_test.go —
// this only proves THIS constraint's demands are legal arbiter input.
func resolveSane(t *testing.T, demands []Demand) {
	t.Helper()
	desired := Resolve(demands) // a malformed Demand would panic or misbehave here
	for dev, d := range desired {
		if dev == "" {
			t.Errorf("resolved demand with empty device key: %+v", d)
		}
		for axis, iv := range d.Bounds {
			loNaN, hiNaN := math.IsNaN(iv.Min), math.IsNaN(iv.Max)
			if !loNaN && !hiNaN && iv.Min > iv.Max {
				t.Errorf("device %s axis %s: resolved interval inverted [%v,%v]", dev, axis, iv.Min, iv.Max)
			}
		}
	}
}

// midnightPlus is a fixed UTC calendar day at the given local hour — used
// wherever a test needs to drive CSIPPassthrough's EVSE schedule off
// Input.State.Timestamp. UTC is an arbitrary but fixed choice here: the
// constraint takes whatever zone the Timestamp carries verbatim (GAP-05), so
// the test only needs a stable, inspectable hour, not any particular zone.
func midnightPlus(hour int) time.Time {
	return time.Date(2026, 7, 8, hour, 0, 0, 0, time.UTC)
}

// ── Name/Tier ────────────────────────────────────────────────────────────────

func TestCSIPPassthrough_NameAndTier(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{})
	if c.Name() != "csip-passthrough" {
		t.Errorf("Name()=%q want csip-passthrough", c.Name())
	}
	if c.Tier() != TierEconomics {
		t.Errorf("Tier()=%v want TierEconomics", c.Tier())
	}
	var _ Constraint = c // interface satisfaction, redundant with the package-level assertion
}

// ── GatewayEVSEPolicy.WithDefaults ───────────────────────────────────────────

func TestGatewayEVSEPolicy_WithDefaults(t *testing.T) {
	got := GatewayEVSEPolicy{}.WithDefaults()
	want := GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}
	if got != want {
		t.Errorf("WithDefaults()=%+v want %+v", got, want)
	}

	custom := GatewayEVSEPolicy{Mode: "full", FullCurrentA: 16}.WithDefaults()
	if custom.Mode != "full" || custom.FullCurrentA != 16 {
		t.Errorf("explicit fields overridden by WithDefaults: %+v", custom)
	}
	// Window still defaults even when only Mode/FullCurrentA are set.
	if custom.WindowStartHH != 23 || custom.WindowEndHH != 7 {
		t.Errorf("window defaults not applied: %+v", custom)
	}
}

// ── Inverter ceiling: MaxLimW → ceiling; absent → explicit restore ─────────

func TestCSIPPassthrough_InverterCeiling(t *testing.T) {
	tests := []struct {
		name   string
		ctl    *orchestrator.CSIPControlState
		wantW  float64 // NaN = restore
		restor bool
	}{
		{name: "maxlim-active-4200w", ctl: maxLimControl(4200), wantW: 4200},
		{name: "maxlim-active-negative-multiplier", ctl: &orchestrator.CSIPControlState{
			Base: model.DERControlBase{OpModMaxLimW: &model.ActivePower{Value: 500, Multiplier: 1}}}, wantW: 5000},
		{name: "no-maxlim-but-other-control-restores", ctl: fixedWControl(1000), wantW: math.NaN(), restor: true},
		{name: "no-control-restores", ctl: nil, wantW: math.NaN(), restor: true},
	}
	c := NewCSIPPassthrough(GatewayEVSEPolicy{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := passthroughInput(tt.ctl, midnightPlus(3))
			demands, breach := c.Evaluate(in, newPassthroughSession())
			if breach != nil {
				t.Fatalf("unexpected breach: %+v", breach)
			}
			for _, name := range []string{"inv1", "inv2"} {
				got := ceilingOf(demands, name)
				switch {
				case tt.restor:
					if !math.IsNaN(got) {
						t.Errorf("%s: ceiling=%.0f want explicit restore (NaN)", name, got)
					}
					if !hasDemand(demands, name, AxisSolarCeilingW, sourceCSIPRestore) {
						t.Errorf("%s: restore demand missing source %q", name, sourceCSIPRestore)
					}
				default:
					if got != tt.wantW {
						t.Errorf("%s: ceiling=%.0f want %.0f", name, got, tt.wantW)
					}
					if !hasDemand(demands, name, AxisSolarCeilingW, sourceCSIPMaxLim) {
						t.Errorf("%s: maxlim demand missing source %q", name, sourceCSIPMaxLim)
					}
				}
			}
			resolveSane(t, demands)
		})
	}
}

// hasDemand reports whether a demand with the given device/axis/source exists.
func hasDemand(demands []Demand, device string, axis Axis, source string) bool {
	for _, d := range demands {
		if d.Device == device && d.Axis == axis && d.Source == source {
			return true
		}
	}
	return false
}

// ── Battery setpoint: FixedW → sign-mapped point; absent → idle 0 W ───────

func TestCSIPPassthrough_BatterySetpoint(t *testing.T) {
	tests := []struct {
		name      string
		ctl       *orchestrator.CSIPControlState
		wantW     float64
		wantIdle  bool
		wantMaxLm bool // active control, but not FixedW (still idles)
	}{
		{name: "fixedw-positive-discharge", ctl: fixedWControl(3000), wantW: 3000},
		{name: "fixedw-negative-charge", ctl: fixedWControl(-2500), wantW: -2500},
		{name: "active-control-without-fixedw-idles", ctl: maxLimControl(1000), wantW: 0, wantIdle: true, wantMaxLm: true},
		{name: "no-control-idles", ctl: nil, wantW: 0, wantIdle: true},
	}
	c := NewCSIPPassthrough(GatewayEVSEPolicy{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := passthroughInput(tt.ctl, midnightPlus(3))
			demands, breach := c.Evaluate(in, newPassthroughSession())
			if breach != nil {
				t.Fatalf("unexpected breach: %+v", breach)
			}
			got := setpointOf(demands, "bat1")
			if got != tt.wantW {
				t.Errorf("bat1: setpoint=%.0f want %.0f", got, tt.wantW)
			}
			if tt.wantIdle && !hasDemand(demands, "bat1", AxisBatterySetpointW, sourceCSIPIdle) {
				t.Errorf("bat1: idle demand missing source %q", sourceCSIPIdle)
			}
			if !tt.wantIdle && !hasDemand(demands, "bat1", AxisBatterySetpointW, sourceCSIPFixedW) {
				t.Errorf("bat1: fixedw demand missing source %q", sourceCSIPFixedW)
			}
			// An active MaxLimW-only control still drives the inverter ceiling —
			// prove the battery idle branch didn't accidentally suppress it.
			if tt.wantMaxLm && ceilingOf(demands, "inv1") != 1000 {
				t.Errorf("inv1: ceiling=%.0f want 1000 (independent of battery idle)", ceilingOf(demands, "inv1"))
			}
			resolveSane(t, demands)
		})
	}
}

// ── Connect: emitted across all three device maps only when CSIP is explicit ─

func TestCSIPPassthrough_ConnectFalse_AllDevices(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{})
	in := passthroughInput(connectOnlyControl(false), midnightPlus(3))
	demands, breach := c.Evaluate(in, newPassthroughSession())
	if breach != nil {
		t.Fatalf("unexpected breach: %+v", breach)
	}
	for _, dev := range passthroughDevices {
		cn := connectOf(demands, dev)
		if cn == nil || *cn != false {
			t.Errorf("%s: connect=%v want false", dev, cn)
		}
	}
	resolveSane(t, demands)
}

func TestCSIPPassthrough_ConnectTrue_AllDevices(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{})
	in := passthroughInput(connectOnlyControl(true), midnightPlus(3))
	demands, _ := c.Evaluate(in, newPassthroughSession())
	for _, dev := range passthroughDevices {
		cn := connectOf(demands, dev)
		if cn == nil || *cn != true {
			t.Errorf("%s: connect=%v want true", dev, cn)
		}
	}
	resolveSane(t, demands)
}

func TestCSIPPassthrough_NoConnectDemand_WhenSilentOrAbsent(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{})
	cases := map[string]*orchestrator.CSIPControlState{
		"active-control-silent-on-connect": maxLimControl(1000),
		"no-control":                       nil,
	}
	for name, ctl := range cases {
		t.Run(name, func(t *testing.T) {
			in := passthroughInput(ctl, midnightPlus(3))
			demands, _ := c.Evaluate(in, newPassthroughSession())
			for _, dev := range passthroughDevices {
				if cn := connectOf(demands, dev); cn != nil {
					t.Errorf("%s: unexpected connect demand %v", dev, *cn)
				}
			}
			resolveSane(t, demands)
		})
	}
}

// ── EVSE policy: full / scheduled inside / outside / wraparound ───────────

func TestCSIPPassthrough_EVSEPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy GatewayEVSEPolicy
		hour   int
		wantA  float64
	}{
		{name: "full-mode-at-midday", policy: GatewayEVSEPolicy{Mode: "full", FullCurrentA: 32}, hour: 12, wantA: 32},
		{name: "full-mode-at-midnight", policy: GatewayEVSEPolicy{Mode: "full", FullCurrentA: 32}, hour: 0, wantA: 32},
		{name: "scheduled-wraparound-inside-at-01h", policy: GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}, hour: 1, wantA: 32},
		{name: "scheduled-wraparound-outside-at-12h", policy: GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}, hour: 12, wantA: 0},
		{name: "scheduled-wraparound-boundary-start-23h-inside", policy: GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}, hour: 23, wantA: 32},
		{name: "scheduled-wraparound-boundary-end-7h-outside", policy: GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}, hour: 7, wantA: 0},
		{name: "scheduled-non-wrapping-window", policy: GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 9, WindowEndHH: 17, FullCurrentA: 24}, hour: 10, wantA: 24},
		{name: "scheduled-non-wrapping-window-outside", policy: GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 9, WindowEndHH: 17, FullCurrentA: 24}, hour: 20, wantA: 0},
		{name: "policy-defaults-inside-23h", policy: GatewayEVSEPolicy{}, hour: 23, wantA: 32},
		{name: "policy-defaults-outside-8h", policy: GatewayEVSEPolicy{}, hour: 8, wantA: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCSIPPassthrough(tt.policy)
			in := passthroughInput(nil, midnightPlus(tt.hour))
			demands, breach := c.Evaluate(in, newPassthroughSession())
			if breach != nil {
				t.Fatalf("unexpected breach: %+v", breach)
			}
			got := evCeilingOf(demands, "evse1")
			if got != tt.wantA {
				t.Errorf("evse1 ceiling=%.1f want %.1f (hour=%d policy=%+v)", got, tt.wantA, tt.hour, tt.policy)
			}
			resolveSane(t, demands)
		})
	}
}

// TestCSIPPassthrough_EVSETimestampDrivesSchedule pins the spec's explicit
// requirement: the SAME policy at two different Input.State.Timestamp values
// produces two different demands — proving the hour is read from the state,
// never from time.Now().
func TestCSIPPassthrough_EVSETimestampDrivesSchedule(t *testing.T) {
	policy := GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}
	c := NewCSIPPassthrough(policy)

	inNight := passthroughInput(nil, midnightPlus(1))
	nightDemands, _ := c.Evaluate(inNight, newPassthroughSession())

	inDay := passthroughInput(nil, midnightPlus(12))
	dayDemands, _ := c.Evaluate(inDay, newPassthroughSession())

	if got := evCeilingOf(nightDemands, "evse1"); got != 32 {
		t.Errorf("01:00 (inside window): ceiling=%.1f want 32", got)
	}
	if got := evCeilingOf(dayDemands, "evse1"); got != 0 {
		t.Errorf("12:00 (outside window): ceiling=%.1f want 0", got)
	}
}

// ── No-control: full restore posture ────────────────────────────────────────

func TestCSIPPassthrough_NoControl_FullRestorePosture(t *testing.T) {
	policy := GatewayEVSEPolicy{Mode: "scheduled", WindowStartHH: 23, WindowEndHH: 7, FullCurrentA: 32}
	c := NewCSIPPassthrough(policy)
	in := passthroughInput(nil, midnightPlus(1)) // 01:00 → inside the scheduled window
	demands, breach := c.Evaluate(in, newPassthroughSession())
	if breach != nil {
		t.Fatalf("passthrough must never diagnose a breach, got %+v", breach)
	}

	for _, name := range []string{"inv1", "inv2"} {
		if got := ceilingOf(demands, name); !math.IsNaN(got) {
			t.Errorf("inverter %s: want explicit restore (NaN), got %.0f", name, got)
		}
	}
	if got := setpointOf(demands, "bat1"); got != 0 {
		t.Errorf("battery bat1: want idle 0W, got %.0f", got)
	}
	if got := evCeilingOf(demands, "evse1"); got != 32 {
		t.Errorf("evse1: want policy full-current 32A (inside window), got %.1f", got)
	}
	for _, dev := range passthroughDevices {
		if cn := connectOf(demands, dev); cn != nil {
			t.Errorf("%s: unexpected connect demand %v with no active control", dev, *cn)
		}
	}
	if len(demands) != 4 { // 2 inverter restores + 1 battery idle + 1 evse ceiling; no connect demands
		t.Errorf("demand count=%d want 4 (2 inverter restores + 1 battery idle + 1 evse ceiling)", len(demands))
	}
	resolveSane(t, demands)
}

// ── Breach is always nil ────────────────────────────────────────────────────

func TestCSIPPassthrough_NeverReturnsBreach(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{Mode: "full", FullCurrentA: 32})
	ctls := []*orchestrator.CSIPControlState{
		nil,
		maxLimControl(100), // an absurdly tight cap — a diagnosing constraint might flag this
		fixedWControl(-9999),
		connectOnlyControl(false),
	}
	for i, ctl := range ctls {
		_, breach := c.Evaluate(passthroughInput(ctl, midnightPlus(12)), newPassthroughSession())
		if breach != nil {
			t.Errorf("case %d: unexpected breach %+v — passthrough must never diagnose", i, breach)
		}
	}
}

// ── Combined control: all four Base fields set together ────────────────────

func TestCSIPPassthrough_CombinedControl_AllAxesTogether(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{Mode: "full", FullCurrentA: 32})
	ctl := &orchestrator.CSIPControlState{
		Source: "event", MRID: "combo-mrid",
		Base: model.DERControlBase{
			OpModConnect: boolPtr(false),
			OpModMaxLimW: &model.ActivePower{Value: 2000},
			OpModFixedW:  &model.ActivePower{Value: -1500},
		},
	}
	in := passthroughInput(ctl, midnightPlus(12))
	demands, breach := c.Evaluate(in, newPassthroughSession())
	if breach != nil {
		t.Fatalf("unexpected breach: %+v", breach)
	}
	if got := ceilingOf(demands, "inv1"); got != 2000 {
		t.Errorf("inv1 ceiling=%.0f want 2000", got)
	}
	if got := setpointOf(demands, "bat1"); got != -1500 {
		t.Errorf("bat1 setpoint=%.0f want -1500", got)
	}
	if got := evCeilingOf(demands, "evse1"); got != 32 {
		t.Errorf("evse1 ceiling=%.1f want 32 (full mode)", got)
	}
	for _, dev := range passthroughDevices {
		cn := connectOf(demands, dev)
		if cn == nil || *cn != false {
			t.Errorf("%s: connect=%v want false", dev, cn)
		}
	}
	// A device carrying BOTH a bound axis (ceiling/point) AND a connect demand
	// must resolve without panicking or losing either axis.
	desired := Resolve(demands)
	if iv, ok := desired["inv1"].Bound(AxisSolarCeilingW); !ok || iv.Max != 2000 {
		t.Errorf("inv1 resolved ceiling wrong: %+v", desired["inv1"])
	}
	if desired["inv1"].Connect == nil || *desired["inv1"].Connect != false {
		t.Errorf("inv1 resolved connect wrong: %+v", desired["inv1"])
	}
	resolveSane(t, demands)
}

// ── No economic opinion: demands must not vary with time-of-day price ──────

// TestCSIPPassthrough_NoEconomicOpinion_TOUInvariant is the behavioral proof
// backing the ground rule "the constraint must not import TOU/cost-model
// code": NewCSIPPassthrough takes only a GatewayEVSEPolicy — no
// *orchestrator.TOUCostModel, unlike EconomicsConstraint (economics.go's
// NewEconomicsConstraint) — so it has no plumbing to form a price opinion at
// all. Proved behaviorally here: the SAME CSIP control produces the SAME
// solar/battery demands whether evaluated at economics_test.go's peakTime()
// (17:00, inside DefaultTOUCostModel's peak window) or offPeakTime() (03:00).
// The EVSE ceiling is EXPECTED to differ between these two instants — that is
// the user's configured clock SCHEDULE (GatewayEVSEPolicy), not a price
// opinion — so this test deliberately does not compare it.
func TestCSIPPassthrough_NoEconomicOpinion_TOUInvariant(t *testing.T) {
	c := NewCSIPPassthrough(GatewayEVSEPolicy{Mode: "full", FullCurrentA: 32})
	ctl := fixedWControl(4000)

	peakDemands, _ := c.Evaluate(passthroughInput(ctl, peakTime()), newPassthroughSession())
	offPeakDemands, _ := c.Evaluate(passthroughInput(ctl, offPeakTime()), newPassthroughSession())

	if got, want := setpointOf(peakDemands, "bat1"), setpointOf(offPeakDemands, "bat1"); got != want {
		t.Errorf("battery setpoint varies with TOU peak/off-peak (peak=%.0f offpeak=%.0f) — passthrough must have no price opinion", got, want)
	}
	for _, name := range []string{"inv1", "inv2"} {
		if got, want := ceilingOf(peakDemands, name), ceilingOf(offPeakDemands, name); !eqBound(got, want) {
			t.Errorf("%s ceiling varies with TOU peak/off-peak (peak=%.0f offpeak=%.0f)", name, got, want)
		}
	}
}

// ── inWindow unit coverage (direct, beyond the Evaluate-level table above) ──

func TestInWindow(t *testing.T) {
	cases := []struct {
		hour, start, end int
		want             bool
	}{
		{hour: 1, start: 23, end: 7, want: true}, // wraps past midnight
		{hour: 12, start: 23, end: 7, want: false},
		{hour: 23, start: 23, end: 7, want: true},  // inclusive start
		{hour: 7, start: 23, end: 7, want: false},  // exclusive end
		{hour: 10, start: 9, end: 17, want: true},  // non-wrapping
		{hour: 20, start: 9, end: 17, want: false}, // non-wrapping, outside
		{hour: 5, start: 5, end: 5, want: false},   // zero-width window: never
	}
	for _, tt := range cases {
		if got := inWindow(tt.hour, tt.start, tt.end); got != tt.want {
			t.Errorf("inWindow(%d,%d,%d)=%v want %v", tt.hour, tt.start, tt.end, got, tt.want)
		}
	}
}
