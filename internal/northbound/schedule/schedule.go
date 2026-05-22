// Package schedule builds a resolved 24-hour DER control schedule from a
// discovered IEEE 2030.5 resource tree.
//
// The schedule covers the window [serverNow, serverNow+24h) and is divided into
// slots. Each slot contains the fully resolved DERControlBase that should be
// active during that period, plus any curve data needed to execute the mode.
//
// Resolution order (highest priority first):
//  1. DERControl events from the highest-primacy program (lowest primacy number).
//  2. Between overlapping events: later creationTime wins; MRID is the tiebreaker.
//  3. Cancelled events (currentStatus=6) are always skipped.
//  4. Superseded events (potentiallySuperseded=true, covered by a newer event) are skipped.
//  5. Gaps between events — or the entire 24h if there are no events — are filled
//     with the program's DefaultDERControl.
//  6. If no DefaultDERControl is defined, the gap carries Source="none".
package schedule

import (
	"sort"
	"time"

	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/model"
)

const windowDuration = 24 * time.Hour

// ResolvedCurves carries the actual DERCurve objects looked up from the
// program's curve map. Fields match the curve-linked modes in ExtendedDERControlBase.
type ResolvedCurves struct {
	VoltVar                *model.DERCurve
	FreqWatt               *model.DERCurve
	WattPF                 *model.DERCurve
	VoltWatt               *model.DERCurve
	HFRTMayTrip            *model.DERCurve
	HFRTMustTrip           *model.DERCurve
	HVRTMayTrip            *model.DERCurve
	HVRTMomentaryCessation *model.DERCurve
	HVRTMustTrip           *model.DERCurve
	LFRTMayTrip            *model.DERCurve
	LFRTMustTrip           *model.DERCurve
	LVRTMayTrip            *model.DERCurve
	LVRTMomentaryCessation *model.DERCurve
	LVRTMustTrip           *model.DERCurve
}

// ScheduleSlot is one contiguous time segment of the 24-hour plan.
type ScheduleSlot struct {
	// Start and End are Unix seconds bounding this slot.
	Start int64
	End   int64

	// Source is "event", "default", or "none".
	Source string
	// MRID identifies the controlling resource.
	MRID string
	// Description is the human-readable label from the resource, if present.
	Description string

	// Base is the scalar DER control parameters for this slot.
	Base model.DERControlBase

	// Extended is the full ExtendedDERControlBase if the program exposes curves.
	// Nil when the program only has scalar modes.
	Extended *model.ExtendedDERControlBase

	// Curves contains the actual DERCurve objects resolved from the program's
	// curve map.
	Curves ResolvedCurves

	// ProgramMRID is the DERProgram this slot came from.
	ProgramMRID string
	// Primacy is the DERProgram's primacy value (lower = higher priority).
	Primacy uint8
}

// DER24hSchedule is the full 24-hour plan for one DER client.
type DER24hSchedule struct {
	// WindowStart and WindowEnd are the UTC Unix boundaries of the schedule.
	WindowStart int64
	WindowEnd   int64
	// BuildTime is when the schedule was computed (Unix seconds, local clock).
	BuildTime int64
	// ClockOffset is (server time − local time) used during this build.
	ClockOffset int64
	// Slots are ordered, non-overlapping, and contiguous from WindowStart to WindowEnd.
	Slots []ScheduleSlot
	// DERResources carries the device-level capability / status / availability snapshot.
	DERResources []discovery.DERResourceState
}

// Build builds a resolved 24-hour DER control schedule from the resource tree
// at the given serverNow timestamp (Unix seconds, server clock).
func Build(tree *discovery.ResourceTree, serverNow int64) *DER24hSchedule {
	windowStart := serverNow
	windowEnd := serverNow + int64(windowDuration.Seconds())

	sched := &DER24hSchedule{
		WindowStart:  windowStart,
		WindowEnd:    windowEnd,
		BuildTime:    time.Now().Unix(),
		ClockOffset:  tree.ClockOffset,
		DERResources: tree.DERResources,
	}

	hp := discovery.HighestPriorityProgram(tree.Programs)
	if hp == nil {
		sched.Slots = []ScheduleSlot{{Start: windowStart, End: windowEnd, Source: "none"}}
		return sched
	}

	// Collect candidates that overlap the window, filter, then build timeline.
	candidates := collectCandidates(hp, windowStart, windowEnd)
	active := filterActive(candidates)
	events := buildEventList(active, windowStart, windowEnd)
	sort.Slice(events, func(i, j int) bool {
		if events[i].start != events[j].start {
			return events[i].start < events[j].start
		}
		if events[i].creationTime != events[j].creationTime {
			return events[i].creationTime < events[j].creationTime
		}
		return events[i].mrid < events[j].mrid
	})
	events = mergeOverlapping(events)

	cursor := windowStart
	for _, ev := range events {
		if ev.start > cursor {
			sched.Slots = append(sched.Slots, makeDefaultSlot(hp, cursor, ev.start))
		}
		sched.Slots = append(sched.Slots, makeEventSlot(hp, ev))
		cursor = ev.end
	}
	if cursor < windowEnd {
		sched.Slots = append(sched.Slots, makeDefaultSlot(hp, cursor, windowEnd))
	}
	if len(sched.Slots) == 0 {
		sched.Slots = []ScheduleSlot{makeDefaultSlot(hp, windowStart, windowEnd)}
	}

	return sched
}

