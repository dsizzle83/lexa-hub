package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// R4 pre-flip investigation (2026-07-09): the constraint-stack shadow soak
// showed a NEW divergence class starting ~18:30 EDT, ~1/min through the TOU
// evening peak (38+ events): battery-setpoint-w / connect diverge with
// candidate author=battery-safety while legacy discharges for TOU peak
// shaving. These tests root-cause and pin that divergence.
//
// ── Root cause ──────────────────────────────────────────────────────────────
//
// Legacy's applyExportLimitRule (Rule 3, optimizer.go:672-1159) ALWAYS runs
// its EV soft-start/ratchet block once an export limit is active — even when
// the site is nowhere near the cap — and unconditionally subtracts the EV's
// commanded power from the surplusW that later flows into Rule 4
// (self-consumption): `surplusW -= newCurrentA*voltage` (optimizer.go:1008).
// That depletion is legacy-cascade-only interleaving. The constraint Stack's
// EconomicsConstraint computes surplusW from RAW state instead
// (economics.go economicSurplusW, no analogous depletion) — a divergence the
// TASK-064 seam review (notes/TASK-063-seam-review.md §3, "On-cap economic
// ticks") already characterizes as EXPECTED for the economics SETPOINT
// itself.
//
// In the bench state (battery-0 SOC=62.37%, PowerW=+1190 discharge,
// solar=3173W, EVSE session 1380W, grid_net_w=-2730, 5000W export cap, TOU
// peak):
//   - legacy: Rule 3's EV block leaves surplusW = 1350 - 1380 = -30 (below
//     the 100W excess-solar threshold) → Rule 4 declines → Rule 5 (TOU) fires,
//     discharging at dischargeCapW = exportLimitW*(1-margin) - exportNowW
//     = 5000*0.8 - 2730 = 1270W (exactly the observed legacy=1270W).
//   - candidate: EconomicsConstraint sees the undepleted raw surplusW=1350
//     (>threshold) → Rule 4 (self-consumption) commits a -160W CHARGE
//     proposal BEFORE Rule 5 can run (the ecoPlan.hasBattery guard,
//     economics.go:443 `if e.hasBattery(b.Name) { continue }`).
//
// Because legacy actuates the real world in shadow mode, the battery's
// MEASURED power keeps reading legacy's sustained discharge while
// BatterySafetyConstraint.PostArbitrate reads the CANDIDATE's own resolved
// charge command for "commanded intent" (batterysafety.go:209-215). Its
// wrong-direction predicate has NO SOC gate — unlike criticalBatteryInversion
// and the reserve-drain check, both of which require SOC at/near reserve:
//
//	// Check 2: wrong direction — commanded charge but measuring discharge
//	// (optimizer.go:1542-1552).
//	cc := chargeCommanded(b.Name)
//	if cc && b.PowerW > exportComplianceBreachW {
//		sess.wrongDirTicks[b.Name]++
//	} ...
//	wrongDirTrip := sess.wrongDirTicks[b.Name] >= threshold
//	(batterysafety.go:175-180, :185)
//
// So a healthy, high-SOC (62.37%) pack trips after batteryReserveDrainTicks
// (3) debounced ticks — purely because the "commanded" side of the
// comparison comes from a different, non-actuating optimizer than the
// "measured" side.
//
// ── Verdict ──────────────────────────────────────────────────────────────
//
// (a) SHADOW ARTIFACT, not a real bug: TestBatterySafety_SelfConsistentFeedbackNeverTrips
// and its minimal twin TestBatterySafetyConstraint_PostArbitrateNoTripWhenMeasuredConsistentWithOwnCommand
// prove that feeding the CANDIDATE's own resolved setpoint back as next-tick
// measured power (the self-consistent, post-flip world) never trips — the
// pack settles into a stable charge equilibrium instead. The predicate is
// working exactly as designed (catch a REAL commanded-vs-measured
// disagreement); shadow mode manufactures a disagreement that has no
// real-world referent because two different, uncoordinated optimizers are
// each looking at "their own intent" vs "the other one's actuation".
//
// ── Why TASK-062/063 parity did not catch this ──────────────────────────────
//
// batterysafety_test.go's PostArbitrate-adjacent coverage (stack_safety_test.go)
// only exercises SOC-near-reserve fixtures (SOC=21-22%, for the immediate
// criticalBatteryInversion path) or a straightforward off-cap self-consumption
// charge — always SELF-CONSISTENT by construction (one Stack, hand-picked
// commanded+measured pair), never a genuinely different legacy actuator.
// TestEconomics_ShadowParityOffCap (economics_test.go), the only test that
// runs the real Wrap(legacy, stack) harness, is explicitly OFF-CAP — TASK-063's
// completion note defers the on-cap/TOU-peak bench soak entirely. The seam
// review (notes/TASK-063-seam-review.md §1) analyzed "Economics discharge vs
// Safety reserve-drain" and concluded battery-safety is bit-faithful — true
// for the post-arbitration LOGIC itself, but that analysis (and §3's
// "bit-faithful... reads this-tick resolved setpoint" claim) implicitly
// assumes a single coherent actuator. Nobody traced the SECOND-ORDER
// consequence: an EXPECTED, already-characterized economics-setpoint
// divergence (TASK-064's finding) cascading through the SOC-blind
// wrong-direction check into a SAFETY-tier disconnect divergence. No existing
// test combines (1) an on-cap, TOU-peak, EVSE-active state that actually
// triggers the Rule-3-depletion-vs-raw-surplus split with (2) the real
// Wrapper driving legacy and the full Stack (safety+compliance+economics)
// side by side for ≥3 ticks.

