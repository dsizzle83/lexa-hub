// Package discovery implements the IEEE 2030.5 resource discovery flow
// as specified by CSIP. It provides a link walker that traverses the
// resource tree starting from DeviceCapability (/dcap) and follows
// links to find the client's EndDevice, FSAs, DERPrograms, and controls.
//
// The walker never hardcodes any URL beyond the initial /dcap endpoint.
// Every other URL comes from link attributes in the XML responses.
// This is a conformance requirement — different utility servers deploy
// resources at different paths, and a compliant client must be flexible.
//
// The Fetcher interface abstracts the HTTP GET operation so the walker
// can be tested without a real server. In production, your TLS client
// implements Fetcher; in tests, a mock does.
package discovery

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lexa-hub/internal/bus"
	model "lexa-proto/csipmodel"
)

// Fetcher abstracts the HTTP GET + XML parse cycle. The walker calls
// Get with a path (e.g., "/edev") and expects the raw XML body back.
// This keeps the discovery logic decoupled from TLS, HTTP, and connection
// management — all of which live in your tlsclient package.
//
// Cancellation contract (TASK-070, R5): Get must check ctx before starting
// a request (dial/write) and return promptly (wrapping ctx.Err()) if it is
// already Done. Implementations are NOT required — and tlsclient's
// wolfSSL-backed one is NOT able — to interrupt a request already in
// flight: wolfSSL performs a blocking read(2) on the connection's fd,
// bounded by SO_RCVTIMEO (tlsclient/client.go), which Go's ctx plumbing
// cannot reach without closing the fd out from under an in-progress read
// (RSK-07 segfault territory — deliberately not attempted). So the honest
// granularity a canceled ctx buys a caller is "stop making NEW requests and
// unwind between fetches"; any one in-flight fetch is still bounded by the
// existing ReadTimeout, not by ctx. See Discover's doc comment for how this
// surfaces to walk callers.
type Fetcher interface {
	// Get performs an HTTPS GET on the given path and returns the raw XML body.
	// The path is always relative (e.g., "/edev/0/fsa"), not an absolute URL.
	Get(ctx context.Context, path string) ([]byte, error)
}

// ResourceTree holds the full discovered resource state after a
// successful discovery walk. This is the "picture of the world" your
// client uses for all subsequent operations.
type ResourceTree struct {
	DeviceCapability  *model.DeviceCapability
	Time              *model.Time
	SelfDevice        *model.EndDevice // the client's own EndDevice (matched by LFDI)
	EndDeviceList     *model.EndDeviceList
	FSAList           *model.FunctionSetAssignmentsList
	Programs          []ProgramState // one per DERProgram
	MirrorUsagePoints *model.MirrorUsagePointList
	DERList           *model.DERList
	// DERResources contains per-DER capability, settings, status, and availability.
	DERResources []DERResourceState

	// Pricing function set (§10.5) — nil/empty when not available.
	PricingProfiles []TariffState

	// Billing function set (§10.7) — nil/empty when not available.
	BillingAccounts []CustomerAccountState

	// Flow Reservation function set (§10.9) — nil when not available.
	// This is the current set of FlowReservationResponses from the server.
	FlowReservations *model.FlowReservationResponseList

	// FlowReservationRequestPath is the href to POST new FlowReservationRequests
	// to (the EndDevice's FlowReservationRequestListLink.Href). Empty when the
	// server does not expose this link.
	FlowReservationRequestPath string

	// ClockOffset is (server time − local time) in seconds, computed from
	// the /tm resource during discovery. Add this to time.Now().Unix() to get
	// estimated server time. Required by CSIP for event scheduling.
	ClockOffset int64

	// TmParsedAt is the LOCAL monotonic instant (time.Now() value, monotonic
	// clock intact) at which ClockOffset was measured against Time.CurrentTime.
	// It lets the anchor be derived from server-time-as-of-/tm plus monotonic
	// elapsed, so a wall-clock step landing between the /tm read and the anchor
	// (or the publish) cannot displace utility time (audit CS-1). Zero when Time
	// was not fetched this walk.
	TmParsedAt time.Time

	// ResponseSetPath is the server-advertised href from DeviceCapability's
	// ResponseSetListLink.  Use this for response POSTs instead of a
	// hardcoded default.  Empty if the server did not advertise the link.
	ResponseSetPath string
}