// ─── Internal types and helpers ───────────────────────────────────────────────

// candidate is a pre-filtered event entry extracted from a DERControlList.
type candidate struct {
	start, end   int64
	creationTime int64
	mrid         string
	potentiallySuperseded bool
	cancelled    bool
	ext          *model.ExtendedDERControl // non-nil if program has extended controls
	simple       *model.DERControl
}

// eventEntry is a clipped, winner-selected event ready for timeline insertion.
type eventEntry struct {
	start, end   int64
	creationTime int64
	mrid         string
	description  string
	ext          *model.ExtendedDERControl
	simple       *model.DERControl
}

func collectCandidates(ps *discovery.ProgramState, windowStart, windowEnd int64) []candidate {
	var out []candidate
	if ps.ExtendedControls != nil {
		for i := range ps.ExtendedControls.DERControl {
			c := &ps.ExtendedControls.DERControl[i]
			start := c.Interval.Start
			end := start + int64(c.Interval.Duration)
			if end <= windowStart || start >= windowEnd {
				continue
			}
			out = append(out, candidate{
				start: start, end: end,
				creationTime:          c.CreationTime,
				mrid:                  c.MRID,
				cancelled:             c.EventStatus != nil && c.EventStatus.CurrentStatus == 6,
				potentiallySuperseded: c.EventStatus != nil && c.EventStatus.PotentiallySuperseded,
				ext:                   c,
			})
		}
	} else if ps.Controls != nil {
		for i := range ps.Controls.DERControl {
			c := &ps.Controls.DERControl[i]
			start := c.Interval.Start
			end := start + int64(c.Interval.Duration)
			if end <= windowStart || start >= windowEnd {
				continue
			}
			out = append(out, candidate{
				start: start, end: end,
				creationTime:          c.CreationTime,
				mrid:                  c.MRID,
				cancelled:             c.EventStatus != nil && c.EventStatus.CurrentStatus == 6,
				potentiallySuperseded: c.EventStatus != nil && c.EventStatus.PotentiallySuperseded,
				simple:                c,
			})
		}
	}
	return out
}

