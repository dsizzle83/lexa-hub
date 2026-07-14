package openadr

import (
	"sort"
	"time"
)

// Reporting (WP-15, minimal-viable): the VEN answers a program's
// reportDescriptors with USAGE reports only — net energy per bus device over
// the window since the last report, fed by cmd/openadr's measurement
// collector. STORAGE_* report kinds (STORAGE_CHARGE_LEVEL,
// STORAGE_USABLE_CAPACITY, STORAGE_MAX_CHARGE/DISCHARGE_POWER — from
// lexa/battery/{device}/metrics, which the collector already tracks) are a
// documented TODO seam: extend supportedReportPayload + BuildUsageReport's
// switch when a program actually requests them; the ACL read grant and the
// collector plumbing are already in place.

// supportedReportPayload reports whether this VEN can serve a
// reportDescriptor's payloadType. "USAGE" is the 3.1 Table 2 energy report;
// "TELEMETRY_USAGE" is tolerated for VTNs speaking 2.0b vocabulary.
func supportedReportPayload(payloadType string) bool {
	switch payloadType {
	case "USAGE", "TELEMETRY_USAGE":
		return true
	// TODO(STORAGE_*): "STORAGE_CHARGE_LEVEL", "STORAGE_USABLE_CAPACITY",
	// "STORAGE_MAX_CHARGE_POWER", "STORAGE_MAX_DISCHARGE_POWER" — battery
	// metrics are collected (cmd/openadr/collector.go) but not yet built
	// into reports; see the package-section comment above.
	default:
		return false
	}
}

// ReportRequest is one due report: the event asking, the descriptor that
// asked, and the resolved cadence.
type ReportRequest struct {
	Event      Event
	Descriptor ReportDescriptor
	Period     time.Duration
}

// key names a (event, payloadType) reporting stream for cadence tracking.
func (r ReportRequest) key() string { return r.Event.ID + "/" + r.Descriptor.PayloadType }

// ReportScheduler rate-tracks report streams: a stream is due when its
// resolved period has elapsed since its last successful POST. Owned by the
// single poll goroutine.
type ReportScheduler struct {
	lastPost map[string]time.Time
}

// NewReportScheduler constructs an empty scheduler.
func NewReportScheduler() *ReportScheduler {
	return &ReportScheduler{lastPost: make(map[string]time.Time)}
}

// resolvePeriod turns a descriptor's frequency (a count of EVENT INTERVALS,
// not a duration — see ReportDescriptor's doc) into a wall-clock cadence:
// frequency × the event's interval duration. frequency <= 0 (including the
// spec's -1 "one shot at event end") and an unresolvable interval duration
// fall back to defaultPeriod — minimal-viable simplification, documented:
// the VTN gets periodic reports at the poll-derived default rather than a
// single end-of-event report. Never faster than defaultPeriod: reports ride
// the poll loop, which cannot observe anything more often than it runs.
func resolvePeriod(ev Event, d ReportDescriptor, defaultPeriod time.Duration) time.Duration {
	if d.Frequency <= 0 {
		return defaultPeriod
	}
	if ev.IntervalPeriod == nil || ev.IntervalPeriod.Duration == "" {
		return defaultPeriod
	}
	ivDur, unbounded, err := ParseDuration(ev.IntervalPeriod.Duration)
	if err != nil || unbounded || ivDur <= 0 {
		return defaultPeriod
	}
	p := time.Duration(d.Frequency) * ivDur
	if p < defaultPeriod {
		return defaultPeriod
	}
	return p
}

