// Package scheduler implements the IEEE 2030.5 DER event state machine.
//
// The scheduler resolves which DERControlBase a client should apply at any
// given moment, given the discovered resource tree and the current server
// time. It handles:
//
//   - Cancelled events (currentStatus=6) — always skipped.
//   - Superseded events — when event A carries potentiallySuperseded=true
//     and event B covers the same time window with a later creationTime,
//     B wins and A is skipped.
//   - Randomized start/duration — randomizeStart and randomizeDuration are
//     applied once per event MRID and cached so subsequent calls produce the
//     same effective start time and window length (IEEE 2030.5 §11.10.4.2).
//   - Default fallback — when no event is active in the highest-priority
//     program, the program's DefaultDERControl is returned.
//
// Cross-program precedence (absolute primacy). The hub resolves the active
// control from the single highest-priority program (lowest primacy value,
// mRID as tiebreak) and does NOT merge events across programs: that program's
// active event — or, when it has none, its DefaultDERControl — is authoritative
// and outranks anything in a lower-priority program (see
// TestEvaluate_HighPriorityDefaultBeatsLowPriorityEvent). Superseding is
// therefore resolved within the highest-priority program's own control list.
// This is a deliberate interpretation of IEEE 2030.5 §10.10/§12.3; revisit it
// only against the aggregator BASIC-021..026 matrix with a Test Server.
//
// Usage:
//
//	sched := scheduler.New()
//	serverNow := time.Now().Unix() + tree.ClockOffset
//	active := sched.Evaluate(tree.Programs, serverNow)
//	if active != nil {
//	    applyControl(active.Base)
//	}
package scheduler

import (
	"math"
	"math/rand"
	"sync"
	"time"

	"lexa-hub/internal/northbound/discovery"
	model "lexa-proto/csipmodel"
)

// ActiveControl is what the client should apply right now.
type ActiveControl struct {
	// Base is the DERControlBase parameters to apply to the DER hardware.
	Base model.DERControlBase

	// Source is "default" when applying the DefaultDERControl fallback,
	// or "event" when a scheduled DERControl event is active.
	Source string

	// MRID identifies the control: the program MRID for "default", or the
	// event MRID for "event". Used when posting Response acknowledgements.
	MRID string

	// ValidUntil is the server-time Unix timestamp when this control expires.
	// 0 means no expiry (DefaultDERControl stays in effect until superseded
	// by an event).
	ValidUntil int64

	// Held is true when this control is being re-served as last-known-good
	// because the current discovery cycle resolved no valid control (empty or
	// malformed resource) while this control had not yet expired. It lets the
	// caller surface the fail-closed hold without changing Source (which the
	// Response/MRID addressing depends on).
	Held bool
}

// Scheduler tracks per-event randomization state and evaluates the active
// DER control at any given server time.
//
// A single Scheduler instance should be reused across successive poll cycles
// so that the random start offsets assigned to events remain stable.
// Creating a new Scheduler on every poll would re-randomize event timing,
// which the spec prohibits (randomization must be computed once per event).
type Scheduler struct {
	mu          sync.Mutex
	randOffsets map[string]int32 // MRID → cached start randomization (seconds)
	randDurs    map[string]int32 // MRID → cached duration randomization (seconds)
	rng         *rand.Rand

	// lastGood is the most recent validly-resolved control, retained so a
	// transient empty/malformed discovery cycle fails CLOSED to it rather than
	// dropping an unexpired safety control (see Evaluate / failClosed).
	lastGood *ActiveControl
}

// maxPlausibleLimitW bounds an adopted CSIP power limit. It sits far above any
// real residential or small-commercial DER site (1 GW) yet well below the
// overflow values a malformed resource can encode — OpModXxxLimW is an
// ActivePower{value int16, multiplier int8}, so value×10^multiplier can reach
// ~3.3e13 (32767×10^9) or +Inf (multiplier up to 127). A limit beyond this is
// treated as malformed and not adopted (audit: malform-huge-activepower).
const maxPlausibleLimitW = 1e9