// ProgramState groups a DERProgram with its discovered controls and curves.
type ProgramState struct {
	Program        model.DERProgram
	DefaultControl *model.DefaultDERControl
	Controls       *model.DERControlList
	// ActiveControls is the active DERControlList (a subset of Controls with
	// only the currently-active events). Fetched from ActiveDERControlListLink
	// when the program exposes it.
	ActiveControls *model.DERControlList
	// ExtendedControls is populated when the program has a DERCurveListLink;
	// it carries the full DERControlBase including curve-linked modes.
	ExtendedControls *model.ExtendedDERControlList
	// ExtendedDefault is the DefaultDERControl with the full extended base.
	ExtendedDefault *model.ExtendedDefaultDERControl
	// DefaultSetGradW/DefaultSetSoftGradW are the DefaultDERControl's
	// setGradW/setSoftGradW ramp-rate defaults (2030.5 PerCent: hundredths
	// of a percent of setMaxW per second), parsed by the walker's local XML
	// wrapper (WP-8) because the vendored csipmodel type predates them and
	// WP-1 was the only planned proto pin bump. Per the CSIP ramp-rate rule
	// these ride ONLY the DefaultDERControl — events ramp via RampTms.
	DefaultSetGradW     *uint16
	DefaultSetSoftGradW *uint16
	// Curves maps DERCurve href → DERCurve for fast resolution of curve links.
	Curves map[string]model.DERCurve
}

// CurveModeLink pairs a curve-linked mode's bus vocabulary name
// (bus.CurveMode*, matching DERScheduleSlot's JSON keys) with that mode's
// CurveLink field on an ExtendedDERControlBase. CurveModeLinks is the single
// owner of the mode↔field mapping — the walker's ignored-content sweep, the
// scheduler's curve plausibility gate, and publish's CurveSet builder all
// iterate it, so a mode added to the model shows up (or alarms) everywhere
// at once instead of silently missing one consumer.
type CurveModeLink struct {
	Mode string
	Link *model.CurveLink
}

// CurveModeLinks returns the 14 curve-linked modes of base in a fixed,
// deterministic order (nil Link entries included — callers decide whether an
// absent link matters).
func CurveModeLinks(base *model.ExtendedDERControlBase) []CurveModeLink {
	return []CurveModeLink{
		{bus.CurveModeVoltVar, base.OpModVoltVar},
		{bus.CurveModeFreqWatt, base.OpModFreqWatt},
		{bus.CurveModeWattPF, base.OpModWattPF},
		{bus.CurveModeVoltWatt, base.OpModVoltWatt},
		{bus.CurveModeHFRTMayTrip, base.OpModHFRTMayTrip},
		{bus.CurveModeHFRTMustTrip, base.OpModHFRTMustTrip},
		{bus.CurveModeHVRTMayTrip, base.OpModHVRTMayTrip},
		{bus.CurveModeHVRTMomentaryCessation, base.OpModHVRTMomentaryCessation},
		{bus.CurveModeHVRTMustTrip, base.OpModHVRTMustTrip},
		{bus.CurveModeLFRTMayTrip, base.OpModLFRTMayTrip},
		{bus.CurveModeLFRTMustTrip, base.OpModLFRTMustTrip},
		{bus.CurveModeLVRTMayTrip, base.OpModLVRTMayTrip},
		{bus.CurveModeLVRTMomentaryCessation, base.OpModLVRTMomentaryCessation},
		{bus.CurveModeLVRTMustTrip, base.OpModLVRTMustTrip},
	}
}

// DERResourceState holds device-level monitoring data for one DER.
type DERResourceState struct {
	DER          model.DER
	Capability   *model.DERCapabilityFull
	Settings     *model.DERSettingsFull
	Status       *model.DERStatusFull
	Availability *model.DERAvailability
}

// Walker performs CSIP-compliant resource discovery.
type Walker struct {
	fetcher Fetcher
	lfdi    string // our LFDI, used to find our EndDevice in the list
}

// logf writes a log message with the "walker: " prefix.
func (w *Walker) logf(format string, args ...interface{}) {
	log.Printf("walker: "+format, args...)
}

// NewWalker creates a walker that will use the given Fetcher for HTTP
// requests and match the given LFDI when searching EndDeviceLists.
func NewWalker(f Fetcher, lfdi string) *Walker {
	return &Walker{fetcher: f, lfdi: strings.ToUpper(lfdi)}
}

