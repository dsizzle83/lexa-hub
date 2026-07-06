package constraint

import (
	"fmt"
	"math"
	"sort"
	"time"

	"lexa-hub/internal/orchestrator"
)

// Stack is a priority-ordered set of constraints that together implement
// orchestrator.Optimizer. It runs every constraint's Evaluate, arbitrates the
// resulting demands into per-device Desired state, converts that into an
// orchestrator.Plan, and keeps the worst compliance breach.
//
// It is UNWIRED as of TASK-058: nothing constructs or calls a Stack in the
// engine or cmd/hub — TASK-059's shadow harness is the first caller. Like
// DefaultOptimizer, Optimize is single-goroutine (each constraint's Session is
// unsynchronised per-tick state).
type Stack struct {
	constraints  []Constraint
	sessions     map[string]*Session
	plant        Plant
	tickInterval time.Duration
}

// compile-time proof the Stack is a drop-in Optimizer.
var _ orchestrator.Optimizer = (*Stack)(nil)

// NewStack builds a Stack from a plant model, the engine cadence (0 = tuned),
// and an ordered list of constraints. Each constraint gets one Session keyed by
// its Name(). With zero constraints the Stack returns an empty plan.
func NewStack(plant Plant, tickInterval time.Duration, constraints ...Constraint) *Stack {
	s := &Stack{
		constraints:  constraints,
		sessions:     make(map[string]*Session, len(constraints)),
		plant:        plant,
		tickInterval: tickInterval,
	}
	for _, c := range constraints {
		if _, ok := s.sessions[c.Name()]; !ok {
			s.sessions[c.Name()] = NewSession(c.Name(), tickInterval)
		}
	}
	return s
}

// SessionNames returns the constraint names in the Stack's fixed evaluation
// order. The shadow harness (TASK-059) records these on a divergence so triage
// can see which candidate constraints were live when the stack disagreed with
// the legacy cascade. Order is the construction order, so it is deterministic.
func (s *Stack) SessionNames() []string {
	names := make([]string, 0, len(s.constraints))
	for _, c := range s.constraints {
		names = append(names, c.Name())
	}
	return names
}

// tickSeconds is the wall-clock length of one tick (tuned cadence when unset).
func (s *Stack) tickSeconds() float64 {
	if s.tickInterval <= 0 {
		return tunedTickInterval.Seconds()
	}
	return s.tickInterval.Seconds()
}

// Optimize implements orchestrator.Optimizer: evaluate → arbitrate → emit.
func (s *Stack) Optimize(state orchestrator.SystemState) orchestrator.Plan {
	now := state.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	plan := orchestrator.Plan{Timestamp: now}

	in := Input{State: state, Plant: s.plant, TickSeconds: s.tickSeconds()}

	var demands []Demand
	for _, c := range s.constraints {
		sess := s.sessions[c.Name()]
		if sess == nil { // defensive: a constraint added after construction
			sess = NewSession(c.Name(), s.tickInterval)
			s.sessions[c.Name()] = sess
		}
		ds, breach := c.Evaluate(in, sess)
		demands = append(demands, ds...)
		if breach != nil {
			recordBreach(&plan, breach)
		}
	}

	desired := Resolve(demands)
	emitCommands(&plan, desired)
	return plan
}

// recordBreach keeps the worst (largest-shortfall) breach across constraints,
// mirroring DefaultOptimizer.recordBreach (optimizer.go).
func recordBreach(plan *orchestrator.Plan, b *orchestrator.ComplianceBreach) {
	if plan.Breach == nil || b.ShortfallW > plan.Breach.ShortfallW {
		plan.Breach = b
	}
}

// emitCommands converts arbitrated Desired state into plan commands and appends
// one Decision per resolved conflict. Devices and axes are walked in a fixed
// order so output is byte-reproducible (shadow diffing, TASK-059).
func emitCommands(plan *orchestrator.Plan, desired map[string]Desired) {
	devices := make([]string, 0, len(desired))
	for dev := range desired {
		devices = append(devices, dev)
	}
	sort.Strings(devices)

	for _, dev := range devices {
		d := desired[dev]

		if iv, ok := d.Bound(AxisSolarCeilingW); ok {
			plan.SolarCommands = append(plan.SolarCommands, orchestrator.SolarCommand{
				Name:       dev,
				CurtailToW: iv.Max, // NaN ⇒ no curtailment / restore to nameplate
			})
		}

		_, hasSetpoint := d.Bounds[AxisBatterySetpointW]
		if hasSetpoint || d.Connect != nil {
			cmd := orchestrator.BatteryCommand{Name: dev, SetpointW: math.NaN(), Connect: d.Connect}
			if iv, ok := d.Bound(AxisBatterySetpointW); ok {
				cmd.SetpointW = projectSetpoint(iv)
			}
			plan.BatteryCommands = append(plan.BatteryCommands, cmd)
		}

		if iv, ok := d.Bound(AxisEVSECurrentA); ok && !math.IsNaN(iv.Max) {
			plan.EVSECommands = append(plan.EVSECommands, orchestrator.EVSECommand{
				StationID:   dev,
				ConnectorID: 0, // whole-EVSE; a concrete constraint pins the connector
				MaxCurrentA: math.Max(0, iv.Max),
			})
		}

		for _, c := range d.Conflicts {
			plan.AddDecision(
				"constraint-arbiter",
				c.Reason,
				fmt.Sprintf("%s/%s resolved most-restrictive (tier %s)", c.Device, c.Axis, c.Tier),
			)
		}
	}
}

// projectSetpoint chooses a battery setpoint from a resolved interval.
//   - both sides unbounded (NaN)   → NaN ("leave unchanged")
//   - pinned (Min==Max)            → that value (economics proposed a point)
//   - otherwise                    → least-action: 0 W clamped into [Min,Max]
//
// The least-action default (idle if idle is admissible, else the nearest bound)
// keeps the skeleton conservative; a concrete economics constraint pins a point
// so this branch is not the operating path once wired (TASK-060+).
func projectSetpoint(iv Interval) float64 {
	loNaN, hiNaN := math.IsNaN(iv.Min), math.IsNaN(iv.Max)
	switch {
	case loNaN && hiNaN:
		return math.NaN()
	case !loNaN && !hiNaN && iv.Min == iv.Max:
		return iv.Min
	}
	lo := iv.Min
	if loNaN {
		lo = math.Inf(-1)
	}
	hi := iv.Max
	if hiNaN {
		hi = math.Inf(1)
	}
	return math.Max(lo, math.Min(hi, 0)) // clamp 0 into [lo, hi]
}
