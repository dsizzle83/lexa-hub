package constraint

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
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

	// lastAuthors is the FIX-F per-tick authorship snapshot from the most
	// recent Optimize call: axisKey(device, axis) → the Constraint.Name() that
	// authored that device's axis value this tick. Single-goroutine state,
	// like sessions — rebuilt wholesale every Optimize call (never mutated in
	// place), so returning the map by reference from AxisAuthors is safe as
	// long as the caller reads it on the same control goroutine before the
	// next Optimize call, exactly the Wrapper's existing contract for
	// SessionNames().
	lastAuthors map[string]string
}

// compile-time proof the Stack is a drop-in Optimizer AND, once a fast-safety
// constraint is wired, a SafetyEvaluator for the engine's protection loop.
var (
	_ orchestrator.Optimizer       = (*Stack)(nil)
	_ orchestrator.SafetyEvaluator = (*Stack)(nil)
)

// postArbiter is an optional constraint capability: a pass that runs AFTER the
// arbiter has resolved every tier, so it sees THIS tick's resolved commands.
// Battery safety uses it (TASK-063) to read this tick's arbitrated battery
// setpoint for commanded-charge intent — closing the ≤1-tick wrong-direction lag
// TASK-062 left — and to override a tripped pack with a force-disconnect that
// dominates every tier, mirroring legacy checkBatterySafety running LAST on the
// built plan. A postArbiter's Evaluate is NOT called by the Stack: it authors its
// effect entirely in PostArbitrate.
type postArbiter interface {
	Constraint
	PostArbitrate(in Input, s *Session, plan *orchestrator.Plan)
}

// fastSafetyEvaluator is an optional constraint capability: the Tier-1 fast
// protection reflex (ADR-0001), evaluated on the short safety cadence, BYPASSING
// the arbiter. The Stack aggregates every wired fast-safety constraint into its
// EvaluateSafety plan.
type fastSafetyEvaluator interface {
	EvaluateFast(state orchestrator.SystemState) []Demand
}