// ── full-stack reproduction (item 2/3: exact divergence, via the real Wrapper) ──

// TestBatterySafety_TOUPeakEVSoftStartCascadesToShadowTrip reproduces the
// observed bench divergence end to end: legacy TOU-discharges at ~1270W,
// candidate economics proposes a charge, and after batteryReserveDrainTicks
// the candidate's battery-safety constraint overrides to a force-disconnect,
// which the Wrapper records as a divergence authored by "battery-safety".
func TestBatterySafety_TOUPeakEVSoftStartCascadesToShadowTrip(t *testing.T) {
	st := orchestrator.SystemState{
		Timestamp: peakTime(), // 17:00 local — inside DefaultTOUCostModel's peak window
		Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 3173, MaxW: 5000, Connected: true, Energized: true}},
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 1190, SOC: 62.37, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		EVSEs:     []orchestrator.EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true, MaxCurrentA: 32, VoltageV: 230, PowerW: 1380}},
		Grid:      orchestrator.GridState{NetW: -2730, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		// The bench default control: a 5000W export cap (DDERC-SP-001).
		CSIPControl: expLimControl(5000),
	}

	legacy := benchLegacy()
	stack := benchFullStack(st)

	var divs []Divergence
	tickN := 0
	w := Wrap(legacy, stack, Options{
		// Advance the injected clock a full hour per tick so the per-signature
		// rate limiter (default 1 min) never suppresses a tick's record —
		// every divergent tick in this short sequence is observable.
		Now: func() time.Time {
			ts := time.Unix(int64(tickN)*3600, 0)
			tickN++
			return ts
		},
		OnDiverge: func(d Divergence) { divs = append(divs, d) },
	})

	const ticks = batteryReserveDrainTicks + 1
	var last orchestrator.Plan
	for i := 0; i < ticks; i++ {
		last = w.Optimize(st)
		st.Timestamp = st.Timestamp.Add(3 * time.Second)
	}

	// Sanity: legacy really does TOU-discharge at the exact dischargeCapW this
	// root-cause analysis rests on (5000*0.8 - 2730 = 1270).
	if legSet := planBatterySetpoint(last, "bat"); math.Abs(legSet-1270) > 1 {
		t.Fatalf("legacy battery setpoint = %.0f, want ~1270 (TOU dischargeCapW = exportLimitW*(1-margin)-exportNowW)", legSet)
	}

	if w.Divergences() == 0 {
		t.Fatal("expected at least one recorded divergence")
	}

	var tripped bool
	for _, d := range divs {
		for _, a := range d.Axes {
			if a.Device == "bat" && a.Axis == AxisConnect.String() && a.Candidate == "disconnect" {
				tripped = true
				if a.Author != "battery-safety" {
					t.Errorf("connect divergence author = %q, want battery-safety", a.Author)
				}
			}
		}
	}
	if !tripped {
		t.Fatalf("expected a battery-safety disconnect divergence within %d ticks; divergences=%+v", ticks, divs)
	}
}

