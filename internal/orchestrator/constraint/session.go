package constraint

import (
	"math"
	"time"
)

// tunedTickInterval is the engine cadence the legacy *BreachTicks constants
// were calibrated at (the FAST replay/QA tick). It MUST equal
// orchestrator.tunedTickInterval (optimizer.go: 3 s); ScaleTicks parity depends
// on it, and TestScaleTicks_ParityWithOptimizer pins the copied semantics.
// Copied (not imported) because the orchestrator constant is unexported and
// this package must not add an export to it.
const tunedTickInterval = 3 * time.Second

// Session is the shared, per-constraint-instance inter-tick scaffolding. There
// is exactly ONE Session per constraint in a Stack — it replaces the scattered
// guard fields on DefaultOptimizer with named, constraint-owned state.
//
// This base type ships only the shared machinery: identity, tick scaling, and
// reset hooks. Concrete constraints (TASK-060/061/062) EMBED Session and add a
// TYPED state struct for their counters/last-command fields — never an untyped
// Counters map (see package doc). Reset semantics are load-bearing and owned by
// each constraint: a constraint registers what to zero via OnReset and decides
// WHEN to call Reset (e.g. a CSIP limit clearing entirely, not merely a cap
// value being rewritten — see optimizer.go's expOverTicks note).
type Session struct {
	name         string
	tickInterval time.Duration
	resetHooks   []func()
}

// NewSession returns a Session for the named constraint at the given engine
// cadence. A zero tickInterval means "use the tuned cadence" — ScaleTicks then
// returns tick counts unchanged, matching DefaultOptimizer's behaviour under
// unit tests that construct it without an interval.
func NewSession(name string, tickInterval time.Duration) *Session {
	return &Session{name: name, tickInterval: tickInterval}
}

// Name is the constraint identity this session belongs to.
func (s *Session) Name() string { return s.name }

// TickInterval is the configured engine cadence (0 = tuned/unset).
func (s *Session) TickInterval() time.Duration { return s.tickInterval }

// ScaleTicks converts a threshold expressed in tuned-cadence ticks into the
// equivalent tick count at the configured engine cadence, preserving the
// wall-clock duration the constant encodes.
//
// This MIRRORS DefaultOptimizer.scaleTicks EXACTLY, including the floor of 2
// (keeps single-glitch tolerance even when one stock tick already exceeds the
// tuned hold) and the round-to-nearest. Returns ticks unchanged when no
// interval is configured (tests) or the cadence matches the tuned one (fast
// mode). FAST/STOCK equivalence of every migrated breach/debounce threshold
// depends on this staying identical — do not "improve" the rounding.
func (s *Session) ScaleTicks(ticks int) int {
	if s.tickInterval <= 0 || s.tickInterval == tunedTickInterval {
		return ticks
	}
	hold := time.Duration(ticks) * tunedTickInterval
	n := int(math.Round(hold.Seconds() / s.tickInterval.Seconds()))
	if n < 2 {
		n = 2
	}
	return n
}

// TickSeconds returns the wall-clock length of one engine tick, defaulting to
// the tuned cadence when unset (mirrors DefaultOptimizer.tickSeconds).
func (s *Session) TickSeconds() float64 {
	if s.tickInterval <= 0 {
		return tunedTickInterval.Seconds()
	}
	return s.tickInterval.Seconds()
}

// OnReset registers a hook that Reset invokes. Concrete constraints use it to
// declare which typed fields zero when their session resets, keeping the reset
// list next to the state it clears.
func (s *Session) OnReset(fn func()) {
	if fn != nil {
		s.resetHooks = append(s.resetHooks, fn)
	}
}

// Reset invokes every registered reset hook in registration order. The
// constraint decides when to call it (its reset policy is load-bearing —
// TASK-060).
func (s *Session) Reset() {
	for _, fn := range s.resetHooks {
		fn()
	}
}
