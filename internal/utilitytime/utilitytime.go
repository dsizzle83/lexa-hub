// Package utilitytime is the single owner of utility (server) time (AD-004,
// docs/refactor/02_ARCHITECTURE_DECISIONS.md, csip-tls-test repo).
//
// # Why this package exists
//
// Before this package, utility time had five independent owners scattered
// across the hub: the walker's /tm resync (internal/northbound/discovery,
// ClockOffset = server − local), the scheduler's ServerNow/Evaluate/
// controlExpired (internal/northbound/scheduler), the hub's expiry debounce
// (cmd/hub/state.go, expiryConfirmTicks=3), the API's report grace
// (cmd/api/handlers.go, csipReportGraceS=15), and the optimizer's TOU check
// (internal/orchestrator/optimizer.go). Four different grace/debounce
// constants existed for one concept, and the 2026-07 clock-jitter QA saga
// required four separate fixes across three services to close. Review
// finding W4 / decision AD-004 (05_ENGINEERING_PRINCIPLES.md §5) make this
// package the one owner: after Phase 3, new grace constants, debounces, or
// ad hoc offset arithmetic living outside utilitytime are review-blocking.
//
// # This task (TASK-034): core library only
//
// This package is a pure, injected-clock library — no I/O, no goroutines, no
// wall-clock reads outside the configurable Now func. It has zero consumers
// as of this task; migrating the five owners above onto it is TASK-035
// (walker + scheduler), TASK-036 (hub/api/optimizer), and TASK-037 (local
// clock-step policy). Those migrations must reproduce today's behavior
// bit-for-bit — see the invariant below — which is why the classification
// logic is kept strictly separate from the ServerNow arithmetic.
//
// # The ServerNow invariant
//
// CLAUDE.md (lexa-hub) states: serverNow = time.Now().Unix() + tree.ClockOffset.
// ServerNow (and the stateless ServerNowAt) reproduce exactly that arithmetic
// on the last ACCEPTED RAW offset. Smoothing and step classification
// (StepClass) are advisory output for policies and logging only — they never
// alter the value ServerNow returns. If they did, TASK-035's verbatim-port
// comparison against scheduler.ServerNow would become impossible to verify.
package utilitytime

import (
	"sync"
	"time"
)

// StepClass classifies an offset update relative to the previously accepted
// offset. It is advisory: consumers/policies may react to it (e.g. logging a
// step, or feeding TASK-037's local clock-step policy), but it never changes
// what ServerNow returns — that always uses the raw accepted offset.
type StepClass int

const (
	// First is returned for the very first offset ever observed by a Clock
	// (there is no previous offset to classify against).
	First StepClass = iota
	// Wobble is returned when the magnitude of the change from the previous
	// offset is <= Config.WobbleMaxS — jitter/drift consistent with normal
	// NTP flap, not a real step.
	Wobble
	// Step is returned when the magnitude of the change from the previous
	// offset is > Config.WobbleMaxS — a real clock step (server correction,
	// discovery talking to a different server, etc).
	Step
)

// String renders the StepClass for logging.
func (c StepClass) String() string {
	switch c {
	case First:
		return "first"
	case Wobble:
		return "wobble"
	case Step:
		return "step"
	default:
		return "unknown"
	}
}

// Config configures a Clock.
type Config struct {
	// WobbleMaxS is the largest absolute change (seconds) between successive
	// accepted offsets that is still classified as Wobble rather than Step.
	// Default (when <= 0 is passed to New) is 60, covering the ±60 s NTP-flap
	// class identified in the clock-jitter QA finding.
	WobbleMaxS int64

	// Now returns the current local wall-clock time. Injected so tests are
	// deterministic; defaults to time.Now when nil. Production callers should
	// leave this nil (or explicitly pass time.Now) — the whole point of the
	// field is to let tests substitute a fake clock, never to add a second
	// wall-clock source in production code.
	Now func() time.Time
}

// DefaultWobbleMaxS is the default Config.WobbleMaxS applied by New when the
// caller passes <= 0.
const DefaultWobbleMaxS = 60

