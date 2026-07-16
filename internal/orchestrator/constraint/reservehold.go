package constraint

// reservehold.go — the shadow-side twin of DefaultOptimizer's per-pack reserve
// latch (audit B-1 / GAP-08). Kept in lockstep with the legacy cascade
// (optimizer.go's battReserveHold / updateReserveHolds / dischargeBlocked) so
// the shadow diff stays clean and the P5 flip does not re-ship the finding-B
// discharge-dither bug. Single owner of the reserve-floor decision for the
// Stack: advanced once per tick before the constraints run, consulted by every
// discharge author via Input.DischargeBlocked.

import (
	"math"
	"time"

	"lexa-hub/internal/orchestrator"
)

// reserveReleaseMarginPct / reserveReleaseTicks mirror the legacy constants of
// the same name (optimizer.go). Enter the hold immediately at SOC ≤ reserve;
// release only after SOC ≥ reserve+margin for scaleTicks(reserveReleaseTicks)
// consecutive ticks. Change both sides together (do-not-strip-either-side).
const (
	reserveReleaseMarginPct = 2.0
	reserveReleaseTicks     = 3
)

// reserveLatch is the Stack's shared hysteretic reserve hold. It mirrors
// DefaultOptimizer's battReserveHold/battReserveRecoverTicks exactly: immediate
// enter, sustained-recovery release above a noise margin, NaN-SOC retains an
// existing hold (fail-safe), disconnect drops the latch (re-armed on reconnect).
type reserveLatch struct {
	socReserve   float64
	tickInterval time.Duration
	hold         map[string]bool
	recoverTicks map[string]int
}

func newReserveLatch(socReserve float64, tickInterval time.Duration) *reserveLatch {
	if socReserve <= 0 {
		socReserve = importSOCReserve // the constraints' shared default (20)
	}
	return &reserveLatch{
		socReserve:   socReserve,
		tickInterval: tickInterval,
		hold:         map[string]bool{},
		recoverTicks: map[string]int{},
	}
}

// scaleTicks mirrors Session.ScaleTicks (and DefaultOptimizer.scaleTicks): hold
// the wall-clock meaning of a tuned-cadence tick count across cadences, floored
// at 2 to preserve single-glitch tolerance.
func (l *reserveLatch) scaleTicks(ticks int) int {
	if l.tickInterval <= 0 || l.tickInterval == tunedTickInterval {
		return ticks
	}
	held := time.Duration(ticks) * tunedTickInterval
	n := int(math.Round(held.Seconds() / l.tickInterval.Seconds()))
	if n < 2 {
		n = 2
	}
	return n
}

// update advances the latch from this tick's measured SOC. Mirrors
// DefaultOptimizer.updateReserveHolds verbatim.
func (l *reserveLatch) update(batteries []orchestrator.BatteryState) {
	release := l.scaleTicks(reserveReleaseTicks)
	seen := make(map[string]bool, len(batteries))
	for _, b := range batteries {
		seen[b.Name] = true
		if !b.Connected {
			delete(l.hold, b.Name)
			delete(l.recoverTicks, b.Name)
			continue
		}
		if math.IsNaN(b.SOC) {
			l.recoverTicks[b.Name] = 0 // retain an existing hold; do not newly arm
			continue
		}
		if b.SOC <= l.socReserve {
			l.hold[b.Name] = true
			l.recoverTicks[b.Name] = 0
			continue
		}
		if !l.hold[b.Name] {
			continue
		}
		if b.SOC >= l.socReserve+reserveReleaseMarginPct {
			l.recoverTicks[b.Name]++
			if l.recoverTicks[b.Name] >= release {
				delete(l.hold, b.Name)
				delete(l.recoverTicks, b.Name)
			}
		} else {
			l.recoverTicks[b.Name] = 0
		}
	}
	for name := range l.hold {
		if !seen[name] {
			delete(l.hold, name)
			delete(l.recoverTicks, name)
		}
	}
}

// blocked mirrors DefaultOptimizer.dischargeBlocked: latched hold OR the
// instantaneous sub-reserve fold-in (belt-and-suspenders on the safety floor).
func (l *reserveLatch) blocked(b orchestrator.BatteryState) bool {
	if l.hold[b.Name] {
		return true
	}
	return !math.IsNaN(b.SOC) && b.SOC <= l.socReserve
}

// reserveBlocker returns the discharge-block predicate a constraint consults:
// the Stack's shared latch when wired (in.DischargeBlocked), else the
// instantaneous SOC ≤ reserve fallback for unit tests that build Input without a
// Stack (so those tests keep their exact prior semantics). Mirrors the `blocked`
// closure the legacy cascade threads into its discharge authors.
func reserveBlocker(in Input, socReserve float64) func(orchestrator.BatteryState) bool {
	if in.DischargeBlocked != nil {
		return in.DischargeBlocked
	}
	return func(b orchestrator.BatteryState) bool {
		return !math.IsNaN(b.SOC) && b.SOC <= socReserve
	}
}
