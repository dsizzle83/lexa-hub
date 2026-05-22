package discovery

import (
	"encoding/xml"
	"fmt"
	"testing"

	"lexa-hub/internal/northbound/model"
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
	m.serve("/edev/2/der/0/dercap", &model.DERCapabilityFull{
		Resource:       model.Resource{Href: "/edev/2/der/0/dercap"},
		Type:           83, // storage
		ModesSupported: model.ModeConnect | model.ModeMaxLimW | model.ModeFixedW | model.ModeVoltVar,
		RtgMaxW:        model.ActivePower{Multiplier: 3, Value: 7}, // 7 kW
	})
	m.serve("/edev/2/der/0/derset", &model.DERSettingsFull{
		Resource:    model.Resource{Href: "/edev/2/der/0/derset"},
		UpdatedTime: 1700000000,
		SetMaxW:     &model.ActivePower{Multiplier: 3, Value: 7},
	})
	m.serve("/edev/2/der/0/derstat", &model.DERStatusFull{
		Resource:    model.Resource{Href: "/edev/2/der/0/derstat"},
		ReadingTime: 1700000000,
		GenConnectStatus: &model.DERStatusValue{
			DateTime: 1700000000,
			Value:    model.GenConnectConnected,
		},
		OperationalModeStatus: &model.DERStatusValue{
			DateTime: 1700000000,
			Value:    model.OpStatusOperating,
		},
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

	m.serve("/derp/0/dderc", &model.ExtendedDefaultDERControl{
		Resource: model.Resource{Href: "/derp/0/dderc"}, MRID: "DD01",
		Description: "Default: export limit 5kW",
		DERControlBase: model.ExtendedDERControlBase{
			OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 5000},
			OpModConnect: &boolTrue, OpModEnergize: &boolTrue,
		},
	})

	m.serve("/derp/0/derc", &model.ExtendedDERControlList{
		Resource: model.Resource{Href: "/derp/0/derc"}, All: 1, Results: 1, PollRate: 60,
		DERControl: []model.ExtendedDERControl{{
			Resource: model.Resource{Href: "/derp/0/derc/0"}, MRID: "C3D4",
			Description: "Limit export to 3kW for 2 hours", CreationTime: 1700000000,
			EventStatus: &model.EventStatus{CurrentStatus: 0, DateTime: 1700000000},
			Interval:    model.DateTimeInterval{Duration: 7200, Start: 1700003600},
			DERControlBase: model.ExtendedDERControlBase{
				OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 3000},
			},
		}},
	})

	m.serve("/derp/0/actderc", &model.DERControlList{
		Resource: model.Resource{Href: "/derp/0/actderc"}, All: 0, Results: 0,
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
	if ps.ExtendedDefault == nil || ps.ExtendedDefault.DERControlBase.OpModExpLimW.Value != 5000 {
		t.Error("ExtendedDefault export limit should be 5000")
	}
	if ps.Controls == nil || len(ps.Controls.DERControl) != 1 {
		t.Fatal("should have 1 DERControl event")
	}
	if ps.Controls.DERControl[0].DERControlBase.OpModExpLimW.Value != 3000 {
		t.Error("DERControl export limit should be 3000")
	}
	if len(tree.DERResources) != 1 {
		t.Errorf("DERResources len = %d, want 1", len(tree.DERResources))
	} else {
		rs := tree.DERResources[0]
		if rs.Capability == nil {
			t.Error("DERResources[0].Capability is nil")
		} else if rs.Capability.ModesSupported&model.ModeVoltVar == 0 {
			t.Error("DERCapability should have VoltVar mode set")
		}
		if rs.Status == nil {
			t.Error("DERResources[0].Status is nil")
		} else if rs.Status.GenConnectStatus == nil {
			t.Error("GenConnectStatus is nil")
		}
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

	if len(m.getCalls) < 8 || len(m.getCalls) > 20 {
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

// ─── Pricing function set discovery tests ────────────────────────────────────

func buildPricingResources(m *mockFetcher) {
	// Add TariffProfileListLink to the FSA in the existing tree.
	m.serve("/edev/2/fsa", &model.FunctionSetAssignmentsList{
		Resource: model.Resource{Href: "/edev/2/fsa"}, All: 1, Results: 1,
		FunctionSetAssignments: []model.FunctionSetAssignments{{
			Resource: model.Resource{Href: "/edev/2/fsa/0"},
			MRID:     "0FB7",
			DERProgramListLink: &model.ListLink{
				Link: model.Link{Href: "/edev/2/fsa/0/derp"}, All: 1},
			TariffProfileListLink: &model.ListLink{
				Link: model.Link{Href: "/tp"}, All: 1},
			TimeLink: &model.Link{Href: "/tm"},
		}},
	})

	m.serve("/tp", &model.TariffProfileList{
		Resource: model.Resource{Href: "/tp"}, All: 1, Results: 1,
		TariffProfile: []model.TariffProfile{{
			Resource:                  model.Resource{Href: "/tp/3"},
			MRID:                      "799794f4620b17e00000e566",
			Description:               "PEV TOU Rate",
			Currency:                  840,
			PricePowerOfTenMultiplier: -6,
			Primacy:                   0,
			RateCode:                  "TOU-D-PEV",
			RateComponentListLink: &model.ListLink{
				Link: model.Link{Href: "/tp/3/rc"}, All: 1},
		}},
	})

	m.serve("/tp/3/rc", &model.RateComponentList{
		Resource: model.Resource{Href: "/tp/3/rc"}, All: 1, Results: 1,
		RateComponent: []model.RateComponent{{
			Resource:    model.Resource{Href: "/tp/3/rc/3"},
			MRID:        "fc000b07143d24fc0000e566",
			Description: "TOU-D-PEV",
			ActiveTimeTariffIntervalListLink: &model.ListLink{
				Link: model.Link{Href: "/tp/3/rc/3/acttti"}, All: 1},
			TimeTariffIntervalListLink: &model.ListLink{
				Link: model.Link{Href: "/tp/3/rc/3/tti"}, All: 5},
		}},
	})

	m.serve("/tp/3/rc/3/acttti", &model.TimeTariffIntervalList{
		Resource: model.Resource{Href: "/tp/3/rc/3/acttti"}, All: 1, Results: 1,
		TimeTariffInterval: []model.TimeTariffInterval{{
			Resource: model.Resource{Href: "/tp/3/rc/3/tti/6"},
			MRID:     "41fc7c07e16820770000e566",
			TouTier:  2,
			Interval: model.DateTimeInterval{Duration: 14400, Start: 1357545600},
		}},
	})

	m.serve("/tp/3/rc/3/tti", &model.TimeTariffIntervalList{
		Resource: model.Resource{Href: "/tp/3/rc/3/tti"}, All: 5, Results: 5,
		TimeTariffInterval: []model.TimeTariffInterval{
			{Resource: model.Resource{Href: "/tp/3/rc/3/tti/5"}, MRID: "ef06fa23",
				TouTier: 1, Interval: model.DateTimeInterval{Duration: 28800, Start: 1357516800}},
			{Resource: model.Resource{Href: "/tp/3/rc/3/tti/6"}, MRID: "41fc7c07",
				TouTier: 2, Interval: model.DateTimeInterval{Duration: 14400, Start: 1357545600}},
		},
	})
}

func TestDiscover_PricingFunctionSet(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	buildPricingResources(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(tree.PricingProfiles) != 1 {
		t.Fatalf("PricingProfiles len = %d, want 1", len(tree.PricingProfiles))
	}
	tp := tree.PricingProfiles[0]
	if tp.Profile.MRID != "799794f4620b17e00000e566" {
		t.Errorf("Profile.MRID = %q", tp.Profile.MRID)
	}
	if tp.Profile.PricePowerOfTenMultiplier != -6 {
		t.Errorf("PricePowerOfTenMultiplier = %d, want -6", tp.Profile.PricePowerOfTenMultiplier)
	}
	if len(tp.RateComponents) != 1 {
		t.Fatalf("RateComponents len = %d, want 1", len(tp.RateComponents))
	}
	rc := tp.RateComponents[0]
	if len(rc.ActiveTimeTariffIntervals) != 1 {
		t.Errorf("ActiveTimeTariffIntervals len = %d, want 1", len(rc.ActiveTimeTariffIntervals))
	}
	if len(rc.TimeTariffIntervals) != 2 {
		t.Errorf("TimeTariffIntervals len = %d, want 2", len(rc.TimeTariffIntervals))
	}
	if rc.TimeTariffIntervals[1].TouTier != 2 {
		t.Errorf("second interval TouTier = %d, want 2", rc.TimeTariffIntervals[1].TouTier)
	}
}

func TestDiscover_PricingAbsent_NoError(t *testing.T) {
	// Baseline tree has no TariffProfileListLink — discovery must succeed anyway.
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(tree.PricingProfiles) != 0 {
		t.Errorf("PricingProfiles = %d, want 0 when absent", len(tree.PricingProfiles))
	}
}

// ─── Billing function set discovery tests ────────────────────────────────────

func buildBillingResources(m *mockFetcher) {
	// Re-serve the FSA with a CustomerAccountListLink added.
	m.serve("/edev/2/fsa", &model.FunctionSetAssignmentsList{
		Resource: model.Resource{Href: "/edev/2/fsa"}, All: 1, Results: 1,
		FunctionSetAssignments: []model.FunctionSetAssignments{{
			Resource: model.Resource{Href: "/edev/2/fsa/0"},
			MRID:     "0FB7",
			DERProgramListLink: &model.ListLink{
				Link: model.Link{Href: "/edev/2/fsa/0/derp"}, All: 1},
			CustomerAccountListLink: &model.ListLink{
				Link: model.Link{Href: "/bill"}, All: 1},
			TimeLink: &model.Link{Href: "/tm"},
		}},
	})

	m.serve("/bill", &model.CustomerAccountList{
		Resource: model.Resource{Href: "/bill"}, All: 1, Results: 1,
		CustomerAccount: []model.CustomerAccount{{
			Resource:     model.Resource{Href: "/bill/1"},
			MRID:         "26d0c9722dd639ab0000e566",
			CustomerName: "John Doe",
			Currency:     840,
			CustomerAgreementListLink: &model.ListLink{
				Link: model.Link{Href: "/bill/1/ca"}, All: 1},
		}},
	})

	m.serve("/bill/1/ca", &model.CustomerAgreementList{
		Resource: model.Resource{Href: "/bill/1/ca"}, All: 1, Results: 1,
		CustomerAgreement: []model.CustomerAgreement{{
			Resource:        model.Resource{Href: "/bill/1/ca/1"},
			MRID:            "65f839fc951345e70000e566",
			ServiceLocation: "Elm St.",
			BillingPeriodListLink: &model.ListLink{
				Link: model.Link{Href: "/bill/1/ca/1/bp"}, All: 2},
		}},
	})

	billToDate := int64(83550000)
	lastPeriod := int64(140730000)
	m.serve("/bill/1/ca/1/bp", &model.BillingPeriodList{
		Resource: model.Resource{Href: "/bill/1/ca/1/bp"}, All: 2, Results: 1,
		BillingPeriod: []model.BillingPeriod{{
			Resource:        model.Resource{Href: "/bill/1/ca/1/bp/1"},
			BillToDate:      &billToDate,
			BillLastPeriod:  &lastPeriod,
			Interval:        model.DateTimeInterval{Start: 1360195200, Duration: 2419200},
			StatusTimeStamp: 1361577600,
		}},
	})
}

func TestDiscover_BillingFunctionSet(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	buildBillingResources(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(tree.BillingAccounts) != 1 {
		t.Fatalf("BillingAccounts len = %d, want 1", len(tree.BillingAccounts))
	}
	acct := tree.BillingAccounts[0]
	if acct.Account.CustomerName != "John Doe" {
		t.Errorf("CustomerName = %q", acct.Account.CustomerName)
	}
	if len(acct.Agreements) != 1 {
		t.Fatalf("Agreements len = %d, want 1", len(acct.Agreements))
	}
	ag := acct.Agreements[0]
	if len(ag.BillingPeriods) != 1 {
		t.Fatalf("BillingPeriods len = %d, want 1", len(ag.BillingPeriods))
	}
	bp := ag.BillingPeriods[0]
	if bp.BillToDate == nil || *bp.BillToDate != 83550000 {
		t.Error("BillToDate wrong")
	}
	if bp.Interval.Start != 1360195200 {
		t.Errorf("BillingPeriod.Interval.Start = %d", bp.Interval.Start)
	}
}

// ─── Flow Reservation function set discovery tests ───────────────────────────

func buildFlowReservationResources(m *mockFetcher) {
	// Re-serve the EndDevice with flow reservation links.
	boolTrue := true
	m.serve("/edev", &model.EndDeviceList{
		Resource: model.Resource{Href: "/edev"}, All: 3, Results: 3, PollRate: 300,
		EndDevice: []model.EndDevice{
			{Resource: model.Resource{Href: "/edev/0"},
				LFDI: "1111111111111111111111111111111111111111", SFDI: 111111111},
			{Resource: model.Resource{Href: "/edev/1"},
				LFDI: "2222222222222222222222222222222222222222", SFDI: 222222222},
			{Resource: model.Resource{Href: "/edev/2"},
				LFDI: testLFDI, SFDI: testSFDI, Enabled: &boolTrue,
				FunctionSetAssignmentsListLink: &model.ListLink{
					Link: model.Link{Href: "/edev/2/fsa"}, All: 1},
				DERListLink: &model.ListLink{
					Link: model.Link{Href: "/edev/2/der"}, All: 1},
				RegistrationLink: &model.Link{Href: "/edev/2/reg"},
				FlowReservationRequestListLink: &model.ListLink{
					Link: model.Link{Href: "/edev/2/frq"}, All: 0},
				FlowReservationResponseListLink: &model.ListLink{
					Link: model.Link{Href: "/edev/2/frp"}, All: 1},
			},
		},
	})

	m.serve("/edev/2/frp", &model.FlowReservationResponseList{
		Resource: model.Resource{Href: "/edev/2/frp"}, All: 1, Results: 1,
		FlowReservationResponse: []model.FlowReservationResponse{{
			Resource:     model.Resource{Href: "/edev/2/frp/1"},
			MRID:         "f8afa6fde40db98d0000ea75",
			Description:  "Charge from 1:00 AM to 5:20 AM",
			CreationTime: 1379869260,
			EventStatus: &model.EventStatus{
				CurrentStatus: model.FlowRespStatusScheduled,
				DateTime:      1379869260,
			},
			Interval: model.DateTimeInterval{Duration: 15600, Start: 1379898000},
			EnergyAvailable: &model.UnitValue{Multiplier: 0, Value: 12000},
			PowerAvailable:  &model.UnitValue{Multiplier: 0, Value: 3000},
			Subject:         "68512866203db3b10000e566",
		}},
	})
}

func TestDiscover_FlowReservation(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)
	buildFlowReservationResources(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if tree.FlowReservationRequestPath != "/edev/2/frq" {
		t.Errorf("FlowReservationRequestPath = %q, want /edev/2/frq",
			tree.FlowReservationRequestPath)
	}
	if tree.FlowReservations == nil {
		t.Fatal("FlowReservations is nil")
	}
	if len(tree.FlowReservations.FlowReservationResponse) != 1 {
		t.Fatalf("FlowReservationResponse len = %d, want 1",
			len(tree.FlowReservations.FlowReservationResponse))
	}
	frr := tree.FlowReservations.FlowReservationResponse[0]
	if frr.MRID != "f8afa6fde40db98d0000ea75" {
		t.Errorf("MRID = %q", frr.MRID)
	}
	if frr.Interval.Duration != 15600 {
		t.Errorf("Duration = %d, want 15600", frr.Interval.Duration)
	}
	if frr.EnergyAvailable == nil || frr.EnergyAvailable.Value != 12000 {
		t.Error("EnergyAvailable wrong")
	}
	if frr.Subject != "68512866203db3b10000e566" {
		t.Errorf("Subject = %q", frr.Subject)
	}
}

func TestDiscover_FlowReservation_AbsentIsNonFatal(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed when FR absent: %v", err)
	}
	if tree.FlowReservations != nil {
		t.Error("FlowReservations should be nil when EndDevice has no link")
	}
	if tree.FlowReservationRequestPath != "" {
		t.Error("FlowReservationRequestPath should be empty when EndDevice has no link")
	}
}
