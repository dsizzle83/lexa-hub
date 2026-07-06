// Package constraint is the priority-ordered constraint controller that
// AD-007 (review W1/R4) uses to replace DefaultOptimizer's implicit rule
// cascade. It is I/O-free (05 §1: internal/orchestrator/* is the defended
// I/O-free layer) and, as of TASK-058, entirely UNWIRED — the engine and
// cmd/hub still run the legacy cascade. TASK-059's shadow harness is the first
// caller; TASK-060/061/062 migrate the real rules into concrete constraints.
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
// # No I/O
//
// Evaluate and Resolve are pure: no time.Now(), no logging side effects, no
// network. The engine cadence and tick length are injected (Session tick
// interval, Input.TickSeconds). This keeps the layer unit-testable and
// deterministic — shadow diffing (TASK-059) requires reproducible output.
package constraint
