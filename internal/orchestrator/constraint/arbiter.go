package constraint

import (
	"fmt"
	"math"
	"sort"
)

// Interval is a resolved admissible bound on one axis. NaN marks an unbounded
// side (matching Demand). A pinned point has Min==Max.
type Interval struct {
	Min, Max float64
}

// Desired is the arbitrated outcome for one device: the resolved admissible
// interval per axis, the resolved connect state, and any conflicts recorded
// while resolving. The Stack converts this into orchestrator commands.
type Desired struct {
	Device    string
	Bounds    map[Axis]Interval
	Connect   *bool
	Conflicts []Conflict
}

// Bound returns the resolved interval for an axis and whether any demand set it.
func (d Desired) Bound(axis Axis) (Interval, bool) {
	iv, ok := d.Bounds[axis]
	return iv, ok
}

// Conflict records a same-tier contention the arbiter resolved by taking the
// most-restrictive demand. It is surfaced in the plan's decision log so an
// operator can see WHICH constraints disagreed — the cascade could not, because
// a later rule silently overwrote an earlier one.
type Conflict struct {
	Device  string
	Axis    Axis
	Tier    Tier
	Sources [2]string // the two contending constraint names
	Reason  string
}

// Resolve arbitrates a tick's demands into per-device Desired state.
//
// Semantics (AD-007):
//   - Demands are grouped per (Device, Axis) and processed SAFETY→ECONOMICS,
//     ties broken by Source name — fully deterministic, no map-iteration order
//     leaks into the result.
//   - Each demand INTERSECTS the running interval: newLo=max(lo,Min),
//     newHi=min(hi,Max). Intersection can only shrink the interval, so a lower
//     tier can never widen what a higher tier set — the narrowing-only invariant
//     is structural, not merely tested (economics can't relax a compliance cap).
//   - An empty intersection (the incoming demand cannot be satisfied together
//     with the accumulated interval) is a same-tier conflict: the MOST
//     RESTRICTIVE demand wins (lowest ceiling), the interval collapses to it,
//     and a Conflict is recorded.
//   - Connect: false (disconnect) is the safe value and always wins; a tier that
//     holds both a true and a false demand is a recorded conflict.
func Resolve(demands []Demand) map[string]Desired {
	// Bucket by device then axis, preserving nothing about input order.
	byDevice := map[string]map[Axis][]Demand{}
	deviceOrder := []string{}
	for _, d := range demands {
		if _, ok := byDevice[d.Device]; !ok {
			byDevice[d.Device] = map[Axis][]Demand{}
			deviceOrder = append(deviceOrder, d.Device)
		}
		byDevice[d.Device][d.Axis] = append(byDevice[d.Device][d.Axis], d)
	}
	sort.Strings(deviceOrder)

	out := make(map[string]Desired, len(byDevice))
	for _, dev := range deviceOrder {
		des := Desired{Device: dev, Bounds: map[Axis]Interval{}}
		axes := byDevice[dev]
		for _, axis := range axisOrder {
			group := axes[axis]
			if len(group) == 0 {
				continue
			}
			if axis == AxisConnect {
				connect, conflicts := resolveConnect(dev, group)
				des.Connect = connect
				des.Conflicts = append(des.Conflicts, conflicts...)
				continue
			}
			iv, conflicts := resolveInterval(dev, axis, group)
			des.Bounds[axis] = iv
			des.Conflicts = append(des.Conflicts, conflicts...)
		}
		out[dev] = des
	}
	return out
}

// sortDemands orders a group deterministically: SAFETY first, then by Source.
func sortDemands(group []Demand) {
	sort.SliceStable(group, func(i, j int) bool {
		if group[i].Tier != group[j].Tier {
			return group[i].Tier < group[j].Tier
		}
		return group[i].Source < group[j].Source
	})
}

// resolveInterval intersects an axis group top tier first. A demand that cannot
// intersect the accumulated interval is a same-tier conflict resolved to the
// tightest ceiling.
func resolveInterval(device string, axis Axis, group []Demand) (Interval, []Conflict) {
	sortDemands(group)

	lo, hi := math.Inf(-1), math.Inf(1)
	tighteningSource := "" // source that last set hi, for conflict reporting
	var conflicts []Conflict

	for _, d := range group {
		dmin := d.Min
		if math.IsNaN(dmin) {
			dmin = math.Inf(-1)
		}
		dmax := d.Max
		if math.IsNaN(dmax) {
			dmax = math.Inf(1)
		}

		newLo := math.Max(lo, dmin)
		newHi := math.Min(hi, dmax)

		if newLo <= newHi {
			lo, hi = newLo, newHi
			if dmax < math.Inf(1) && (tighteningSource == "" || dmax <= hi) {
				tighteningSource = d.Source
			}
			continue
		}

		// Empty intersection: keep the most-restrictive (lowest) ceiling and
		// collapse the interval to it so we never exceed either bound.
		prev := tighteningSource
		hi = math.Min(hi, dmax)
		lo = math.Min(lo, hi)
		tighteningSource = d.Source
		conflicts = append(conflicts, Conflict{
			Device:  device,
			Axis:    axis,
			Tier:    d.Tier,
			Sources: [2]string{prev, d.Source},
			Reason: fmt.Sprintf("%s: %s bounds do not intersect; collapsed to most-restrictive ceiling %.0f",
				axis, d.Tier, hi),
		})
	}

	return Interval{Min: infToNaN(lo), Max: infToNaN(hi)}, conflicts
}

// resolveConnect resolves the connect axis: false wins; a tier holding both a
// true and a false demand is a recorded conflict.
func resolveConnect(device string, group []Demand) (*bool, []Conflict) {
	sortDemands(group)

	anyFalse := false
	anyTrue := false
	var falseSrc, trueSrc string
	perTier := map[Tier][2]string{} // tier → {trueSrc, falseSrc}
	var conflicts []Conflict

	for _, d := range group {
		if d.Connect == nil {
			continue
		}
		pair := perTier[d.Tier]
		if *d.Connect {
			anyTrue = true
			if trueSrc == "" {
				trueSrc = d.Source
			}
			if pair[0] == "" {
				pair[0] = d.Source
			}
		} else {
			anyFalse = true
			if falseSrc == "" {
				falseSrc = d.Source
			}
			if pair[1] == "" {
				pair[1] = d.Source
			}
		}
		perTier[d.Tier] = pair
	}

	// Record same-tier true/false contentions in deterministic tier order.
	tiers := make([]Tier, 0, len(perTier))
	for t := range perTier {
		tiers = append(tiers, t)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })
	for _, t := range tiers {
		pair := perTier[t]
		if pair[0] != "" && pair[1] != "" {
			conflicts = append(conflicts, Conflict{
				Device:  device,
				Axis:    AxisConnect,
				Tier:    t,
				Sources: [2]string{pair[0], pair[1]},
				Reason:  fmt.Sprintf("connect: %s wants connect, %s wants disconnect; disconnect wins", pair[0], pair[1]),
			})
		}
	}

	switch {
	case anyFalse:
		v := false
		return &v, conflicts
	case anyTrue:
		v := true
		return &v, conflicts
	default:
		return nil, conflicts
	}
}

// infToNaN maps ±Inf sentinels back to NaN (the Demand/Interval "unbounded"
// marker) so a fully-unbounded side reads as NaN, not ±Inf.
func infToNaN(v float64) float64 {
	if math.IsInf(v, 0) {
		return math.NaN()
	}
	return v
}