// Clock owns the accepted utility-time offset and derives ServerNow from it.
// Safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	cfg Config

	offset     int64
	haveOffset bool
	lastUpdate time.Time // local time at which the offset was last accepted (zero if never)
}

// New creates a Clock from cfg. A zero-value Config is valid: WobbleMaxS
// defaults to DefaultWobbleMaxS and Now defaults to time.Now.
func New(cfg Config) *Clock {
	if cfg.WobbleMaxS <= 0 {
		cfg.WobbleMaxS = DefaultWobbleMaxS
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Clock{cfg: cfg}
}

// SetOffset accepts a newly-acquired raw offset (server − local, seconds —
// the same sign convention as walker.ResourceTree.ClockOffset) and returns
// its classification relative to the previously accepted offset.
//
// The offset is always accepted (this is not a filter): classification is
// advisory output for policies/logging, never a gate on what ServerNow
// subsequently returns. This mirrors the "fail closed on control, not on
// telemetry" split — a wobble and a step both become the new truth, but
// callers can choose to react differently to each (e.g. TASK-037's local
// clock-step policy watching for repeated Step classifications).
func (c *Clock) SetOffset(offset int64) StepClass {
	c.mu.Lock()
	defer c.mu.Unlock()

	var class StepClass
	switch {
	case !c.haveOffset:
		class = First
	default:
		delta := offset - c.offset
		if delta < 0 {
			delta = -delta
		}
		if delta > c.cfg.WobbleMaxS {
			class = Step
		} else {
			class = Wobble
		}
	}

	c.offset = offset
	c.haveOffset = true
	c.lastUpdate = c.cfg.Now()
	return class
}

// Offset returns the last accepted raw offset and whether one has ever been
// set. Before the first SetOffset call, it returns (0, false).
func (c *Clock) Offset() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.haveOffset
}

// ServerNow returns the current estimated utility (server) time: the
// injected local clock plus the last accepted raw offset. Before any offset
// has been accepted, the offset is treated as 0 (i.e. ServerNow degrades to
// the local clock) — this matches every existing consumer's behavior with a
// zero-value ClockOffset field prior to the first successful /tm resync.
//
// This is exactly the CLAUDE.md formula serverNow = time.Now().Unix() +
// tree.ClockOffset, with cfg.Now in place of time.Now and the last accepted
// SetOffset value in place of tree.ClockOffset. It is NEVER smoothed.
func (c *Clock) ServerNow() int64 {
	c.mu.Lock()
	now := c.cfg.Now()
	offset := c.offset
	c.mu.Unlock()
	return ServerNowAt(now, offset)
}

// ServerNowAt is the stateless form of the ServerNow arithmetic:
// now.Unix() + offset. It lets consumers that already own their offset value
// (e.g. the scheduler in TASK-035, which is handed clockOffset directly) use
// the single canonical formula without constructing a Clock.
func ServerNowAt(now time.Time, offset int64) int64 {
	return now.Unix() + offset
}

// Expired reports whether a control with the given validUntil (server-time
// Unix seconds, 0 meaning "never expires") is expired at serverNow.
//
// Semantics are identical to scheduler.controlExpired
// (internal/northbound/scheduler/scheduler.go:278-280) and the equivalent
// check in cmd/hub/state.go:397: validUntil != 0 && serverNow >= validUntil.
// The boundary is inclusive of validUntil itself (>=, not >) — an event is
// considered expired the instant serverNow reaches its ValidUntil.
func Expired(validUntil, serverNow int64) bool {
	return validUntil != 0 && serverNow >= validUntil
}

// InWindow reports whether serverNow falls within [start, end): start
// inclusive, end exclusive. Matches scheduler.activeEvent's interval check
// (internal/northbound/scheduler/scheduler.go:341: `serverNow < start ||
// serverNow >= end` continues/skips, i.e. the event is active over
// [start, end)).
func InWindow(start, end, serverNow int64) bool {
	return serverNow >= start && serverNow < end
}