// Discover performs the full CSIP discovery sequence:
//
//  1. GET /dcap → DeviceCapability
//  2. Follow TimeLink → Time
//  3. Follow EndDeviceListLink → EndDeviceList, find our EndDevice by LFDI
//  4. Follow FunctionSetAssignmentsListLink → FSAList
//  5. For each FSA, follow DERProgramListLink → DERProgramList
//  6. For each DERProgram, follow DERControlListLink and DefaultDERControlLink
//
// If any required link is missing or our LFDI is not found, Discover
// returns an error describing exactly where the chain broke.
//
// ctx (TASK-070, R5): checked at every one of the ~15 fetch sites below (all
// of them funnel through fetchAndParse). A ctx that is already Done when a
// fetch site is reached aborts the walk immediately without starting that
// request; the error is fmt.Errorf("walk canceled at %s: %w", path,
// ctx.Err())-shaped so callers can errors.Is(err, context.Canceled) it apart
// from a genuine walk failure (see internal/northbound/run.RunOnce, which
// must NOT count a shutdown-cancel as a failed walk or log it at the
// fail-closed WARN level). This is between-request granularity only — see
// the Fetcher interface doc for why a single in-flight fetch cannot be
// interrupted early.
func (w *Walker) Discover(ctx context.Context, dcapPath string) (*ResourceTree, error) {
	tree := &ResourceTree{}

	// Step 1: DeviceCapability
	dcap, err := w.fetchDeviceCapability(ctx, dcapPath)
	if err != nil {
		return nil, fmt.Errorf("step 1 (DeviceCapability): %w", err)
	}
	tree.DeviceCapability = dcap
	// ResponseSetListLink points to the ResponseSetList (/rsps), NOT the POST
	// target.  Follow it one level deeper to find ResponseSet[0].ResponseList.Href
	// which is the actual URL to POST Response resources to (/rsps/0/r).
	if dcap.ResponseSetListLink != nil && dcap.ResponseSetListLink.Href != "" {
		if path, err := w.fetchResponseSetPostPath(ctx, dcap.ResponseSetListLink.Href); err != nil {
			log.Printf("walker: ResponseSetList: %v — response posting will use config default", err)
		} else {
			tree.ResponseSetPath = path
		}
	}

	// Step 2: Time (optional per spec, but CSIP requires it)
	if dcap.TimeLink != nil {
		tm, err := w.fetchTime(ctx, dcap.TimeLink.Href)
		if err != nil {
			return nil, fmt.Errorf("step 2 (Time): %w", err)
		}
		tree.Time = tm
		// Clock offset: server time minus local time. Add to time.Now().Unix()
		// to get estimated server time. Required for correct event scheduling
		// (CSIP §5.2.1.3: client clock must be within 30s of server time).
		// Capture the offset AND the local monotonic instant it was measured at
		// from ONE clock read, so the anchor/publish can derive server-time from
		// server-time-as-of-/tm + monotonic elapsed rather than a second, later
		// wall read (audit CS-1: a wall step between reads must not poison utility
		// time).
		now := time.Now()
		tree.ClockOffset = tm.CurrentTime - now.Unix()
		tree.TmParsedAt = now
	}

	// Step 3: EndDeviceList — find ourselves by LFDI
	if dcap.EndDeviceListLink == nil {
		return nil, fmt.Errorf("step 3: DeviceCapability has no EndDeviceListLink")
	}
	edl, err := w.fetchEndDeviceListPaged(ctx, dcap.EndDeviceListLink.Href)
	if err != nil {
		return nil, fmt.Errorf("step 3 (EndDeviceList): %w", err)
	}
	tree.EndDeviceList = edl

	self := w.findSelfDevice(edl)
	if self == nil {
		return nil, fmt.Errorf("step 3: LFDI %s not found in EndDeviceList (%d devices)", w.lfdi, len(edl.EndDevice))
	}
	tree.SelfDevice = self

	// Step 3b: DERList from our EndDevice (for monitoring/reporting)
	if self.DERListLink != nil {
		derList, err := w.fetchDERList(ctx, self.DERListLink.Href)
		if err != nil {
			return nil, fmt.Errorf("step 3b (DERList): %w", err)
		}
		tree.DERList = derList

		// Step 3c: Per-DER capability, status, settings, and availability.
		// All sub-resources are non-fatal — a DER that doesn't expose them still
		// participates in control via the DERControlList above.
		for _, der := range derList.DER {
			rs := DERResourceState{DER: der}
			if der.DERCapabilityLink != nil {
				if cap, err := w.fetchDERCapabilityFull(ctx, der.DERCapabilityLink.Href); err != nil {
					w.logf("DERCapability %s: %v (skipped)", der.DERCapabilityLink.Href, err)
				} else {
					rs.Capability = cap
				}
			}
			if der.DERSettingsLink != nil {
				if set, err := w.fetchDERSettingsFull(ctx, der.DERSettingsLink.Href); err != nil {
					w.logf("DERSettings %s: %v (skipped)", der.DERSettingsLink.Href, err)
				} else {
					rs.Settings = set
				}
			}
			if der.DERStatusLink != nil {
				if st, err := w.fetchDERStatusFull(ctx, der.DERStatusLink.Href); err != nil {
					w.logf("DERStatus %s: %v (skipped)", der.DERStatusLink.Href, err)
				} else {
					rs.Status = st
				}
			}
			if der.DERAvailabilityLink != nil {
				if av, err := w.fetchDERAvailability(ctx, der.DERAvailabilityLink.Href); err != nil {
					w.logf("DERAvailability %s: %v (skipped)", der.DERAvailabilityLink.Href, err)
				} else {
					rs.Availability = av
				}
			}
			tree.DERResources = append(tree.DERResources, rs)
		}
	}

	// Step 4: FunctionSetAssignmentsList
	if self.FunctionSetAssignmentsListLink == nil {
		return nil, fmt.Errorf("step 4: EndDevice has no FunctionSetAssignmentsListLink")
	}
	fsaList, err := w.fetchFSAList(ctx, self.FunctionSetAssignmentsListLink.Href)
	if err != nil {
		return nil, fmt.Errorf("step 4 (FSAList): %w", err)
	}
	tree.FSAList = fsaList

	// Steps 5-6: Walk each FSA → DERPrograms → Controls
	for i, fsa := range fsaList.FunctionSetAssignments {
		if fsa.DERProgramListLink == nil {
			continue // FSA with no programs is valid but uninteresting
		}

		progList, err := w.fetchDERProgramListPaged(ctx, fsa.DERProgramListLink.Href)
		if err != nil {
			return nil, fmt.Errorf("step 5 (DERProgramList from FSA %d): %w", i, err)
		}

		for _, prog := range progList.DERProgram {
			ps := ProgramState{Program: prog}

			// Step 6a: DefaultDERControl — fetch once as extended; derive the
			// scheduler's scalar evaluation view alongside (the full base is
			// carried in ExtendedDefault, WP-8 — nothing is dropped).
			if prog.DefaultDERControlLink != nil {
				exd, err := w.fetchExtendedDefaultDERControl(ctx, prog.DefaultDERControlLink.Href)
				if err != nil {
					return nil, fmt.Errorf("step 6 (DefaultDERControl for %s): %w", prog.MRID, err)
				}
				ps.ExtendedDefault = &exd.ExtendedDefaultDERControl
				ps.DefaultSetGradW = exd.SetGradW
				ps.DefaultSetSoftGradW = exd.SetSoftGradW
				ps.DefaultControl = extendedDefaultToSimple(&exd.ExtendedDefaultDERControl)
			}

			// Step 6b: DERControlList — fetch once as extended; derive the
			// scheduler's scalar evaluation view alongside (the full base is
			// carried in ExtendedControls, WP-8 — nothing is dropped).
			if prog.DERControlListLink != nil {
				ext, err := w.fetchExtendedDERControlListPaged(ctx, prog.DERControlListLink.Href)
				if err != nil {
					return nil, fmt.Errorf("step 6 (DERControlList for %s): %w", prog.MRID, err)
				}
				ps.ExtendedControls = ext
				ps.Controls = extendedListToSimple(ext)
			}

			// Step 6c: ActiveDERControlList (currently active events only).
			// Non-fatal: not all servers publish this endpoint.
			if prog.ActiveDERControlListLink != nil {
				act, err := w.fetchDERControlList(ctx, prog.ActiveDERControlListLink.Href)
				if err != nil {
					w.logf("ActiveDERControlList for %s: %v (skipped)", prog.MRID, err)
				} else {
					ps.ActiveControls = act
				}
			}

			// Step 6d: DERCurveList — resolve all curves for this program.
			// Non-fatal: programs without curves just use scalar modes.
			if prog.DERCurveListLink != nil {
				curves, err := w.fetchDERCurveList(ctx, prog.DERCurveListLink.Href)
				if err != nil {
					w.logf("DERCurveList for %s: %v (skipped)", prog.MRID, err)
				} else {
					ps.Curves = make(map[string]model.DERCurve, len(curves.DERCurve))
					for _, c := range curves.DERCurve {
						ps.Curves[c.Href] = c
					}
				}
			}

			// WP-8: alarm (never silently drop) any served control content
			// the bus carriage still cannot represent — see
			// reportIgnoredContent.
			w.reportIgnoredContent(&ps)

			tree.Programs = append(tree.Programs, ps)
		}
	}

	// Step 7: Pricing function set (§10.5) — walk each FSA's TariffProfileListLink.
	// Failures are non-fatal: pricing is optional and its absence must not
	// prevent DER control from being published.
	for _, fsa := range fsaList.FunctionSetAssignments {
		states := w.discoverPricingFromFSA(ctx, fsa)
		tree.PricingProfiles = append(tree.PricingProfiles, states...)
	}

	// Step 8: Billing function set (§10.7) — walk each FSA's CustomerAccountListLink.
	for _, fsa := range fsaList.FunctionSetAssignments {
		states := w.discoverBillingFromFSA(ctx, fsa)
		tree.BillingAccounts = append(tree.BillingAccounts, states...)
	}

	// Step 9: Flow Reservation function set (§10.9) — read existing responses
	// from our EndDevice and record the request list path for future POSTs.
	if self.FlowReservationRequestListLink != nil {
		tree.FlowReservationRequestPath = self.FlowReservationRequestListLink.Href
	}
	tree.FlowReservations = w.discoverFlowReservation(ctx, self)

	// MirrorUsagePointList (for telemetry — Milestone 5, but discover it now)
	if dcap.MirrorUsagePointListLink != nil {
		mupList, err := w.fetchMirrorUsagePointListPaged(ctx, dcap.MirrorUsagePointListLink.Href)
		if err != nil {
			// MUP failure is not fatal to discovery — we can still operate
			// without telemetry. Log it but don't fail.
			log.Printf("walker: MUP list fetch: %v", err)
		} else {
			tree.MirrorUsagePoints = mupList
		}
	}

	return tree, nil
}

