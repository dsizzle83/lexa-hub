package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
)

// batteryReserveDrainTicks is how many consecutive ticks a pack may read as
// discharging at/below its SOC reserve — or as discharging against a commanded
// charge — before the constraint force-disconnects it. Ported verbatim from
// optimizer.go:1448 (batteryReserveDrainTicks = 3). Confirming over a few ticks
// rides out a single telemetry glitch (audit: battery-wrong-sign; HIL meter
// blips — HARD preserve, TASK-062 preservation ledger).
const batteryReserveDrainTicks = 3

// BatterySafetyConstraint is the SAFETY-tier home of the battery direction
// protection that DefaultOptimizer.checkBatterySafety + EvaluateSafety
// (optimizer.go:1450-1596) implement today. It completes W1/R4's state
// consolidation: the three remaining scattered guard maps
// (battDrainTicks / battWrongDirTicks / lastBattCmd) become one typed,
// reset-specified BatterySafetySession with a single owner.
//
// It is a FAITHFUL, self-contained port — like the 060/061 compliance
// constraints, it is UNWIRED as of TASK-062. The legacy cascade
// (checkBatterySafety on the economic tick, EvaluateSafety on the fast tick)
// stays LIVE and authoritative; this constraint's completeness is what makes
// TASK-063 (economic isolation) and TASK-066 (legacy-cascade deletion) safe to
// review. See the wiring handoff note below.
//
// # Two entry points, ONE state owner (step-2 DECISION, recorded in 02 AD-007)
//
// Battery safety is evaluated on BOTH loops, exactly as today:
//
//   - Evaluate (economic tick, TierSafety, arbiter-mediated): the FULL logic —
//     both debounced counters plus the critical fast trip — matching
//     checkBatterySafety. Runs through the Stack/arbiter with the other tiers so
//     its disconnect wins over any economic setpoint (safety is Tier 0).
//
//   - EvaluateFast (fast protection tick): ONLY the unambiguous, act-now
//     criticalBatteryInversion predicate, matching legacy EvaluateSafety. It
//     BYPASSES the arbiter and the demand pipeline BY DESIGN: adding arbitration
//     to a 1 s protective reflex buys latency and allocation for zero benefit,
//     and the trip it issues (force-disconnect) already dominates every tier.
//     Keeping it a direct, allocation-light call preserves the ADR-0001 Tier-1
//     latency contract. It reads the SAME session on the SAME control goroutine
//     (engine.go run() serialises tick()/safetyTick()), so it takes no lock — a
//     mutex would mask a future contract violation instead of failing -race.
//
// Both counters and lastCmdW live in ONE BatterySafetySession, so there is
// exactly one owner for the state whichever loop observes the fault first.
//
// # Wiring handoff to TASK-063 (documented ambiguity, not forced here)
//
// Legacy checkBatterySafety infers "charge commanded" from THIS tick's freshly
// built economic plan first (chargeCommandedFor(plan), optimizer.go:1477-1479),
// falling back to lastBattCmd only when the plan is absent. A pre-arbitration
// TierSafety constraint cannot see this tick's sibling/economic demands during
// its Evaluate (safety resolves FIRST), so both entry points here source
// commanded intent from the session's lastCmdW — the LAST committed command.
// That is a faithful, well-defined, lock-free signal, but it lags the legacy
// economic-tick check by up to one tick on the wrong-direction path. Closing
// that gap — running the economic-tick battery-safety pass AFTER arbitration so
// it sees this tick's resolved battery setpoint, or feeding the arbiter's
// resolved commands back in — is a TASK-063 economics/ordering decision, not a
// state-consolidation one. The critical fast path and the reserve-drain path
// (which have NO command dependency, or use lastBattCmd exactly as legacy
// EvaluateSafety does) are bit-faithful today.
type BatterySafetyConstraint struct {
	sess BatterySafetySession
	// socReserve is the minimum SOC the protection defends (percent). Ported from
	// DefaultOptimizer.SOCReserve (optimizer.go:94, default 20). Parameterisation
	// through config is TASK-064; the wiring layer sets it at construction.
	socReserve float64
}

// compile-time proof BatterySafetyConstraint satisfies the Constraint interface.
var _ Constraint = (*BatterySafetyConstraint)(nil)

// NewBatterySafetyConstraint builds the constraint with an empty session and the
// given SOC reserve (percent). Pass DefaultOptimizer.SOCReserve to match legacy.
func NewBatterySafetyConstraint(socReserve float64) *BatterySafetyConstraint {
	return &BatterySafetyConstraint{sess: newBatterySafetySession(), socReserve: socReserve}
}

// Name is the stable identity; it keys the Session and appears as Demand.Source.
func (c *BatterySafetyConstraint) Name() string { return "battery-safety" }

// Tier places battery direction protection in the SAFETY band — its disconnects
// win over every compliance and economics demand.
func (c *BatterySafetyConstraint) Tier() Tier { return TierSafety }

