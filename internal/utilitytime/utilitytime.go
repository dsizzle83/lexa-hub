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
//
// # Local (SOM) clock steps (TASK-037, GAP-04, AD-004 extension)
//
// Everything above hardens against the *utility server's* clock stepping.
// TASK-037 addresses the *local* (hub/SOM) wall clock stepping instead — an
// NTP correction at commissioning, or an RTC drift fix-up. Freshness windows
// (cmd/hub/state.go's measStaleAfter/evseStaleAfter/meterFrozenAfter) are
// already immune to this: they stamp arrival with time.Now() and compare
// with now.Sub(arrival), and Go's time.Time carries a monotonic reading from
// time.Now() that Sub prefers over the wall-clock reading. Receiver-side
// arrival stamping — not the message's own Ts field — is THE cross-process
// freshness mechanism; this package documents that decision but does not
// implement it (it already lives in cmd/hub/state.go). A message's Ts field
// (bus.Measurement, bus.ActiveControl, bus.DERScheduleMsg, ...) is
// publisher-side observability only — no freshness check reads it, and this
// package's anchoring below does not change that.
//
// What IS exposed to a local wall step is the *utility-time* arithmetic:
// ServerNow = local + offset. A local step immediately and permanently
// (until the next accepted offset) shifts every ServerNow value by the step
// size, which then feeds control expiry, TOU, and reporting grace — all of
// them genuinely wall-clock comparisons today. Anchor and LocalStep close
// that gap without touching freshness:
//
//   - Anchor(serverUnix) records the pair (serverUnix, the monotonic instant
//     of the call, via cfg.Now()).
//   - ServerNow(), once anchored, derives purely from monotonic elapsed time
//     since that instant (anchorServer + elapsed), so a local wall step
//     occurring *after* the anchor cannot move it.
//   - Every fresh utility-time observation (a discovery walk, an arriving
//     control/schedule message) re-anchors, so the anchor is never more than
//     one such interval stale, and a server-side clock correction is picked
//     up on the very next anchor exactly as before.
//   - LocalStep reports the local step itself (magnitude + classification)
//     so callers can log/alarm on it — forward steps re-anchor silently
//     (logged as a transition only); backward steps get the same anchored
//     correctness plus an alarm log, per TASK-037's policy.
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

	// StepThresholdS is the smallest absolute drift (seconds) between the
	// local wall clock's elapsed time and monotonic elapsed time since the
	// last Anchor call that LocalStep classifies as a genuine local clock
	// step rather than clock-read jitter. Default (when <= 0 is passed to
	// New) is DefaultStepThresholdS (30s), matching CSIP §5.2.1.3's client-
	// clock tolerance.
	StepThresholdS int64

	// Now returns the current local wall-clock time. Injected so tests are
	// deterministic; defaults to time.Now when nil. Production callers should
	// leave this nil (or explicitly pass time.Now) — the whole point of the
	// field is to let tests substitute a fake clock, never to add a second
	// wall-clock source in production code.
	//
	// TASK-037 note: a fake Now for anchored-clock tests must return
	// time.Time values built from a single base via .Add() (e.g.
	// base.Add(5*time.Second)) so the monotonic reading each value carries
	// advances consistently with the simulated wall-clock value — the same
	// way production time.Now() values do. Constructing independent
	// time.Time values (e.g. via time.Date) for "before" and "after" loses
	// the monotonic reading, which would make Sub fall back to wall-clock
	// arithmetic and defeat the entire point of the test.
	Now func() time.Time
}

// DefaultWobbleMaxS is the default Config.WobbleMaxS applied by New when the
// caller passes <= 0.
const DefaultWobbleMaxS = 60

// DefaultStepThresholdS is the default Config.StepThresholdS applied by New
// when the caller passes <= 0.
const DefaultStepThresholdS = 30

// Clock owns the accepted utility-time offset and derives ServerNow from it.
// Safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	cfg Config

	offset     int64
	haveOffset bool
	lastUpdate time.Time // local time at which the offset was last accepted (zero if never)

	// Monotonic anchor (TASK-037): once anchored, ServerNow derives from
	// monotonic elapsed time since anchorMono rather than from offset/cfg.Now
	// directly, making it immune to a local wall-clock step. anchorMono MUST
	// be an unmodified cfg.Now() value (see Anchor) — Round(0), marshaling,
	// or Unix-second round trips all strip the monotonic reading a time.Time
	// carries, silently falling back to wall-clock arithmetic.
	anchored     bool
	anchorServer int64     // server (utility) time at the anchor instant
	anchorMono   time.Time // cfg.Now() value at the anchor instant (monotonic reading intact)
	anchorWall   int64     // anchorMono.Unix() — wall seconds at the anchor, for LocalStep drift math
}