// New creates a new Scheduler with a random seed.
func New() *Scheduler {
	return &Scheduler{
		randOffsets: make(map[string]int32),
		randDurs:    make(map[string]int32),
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ServerNow returns the estimated current server time by adding the clock
// offset (from ResourceTree.ClockOffset) to the local wall clock.
// Use this as the serverNow argument to Evaluate.
func ServerNow(clockOffset int64) int64 {
	return time.Now().Unix() + clockOffset
}

// Evaluate returns the DERControlBase the client should apply right now.
//
// Precedence (IEEE 2030.5 §12.3):
//  1. From the highest-priority program (lowest primacy value), find any
//     DERControl event whose (randomized) interval contains serverNow.
//  2. Among active events, skip cancelled (status=6) and superseded ones.
//  3. If multiple events are active after filtering, the one with the
//     latest creationTime wins; MRID is the tiebreaker (lexicographic).
//  4. If no event is active, return the program's DefaultDERControl.
//
// Fail-closed: a discovery cycle that resolves no control (empty/missing
// programs) or a malformed one (an implausible OpModXxxLimW) does NOT drop an
// already-adopted control — Evaluate re-serves the last-known-good control
// (marked Held) until that control's own ValidUntil expires. This prevents a
// transient or hostile resource from unseating a live safety cap (audit:
// malform-empty-program, malform-huge-activepower). Returns nil only when there
// is neither a valid current control nor an unexpired last-known-good.
//
// Exception — explicit server clear: when the highest-priority program EXISTS
// on the server but carries no active event and no DefaultDERControl, the
// server has intentionally cleared its controls (e.g. via DELETE /admin/control
// or an event whose ValidUntil passed). That is a meaningful server action, not
// a discovery anomaly, so the hub releases the last-known-good immediately
// rather than holding it until its own expiry (audit: curtailment-release).
// Only a fully absent program list (all=0, or a discovery failure returning no
// programs) triggers the fail-closed hold.
func (s *Scheduler) Evaluate(programs []discovery.ProgramState, serverNow int64) *ActiveControl {
	resolved, programFound := s.resolve(programs, serverNow)
	return s.failClosed(resolved, programFound, discovery.HighestPriorityProgram(programs), serverNow)
}

// resolve performs the pure §12.3 precedence resolution, with no fail-closed
// retention. Returns (nil, false) when there is no program at all, or
// (nil, true) when the highest-priority program exists but has no active event
// or DefaultDERControl. The bool is used by failClosed to distinguish an
// explicit server clear from a fully absent program list.
func (s *Scheduler) resolve(programs []discovery.ProgramState, serverNow int64) (*ActiveControl, bool) {
	hp := discovery.HighestPriorityProgram(programs)
	if hp == nil {
		return nil, false // no programs at all
	}

	s.pruneRandOffsets(programs)

	if hp.Controls != nil && len(hp.Controls.DERControl) > 0 {
		if ac := s.activeEvent(hp, serverNow); ac != nil {
			return ac, true
		}
	}

	// No active event — apply DefaultDERControl fallback.
	if hp.DefaultControl != nil {
		return &ActiveControl{
			Base:   hp.DefaultControl.DERControlBase,
			Source: "default",
			MRID:   hp.DefaultControl.MRID,
		}, true
	}

	return nil, true // program exists but no active control — explicit clear
}

// failClosed adopts a freshly-resolved control when it is valid, otherwise
// re-serves the last-known-good control until it expires. A malformed control
// (implausible limit) is never adopted and never stored as last-known-good.
//
// programFound indicates whether the discovery found at least one program on
// the server. When false (no programs at all) and nothing resolved, we hold
// fail-closed. When true and nothing resolved, it is an explicit server clear
// (e.g. the utility deleted its controls but kept the DERProgram); release —
// UNLESS the control we were enforcing is still being served (present in the
// program, not cancelled) and unexpired: then "no active event" only means the
// server's clock currently reads BEFORE the event's start (an NTP correction
// stepped it back), not that the event was withdrawn. hp is the
// highest-priority program, for that still-served check.
func (s *Scheduler) failClosed(resolved *ActiveControl, programFound bool, hp *discovery.ProgramState, serverNow int64) *ActiveControl {
	s.mu.Lock()
	defer s.mu.Unlock()

	if resolved != nil && plausibleControl(resolved) {
		// Clock-regression guard, default-fallback half (QA 2026-07-03 v6:
		// clock-jitter). The 2026-07-02 guard below covers a regression that
		// resolves to NOTHING — but a program that carries a DefaultDERControl
		// (this bench's program 0: a 5 kW export cap) never resolves to
		// nothing: when serverNow steps back past the adopted event's start,
		// §12.3 sees "no active event" and falls back to the DEFAULT, which
		// would be adopted here, clobbering lastGood. Enforcement then flapped
		// between the event cap and the default with every jitter cycle
		// (V6 C3/C4: 0 W ↔ 5 kW every walk — non-convergence all window, and
		// the export breach counter drained on every default tick so no
		// CannotComply ever fired). An active event always outranks the
		// default, and the only legitimate paths from event to default are
		// expiry (controlExpired) and withdrawal/cancellation (stillServed) —
		// "the clock stepped back before start" is neither. Hold the event.
		if resolved.Source == "default" &&
			s.lastGood != nil && s.lastGood.Source == "event" &&
			!controlExpired(s.lastGood, serverNow) && stillServed(hp, s.lastGood.MRID) {
			held := *s.lastGood
			held.Held = true
			return &held
		}
		stored := *resolved
		s.lastGood = &stored
		return resolved
	}

	// Malformed control (implausible limit): hold last-known-good so a bad
	// server response cannot unseat an active safety cap. programFound is
	// irrelevant here — a malformed control is never an explicit clear.
	if resolved != nil {
		if s.lastGood != nil && !controlExpired(s.lastGood, serverNow) {
			held := *s.lastGood
			held.Held = true
			return &held
		}
		s.lastGood = nil
		return nil
	}

	// resolved == nil. Distinguish the two cases:
	//
	// • programFound=true: the server has a program but no active event or
	//   DefaultDERControl — an explicit "nothing to enforce" signal. Release
	//   rather than holding last-known-good (the utility cleared its controls).
	//
	// • programFound=false: no programs at all (server returned an empty list or
	//   discovery returned nothing). Treat conservatively as an anomaly and hold
	//   last-known-good fail-closed so a transient/hostile empty program list
	//   does not silently drop an active safety cap.
	if programFound {
		// Clock-regression guard (QA 2026-07-02: clock-jitter): if the event we
		// were enforcing is STILL served by the program — same mRID, not
		// cancelled — and unexpired, then it did not "end" and was not
		// withdrawn; the server's clock stepped back past its start (a ±60 s
		// NTP correction), making it read as not-yet-started for a walk or two.
		// Releasing here flapped the cap with the jitter (the 5 s discovery
		// period aliased against the correction cycle, leaving whole windows
		// unenforced). Hold the control instead; a genuine withdrawal removes
		// or cancels the event and still releases below, and a genuine end
		// trips controlExpired.
		if s.lastGood != nil && !controlExpired(s.lastGood, serverNow) && stillServed(hp, s.lastGood.MRID) {
			held := *s.lastGood
			held.Held = true
			return &held
		}
		s.lastGood = nil
		return nil
	}

	if s.lastGood != nil && !controlExpired(s.lastGood, serverNow) {
		held := *s.lastGood
		held.Held = true
		return &held
	}

	// No safe control to fall back to — release (and forget an expired one).
	s.lastGood = nil
	return nil
}

// controlExpired reports whether ac is past its ValidUntil at serverNow.
// A control with ValidUntil==0 (a DefaultDERControl) never expires on its own.
func controlExpired(ac *ActiveControl, serverNow int64) bool {
	return ac.ValidUntil != 0 && serverNow >= ac.ValidUntil
}

// stillServed reports whether the program still lists an event with the given
// mRID that has not been cancelled (currentStatus=6). Used by the clock-
// regression guard: an event that is still served did not end or get
// withdrawn, no matter what the (possibly stepped-back) server clock says
// about its start time. Also matches a held DefaultDERControl's mRID.
func stillServed(hp *discovery.ProgramState, mrid string) bool {
	if hp == nil || mrid == "" {
		return false
	}
	if hp.Controls != nil {
		for i := range hp.Controls.DERControl {
			ctrl := &hp.Controls.DERControl[i]
			if ctrl.MRID == mrid {
				return ctrl.EventStatus == nil || ctrl.EventStatus.CurrentStatus != 6
			}
		}
	}
	return hp.DefaultControl != nil && hp.DefaultControl.MRID == mrid
}

// plausibleControl reports whether every active-power limit on the control
// decodes to a finite, physically-plausible magnitude (≤ maxPlausibleLimitW).
// A malformed/overflow value (audit: malform-huge-activepower) makes this false
// so the control is rejected rather than adopted as an effectively-infinite cap.
func plausibleControl(ac *ActiveControl) bool {
	b := ac.Base
	return plausibleLimit(b.OpModExpLimW) &&
		plausibleLimit(b.OpModMaxLimW) &&
		plausibleLimit(b.OpModImpLimW) &&
		plausibleLimit(b.OpModFixedW)
}

// plausibleLimit reports whether ap (when present) decodes to a finite watt
// value within maxPlausibleLimitW. A nil field imposes no limit and is plausible.
func plausibleLimit(ap *model.ActivePower) bool {
	if ap == nil {
		return true
	}
	w := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
	return !math.IsNaN(w) && !math.IsInf(w, 0) && math.Abs(w) <= maxPlausibleLimitW
}

// activeEvent finds the active DERControl in ps at serverNow, if any.
func (s *Scheduler) activeEvent(ps *discovery.ProgramState, serverNow int64) *ActiveControl {
	var best *model.DERControl

	for i := range ps.Controls.DERControl {
		ctrl := &ps.Controls.DERControl[i]

		// Skip cancelled events (currentStatus=6).
		if ctrl.EventStatus != nil && ctrl.EventStatus.CurrentStatus == 6 {
			continue
		}

		// Compute the effective start time and window length (with randomization).
		start := s.randomizedStart(ctrl)
		end := start + s.randomizedDuration(ctrl)

		// Check whether serverNow is within the event interval.
		if serverNow < start || serverNow >= end {
			continue
		}

		// Skip if this event is superseded by a newer overlapping event.
		if ctrl.EventStatus != nil && ctrl.EventStatus.PotentiallySuperseded {
			if s.isSuperseded(ctrl, ps.Controls.DERControl, serverNow) {
				continue
			}
		}

		// Among overlapping events, the one with the latest creationTime wins.
		// MRID is the secondary sort key (lexicographic, higher wins) for a
		// deterministic result when two events share a creationTime.
		if best == nil ||
			ctrl.CreationTime > best.CreationTime ||
			(ctrl.CreationTime == best.CreationTime && ctrl.MRID > best.MRID) {
			best = ctrl
		}
	}

	if best == nil {
		return nil
	}

	effectiveStart := s.randomizedStart(best)
	return &ActiveControl{
		Base:       best.DERControlBase,
		Source:     "event",
		MRID:       best.MRID,
		ValidUntil: effectiveStart + s.randomizedDuration(best),
	}
}

// randomizedStart returns the effective start time for ctrl, applying the
// randomizeStart offset per IEEE 2030.5 §11.10.4.2. The per-MRID offset is
// computed once and cached so that successive Evaluate calls produce the same
// effective timing for the same event.
func (s *Scheduler) randomizedStart(ctrl *model.DERControl) int64 {
	base := ctrl.Interval.Start
	if ctrl.RandomizeStart == nil || *ctrl.RandomizeStart == 0 {
		return base
	}

	s.mu.Lock()
	offset, ok := s.randOffsets[ctrl.MRID]
	if !ok {
		window := *ctrl.RandomizeStart
		if window < 0 {
			window = -window // spec says the value is the magnitude
		}
		// Uniform random integer in [-window, +window].
		offset = int32(s.rng.Int63n(int64(2*window+1))) - window
		s.randOffsets[ctrl.MRID] = offset
	}
	s.mu.Unlock()

	return base + int64(offset)
}

// randomizedDuration returns the effective window length for ctrl in seconds,
// applying the randomizeDuration offset per IEEE 2030.5 §11.10.4.2. Like the
// start offset, the per-MRID duration offset is computed once and cached so the
// event window stays stable across successive Evaluate calls. The result is
// clamped at zero so randomization can never produce a negative-length window.
func (s *Scheduler) randomizedDuration(ctrl *model.DERControl) int64 {
	base := int64(ctrl.Interval.Duration)
	if ctrl.RandomizeDuration == nil || *ctrl.RandomizeDuration == 0 {
		return base
	}

	s.mu.Lock()
	offset, ok := s.randDurs[ctrl.MRID]
	if !ok {
		window := *ctrl.RandomizeDuration
		if window < 0 {
			window = -window // the value is the magnitude
		}
		offset = int32(s.rng.Int63n(int64(2*window+1))) - window
		s.randDurs[ctrl.MRID] = offset
	}
	s.mu.Unlock()

	if d := base + int64(offset); d > 0 {
		return d
	}
	return 0
}

// SupersededMRIDs returns the set of event mRIDs in the highest-priority
// program that are superseded at serverNow: within their (randomized) window
// but losing to an overlapping event with a later creationTime. Used by the
// response state machine to emit status=7 (superseded) acknowledgements
// (CORE-022 / CORE-023). Cancelled events (status=6) are not included here —
// the caller reports those as status=6.
func (s *Scheduler) SupersededMRIDs(programs []discovery.ProgramState, serverNow int64) map[string]bool {
	hp := discovery.HighestPriorityProgram(programs)
	if hp == nil || hp.Controls == nil {
		return nil
	}
	out := make(map[string]bool)
	for i := range hp.Controls.DERControl {
		ctrl := &hp.Controls.DERControl[i]
		if ctrl.EventStatus != nil && ctrl.EventStatus.CurrentStatus == 6 {
			continue
		}
		start := s.randomizedStart(ctrl)
		end := start + s.randomizedDuration(ctrl)
		if serverNow < start || serverNow >= end {
			continue
		}
		if s.isSuperseded(ctrl, hp.Controls.DERControl, serverNow) {
			out[ctrl.MRID] = true
		}
	}
	return out
}

// isSuperseded returns true when another event in controls overlaps ctrl's
// interval at serverNow and has a later creationTime. This is the client-side
// supersede check per IEEE 2030.5 §12.3.
func (s *Scheduler) isSuperseded(ctrl *model.DERControl, controls []model.DERControl, serverNow int64) bool {
	for i := range controls {
		other := &controls[i]
		if other.MRID == ctrl.MRID {
			continue
		}
		if other.EventStatus != nil && other.EventStatus.CurrentStatus == 6 {
			continue
		}
		otherStart := s.randomizedStart(other)
		otherEnd := otherStart + s.randomizedDuration(other)
		if serverNow >= otherStart && serverNow < otherEnd && other.CreationTime > ctrl.CreationTime {
			return true
		}
	}
	return false
}

// pruneRandOffsets removes cached randomization offsets for MRIDs that are no
// longer present in any program's control list.
func (s *Scheduler) pruneRandOffsets(programs []discovery.ProgramState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.randOffsets) == 0 && len(s.randDurs) == 0 {
		return
	}

	live := make(map[string]struct{}, len(s.randOffsets))
	for _, ps := range programs {
		if ps.Controls == nil {
			continue
		}
		for i := range ps.Controls.DERControl {
			live[ps.Controls.DERControl[i].MRID] = struct{}{}
		}
	}
	for mrid := range s.randOffsets {
		if _, ok := live[mrid]; !ok {
			delete(s.randOffsets, mrid)
		}
	}
	for mrid := range s.randDurs {
		if _, ok := live[mrid]; !ok {
			delete(s.randDurs, mrid)
		}
	}
}
