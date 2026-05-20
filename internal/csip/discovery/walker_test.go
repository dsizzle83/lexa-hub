package discovery

import (
	"encoding/xml"
	"fmt"
	"testing"

	"lexa-hub/internal/csip/model"
)

// ───────────────────────────────────────────────────────────────────────
// Mock Fetcher — serves pre-built XML keyed by path
// ───────────────────────────────────────────────────────────────────────

type mockFetcher struct {
	responses map[string]interface{}
	getCalls  []string
}

func newMockFetcher() *mockFetcher {
	return &mockFetcher{responses: make(map[string]interface{})}
}

func (m *mockFetcher) serve(path string, resource interface{}) {
	m.responses[path] = resource
}

func (m *mockFetcher) Get(path string) ([]byte, error) {
	m.getCalls = append(m.getCalls, path)
	r, ok := m.responses[path]
	if !ok {
		return nil, fmt.Errorf("404: no resource at %s", path)
	}
	return xml.Marshal(r)
}

// ───────────────────────────────────────────────────────────────────────
// Constants matching CSIP conformance test assumptions (spec section 3.2)
// ───────────────────────────────────────────────────────────────────────

const (
	testLFDI = "AB12CD34EF56789012345678901234567890ABCD"
	testSFDI = 123456789
	testPIN  = 111115 // CSIP spec section 3.2.3
)

// ───────────────────────────────────────────────────────────────────────
// buildFullResourceTree populates a mockFetcher with a complete
// CSIP-conformant resource tree matching the conformance test setup.
// ───────────────────────────────────────────────────────────────────────

func buildFullResourceTree(m *mockFetcher) {
	boolTrue := true

	m.serve("/dcap", &model.DeviceCapability{
		Resource: model.Resource{Href: "/dcap"},
		PollRate: 300,
		TimeLink: &model.Link{Href: "/tm"},
		EndDeviceListLink: &model.ListLink{
			Link: model.Link{Href: "/edev"}, All: 3,
		},
		MirrorUsagePointListLink: &model.ListLink{
			Link: model.Link{Href: "/mup"}, All: 0,
		},
		SelfDeviceLink: &model.Link{Href: "/sdev"},
	})

	m.serve("/tm", &model.Time{
		Resource: model.Resource{Href: "/tm"}, CurrentTime: 1700000000,
		DstEndTime: 1699160400, DstOffset: 3600, TzOffset: -18000,
		Quality: 7, PollRate: 900,
	})

	m.serve("/edev", &model.EndDeviceList{
		Resource: model.Resource{Href: "/edev"}, All: 3, Results: 3, PollRate: 300,
		EndDevice: []model.EndDevice{
			{Resource: model.Resource{Href: "/edev/0"},
				LFDI: "1111111111111111111111111111111111111111",
				SFDI: 111111111, ChangedTime: 1700000100},
			{Resource: model.Resource{Href: "/edev/1"},
				LFDI: "2222222222222222222222222222222222222222",
				SFDI: 222222222, ChangedTime: 1700000200},
			{Resource: model.Resource{Href: "/edev/2"},
				LFDI: testLFDI, SFDI: testSFDI, ChangedTime: 1700000300,
				Enabled: &boolTrue,
				FunctionSetAssignmentsListLink: &model.ListLink{
					Link: model.Link{Href: "/edev/2/fsa"}, All: 1},
				DERListLink: &model.ListLink{
					Link: model.Link{Href: "/edev/2/der"}, All: 1},
				RegistrationLink: &model.Link{Href: "/edev/2/reg"}},
		},
	})

	m.serve("/edev/2/reg", &model.Registration{
		Resource:           model.Resource{Href: "/edev/2/reg"},
		DateTimeRegistered: 1700000000, PIN: testPIN,
	})

	m.serve("/edev/2/der", &model.DERList{
		Resource: model.Resource{Href: "/edev/2/der"}, All: 1, Results: 1,
		DER: []model.DER{{
			Resource:          model.Resource{Href: "/edev/2/der/0"},
			DERCapabilityLink: &model.Link{Href: "/edev/2/der/0/dercap"},
			DERSettingsLink:   &model.Link{Href: "/edev/2/der/0/derset"},
			DERStatusLink:     &model.Link{Href: "/edev/2/der/0/derstat"},
		}},
	})

	m.serve("/edev/2/fsa", &model.FunctionSetAssignmentsList{
		Resource: model.Resource{Href: "/edev/2/fsa"}, All: 1, Results: 1, PollRate: 300,
		FunctionSetAssignments: []model.FunctionSetAssignments{{
			Resource: model.Resource{Href: "/edev/2/fsa/0"},
			MRID:     "0FB7", Description: "Service Point FSA",
			DERProgramListLink: &model.ListLink{
				Link: model.Link{Href: "/edev/2/fsa/0/derp"}, All: 1},
			TimeLink: &model.Link{Href: "/tm"},
		}},
	})

	m.serve("/edev/2/fsa/0/derp", &model.DERProgramList{
		Resource: model.Resource{Href: "/edev/2/fsa/0/derp"}, All: 1, Results: 1, PollRate: 60,
		DERProgram: []model.DERProgram{{
			Resource: model.Resource{Href: "/derp/0"},
			MRID:     "A1B2", Description: "Service Point DER Program", Primacy: 1,
			DefaultDERControlLink:    &model.Link{Href: "/derp/0/dderc"},
			DERControlListLink:       &model.ListLink{Link: model.Link{Href: "/derp/0/derc"}, All: 1},
			ActiveDERControlListLink: &model.ListLink{Link: model.Link{Href: "/derp/0/actderc"}, All: 0},
		}},
	})

	m.serve("/derp/0/dderc", &model.DefaultDERControl{
		Resource: model.Resource{Href: "/derp/0/dderc"}, MRID: "DD01",
		Description: "Default: export limit 5kW",
		DERControlBase: model.DERControlBase{
			OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 5000},
			OpModConnect: &boolTrue, OpModEnergize: &boolTrue,
		},
	})

	m.serve("/derp/0/derc", &model.DERControlList{
		Resource: model.Resource{Href: "/derp/0/derc"}, All: 1, Results: 1, PollRate: 60,
		DERControl: []model.DERControl{{
			Resource: model.Resource{Href: "/derp/0/derc/0"}, MRID: "C3D4",
			Description: "Limit export to 3kW for 2 hours", CreationTime: 1700000000,
			EventStatus: &model.EventStatus{CurrentStatus: 0, DateTime: 1700000000},
			Interval:    model.DateTimeInterval{Duration: 7200, Start: 1700003600},
			DERControlBase: model.DERControlBase{
				OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 3000},
			},
		}},
	})

	m.serve("/mup", &model.MirrorUsagePointList{
		Resource: model.Resource{Href: "/mup"}, All: 0, Results: 0,
	})
}