// New creates a Clock from cfg. A zero-value Config is valid: WobbleMaxS
// defaults to DefaultWobbleMaxS, StepThresholdS defaults to
// DefaultStepThresholdS, and Now defaults to time.Now.
func New(cfg Config) *Clock {
	if cfg.WobbleMaxS <= 0 {
		cfg.WobbleMaxS = DefaultWobbleMaxS
	}
	if cfg.StepThresholdS <= 0 {
		cfg.StepThresholdS = DefaultStepThresholdS
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

// ServerNow returns the current estimated utility (server) time.
//
// If the Clock has been Anchor-ed, ServerNow derives purely from monotonic
// elapsed time since the anchor instant (anchorServer + elapsed) — a local
// wall-clock step occurring after the anchor cannot move it (TASK-037).
//
// Otherwise (never anchored) it falls back to the pre-TASK-037 formula: the
// injected local clock plus the last accepted raw offset, treated as 0
// before the first SetOffset call — this matches every existing consumer's
// behavior with a zero-value ClockOffset field prior to the first successful
// /tm resync (or, for a Clock that only ever anchors and never calls
// SetOffset, prior to the first Anchor call). This is exactly the CLAUDE.md
// formula serverNow = time.Now().Unix() + tree.ClockOffset, with cfg.Now in
// place of time.Now and the last accepted SetOffset value in place of
// tree.ClockOffset. Neither path is ever smoothed.
func (c *Clock) ServerNow() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	if c.anchored {
		// int64(Sub(...).Seconds()) floors toward zero, which for a
		// non-negative elapsed duration is a floor — matches the Unix-second
		// truncation ServerNowAt/time.Time.Unix() already apply everywhere
		// else in this package.
		elapsed := int64(now.Sub(c.anchorMono).Seconds())
		return c.anchorServer + elapsed
	}
	return ServerNowAt(now, c.offset)
}

// Anchor records serverUnix (a utility-time value the caller has just
// derived from a fresh observation — a discovery walk, an arriving
// bus.ActiveControl/bus.DERScheduleMsg) together with the monotonic instant
// of this call. Every subsequent ServerNow() call derives from that pair
// until the next Anchor call, making it immune to a local wall-clock step
// occurring in between (TASK-037).
//
// Callers own how serverUnix is derived (typically ServerNowAt(now, offset)
// or, for a same-host publisher, msg.Ts+msg.ClockOffset — see cmd/hub's
// onCSIPControl) precisely so Anchor itself never reads a second wall clock
// or makes a same-host assumption of its own.
//
// Anchoring never expires on its own: if no fresh observation arrives, the
// anchor keeps advancing correctly via the monotonic clock (that is the
// entire point) rather than needing a refresh deadline. A stale *retained*
// control going stale for other reasons is a freshness concern, not a
// utility-time concern — out of scope here (TASK-042).
func (c *Clock) Anchor(serverUnix int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchorMono = c.cfg.Now()
	c.anchorServer = serverUnix
	c.anchorWall = c.anchorMono.Unix()
	c.anchored = true
}

// Anchored reports whether Anchor has ever been called on this Clock.
func (c *Clock) Anchored() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.anchored
}

// LocalStep detects a step in the LOCAL wall clock since the last Anchor
// call, by comparing wall-clock elapsed time against monotonic elapsed time
// over the same interval — the two run at the same rate unless something
// stepped the wall clock (NTP correction, RTC fix-up, manual date set).
// driftS is wall-elapsed minus monotonic-elapsed (positive: wall jumped
// forward; negative: wall jumped backward); stepped reports whether
// |driftS| >= Config.StepThresholdS. Returns (0, false) if never anchored.
//
// This does not affect ServerNow: it is purely a detector for TASK-037's
// forward/backward local-step policy (log a transition; alarm on backward).
// ServerNow stays correct throughout because it is monotonic-anchored
// already — LocalStep is telling the caller "the wall clock moved", not
// "utility time moved".
func (c *Clock) LocalStep() (driftS int64, stepped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.anchored {
		return 0, false
	}
	now := c.cfg.Now()
	wallElapsed := now.Unix() - c.anchorWall
	monoElapsed := int64(now.Sub(c.anchorMono).Seconds())
	drift := wallElapsed - monoElapsed
	threshold := c.cfg.StepThresholdS
	if threshold <= 0 {
		threshold = DefaultStepThresholdS
	}
	if drift < 0 {
		stepped = -drift >= threshold
	} else {
		stepped = drift >= threshold
	}
	return drift, stepped
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
