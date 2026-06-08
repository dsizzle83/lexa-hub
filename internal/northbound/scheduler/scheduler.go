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
	"math/rand"
	"sync"
	"time"

	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/model"
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
}

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
// Returns nil if programs is empty or the highest-priority program has
// neither an active event nor a DefaultDERControl.
func (s *Scheduler) Evaluate(programs []discovery.ProgramState, serverNow int64) *ActiveControl {
	hp := discovery.HighestPriorityProgram(programs)
	if hp == nil {
		return nil
	}

	s.pruneRandOffsets(programs)

	if hp.Controls != nil && len(hp.Controls.DERControl) > 0 {
		if ac := s.activeEvent(hp, serverNow); ac != nil {
			return ac
		}
	}

	// No active event — apply DefaultDERControl fallback.
	if hp.DefaultControl != nil {
		return &ActiveControl{
			Base:   hp.DefaultControl.DERControlBase,
			Source: "default",
			MRID:   hp.DefaultControl.MRID,
		}
	}

	return nil
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
