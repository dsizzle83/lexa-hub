package openadr

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	"lexa-hub/internal/bus"
)

// EventInstance is a tracked event plus its once-assigned randomizeStart
// offset (EventStore.Instances) — the unit of translation.
type EventInstance struct {
	Event      Event
	RandOffset time.Duration
}

// slot is one expanded interval: absolute start, resolved duration, and the
// interval's payload valuesMaps.
type slot struct {
	start     time.Time
	dur       time.Duration
	unbounded bool
	payloads  []ValuesMap
}

// activeAt reports whether the slot's window covers t.
func (s slot) activeAt(t time.Time) bool {
	if t.Before(s.start) {
		return false
	}
	return s.unbounded || t.Before(s.start.Add(s.dur))
}

// end returns the slot's end (meaningless when unbounded).
func (s slot) end() time.Time { return s.start.Add(s.dur) }

// expandIntervals resolves an event's intervals to absolute time slots per
// the 3.1 interval model: the event-level intervalPeriod supplies the series
// start and the default per-interval duration; an interval WITHOUT its own
// intervalPeriod occupies the next consecutive slot; an interval WITH one is
// absolute (its own start/duration). The instance's randomizeStart offset
// shifts every start uniformly (the whole event shifts once — 3.1
// randomization delays the event, it does not reshuffle its interior).
// A "0001-01-01" sentinel start means now.
func expandIntervals(ei EventInstance, now time.Time) []slot {
	ev := ei.Event
	var (
		cursor       time.Time
		defDur       time.Duration
		defUnbounded bool
		haveCursor   bool
	)
	if ev.IntervalPeriod != nil {
		st, nowSentinel, err := ParseStart(ev.IntervalPeriod.Start)
		if err == nil {
			if nowSentinel {
				st = now
			}
			cursor = st.Add(ei.RandOffset)
			haveCursor = true
		} else {
			slog.Warn("openadr: unparsable event start — skipping schedule anchoring",
				"event", ev.ID, "start", ev.IntervalPeriod.Start, "err", err)
		}
		if ev.IntervalPeriod.Duration != "" {
			if d, unb, err := ParseDuration(ev.IntervalPeriod.Duration); err == nil {
				defDur, defUnbounded = d, unb
			} else {
				slog.Warn("openadr: unparsable event duration",
					"event", ev.ID, "duration", ev.IntervalPeriod.Duration, "err", err)
			}
		}
	}
	var out []slot
	for _, iv := range ev.Intervals {
		if iv.IntervalPeriod != nil {
			st, nowSentinel, err := ParseStart(iv.IntervalPeriod.Start)
			if err != nil {
				slog.Warn("openadr: unparsable interval start — interval dropped",
					"event", ev.ID, "interval", iv.ID, "err", err)
				continue
			}
			if nowSentinel {
				st = now
			}
			st = st.Add(ei.RandOffset)
			dur, unb := defDur, defUnbounded
			if iv.IntervalPeriod.Duration != "" {
				if d, u, err := ParseDuration(iv.IntervalPeriod.Duration); err == nil {
					dur, unb = d, u
				}
			}
			out = append(out, slot{start: st, dur: dur, unbounded: unb, payloads: iv.Payloads})
			continue
		}
		if !haveCursor {
			slog.Warn("openadr: interval has no period and event has no anchor — dropped",
				"event", ev.ID, "interval", iv.ID)
			continue
		}
		s := slot{start: cursor, dur: defDur, unbounded: defUnbounded, payloads: iv.Payloads}
		out = append(out, s)
		if defUnbounded {
			// An unbounded default duration means this consecutive slot never
			// ends; anything after it is unreachable.
			break
		}
		cursor = cursor.Add(defDur)
	}
	return out
}

// eventEnd returns the latest slot end across slots; unbounded=true when any
// slot is unbounded.
func eventEnd(slots []slot) (end time.Time, unbounded bool) {
	for _, s := range slots {
		if s.unbounded {
			return time.Time{}, true
		}
		if e := s.end(); e.After(end) {
			end = e
		}
	}
	return end, false
}