// TestBatterySafety_SelfConsistentFeedbackNeverTrips is the verdict-(a) proof:
// re-run the SAME initial state and the SAME candidate Stack, but simulate a
// SELF-CONSISTENT world — the candidate is the SOLE authoritative actuator
// (post-flip), so each tick's measured battery power is fed from the
// candidate's OWN previous resolved setpoint instead of legacy's. With no
// second, uncoordinated optimizer driving the world, the commanded-vs-measured
// mismatch that trips battery-safety in the shadow test above cannot arise:
// the pack converges to a stable charge equilibrium and is never
// force-disconnected.
func TestBatterySafety_SelfConsistentFeedbackNeverTrips(t *testing.T) {
	st := orchestrator.SystemState{
		Timestamp:   peakTime(),
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 3173, MaxW: 5000, Connected: true, Energized: true}},
		Batteries:   []orchestrator.BatteryState{{Name: "bat", PowerW: 1190, SOC: 62.37, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		EVSEs:       []orchestrator.EVSEState{{StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true, MaxCurrentA: 32, VoltageV: 230, PowerW: 1380}},
		Grid:        orchestrator.GridState{NetW: -2730, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: expLimControl(5000),
	}
	stack := benchFullStack(st)

	const ticks = batteryReserveDrainTicks + 5
	for i := 0; i < ticks; i++ {
		plan := stack.Optimize(st)
		for _, c := range plan.BatteryCommands {
			if c.Name != "bat" {
				continue
			}
			if c.Connect != nil && !*c.Connect {
				t.Fatalf("tick %d: battery-safety force-disconnected a battery whose measured power is SELF-CONSISTENT with its own commanded setpoint (no shadow mismatch possible post-flip); plan=%+v", i, plan)
			}
			if !math.IsNaN(c.SetpointW) {
				// Instant full actuation by the sole authoritative candidate:
				// next tick's measured power IS this tick's commanded setpoint.
				st.Batteries[0].PowerW = c.SetpointW
			}
		}
		st.Timestamp = st.Timestamp.Add(3 * time.Second)
	}
}

// ── minimal, economics-independent pin (exact predicate, item 1) ────────────

// TestBatterySafetyConstraint_PostArbitrateTripsOnShadowCommandMismatch isolates
// the tripping predicate from the economics machinery above: PostArbitrate's
// chargeCommanded closure reads THIS tick's arbitrated plan
// (batterysafety.go:209-215), and evaluateTrips' wrong-direction check
// (batterysafety.go:173-180) has no SOC gate, unlike criticalBatteryInversion
// and the reserve-drain check. Feeding it a commanded charge each tick against
// a measured discharge FAR from reserve (SOC 62.37%, where neither SOC-gated
// predicate can fire) reproduces the trip in isolation.
func TestBatterySafetyConstraint_PostArbitrateTripsOnShadowCommandMismatch(t *testing.T) {
	c, s := newBattSafetyPair()
	in := benchInput(battState("bat", 1190, 62.37))

	// Sanity: neither SOC-gated predicate can fire here — this isolates the
	// SOC-blind wrong-direction check.
	if criticalBatteryInversion(1190, 62.37, benchSOCReserve, true) {
		t.Fatal("test setup invalid: criticalBatteryInversion fired — scenario does not isolate the wrong-direction-only path")
	}

	var plan orchestrator.Plan
	for i := 0; i < batteryReserveDrainTicks; i++ {
		// A fresh economics-only proposal each tick, exactly as the Stack
		// would resolve it (arbiter output feeding PostArbitrate).
		plan = orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{{Name: "bat", SetpointW: -160}}}
		c.PostArbitrate(in, s, &plan)
	}
	cmd := plan.BatteryCommands[0]
	if cmd.Connect == nil || *cmd.Connect != false || cmd.SetpointW != 0 {
		t.Fatalf("expected force-disconnect {0,false} at the debounce threshold on a healthy (SOC 62.37%%) pack fed a shadow-mismatched commanded-charge/measured-discharge state each tick; got %+v", cmd)
	}
}

// TestBatterySafetyConstraint_PostArbitrateNoTripWhenMeasuredConsistentWithOwnCommand
// is the minimal twin of TestBatterySafety_SelfConsistentFeedbackNeverTrips:
// same commanded charge every tick, but measured power now agrees with it
// (charging, not discharging) — the self-consistent, single-actuator world.
// The wrong-direction counter never advances.
func TestBatterySafetyConstraint_PostArbitrateNoTripWhenMeasuredConsistentWithOwnCommand(t *testing.T) {
	c, s := newBattSafetyPair()
	for i := 0; i < batteryReserveDrainTicks+2; i++ {
		plan := orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{{Name: "bat", SetpointW: -160}}}
		in := benchInput(battState("bat", -160, 62.37)) // measured charging, matching the command
		c.PostArbitrate(in, s, &plan)
		if plan.BatteryCommands[0].Connect != nil {
			t.Fatalf("tick %d: tripped on a pack whose measured power matches its own commanded charge (no shadow mismatch possible)", i)
		}
	}
}