// NewStack builds a Stack from a plant model, the engine cadence (0 = tuned),
// and an ordered list of constraints. Each constraint gets one Session keyed by
// its Name(). With zero constraints the Stack returns an empty plan.
func NewStack(plant Plant, tickInterval time.Duration, constraints ...Constraint) *Stack {
	s := &Stack{
		constraints:  constraints,
		sessions:     make(map[string]*Session, len(constraints)),
		plant:        plant,
		tickInterval: tickInterval,
		lastAuthors:  map[string]string{},
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

// AxisAuthors returns the FIX-F per-tick authorship snapshot from the last
// Optimize call: axisKey(device, axis) → the constraint Name() that authored
// that device's axis value this tick (Desired.Author, plus any post-arbiter
// override — battery safety's force-disconnect always attributes to
// "battery-safety" for the axes it actually overrides). The Wrapper
// (shadow.go) reads this to fill AxisDivergence.Author and to decide which
// axes active-mode composition may take from the candidate plan. Empty before
// the first Optimize call.
func (s *Stack) AxisAuthors() map[string]string {
	return s.lastAuthors
}

// tickSeconds is the wall-clock length of one tick (tuned cadence when unset).
func (s *Stack) tickSeconds() float64 {
	if s.tickInterval <= 0 {
		return tunedTickInterval.Seconds()
	}
	return s.tickInterval.Seconds()
}

// session returns the constraint's persistent Session, creating it defensively
// if a constraint was added after construction.
func (s *Stack) session(c Constraint) *Session {
	sess := s.sessions[c.Name()]
	if sess == nil {
		sess = NewSession(c.Name(), s.tickInterval)
		s.sessions[c.Name()] = sess
	}
	return sess
}

// Optimize implements orchestrator.Optimizer: evaluate → arbitrate → emit, then
// a post-arbitration pass for constraints that must see this tick's resolved
// commands (battery safety, TASK-063).
func (s *Stack) Optimize(state orchestrator.SystemState) orchestrator.Plan {
	now := state.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	plan := orchestrator.Plan{Timestamp: now}

	in := Input{State: state, Plant: s.plant, TickSeconds: s.tickSeconds()}

	var demands []Demand
	var post []postArbiter
	for _, c := range s.constraints {
		// A post-arbiter authors its whole effect after the arbiter; its Evaluate
		// is intentionally NOT run in the demand pass (it would double-count its
		// debounce counters and pre-date its command read by a tick).
		if pa, ok := c.(postArbiter); ok {
			post = append(post, pa)
			continue
		}
		ds, breach := c.Evaluate(in, s.session(c))
		demands = append(demands, ds...)
		if breach != nil {
			recordBreach(&plan, breach)
		}
	}

	desired := Resolve(demands)
	emitCommands(&plan, desired)

	// FIX-F: snapshot per-axis authorship from the arbiter's fold BEFORE the
	// post-arbitration pass runs, so a postArbiter's override (below) can be
	// distinguished from — and takes priority over — the demand-pipeline
	// authorship the arbiter already resolved.
	authors := make(map[string]string, len(desired))
	for dev, d := range desired {
		for axis, src := range d.Authors {
			if src != "" {
				authors[axisKey(dev, axis)] = src
			}
		}
	}

	// Post-arbitration tier (SAFETY, after economics+compliance are resolved):
	// battery safety reads this tick's arbitrated battery setpoints, may override
	// a tripped pack with a force-disconnect, and records the FINAL commands for
	// the fast protection loop.
	for _, pa := range post {
		before := snapshotBatteryCommands(plan.BatteryCommands)
		pa.PostArbitrate(in, s.session(pa), &plan)
		attributePostArbiterAuthorship(before, plan.BatteryCommands, pa.Name(), authors)
	}
	s.lastAuthors = authors

	// WS-4.3 parity with the legacy cascade (optimizer.go's blanket stamp):
	// every command carries the active control's MRID so device-level
	// non-convergence evidence survives the flip — a composed plan that
	// APPENDS a candidate-only command (no legacy twin to inherit from) and,
	// after TASK-066 deletes the cascade, every command outright, would
	// otherwise regress to MRID:"" and be dropped by breach.go by design.
	if state.CSIPControl != nil {
		for i := range plan.BatteryCommands {
			plan.BatteryCommands[i].MRID = state.CSIPControl.MRID
		}
		for i := range plan.SolarCommands {
			plan.SolarCommands[i].MRID = state.CSIPControl.MRID
		}
		for i := range plan.EVSECommands {
			plan.EVSECommands[i].MRID = state.CSIPControl.MRID
		}
	}
	return plan
}

// snapshotBatteryCommands is a minimal comparable view of a plan's battery
// commands, keyed by device name, for the FIX-F post-arbiter authorship diff
// below.
func snapshotBatteryCommands(cmds []orchestrator.BatteryCommand) map[string]orchestrator.BatteryCommand {
	m := make(map[string]orchestrator.BatteryCommand, len(cmds))
	for _, c := range cmds {
		m[c.Name] = c
	}
	return m
}

// attributePostArbiterAuthorship marks every battery-setpoint-w/connect axis a
// postArbiter's PostArbitrate call actually changed (added or overwrote,
// relative to the pre-call snapshot) as authored by that postArbiter's Name()
// (FIX-F). PostArbitrate bypasses the demand pipeline entirely (stack.go's
// postArbiter doc) — battery safety's force-disconnect override writes
// straight into plan.BatteryCommands — so authorship for it must be observed
// by EFFECT rather than read off a Demand/Desired.Author the arbiter never
// saw. A pack the postArbiter did not touch this tick (not tripped) is
// unaffected: its authorship, if any, stays whatever the arbiter fold already
// recorded.
func attributePostArbiterAuthorship(before map[string]orchestrator.BatteryCommand, after []orchestrator.BatteryCommand, name string, authors map[string]string) {
	for _, c := range after {
		b, existed := before[c.Name]
		setpointChanged := !math.IsNaN(c.SetpointW) && (!existed || b.SetpointW != c.SetpointW)
		connectChanged := c.Connect != nil && (!existed || b.Connect == nil || *b.Connect != *c.Connect)
		if setpointChanged {
			authors[axisKey(c.Name, AxisBatterySetpointW)] = name
		}
		if connectChanged {
			authors[axisKey(c.Name, AxisConnect)] = name
		}
	}
}

// EvaluateSafety implements orchestrator.SafetyEvaluator: the Tier-1 fast
// protection pass. It aggregates every wired fast-safety constraint's immediate
// protective disconnects, resolves them (disconnect always wins), and emits a
// Safety-marked plan. With no fast-safety constraint wired it returns an inert
// Safety plan, so the engine's fast loop is a no-op — matching the legacy
// EvaluateSafety contract (Breach==nil on a Safety plan means "not assessed").
func (s *Stack) EvaluateSafety(state orchestrator.SystemState) orchestrator.Plan {
	now := state.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	plan := orchestrator.Plan{Timestamp: now, Safety: true}

	var demands []Demand
	for _, c := range s.constraints {
		if fs, ok := c.(fastSafetyEvaluator); ok {
			demands = append(demands, fs.EvaluateFast(state)...)
		}
	}
	emitCommands(&plan, Resolve(demands))
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
			// EVSE demands carry the OCPP connector in the device key
			// ("station#connector", see evseKey/economics.go). A bare device name
			// (the 058 skeleton and any whole-EVSE demand) maps to connector 0.
			station, connector := parseEVSEDevice(dev)
			plan.EVSECommands = append(plan.EVSECommands, orchestrator.EVSECommand{
				StationID:   station,
				ConnectorID: connector,
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

// parseEVSEDevice splits an EVSE device key "station#connector" into its parts.
// A key with no "#" (or an unparseable connector) is treated as a whole-EVSE
// demand on connector 0 — preserving the 058 skeleton's behaviour and the
// fakeConstraint tests that emit a bare station name. It is the inverse of
// evseKey (shadow.go).
func parseEVSEDevice(dev string) (station string, connector int) {
	i := strings.LastIndexByte(dev, '#')
	if i < 0 {
		return dev, 0
	}
	n, err := strconv.Atoi(dev[i+1:])
	if err != nil {
		return dev, 0
	}
	return dev[:i], n
}

// batteryCommandIndex returns the index of the command for name, or −1 if absent.
// Mirrors the unexported optimizer helper (optimizer.go:2259) for the
// post-arbitration safety override.
func batteryCommandIndex(cmds []orchestrator.BatteryCommand, name string) int {
	for i := range cmds {
		if cmds[i].Name == name {
			return i
		}
	}
	return -1
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