// DescriptorLookup resolves the payload descriptor for (event, payloadType):
// event-level descriptors win, then the owning program's (a tariff program
// hangs its PRICE descriptor at program level), then nil.
type DescriptorLookup func(ev Event, payloadType string) *PayloadDescriptor

// NewDescriptorLookup builds a DescriptorLookup over a programID→Program map.
func NewDescriptorLookup(programs map[string]Program) DescriptorLookup {
	return func(ev Event, payloadType string) *PayloadDescriptor {
		for i := range ev.PayloadDescriptors {
			if ev.PayloadDescriptors[i].PayloadType == payloadType {
				return &ev.PayloadDescriptors[i]
			}
		}
		if p, ok := programs[ev.ProgramID]; ok {
			for i := range p.PayloadDescriptors {
				if p.PayloadDescriptors[i].PayloadType == payloadType {
					return &p.PayloadDescriptors[i]
				}
			}
		}
		return nil
	}
}

// TranslatePrices builds the retained lexa/openadr/prices document from
// every tracked event that has not yet fully ended: prices are planner
// forward-input, so FUTURE intervals are included (unlike limits). One
// series per (event, price-kind); series ordered (Kind, EventID) and
// intervals ordered by start so unchanged content marshals byte-identically
// — the publisher's change-detection contract.
func TranslatePrices(instances []EventInstance, lookup DescriptorLookup, now time.Time) bus.OpenADRPrices {
	var series []bus.OpenADRPriceSeries
	for _, ei := range instances {
		slots := expandIntervals(ei, now)
		if len(slots) == 0 {
			continue
		}
		if end, unbounded := eventEnd(slots); !unbounded && !end.After(now) {
			continue // fully in the past
		}
		// Which price kinds does this event carry?
		kinds := map[string]bool{}
		for _, s := range slots {
			for _, p := range s.payloads {
				if IsPriceKind(p.Type) {
					kinds[p.Type] = true
				}
			}
		}
		kindList := make([]string, 0, len(kinds))
		for k := range kinds {
			kindList = append(kindList, k)
		}
		sort.Strings(kindList)
		for _, kind := range kindList {
			sr := bus.OpenADRPriceSeries{
				ProgramID: ei.Event.ProgramID,
				EventID:   ei.Event.ID,
				Kind:      kind,
			}
			if d := lookup(ei.Event, kind); d != nil {
				sr.Currency = d.Currency
				sr.Units = d.Units
			}
			for _, s := range slots {
				for _, p := range s.payloads {
					if p.Type != kind {
						continue
					}
					v, ok := p.FirstNumber()
					if !ok {
						continue // non-numeric payload (e.g. a string ALERT detail) — nothing to chart
					}
					iv := bus.OpenADRPriceInterval{
						StartTs: s.start.Unix(),
						Start:   s.start.UTC().Format(time.RFC3339),
						Value:   v,
					}
					if !s.unbounded {
						iv.DurationS = int64(s.dur / time.Second)
					}
					sr.Intervals = append(sr.Intervals, iv)
					break // one value per (slot, kind)
				}
			}
			if len(sr.Intervals) > 0 {
				sort.Slice(sr.Intervals, func(i, j int) bool { return sr.Intervals[i].StartTs < sr.Intervals[j].StartTs })
				series = append(series, sr)
			}
		}
	}
	sort.Slice(series, func(i, j int) bool {
		if series[i].Kind != series[j].Kind {
			return series[i].Kind < series[j].Kind
		}
		return series[i].EventID < series[j].EventID
	})
	return bus.OpenADRPrices{
		Envelope: bus.Envelope{V: bus.OpenADRPricesV},
		Series:   series,
		Ts:       now.Unix(),
	}
}