// Due returns the report streams whose cadence has elapsed, across every
// CURRENTLY ACTIVE instance carrying a supported reportDescriptor. Sorted by
// stream key for deterministic ordering.
func (rs *ReportScheduler) Due(instances []EventInstance, now time.Time, defaultPeriod time.Duration) []ReportRequest {
	var due []ReportRequest
	for _, ei := range instances {
		slots := expandIntervals(ei, now)
		active := false
		for _, s := range slots {
			if s.activeAt(now) {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		for _, d := range ei.Event.ReportDescriptors {
			if !supportedReportPayload(d.PayloadType) {
				continue
			}
			req := ReportRequest{Event: ei.Event, Descriptor: d, Period: resolvePeriod(ei.Event, d, defaultPeriod)}
			if last, ok := rs.lastPost[req.key()]; ok && now.Sub(last) < req.Period {
				continue
			}
			due = append(due, req)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].key() < due[j].key() })
	return due
}

// MarkPosted records a successful POST for req's stream. Deliberately only
// called on success (cmd/openadr), so a failed POST retries next cycle.
func (rs *ReportScheduler) MarkPosted(req ReportRequest, now time.Time) {
	rs.lastPost[req.key()] = now
}

// UsageWindow is one device's accumulated measurement window (cmd/openadr's
// collector produces these; see that file for the accumulation rules).
type UsageWindow struct {
	// AvgW is the mean of the W samples seen in the window (bus sign
	// convention: + discharge/generation, − charge/load). nil when the
	// device reported no W samples.
	AvgW *float64
	// WhImpDelta/WhExpDelta are the lifetime-accumulator deltas over the
	// window (Wh), nil when the device lacks the counters.
	WhImpDelta *float64
	WhExpDelta *float64
}

// UsageSnapshot is one collector window across all devices.
type UsageSnapshot struct {
	StartTs int64
	EndTs   int64
	Devices map[string]UsageWindow
}

// energyKWh derives one device's net-consumption energy (kWh) for the
// window: preferred source is the Wh accumulator deltas (import − export —
// a register-derived DIRECT_READ), falling back to integrating the average
// power over the window (an ESTIMATED value; the bus W sign is
// +generation, so consumption = −W).
func energyKWh(w UsageWindow, windowS float64) (kwh float64, direct, ok bool) {
	if w.WhImpDelta != nil || w.WhExpDelta != nil {
		var imp, exp float64
		if w.WhImpDelta != nil {
			imp = *w.WhImpDelta
		}
		if w.WhExpDelta != nil {
			exp = *w.WhExpDelta
		}
		return (imp - exp) / 1000, true, true
	}
	if w.AvgW != nil && windowS > 0 {
		return (-*w.AvgW) * windowS / 3600 / 1000, false, true
	}
	return 0, false, false
}

// BuildUsageReport assembles the 3.1 report object for one due stream from a
// collector snapshot. Honors the descriptor's aggregate flag: aggregate ⇒
// one summed resource named venName; else one resource per device. Returns
// ok=false when the snapshot carries no usable data (nothing measured yet)
// — the caller skips the POST rather than fabricating zeros (G27).
func BuildUsageReport(req ReportRequest, snap UsageSnapshot, venName string) (Report, bool) {
	windowS := float64(snap.EndTs - snap.StartTs)
	period := &IntervalPeriod{
		Start:    time.Unix(snap.StartTs, 0).UTC().Format(time.RFC3339),
		Duration: FormatDurationISO(time.Duration(snap.EndTs-snap.StartTs) * time.Second),
	}
	names := make([]string, 0, len(snap.Devices))
	for name := range snap.Devices {
		names = append(names, name)
	}
	sort.Strings(names)

	allDirect := true
	var resources []ReportResource
	var aggKWh float64
	aggAny := false
	for _, name := range names {
		kwh, direct, ok := energyKWh(snap.Devices[name], windowS)
		if !ok {
			continue
		}
		if !direct {
			allDirect = false
		}
		if req.Descriptor.Aggregate {
			aggKWh += kwh
			aggAny = true
			continue
		}
		resources = append(resources, ReportResource{
			ResourceName:   name,
			IntervalPeriod: period,
			Intervals: []Interval{{
				ID:       0,
				Payloads: []ValuesMap{{Type: req.Descriptor.PayloadType, Values: []any{kwh}}},
			}},
		})
	}
	if req.Descriptor.Aggregate && aggAny {
		resources = []ReportResource{{
			ResourceName:   venName,
			IntervalPeriod: period,
			Intervals: []Interval{{
				ID:       0,
				Payloads: []ValuesMap{{Type: req.Descriptor.PayloadType, Values: []any{aggKWh}}},
			}},
		}}
	}
	if len(resources) == 0 {
		return Report{}, false
	}
	readingType := "DIRECT_READ"
	if !allDirect {
		readingType = "ESTIMATED"
	}
	return Report{
		ObjectType: "REPORT",
		ProgramID:  req.Event.ProgramID,
		EventID:    req.Event.ID,
		ClientName: venName,
		PayloadDescriptors: []PayloadDescriptor{{
			ObjectType:  "REPORT_PAYLOAD_DESCRIPTOR",
			PayloadType: req.Descriptor.PayloadType,
			ReadingType: readingType,
			Units:       "KWH",
		}},
		Resources: resources,
	}, true
}