// findSelfDevice searches the EndDeviceList for a device whose LFDI
// matches ours. Case-insensitive comparison because the spec doesn't
// mandate case for hex-encoded LFDIs.
func (w *Walker) findSelfDevice(edl *model.EndDeviceList) *model.EndDevice {
	for i := range edl.EndDevice {
		if strings.EqualFold(edl.EndDevice[i].LFDI, w.lfdi) {
			return &edl.EndDevice[i]
		}
	}
	return nil
}

// ───────────────────────────────────────────────────────────────────────
// Typed fetch helpers — each does GET + unmarshal into the right struct.
// These keep the main Discover method readable.
// ───────────────────────────────────────────────────────────────────────

func (w *Walker) fetchDeviceCapability(ctx context.Context, path string) (*model.DeviceCapability, error) {
	var r model.DeviceCapability
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchTime(ctx context.Context, path string) (*model.Time, error) {
	var r model.Time
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchEndDeviceList(ctx context.Context, path string) (*model.EndDeviceList, error) {
	var r model.EndDeviceList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchFSAList(ctx context.Context, path string) (*model.FunctionSetAssignmentsList, error) {
	var r model.FunctionSetAssignmentsList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERProgramList(ctx context.Context, path string) (*model.DERProgramList, error) {
	var r model.DERProgramList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERControlList(ctx context.Context, path string) (*model.DERControlList, error) {
	var r model.DERControlList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERList(ctx context.Context, path string) (*model.DERList, error) {
	var r model.DERList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchMirrorUsagePointList(ctx context.Context, path string) (*model.MirrorUsagePointList, error) {
	var r model.MirrorUsagePointList
	return &r, w.fetchAndParse(ctx, path, &r)
}

// ───────────────────────────────────────────────────────────────────────
// Paged list fetchers (P1-1) — each wraps a single-page typed helper above
// in fetchPagedList so a spec-compliant paging server (results < all across
// pages) has every entry collected, not just the first page. See paginate.go
// for the loop + its fail-closed contract. The single-page helpers stay for
// the non-list resources and as the per-page fetch func here.
// ───────────────────────────────────────────────────────────────────────

func (w *Walker) fetchEndDeviceListPaged(ctx context.Context, path string) (*model.EndDeviceList, error) {
	return fetchPagedList(ctx, path, pagedList[model.EndDeviceList, model.EndDevice]{
		fetch:   w.fetchEndDeviceList,
		all:     func(l *model.EndDeviceList) uint32 { return l.All },
		results: func(l *model.EndDeviceList) uint32 { return l.Results },
		entries: func(l *model.EndDeviceList) []model.EndDevice { return l.EndDevice },
		href:    func(e model.EndDevice) string { return e.Href },
		install: func(l *model.EndDeviceList, e []model.EndDevice) {
			l.EndDevice = e
			l.All, l.Results = uint32(len(e)), uint32(len(e))
		},
	})
}

func (w *Walker) fetchDERProgramListPaged(ctx context.Context, path string) (*model.DERProgramList, error) {
	return fetchPagedList(ctx, path, pagedList[model.DERProgramList, model.DERProgram]{
		fetch:   w.fetchDERProgramList,
		all:     func(l *model.DERProgramList) uint32 { return l.All },
		results: func(l *model.DERProgramList) uint32 { return l.Results },
		entries: func(l *model.DERProgramList) []model.DERProgram { return l.DERProgram },
		href:    func(e model.DERProgram) string { return e.Href },
		install: func(l *model.DERProgramList, e []model.DERProgram) {
			l.DERProgram = e
			l.All, l.Results = uint32(len(e)), uint32(len(e))
		},
	})
}

func (w *Walker) fetchExtendedDERControlListPaged(ctx context.Context, path string) (*model.ExtendedDERControlList, error) {
	return fetchPagedList(ctx, path, pagedList[model.ExtendedDERControlList, model.ExtendedDERControl]{
		fetch:   w.fetchExtendedDERControlList,
		all:     func(l *model.ExtendedDERControlList) uint32 { return l.All },
		results: func(l *model.ExtendedDERControlList) uint32 { return l.Results },
		entries: func(l *model.ExtendedDERControlList) []model.ExtendedDERControl { return l.DERControl },
		href:    func(e model.ExtendedDERControl) string { return e.Href },
		install: func(l *model.ExtendedDERControlList, e []model.ExtendedDERControl) {
			l.DERControl = e
			l.All, l.Results = uint32(len(e)), uint32(len(e))
		},
	})
}

func (w *Walker) fetchMirrorUsagePointListPaged(ctx context.Context, path string) (*model.MirrorUsagePointList, error) {
	return fetchPagedList(ctx, path, pagedList[model.MirrorUsagePointList, model.MirrorUsagePoint]{
		fetch:   w.fetchMirrorUsagePointList,
		all:     func(l *model.MirrorUsagePointList) uint32 { return l.All },
		results: func(l *model.MirrorUsagePointList) uint32 { return l.Results },
		entries: func(l *model.MirrorUsagePointList) []model.MirrorUsagePoint { return l.MirrorUsagePoint },
		href:    func(e model.MirrorUsagePoint) string { return e.Href },
		install: func(l *model.MirrorUsagePointList, e []model.MirrorUsagePoint) {
			l.MirrorUsagePoint = e
			l.All, l.Results = uint32(len(e)), uint32(len(e))
		},
	})
}

func (w *Walker) fetchDERCurveList(ctx context.Context, path string) (*model.DERCurveList, error) {
	var r model.DERCurveList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchExtendedDERControlList(ctx context.Context, path string) (*model.ExtendedDERControlList, error) {
	var r model.ExtendedDERControlList
	return &r, w.fetchAndParse(ctx, path, &r)
}

// extendedDefaultDERControlDoc wraps the vendored ExtendedDefaultDERControl
// with the two DefaultDERControl-only ramp-rate defaults the vendored model
// predates (2030.5 setGradW/setSoftGradW, PerCent: hundredths of a percent
// of setMaxW per second). encoding/xml promotes the embedded struct's fields
// (including its XMLName), so this parses the exact same <DefaultDERControl>
// element with two extra children — no proto pin bump needed (WP-1 was the
// only planned bump).
type extendedDefaultDERControlDoc struct {
	model.ExtendedDefaultDERControl
	SetGradW     *uint16 `xml:"setGradW"`
	SetSoftGradW *uint16 `xml:"setSoftGradW"`
}

func (w *Walker) fetchExtendedDefaultDERControl(ctx context.Context, path string) (*extendedDefaultDERControlDoc, error) {
	var r extendedDefaultDERControlDoc
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERCapabilityFull(ctx context.Context, path string) (*model.DERCapabilityFull, error) {
	var r model.DERCapabilityFull
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERSettingsFull(ctx context.Context, path string) (*model.DERSettingsFull, error) {
	var r model.DERSettingsFull
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERStatusFull(ctx context.Context, path string) (*model.DERStatusFull, error) {
	var r model.DERStatusFull
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchDERAvailability(ctx context.Context, path string) (*model.DERAvailability, error) {
	var r model.DERAvailability
	return &r, w.fetchAndParse(ctx, path, &r)
}

// fetchResponseSetPostPath follows ResponseSetListLink → ResponseSetList →
// ResponseSet[0].ResponseList.Href to find the URL clients POST Response
// resources to.  The list-level href (/rsps) and the POST target (/rsps/0/r)
// are different resources.
func (w *Walker) fetchResponseSetPostPath(ctx context.Context, listHref string) (string, error) {
	var rsl model.ResponseSetList
	if err := w.fetchAndParse(ctx, listHref, &rsl); err != nil {
		return "", err
	}
	if len(rsl.ResponseSet) == 0 {
		return "", fmt.Errorf("ResponseSetList at %s is empty", listHref)
	}
	if rsl.ResponseSet[0].ResponseList == nil || rsl.ResponseSet[0].ResponseList.Href == "" {
		return "", fmt.Errorf("ResponseSet[0] at %s has no ResponseListLink", listHref)
	}
	return rsl.ResponseSet[0].ResponseList.Href, nil
}

// extendedDefaultToSimple projects an ExtendedDefaultDERControl onto the
// plain DefaultDERControl the scheduler's decision logic evaluates (scalar
// modes only). Since WP-8 this is a VIEW, not a drop: the full extended base
// stays carried in ProgramState.ExtendedDefault, rides through
// scheduler.Evaluate on ActiveControl.Extended, and is emitted on the bus
// (publish.ToActiveControl + publish.Curves); anything the carriage still
// cannot represent is alarmed by reportIgnoredContent, never silently lost.
func extendedDefaultToSimple(e *model.ExtendedDefaultDERControl) *model.DefaultDERControl {
	return &model.DefaultDERControl{
		Resource:    e.Resource,
		MRID:        e.MRID,
		Description: e.Description,
		Version:     e.Version,
		DERControlBase: model.DERControlBase{
			OpModConnect:        e.DERControlBase.OpModConnect,
			OpModEnergize:       e.DERControlBase.OpModEnergize,
			OpModFixedPFAbsorbW: e.DERControlBase.OpModFixedPFAbsorbW,
			OpModFixedPFInjectW: e.DERControlBase.OpModFixedPFInjectW,
			OpModFixedVar:       e.DERControlBase.OpModFixedVar,
			OpModFixedW:         e.DERControlBase.OpModFixedW,
			OpModMaxLimW:        e.DERControlBase.OpModMaxLimW,
			OpModExpLimW:        e.DERControlBase.OpModExpLimW,
			OpModGenLimW:        e.DERControlBase.OpModGenLimW,
			OpModImpLimW:        e.DERControlBase.OpModImpLimW,
			OpModLoadLimW:       e.DERControlBase.OpModLoadLimW,
			RampTms:             e.DERControlBase.RampTms,
		},
	}
}

// extendedListToSimple projects an ExtendedDERControlList onto the plain
// DERControlList the scheduler's decision logic evaluates (scalar modes
// only). Since WP-8 this is a VIEW, not a drop — same contract as
// extendedDefaultToSimple above: the full extended bases stay carried in
// ProgramState.ExtendedControls and ride through to the bus; unrepresentable
// content is alarmed by reportIgnoredContent, never silently lost.
func extendedListToSimple(e *model.ExtendedDERControlList) *model.DERControlList {
	list := &model.DERControlList{
		Resource: e.Resource,
		All:      e.All,
		Results:  e.Results,
		PollRate: e.PollRate,
	}
	for _, c := range e.DERControl {
		list.DERControl = append(list.DERControl, model.DERControl{
			Resource:     c.Resource,
			MRID:         c.MRID,
			Description:  c.Description,
			Version:      c.Version,
			CreationTime: c.CreationTime,
			EventStatus:  c.EventStatus,
			Interval:     c.Interval,
			DERControlBase: model.DERControlBase{
				OpModConnect:        c.DERControlBase.OpModConnect,
				OpModEnergize:       c.DERControlBase.OpModEnergize,
				OpModFixedPFAbsorbW: c.DERControlBase.OpModFixedPFAbsorbW,
				OpModFixedPFInjectW: c.DERControlBase.OpModFixedPFInjectW,
				OpModFixedVar:       c.DERControlBase.OpModFixedVar,
				OpModFixedW:         c.DERControlBase.OpModFixedW,
				OpModMaxLimW:        c.DERControlBase.OpModMaxLimW,
				OpModExpLimW:        c.DERControlBase.OpModExpLimW,
				OpModGenLimW:        c.DERControlBase.OpModGenLimW,
				OpModImpLimW:        c.DERControlBase.OpModImpLimW,
				OpModLoadLimW:       c.DERControlBase.OpModLoadLimW,
				RampTms:             c.DERControlBase.RampTms,
			},
			RandomizeStart:    c.RandomizeStart,
			RandomizeDuration: c.RandomizeDuration,
		})
	}
	return list
}

// ─── Ignored-content alarm (WP-8, closes the 10_BACKLOG ignored-curve item) ──
//
// The extended→simple projection used to drop curve/PF/energize content
// silently. WP-8 carries the full extended base + resolved curves through to
// the bus, so the only content still lost is what the ActiveControl/CurveSet
// carriage cannot represent yet. That residue is ALARMED here — a
// rate-limited WARN edge (first occurrence of a distinct item, then every
// ignoredLogEveryNth, mirroring bus.RejectAndAlarm's journal-budget shape)
// plus a monotonic total for the lexa_nb_ignored_control_content_total
// counter (exposed via IgnoredContentTotal for cmd/northbound's metrics
// Collect callback). Package-level state, not Walker fields, because run.
// RunOnce constructs a fresh Walker every walk cycle.
//
// Detected kinds:
//   - "target_var":   opModTargetVar — no ActiveControl field yet (§2.2 has
//     target_w only).
//   - "freq_droop":   opModFreqDroop — inline droop params ride neither
//     ActiveControl nor CurveSet yet (they DO ride DERScheduleMsg slots,
//     but the WP-9 adv-doc author consumes ActiveControl+CurveSet).
//   - "unresolvable-curve": a curve-linked opMod whose href is empty or not
//     present in the program's fetched DERCurveList (including the whole
//     list failing to fetch) — the mode is commanded but its content cannot
//     be published.
//   - "malformed-curve": a resolved curve that fails the content gate
//     (publish-side defense; the scheduler separately REJECTS a control
//     whose active curves fail plausibility — that path alarms via
//     RejectHook/ImplausibleReject, not here).
//
// Truly unknown XML opMod elements (outside the vendored model entirely) are
// discarded by encoding/xml before any code here runs; detecting those needs
// schema-aware raw parsing and stays out of scope.

var (
	ignoredContentTotal uint64   // atomic; monotonic across all kinds
	ignoredContentSeen  sync.Map // "kind|detail" → *uint64 per-item count
)

// ignoredLogEveryN is the WARN rate-limit divisor (first + every Nth per
// distinct item). A var, not a const, only so tests can shrink it.
var ignoredLogEveryN uint64 = 100

// ReportIgnoredContent records one occurrence of served control content the
// bus carriage cannot represent. Exported because publish's CurveSet builder
// shares the same alarm for its defensive malformed-entry drop.
func ReportIgnoredContent(kind, detail string) {
	atomic.AddUint64(&ignoredContentTotal, 1)
	v, _ := ignoredContentSeen.LoadOrStore(kind+"|"+detail, new(uint64))
	n := atomic.AddUint64(v.(*uint64), 1)
	if n == 1 || n%ignoredLogEveryN == 0 {
		slog.Warn("walker: IGNORED control content the bus carriage cannot represent",
			"kind", kind, "detail", detail, "count", n)
	}
}

// IgnoredContentTotal returns the monotonic total of ignored-content
// occurrences, for lexa_nb_ignored_control_content_total (a metrics Collect
// callback in cmd/northbound scrapes it — same pattern as bus.VersionRejects).
func IgnoredContentTotal() uint64 {
	return atomic.LoadUint64(&ignoredContentTotal)
}

// reportIgnoredContent sweeps one discovered program's served controls
// (extended default + every extended event) for content the carriage cannot
// represent and alarms each occurrence. Runs every walk on everything the
// server serves — not just the currently-active control — because any served
// event is a future adoption candidate and the alarm must fire before the
// content is needed, not after.
func (w *Walker) reportIgnoredContent(ps *ProgramState) {
	if ps.ExtendedDefault != nil {
		w.sweepExtendedBase(ps, &ps.ExtendedDefault.DERControlBase, ps.ExtendedDefault.MRID)
	}
	if ps.ExtendedControls != nil {
		for i := range ps.ExtendedControls.DERControl {
			c := &ps.ExtendedControls.DERControl[i]
			w.sweepExtendedBase(ps, &c.DERControlBase, c.MRID)
		}
	}
}

func (w *Walker) sweepExtendedBase(ps *ProgramState, base *model.ExtendedDERControlBase, mrid string) {
	where := ps.Program.MRID + "/" + mrid
	if base.OpModTargetVar != nil {
		ReportIgnoredContent("target_var", where)
	}
	if base.OpModFreqDroop != nil {
		ReportIgnoredContent("freq_droop", where)
	}
	for _, ml := range CurveModeLinks(base) {
		if ml.Link == nil {
			continue
		}
		if ml.Link.Href == "" {
			ReportIgnoredContent("unresolvable-curve", where+"/"+ml.Mode+"(empty href)")
			continue
		}
		if _, ok := ps.Curves[ml.Link.Href]; !ok {
			ReportIgnoredContent("unresolvable-curve", where+"/"+ml.Mode+"("+ml.Link.Href+")")
		}
	}
}

// fetchAndParse is the single point where HTTP and XML meet.
// Every resource retrieval goes through here — which makes it the single
// place TASK-070 needed to add cancellation checking for the whole walk to
// get "check ctx at each fetch site" for free.
//
// ctx is checked BEFORE calling the Fetcher (so a walk already canceled
// when it reaches the next resource never starts that request) and AGAIN
// after a Get error (so a cancellation racing in during the call — the
// Fetcher's own preflight check catching it, e.g. tlsclient.WolfSSLFetcher.
// Get — is classified as a cancellation, not a generic GET failure). Both
// cases return the same "walk canceled at %s: %w" shape so
// errors.Is(err, context.Canceled) works no matter which check caught it.
func (w *Walker) fetchAndParse(ctx context.Context, path string, dest interface{}) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("walk canceled at %s: %w", path, err)
	}
	body, err := w.fetcher.Get(ctx, path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("walk canceled at %s: %w", path, ctxErr)
		}
		return fmt.Errorf("GET %s: %w", path, err)
	}
	if err := xml.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("parse XML from %s: %w", path, err)
	}
	return nil
}