// toWatts converts a capacity-limit payload value to watts per its
// descriptor units. The 3.1 units enum's power member is KW, which is also
// the assumption for an absent descriptor; a literal "W"/"MW" is tolerated.
// Anything else returns ok=false — the value is DROPPED with an alarm by the
// caller rather than guessed (G27/fail-closed: a mis-scaled cap is worse
// than no cap adopted from this event).
func toWatts(v float64, units string) (float64, bool) {
	switch strings.ToUpper(strings.TrimSpace(units)) {
	case "", "KW":
		return v * 1000, true
	case "W":
		return v, true
	case "MW":
		return v * 1e6, true
	default:
		return 0, false
	}
}

// axisBind tracks the binding (most-restrictive) candidate for one limit axis.
type axisBind struct {
	set          bool
	valW         float64
	eventID      string
	end          time.Time
	endUnbounded bool
}

func (a *axisBind) consider(w float64, eventID string, end time.Time, unbounded bool) {
	if a.set && w >= a.valW {
		return
	}
	*a = axisBind{set: true, valW: w, eventID: eventID, end: end, endUnbounded: unbounded}
}

// TranslateLimits builds the retained lexa/openadr/limits document: the
// most-restrictive IMPORT/EXPORT_CAPACITY_LIMIT per axis across every
// CURRENTLY ACTIVE slot (limits are live obligations — a future event's cap
// appears only once its interval begins; the poll loop recomputes each
// cycle). EventID/ValidUntil semantics are documented on bus.OpenADRLimits;
// this function is their single producer.
func TranslateLimits(instances []EventInstance, lookup DescriptorLookup, now time.Time) bus.OpenADRLimits {
	var imp, exp axisBind
	for _, ei := range instances {
		slots := expandIntervals(ei, now)
		evEnd, evUnbounded := eventEnd(slots)
		for _, s := range slots {
			if !s.activeAt(now) {
				continue
			}
			for _, p := range s.payloads {
				if p.Type != PayloadImportCapacityLimit && p.Type != PayloadExportCapacityLimit {
					continue
				}
				v, ok := p.FirstNumber()
				if !ok {
					continue
				}
				units := ""
				if d := lookup(ei.Event, p.Type); d != nil {
					units = d.Units
				}
				w, ok := toWatts(v, units)
				if !ok {
					slog.Warn("openadr: capacity limit with unconvertible units — dropped, not guessed",
						"event", ei.Event.ID, "payload", p.Type, "units", units)
					continue
				}
				switch p.Type {
				case PayloadImportCapacityLimit:
					imp.consider(w, ei.Event.ID, evEnd, evUnbounded)
				case PayloadExportCapacityLimit:
					exp.consider(w, ei.Event.ID, evEnd, evUnbounded)
				}
			}
		}
	}
	out := bus.OpenADRLimits{
		Envelope: bus.Envelope{V: bus.OpenADRLimitsV},
		Ts:       now.Unix(),
	}
	var ids []string
	var earliest time.Time
	unbounded := true
	noteBind := func(a axisBind) {
		dup := false
		for _, id := range ids {
			if id == a.eventID {
				dup = true
			}
		}
		if !dup {
			ids = append(ids, a.eventID)
		}
		if !a.endUnbounded {
			if unbounded || a.end.Before(earliest) {
				earliest = a.end
			}
			unbounded = false
		}
	}
	if imp.set {
		v := imp.valW
		out.ImpLimW = &v
		noteBind(imp)
	}
	if exp.set {
		v := exp.valW
		out.ExpLimW = &v
		noteBind(exp)
	}
	out.EventID = strings.Join(ids, ",")
	if !unbounded {
		out.ValidUntil = earliest.Unix()
	}
	return out
}

// CountActive reports how many instances have at least one slot active now —
// the lexa_openadr_events_active gauge / OpenADRStatus.ActiveEvents source.
func CountActive(instances []EventInstance, now time.Time) int {
	n := 0
	for _, ei := range instances {
		for _, s := range expandIntervals(ei, now) {
			if s.activeAt(now) {
				n++
				break
			}
		}
	}
	return n
}
