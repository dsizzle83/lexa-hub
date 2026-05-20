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
	"encoding/xml"
	"fmt"
	"log"
	"strings"
	"time"

	"lexa-hub/internal/csip/model"
)

// Fetcher abstracts the HTTP GET + XML parse cycle. The walker calls
// Get with a path (e.g., "/edev") and expects the raw XML body back.
// This keeps the discovery logic decoupled from TLS, HTTP, and connection
// management — all of which live in your tlsclient package.
type Fetcher interface {
	// Get performs an HTTPS GET on the given path and returns the raw XML body.
	// The path is always relative (e.g., "/edev/0/fsa"), not an absolute URL.
	Get(path string) ([]byte, error)
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

	// ClockOffset is (server time − local time) in seconds, computed from
	// the /tm resource during discovery. Add this to time.Now().Unix() to get
	// estimated server time. Required by CSIP for event scheduling.
	ClockOffset int64

	// ResponseSetPath is the server-advertised href from DeviceCapability's
	// ResponseSetListLink.  Use this for response POSTs instead of a
	// hardcoded default.  Empty if the server did not advertise the link.
	ResponseSetPath string
}

// ProgramState groups a DERProgram with its discovered controls.
type ProgramState struct {
	Program        model.DERProgram
	DefaultControl *model.DefaultDERControl
	Controls       *model.DERControlList
}

// Walker performs CSIP-compliant resource discovery.
type Walker struct {
	fetcher Fetcher
	lfdi    string // our LFDI, used to find our EndDevice in the list
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
func (w *Walker) Discover(dcapPath string) (*ResourceTree, error) {
	tree := &ResourceTree{}

	// Step 1: DeviceCapability
	dcap, err := w.fetchDeviceCapability(dcapPath)
	if err != nil {
		return nil, fmt.Errorf("step 1 (DeviceCapability): %w", err)
	}
	tree.DeviceCapability = dcap
	// ResponseSetListLink points to the ResponseSetList (/rsps), NOT the POST
	// target.  Follow it one level deeper to find ResponseSet[0].ResponseList.Href
	// which is the actual URL to POST Response resources to (/rsps/0/r).
	if dcap.ResponseSetListLink != nil && dcap.ResponseSetListLink.Href != "" {
		if path, err := w.fetchResponseSetPostPath(dcap.ResponseSetListLink.Href); err != nil {
			log.Printf("walker: ResponseSetList: %v — response posting will use config default", err)
		} else {
			tree.ResponseSetPath = path
		}
	}

	// Step 2: Time (optional per spec, but CSIP requires it)
	if dcap.TimeLink != nil {
		tm, err := w.fetchTime(dcap.TimeLink.Href)
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
	edl, err := w.fetchEndDeviceList(dcap.EndDeviceListLink.Href)
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
		derList, err := w.fetchDERList(self.DERListLink.Href)
		if err != nil {
			return nil, fmt.Errorf("step 3b (DERList): %w", err)
		}
		tree.DERList = derList
	}

	// Step 4: FunctionSetAssignmentsList
	if self.FunctionSetAssignmentsListLink == nil {
		return nil, fmt.Errorf("step 4: EndDevice has no FunctionSetAssignmentsListLink")
	}
	fsaList, err := w.fetchFSAList(self.FunctionSetAssignmentsListLink.Href)
	if err != nil {
		return nil, fmt.Errorf("step 4 (FSAList): %w", err)
	}
	tree.FSAList = fsaList

	// Steps 5-6: Walk each FSA → DERPrograms → Controls
	for i, fsa := range fsaList.FunctionSetAssignments {
		if fsa.DERProgramListLink == nil {
			continue // FSA with no programs is valid but uninteresting
		}

		progList, err := w.fetchDERProgramList(fsa.DERProgramListLink.Href)
		if err != nil {
			return nil, fmt.Errorf("step 5 (DERProgramList from FSA %d): %w", i, err)
		}

		for _, prog := range progList.DERProgram {
			ps := ProgramState{Program: prog}

			// Step 6a: DefaultDERControl (the fallback)
			if prog.DefaultDERControlLink != nil {
				dderc, err := w.fetchDefaultDERControl(prog.DefaultDERControlLink.Href)
				if err != nil {
					return nil, fmt.Errorf("step 6 (DefaultDERControl for %s): %w", prog.MRID, err)
				}
				ps.DefaultControl = dderc
			}

			// Step 6b: DERControlList (active/scheduled events)
			if prog.DERControlListLink != nil {
				ctrls, err := w.fetchDERControlList(prog.DERControlListLink.Href)
				if err != nil {
					return nil, fmt.Errorf("step 6 (DERControlList for %s): %w", prog.MRID, err)
				}
				ps.Controls = ctrls
			}

			tree.Programs = append(tree.Programs, ps)
		}
	}

	// MirrorUsagePointList (for telemetry — Milestone 5, but discover it now)
	if dcap.MirrorUsagePointListLink != nil {
		mupList, err := w.fetchMirrorUsagePointList(dcap.MirrorUsagePointListLink.Href)
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

func (w *Walker) fetchDeviceCapability(path string) (*model.DeviceCapability, error) {
	var r model.DeviceCapability
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchTime(path string) (*model.Time, error) {
	var r model.Time
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchEndDeviceList(path string) (*model.EndDeviceList, error) {
	var r model.EndDeviceList
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchFSAList(path string) (*model.FunctionSetAssignmentsList, error) {
	var r model.FunctionSetAssignmentsList
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchDERProgramList(path string) (*model.DERProgramList, error) {
	var r model.DERProgramList
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchDERControlList(path string) (*model.DERControlList, error) {
	var r model.DERControlList
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchDefaultDERControl(path string) (*model.DefaultDERControl, error) {
	var r model.DefaultDERControl
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchDERList(path string) (*model.DERList, error) {
	var r model.DERList
	return &r, w.fetchAndParse(path, &r)
}

func (w *Walker) fetchMirrorUsagePointList(path string) (*model.MirrorUsagePointList, error) {
	var r model.MirrorUsagePointList
	return &r, w.fetchAndParse(path, &r)
}

// fetchResponseSetPostPath follows ResponseSetListLink → ResponseSetList →
// ResponseSet[0].ResponseList.Href to find the URL clients POST Response
// resources to.  The list-level href (/rsps) and the POST target (/rsps/0/r)
// are different resources.
func (w *Walker) fetchResponseSetPostPath(listHref string) (string, error) {
	var rsl model.ResponseSetList
	if err := w.fetchAndParse(listHref, &rsl); err != nil {
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

// fetchAndParse is the single point where HTTP and XML meet.
// Every resource retrieval goes through here.
func (w *Walker) fetchAndParse(path string, dest interface{}) error {
	body, err := w.fetcher.Get(path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	if err := xml.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("parse XML from %s: %w", path, err)
	}
	return nil
}