// criticalBatteryInversion reports the unambiguous, act-now battery fault: a pack
// commanded to charge but measured discharging (>complianceBreachW) while at/near
// its SOC reserve (≤ reserve+5%). No correct command produces this, and at a full
// discharge the pack crosses the reserve floor in seconds — so it warrants an
// immediate disconnect with no debounce, on whichever loop observes it first.
// Ported verbatim from optimizer.go:1469-1472 (audit: battery-wrong-sign).
func criticalBatteryInversion(powerW, soc, socReserve float64, chargeCommanded bool) bool {
	return chargeCommanded && powerW > exportComplianceBreachW &&
		!math.IsNaN(soc) && soc <= socReserve+5
}

// RecordCommands records the setpoints an economic tick committed so the fast
// protection path can infer commanded direction between ticks. Ports the
// lastBattCmd write loop at optimizer.go:378-387 field-for-field: only finite
// setpoints are recorded (a NaN "leave unchanged" command carries no direction),
// and entries are overwritten, never pruned. The wiring layer (TASK-063) calls
// this with the arbitrated plan's BatteryCommands after each Optimize.
func (c *BatterySafetyConstraint) RecordCommands(cmds []orchestrator.BatteryCommand) {
	for _, cmd := range cmds {
		if !math.IsNaN(cmd.SetpointW) {
			c.sess.lastCmdW[cmd.Name] = cmd.SetpointW
		}
	}
}

// Evaluate ports checkBatterySafety (optimizer.go:1520-1596): the two debounced
// fault detectors plus the critical fast trip, emitting a force-disconnect demand
// per pack that trips. It is the economic-tick, arbiter-mediated entry point.
//
// The tick threshold is scaled through the shared Session helper (058) so the
// wall-clock debounce is constant across FAST/STOCK cadences, exactly as
// DefaultOptimizer.scaleTicks(batteryReserveDrainTicks) does (optimizer.go:1563).
func (c *BatterySafetyConstraint) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	sess := &c.sess
	threshold := s.ScaleTicks(batteryReserveDrainTicks)

	var demands []Demand
	for _, b := range in.State.Batteries {
		// Prune + skip offline / unmeasurable packs (optimizer.go:1527-1532).
		if !b.Connected || math.IsNaN(b.SOC) {
			sess.prune(b.Name)
			continue
		}

		// Check 1: reserve drain (SOC-gated) — optimizer.go:1534-1540.
		if b.PowerW > exportComplianceBreachW && b.SOC <= c.socReserve {
			sess.drainTicks[b.Name]++
		} else {
			sess.drainTicks[b.Name] = 0
		}

		// Check 2: wrong direction — commanded charge but measuring discharge
		// (optimizer.go:1542-1552). Commanded intent comes from lastCmdW (the last
		// committed command); see the type doc's TASK-063 handoff note.
		chargeCommanded := sess.chargeCommanded(b.Name)
		if chargeCommanded && b.PowerW > exportComplianceBreachW {
			sess.wrongDirTicks[b.Name]++
		} else {
			sess.wrongDirTicks[b.Name] = 0
		}

		// Trip conditions — optimizer.go:1563-1569.
		criticalTrip := criticalBatteryInversion(b.PowerW, b.SOC, c.socReserve, chargeCommanded)
		drainTrip := sess.drainTicks[b.Name] >= threshold
		wrongDirTrip := sess.wrongDirTicks[b.Name] >= threshold
		if !criticalTrip && !drainTrip && !wrongDirTrip {
			continue
		}
		demands = append(demands, c.disconnectDemands(b.Name)...)
	}
	return demands, nil
}

// EvaluateFast is the Tier-1 fast protection pass (ADR-0001). It issues ONLY the
// immediate, unambiguous protective disconnect (critical sign-inversion at
// reserve), so a mis-wired pack is ceased in ~1 tick instead of waiting a full
// economic interval. Ports EvaluateSafety (optimizer.go:1497-1518) — deliberately
// stateless (no debounce counters), reading only the session's lastCmdW for
// commanded intent, and BYPASSING the arbiter/demand pipeline (see the type doc).
// Returns the raw disconnect demands; the caller (fast loop) actuates them
// directly without arbitration.
func (c *BatterySafetyConstraint) EvaluateFast(state orchestrator.SystemState) []Demand {
	var demands []Demand
	for _, b := range state.Batteries {
		if !b.Connected {
			continue
		}
		chargeCommanded := c.sess.chargeCommanded(b.Name)
		if !criticalBatteryInversion(b.PowerW, b.SOC, c.socReserve, chargeCommanded) {
			continue
		}
		demands = append(demands, c.disconnectDemands(b.Name)...)
	}
	return demands
}

// disconnectDemands is the force-disconnect a trip issues: pin the setpoint to
// 0 W and command disconnect. Ports the BatteryCommand{SetpointW:0, Connect:&f}
// both entry points build (optimizer.go:1509-1511, :1572-1582). Through the
// arbiter/emitCommands this yields exactly that BatteryCommand; on the fast path
// the caller maps it directly.
func (c *BatterySafetyConstraint) disconnectDemands(name string) []Demand {
	return []Demand{
		PointDemand(name, AxisBatterySetpointW, 0, TierSafety, c.Name()),
		ConnectDemand(name, false, TierSafety, c.Name()),
	}
}