func filterActive(candidates []candidate) []candidate {
	var out []candidate
	for i, c := range candidates {
		if c.cancelled {
			continue
		}
		if c.potentiallySuperseded && isSuperseded(candidates, i) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func isSuperseded(candidates []candidate, idx int) bool {
	c := candidates[idx]
	for j, other := range candidates {
		if j == idx || other.cancelled {
			continue
		}
		if other.end <= c.start || other.start >= c.end {
			continue
		}
		if other.creationTime > c.creationTime {
			return true
		}
	}
	return false
}

func buildEventList(active []candidate, windowStart, windowEnd int64) []eventEntry {
	var out []eventEntry
	for _, c := range active {
		start := c.start
		if start < windowStart {
			start = windowStart
		}
		end := c.end
		if end > windowEnd {
			end = windowEnd
		}
		if start >= end {
			continue
		}
		ev := eventEntry{
			start: start, end: end,
			creationTime: c.creationTime,
		}
		if c.ext != nil {
			ev.ext = c.ext
			ev.mrid = c.ext.MRID
			ev.description = c.ext.Description
		} else {
			ev.simple = c.simple
			ev.mrid = c.simple.MRID
			ev.description = c.simple.Description
		}
		out = append(out, ev)
	}
	return out
}

// mergeOverlapping resolves remaining overlaps between events: when two events
// overlap after filtering, the one with the higher creationTime (or MRID) wins.
// The result is a non-overlapping, sorted list.
func mergeOverlapping(events []eventEntry) []eventEntry {
	if len(events) == 0 {
		return nil
	}
	result := []eventEntry{events[0]}
	for i := 1; i < len(events); i++ {
		last := &result[len(result)-1]
		cur := events[i]
		if cur.start >= last.end {
			result = append(result, cur)
			continue
		}
		// Overlapping. Determine winner.
		curWins := cur.creationTime > last.creationTime ||
			(cur.creationTime == last.creationTime && cur.mrid > last.mrid)
		if curWins {
			// Shrink last to end before cur starts; append cur normally.
			last.end = cur.start
			if last.end <= last.start {
				// Last was fully replaced — overwrite it.
				*last = cur
			} else {
				result = append(result, cur)
			}
		}
		// else last wins; cur is ignored (fully overlapped by a higher-priority last).
	}
	return result
}

func makeDefaultSlot(ps *discovery.ProgramState, start, end int64) ScheduleSlot {
	slot := ScheduleSlot{
		Start:       start,
		End:         end,
		ProgramMRID: ps.Program.MRID,
		Primacy:     ps.Program.Primacy,
	}
	if ps.ExtendedDefault != nil {
		slot.Source = "default"
		slot.MRID = ps.ExtendedDefault.MRID
		slot.Description = ps.ExtendedDefault.Description
		base := ps.ExtendedDefault.DERControlBase
		slot.Extended = &base
		slot.Base = extendedToBase(base)
		slot.Curves = resolveCurves(base, ps.Curves)
	} else if ps.DefaultControl != nil {
		slot.Source = "default"
		slot.MRID = ps.DefaultControl.MRID
		slot.Description = ps.DefaultControl.Description
		slot.Base = ps.DefaultControl.DERControlBase
	} else {
		slot.Source = "none"
	}
	return slot
}

func makeEventSlot(ps *discovery.ProgramState, ev eventEntry) ScheduleSlot {
	slot := ScheduleSlot{
		Start:       ev.start,
		End:         ev.end,
		Source:      "event",
		MRID:        ev.mrid,
		Description: ev.description,
		ProgramMRID: ps.Program.MRID,
		Primacy:     ps.Program.Primacy,
	}
	if ev.ext != nil {
		base := ev.ext.DERControlBase
		slot.Extended = &base
		slot.Base = extendedToBase(base)
		slot.Curves = resolveCurves(base, ps.Curves)
	} else if ev.simple != nil {
		slot.Base = ev.simple.DERControlBase
	}
	return slot
}

// extendedToBase extracts the scalar fields from an ExtendedDERControlBase.
func extendedToBase(ext model.ExtendedDERControlBase) model.DERControlBase {
	return model.DERControlBase{
		OpModConnect:        ext.OpModConnect,
		OpModEnergize:       ext.OpModEnergize,
		OpModFixedPFAbsorbW: ext.OpModFixedPFAbsorbW,
		OpModFixedPFInjectW: ext.OpModFixedPFInjectW,
		OpModFixedVar:       ext.OpModFixedVar,
		OpModFixedW:         ext.OpModFixedW,
		OpModMaxLimW:        ext.OpModMaxLimW,
		OpModExpLimW:        ext.OpModExpLimW,
		OpModGenLimW:        ext.OpModGenLimW,
		OpModImpLimW:        ext.OpModImpLimW,
		OpModLoadLimW:       ext.OpModLoadLimW,
		RampTms:             ext.RampTms,
	}
}

// resolveCurves looks up each CurveLink href in the program's curve map.
func resolveCurves(base model.ExtendedDERControlBase, curves map[string]model.DERCurve) ResolvedCurves {
	lookup := func(link *model.CurveLink) *model.DERCurve {
		if link == nil || link.Href == "" || curves == nil {
			return nil
		}
		if c, ok := curves[link.Href]; ok {
			cp := c
			return &cp
		}
		return nil
	}
	return ResolvedCurves{
		VoltVar:                lookup(base.OpModVoltVar),
		FreqWatt:               lookup(base.OpModFreqWatt),
		WattPF:                 lookup(base.OpModWattPF),
		VoltWatt:               lookup(base.OpModVoltWatt),
		HFRTMayTrip:            lookup(base.OpModHFRTMayTrip),
		HFRTMustTrip:           lookup(base.OpModHFRTMustTrip),
		HVRTMayTrip:            lookup(base.OpModHVRTMayTrip),
		HVRTMomentaryCessation: lookup(base.OpModHVRTMomentaryCessation),
		HVRTMustTrip:           lookup(base.OpModHVRTMustTrip),
		LFRTMayTrip:            lookup(base.OpModLFRTMayTrip),
		LFRTMustTrip:           lookup(base.OpModLFRTMustTrip),
		LVRTMayTrip:            lookup(base.OpModLVRTMayTrip),
		LVRTMomentaryCessation: lookup(base.OpModLVRTMomentaryCessation),
		LVRTMustTrip:           lookup(base.OpModLVRTMustTrip),
	}
}
