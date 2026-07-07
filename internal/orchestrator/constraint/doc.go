// Package constraint is the priority-ordered constraint controller that
// AD-007 (review W1/R4) uses to replace DefaultOptimizer's implicit rule
// cascade. It is I/O-free (05 §1: internal/orchestrator/* is the defended
// I/O-free layer). As of TASK-063 the WHOLE controller is ported across all three
// tiers (safety + compliance + economics) and runs in SHADOW: TASK-059's Wrapper
// dual-runs it against the legacy cascade every economic tick and diffs the
// outputs, but the legacy cascade stays authoritative until the soak-gated flip.
// The migration order was TASK-058 skeleton → 059 shadow harness → 060 export →
// 061 import/gen → 062 battery safety → 063 economics + full-stack wiring.
//
// # The ladder
//
// Every constraint declares a Tier. Resolution is strictly priority-ordered:
//
//	TierSafety  >  TierCompliance  >  TierEconomics
//
// Safety demands win over compliance demands, which win over economics. This
// makes the ordering EXPLICIT and testable, replacing the cascade's "a later
// rule silently overwrites an earlier one" (the guard×guard interaction that
// review W1 named the dominant defect source).
//
// # Constraints narrow, economics propose
//
// Demands are modelled as BOUNDS (an admissible [Min, Max] interval per
// actuator axis), not commands. Arbitration is interval intersection with a
// hard invariant: a lower tier may only NARROW a bound, never widen it —
// economics can never relax a compliance ceiling. Safety and compliance
// constraints tighten the admissible set; economics proposes a working point
// inside it (a point-demand, Min==Max). The Arbiter enforces narrowing-only
// structurally (it always intersects), so the property holds by construction,
// not merely by test.
//
// # Sessions are constraint-owned
//
// Each constraint instance gets exactly one Session for its inter-tick state,
// replacing the 9+ guard fields scattered across DefaultOptimizer
// (expGuard/impGuard/genGuard/expOverTicks/…). This package ships only the
// shared Session scaffolding (naming, tick scaling, reset hooks). Concrete
// constraints embed Session and add a TYPED state struct — never an untyped
// Counters map. Session RESET semantics are load-bearing and owned by each
// constraint (a rewritten CSIP cap is a new controller session but the SAME
// compliance obligation — see optimizer.go's expOverTicks note and TASK-060).
//
// # Adaptive detection windows
//
// A compliance constraint sizes its breach-detection window from the per-device
// plant model (control latency + meter lag) rather than a fixed tick constant —
// see DetectionWindowTicks / Plant.ExportDetectionWindowTicks. This is the
// capability AD-007 exists to enable: the fixed exportBreachTicks=3 (~9 s) races
// the ~11 s oracle boundary on battery-charge-disabled (M2 soak, 2026-07-03);
// deriving the window from real plant latency is the fix (concrete use:
// TASK-060).
//
// # Economics propose, constraints dispose (TASK-063 seam)
//
// The economic rules — CSIP fixed dispatch (a TARGET, not a limit — legacy Rule 2,
// above plan, below disconnect), DP plan-following, self-consumption, TOU peak
// discharge, and EV allocation — live in ONE EconomicsConstraint at TierEconomics.
// They only ever PROPOSE: every setpoint is a PointDemand the arbiter intersects
// UNDER the safety/compliance tiers. A proposal can never relax, flip, or step
// outside a higher bound — the tier-aware Resolve keeps the higher tier's interval
// and clamps the economics point into it. So economics is structurally unable to
// violate a CSIP cap; it is not merely told not to. Internal economic precedence
// (fixed-dispatch > plan > self-use/TOU, and plan suppresses the reactive rules)
// is reproduced INSIDE the one constraint (an ecoPlan tracks committed devices),
// mirroring the legacy if-nesting so a full-stack shadow diff isolates tier-seam
// changes from intra-economics changes. What economics CANNOT see — the shared
// surplusW/battery state the compliance rules mutate BETWEEN the economic rules in
// the cascade — is the source of the on-cap shadow divergence characterised for
// TASK-064; off-cap (the compliance rules are no-ops) economics is faithful.
//
// # Battery safety runs POST-arbitration (TASK-063 ordering)
//
// Battery direction protection (TASK-062) is a SAFETY-tier constraint, but the
// Stack runs it AFTER the arbiter (PostArbitrate), not in the pre-arbitration
// demand pass. This is the ordering decision TASK-062 deferred: running last lets
// its wrong-direction/critical checks read THIS tick's RESOLVED battery setpoint
// for commanded-charge intent — exactly like legacy checkBatterySafety's
// chargeCommandedFor(plan) — closing the ≤1-tick lag a pre-arbitration read of the
// last-committed command would carry. A trip overrides the resolved command with a
// force-disconnect that dominates every tier (safety runs last), and the FINAL
// commands are recorded (RecordCommands) so the Tier-1 fast loop (EvaluateFast,
// bypassing the arbiter) can infer direction between economic ticks.
//
// # No I/O
//
// Evaluate and Resolve are pure: no time.Now(), no logging side effects, no
// network. The engine cadence and tick length are injected (Session tick
// interval, Input.TickSeconds). This keeps the layer unit-testable and
// deterministic — shadow diffing (TASK-059) requires reproducible output.
package constraint
