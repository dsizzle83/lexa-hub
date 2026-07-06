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
	"strings"
	"time"

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
	// Curves maps DERCurve href → DERCurve for fast resolution of curve links.
	Curves map[string]model.DERCurve
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
		tree.ClockOffset = tm.CurrentTime - time.Now().Unix()
	}

	// Step 3: EndDeviceList — find ourselves by LFDI
	if dcap.EndDeviceListLink == nil {
		return nil, fmt.Errorf("step 3: DeviceCapability has no EndDeviceListLink")
	}
	edl, err := w.fetchEndDeviceList(ctx, dcap.EndDeviceListLink.Href)
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

		progList, err := w.fetchDERProgramList(ctx, fsa.DERProgramListLink.Href)
		if err != nil {
			return nil, fmt.Errorf("step 5 (DERProgramList from FSA %d): %w", i, err)
		}

		for _, prog := range progList.DERProgram {
			ps := ProgramState{Program: prog}

			// Step 6a: DefaultDERControl — fetch once as extended; derive simple for scheduler.
			if prog.DefaultDERControlLink != nil {
				exd, err := w.fetchExtendedDefaultDERControl(ctx, prog.DefaultDERControlLink.Href)
				if err != nil {
					return nil, fmt.Errorf("step 6 (DefaultDERControl for %s): %w", prog.MRID, err)
				}
				ps.ExtendedDefault = exd
				ps.DefaultControl = extendedDefaultToSimple(exd)
			}

			// Step 6b: DERControlList — fetch once as extended; derive simple for scheduler.
			if prog.DERControlListLink != nil {
				ext, err := w.fetchExtendedDERControlList(ctx, prog.DERControlListLink.Href)
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
		mupList, err := w.fetchMirrorUsagePointList(ctx, dcap.MirrorUsagePointListLink.Href)
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

func (w *Walker) fetchDERCurveList(ctx context.Context, path string) (*model.DERCurveList, error) {
	var r model.DERCurveList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchExtendedDERControlList(ctx context.Context, path string) (*model.ExtendedDERControlList, error) {
	var r model.ExtendedDERControlList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchExtendedDefaultDERControl(ctx context.Context, path string) (*model.ExtendedDefaultDERControl, error) {
	var r model.ExtendedDefaultDERControl
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

// extendedDefaultToSimple converts ExtendedDefaultDERControl to the plain
// DefaultDERControl used by the scheduler (which only touches scalar modes).
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

// extendedListToSimple converts ExtendedDERControlList to DERControlList for
// the scheduler (which only touches scalar modes).
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