// ───────────────────────────────────────────────────────────────────────
// Test: Full Discovery Walk (COMM-002, CORE-005, CORE-009, CORE-010, CORE-012)
// ───────────────────────────────────────────────────────────────────────

func TestFullDiscoveryWalk(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	// COMM-002: DeviceCapability retrieved
	if tree.DeviceCapability == nil {
		t.Fatal("DeviceCapability is nil")
	}
	if tree.DeviceCapability.PollRate != 300 {
		t.Errorf("dcap PollRate = %d, want 300", tree.DeviceCapability.PollRate)
	}

	// CORE-005: Time retrieved, quality=7
	if tree.Time == nil {
		t.Fatal("Time is nil")
	}
	if tree.Time.Quality != 7 {
		t.Errorf("Time.Quality = %d, want 7", tree.Time.Quality)
	}
	if tree.Time.TzOffset != -18000 {
		t.Errorf("Time.TzOffset = %d, want -18000", tree.Time.TzOffset)
	}

	// CORE-005 / ClockOffset: mock /tm has CurrentTime=1700000000 (past).
	// ClockOffset = serverTime - localTime; local time is 2026-era so offset is
	// deeply negative (~−44M s). Verify it was populated (non-zero) and negative.
	if tree.ClockOffset >= 0 {
		t.Errorf("ClockOffset = %d, want negative (mock server time is in the past)", tree.ClockOffset)
	}

	// CORE-009 / BASIC-001: Found our EndDevice by LFDI
	if tree.SelfDevice == nil {
		t.Fatal("SelfDevice is nil")
	}
	if tree.SelfDevice.LFDI != testLFDI {
		t.Errorf("SelfDevice.LFDI = %q", tree.SelfDevice.LFDI)
	}
	if tree.SelfDevice.SFDI != testSFDI {
		t.Errorf("SelfDevice.SFDI = %d", tree.SelfDevice.SFDI)
	}
	if tree.EndDeviceList == nil || len(tree.EndDeviceList.EndDevice) != 3 {
		t.Fatal("EndDeviceList should have 3 devices")
	}

	// DERList discovered
	if tree.DERList == nil || len(tree.DERList.DER) != 1 {
		t.Error("DERList not discovered properly")
	}

	// CORE-010: FSAList retrieved
	if tree.FSAList == nil || len(tree.FSAList.FunctionSetAssignments) != 1 {
		t.Fatal("FSAList not discovered properly")
	}

	// CORE-012: DERProgram + controls
	if len(tree.Programs) != 1 {
		t.Fatalf("got %d programs, want 1", len(tree.Programs))
	}
	ps := tree.Programs[0]
	if ps.Program.Primacy != 1 {
		t.Errorf("Primacy = %d, want 1", ps.Program.Primacy)
	}
	if ps.DefaultControl == nil || ps.DefaultControl.DERControlBase.OpModExpLimW.Value != 5000 {
		t.Error("DefaultControl export limit should be 5000")
	}
	if ps.Controls == nil || len(ps.Controls.DERControl) != 1 {
		t.Fatal("should have 1 DERControl event")
	}
	if ps.Controls.DERControl[0].DERControlBase.OpModExpLimW.Value != 3000 {
		t.Error("DERControl export limit should be 3000")
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: No URL Hardcoding (GEN.004, GEN.010)
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryNoUrlHardcoding(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	_, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if m.getCalls[0] != "/dcap" {
		t.Fatal("first GET was not /dcap")
	}
	for _, path := range m.getCalls {
		if _, ok := m.responses[path]; !ok {
			t.Errorf("walker requested %q which was not served", path)
		}
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: LFDI Not Found (BASIC-001 failure case)
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryLFDINotFound(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
	_, err := w.Discover("/dcap")
	if err == nil {
		t.Fatal("expected error when LFDI not found")
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: LFDI Case Insensitive
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryLFDICaseInsensitive(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, "ab12cd34ef56789012345678901234567890abcd")
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover with lowercase LFDI failed: %v", err)
	}
	if tree.SelfDevice == nil {
		t.Fatal("SelfDevice not found with lowercase LFDI")
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: Missing EndDeviceListLink (GEN.012)
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryMissingEndDeviceListLink(t *testing.T) {
	m := newMockFetcher()
	m.serve("/dcap", &model.DeviceCapability{
		Resource: model.Resource{Href: "/dcap"},
		TimeLink: &model.Link{Href: "/tm"},
	})
	m.serve("/tm", &model.Time{Resource: model.Resource{Href: "/tm"}, CurrentTime: 1700000000, Quality: 7})

	w := NewWalker(m, testLFDI)
	_, err := w.Discover("/dcap")
	if err == nil {
		t.Fatal("expected error when EndDeviceListLink missing")
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: Missing FSAListLink
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryMissingFSALink(t *testing.T) {
	m := newMockFetcher()
	m.serve("/dcap", &model.DeviceCapability{
		Resource:          model.Resource{Href: "/dcap"},
		EndDeviceListLink: &model.ListLink{Link: model.Link{Href: "/edev"}, All: 1},
	})
	m.serve("/edev", &model.EndDeviceList{
		Resource: model.Resource{Href: "/edev"}, All: 1, Results: 1,
		EndDevice: []model.EndDevice{{
			Resource: model.Resource{Href: "/edev/0"},
			LFDI:     testLFDI, SFDI: testSFDI,
		}},
	})

	w := NewWalker(m, testLFDI)
	_, err := w.Discover("/dcap")
	if err == nil {
		t.Fatal("expected error when FSAListLink missing")
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: Two DERPrograms with Different Primacy (BASIC-016, CORE-012)
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryMultiplePrograms(t *testing.T) {
	m := newMockFetcher()
	boolTrue := true

	m.serve("/dcap", &model.DeviceCapability{
		Resource:          model.Resource{Href: "/dcap"},
		EndDeviceListLink: &model.ListLink{Link: model.Link{Href: "/edev"}, All: 1},
	})
	m.serve("/edev", &model.EndDeviceList{
		Resource: model.Resource{Href: "/edev"}, All: 1, Results: 1,
		EndDevice: []model.EndDevice{{
			Resource: model.Resource{Href: "/edev/0"},
			LFDI:     testLFDI, SFDI: testSFDI, Enabled: &boolTrue,
			FunctionSetAssignmentsListLink: &model.ListLink{
				Link: model.Link{Href: "/edev/0/fsa"}, All: 1},
		}},
	})
	m.serve("/edev/0/fsa", &model.FunctionSetAssignmentsList{
		Resource: model.Resource{Href: "/edev/0/fsa"}, All: 1, Results: 1,
		FunctionSetAssignments: []model.FunctionSetAssignments{{
			Resource: model.Resource{Href: "/edev/0/fsa/0"},
			DERProgramListLink: &model.ListLink{
				Link: model.Link{Href: "/edev/0/fsa/0/derp"}, All: 2},
		}},
	})
	m.serve("/edev/0/fsa/0/derp", &model.DERProgramList{
		Resource: model.Resource{Href: "/edev/0/fsa/0/derp"}, All: 2, Results: 2,
		DERProgram: []model.DERProgram{
			{Resource: model.Resource{Href: "/derp/sy"}, MRID: "SY01",
				Primacy: 10, DefaultDERControlLink: &model.Link{Href: "/derp/sy/dderc"},
				DERControlListLink: &model.ListLink{Link: model.Link{Href: "/derp/sy/derc"}, All: 0}},
			{Resource: model.Resource{Href: "/derp/sp"}, MRID: "SP01",
				Primacy: 1, DefaultDERControlLink: &model.Link{Href: "/derp/sp/dderc"},
				DERControlListLink: &model.ListLink{Link: model.Link{Href: "/derp/sp/derc"}, All: 0}},
		},
	})
	m.serve("/derp/sy/dderc", &model.DefaultDERControl{
		Resource: model.Resource{Href: "/derp/sy/dderc"}, MRID: "SYDD",
		DERControlBase: model.DERControlBase{OpModExpLimW: &model.ActivePower{Value: 10000}},
	})
	m.serve("/derp/sp/dderc", &model.DefaultDERControl{
		Resource: model.Resource{Href: "/derp/sp/dderc"}, MRID: "SPDD",
		DERControlBase: model.DERControlBase{OpModExpLimW: &model.ActivePower{Value: 5000}},
	})
	m.serve("/derp/sy/derc", &model.DERControlList{
		Resource: model.Resource{Href: "/derp/sy/derc"}, All: 0, Results: 0})
	m.serve("/derp/sp/derc", &model.DERControlList{
		Resource: model.Resource{Href: "/derp/sp/derc"}, All: 0, Results: 0})

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(tree.Programs) != 2 {
		t.Fatalf("got %d programs, want 2", len(tree.Programs))
	}
	for _, ps := range tree.Programs {
		if ps.DefaultControl == nil {
			t.Errorf("Program %s missing DefaultControl", ps.Program.MRID)
		}
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: PollRate Preserved (CORE-003)
// ───────────────────────────────────────────────────────────────────────

func TestPollRatePreserved(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if tree.DeviceCapability.PollRate != 300 {
		t.Errorf("dcap PollRate = %d, want 300", tree.DeviceCapability.PollRate)
	}
	if tree.Time.PollRate != 900 {
		t.Errorf("Time PollRate = %d, want 900", tree.Time.PollRate)
	}
	if tree.Programs[0].Controls.PollRate != 60 {
		t.Errorf("DERControlList PollRate = %d, want 60", tree.Programs[0].Controls.PollRate)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Test: GET Sequence Audit
// ───────────────────────────────────────────────────────────────────────

func TestDiscoveryGETSequence(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	_, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	t.Logf("Total GETs: %d", len(m.getCalls))
	for i, path := range m.getCalls {
		t.Logf("  GET[%d] = %s", i, path)
	}

	if len(m.getCalls) < 8 || len(m.getCalls) > 15 {
		t.Errorf("unexpected number of GETs: %d", len(m.getCalls))
	}
}

func TestDiscover_ResponseSetPath_Populated(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	// dcap points to /rsps (the list), not the POST target directly.
	m.serve("/dcap", &model.DeviceCapability{
		Resource:         model.Resource{Href: "/dcap"},
		PollRate:         300,
		TimeLink:         &model.Link{Href: "/tm"},
		EndDeviceListLink: &model.ListLink{Link: model.Link{Href: "/edev"}, All: 3},
		MirrorUsagePointListLink: &model.ListLink{Link: model.Link{Href: "/mup"}, All: 0},
		ResponseSetListLink: &model.ListLink{Link: model.Link{Href: "/rsps"}, All: 1},
		SelfDeviceLink:   &model.Link{Href: "/sdev"},
	})
	// /rsps → ResponseSetList → ResponseSet[0].ResponseList.Href is the POST target.
	m.serve("/rsps", &model.ResponseSetList{
		Resource: model.Resource{Href: "/rsps"},
		ResponseSet: []model.ResponseSet{
			{
				Resource:     model.Resource{Href: "/rsps/0"},
				ResponseList: &model.ListLink{Link: model.Link{Href: "/rsps/0/r"}},
			},
		},
	})

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Must be the nested POST target, NOT the list href.
	if tree.ResponseSetPath != "/rsps/0/r" {
		t.Errorf("ResponseSetPath = %q, want /rsps/0/r (ResponseSet[0].ResponseListLink)", tree.ResponseSetPath)
	}
}

func TestDiscover_ResponseSetPath_EmptyWhenAbsent(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	// buildFullResourceTree serves /dcap without ResponseSetListLink.

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tree.ResponseSetPath != "" {
		t.Errorf("ResponseSetPath = %q, want empty when dcap has no ResponseSetListLink",
			tree.ResponseSetPath)
	}
}
