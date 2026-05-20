package model

import (
	"encoding/xml"
	"strings"
	"testing"
)

// These tests verify that our Go structs correctly parse XML that
// matches the IEEE 2030.5 schema. The XML samples are based on the
// CSIP conformance test procedure and the EPRI reference implementation.

func TestDeviceCapabilityParse(t *testing.T) {
	// This is what a real 2030.5 server sends when you GET /dcap.
	// The namespace must be urn:ieee:std:2030.5:ns on the root element.
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<DeviceCapability xmlns="urn:ieee:std:2030.5:ns" href="/dcap" pollRate="300">
  <TimeLink href="/tm"/>
  <EndDeviceListLink href="/edev" all="1"/>
  <MirrorUsagePointListLink href="/mup" all="0"/>
  <SelfDeviceLink href="/sdev"/>
</DeviceCapability>`

	var dcap DeviceCapability
	if err := xml.Unmarshal([]byte(raw), &dcap); err != nil {
		t.Fatalf("Unmarshal DeviceCapability: %v", err)
	}

	if dcap.Href != "/dcap" {
		t.Errorf("Href = %q, want /dcap", dcap.Href)
	}
	if dcap.PollRate != 300 {
		t.Errorf("PollRate = %d, want 300", dcap.PollRate)
	}
	if dcap.TimeLink == nil || dcap.TimeLink.Href != "/tm" {
		t.Error("TimeLink missing or wrong href")
	}
	if dcap.EndDeviceListLink == nil || dcap.EndDeviceListLink.Href != "/edev" {
		t.Error("EndDeviceListLink missing or wrong href")
	}
	if dcap.EndDeviceListLink.All != 1 {
		t.Errorf("EndDeviceListLink.All = %d, want 1", dcap.EndDeviceListLink.All)
	}
	if dcap.MirrorUsagePointListLink == nil || dcap.MirrorUsagePointListLink.Href != "/mup" {
		t.Error("MirrorUsagePointListLink missing or wrong href")
	}
	if dcap.SelfDeviceLink == nil || dcap.SelfDeviceLink.Href != "/sdev" {
		t.Error("SelfDeviceLink missing or wrong href")
	}
}

func TestDeviceCapabilityRoundTrip(t *testing.T) {
	dcap := DeviceCapability{
		Resource: Resource{Href: "/dcap"},
		PollRate: 900,
		TimeLink: &Link{Href: "/tm"},
		EndDeviceListLink: &ListLink{
			Link: Link{Href: "/edev"},
			All:  2,
		},
	}

	data, err := xml.MarshalIndent(dcap, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify namespace is present in output
	if !strings.Contains(string(data), XMLNamespace) {
		t.Errorf("marshalled XML missing namespace %s", XMLNamespace)
	}

	var parsed DeviceCapability
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if parsed.Href != dcap.Href {
		t.Errorf("round-trip Href = %q, want %q", parsed.Href, dcap.Href)
	}
	if parsed.PollRate != dcap.PollRate {
		t.Errorf("round-trip PollRate = %d, want %d", parsed.PollRate, dcap.PollRate)
	}
}

func TestEndDeviceListParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<EndDeviceList xmlns="urn:ieee:std:2030.5:ns" href="/edev" all="1" results="1">
  <EndDevice href="/edev/0" subscribable="0">
    <lFDI>AB12CD34EF56789012345678901234567890ABCD</lFDI>
    <sFDI>123456789</sFDI>
    <changedTime>1700000000</changedTime>
    <enabled>true</enabled>
    <FunctionSetAssignmentsListLink href="/edev/0/fsa" all="1"/>
    <DERListLink href="/edev/0/der" all="1"/>
    <RegistrationLink href="/edev/0/reg"/>
  </EndDevice>
</EndDeviceList>`

	var list EndDeviceList
	if err := xml.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("Unmarshal EndDeviceList: %v", err)
	}

	if list.All != 1 || list.Results != 1 {
		t.Errorf("All=%d Results=%d, want 1/1", list.All, list.Results)
	}
	if len(list.EndDevice) != 1 {
		t.Fatalf("got %d EndDevices, want 1", len(list.EndDevice))
	}

	ed := list.EndDevice[0]
	if ed.Href != "/edev/0" {
		t.Errorf("EndDevice.Href = %q", ed.Href)
	}
	if ed.LFDI != "AB12CD34EF56789012345678901234567890ABCD" {
		t.Errorf("LFDI = %q", ed.LFDI)
	}
	if ed.SFDI != 123456789 {
		t.Errorf("SFDI = %d", ed.SFDI)
	}
	if ed.FunctionSetAssignmentsListLink == nil || ed.FunctionSetAssignmentsListLink.Href != "/edev/0/fsa" {
		t.Error("FSAListLink missing or wrong")
	}
	if ed.DERListLink == nil || ed.DERListLink.Href != "/edev/0/der" {
		t.Error("DERListLink missing or wrong")
	}
}

func TestFunctionSetAssignmentsListParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<FunctionSetAssignmentsList xmlns="urn:ieee:std:2030.5:ns" href="/edev/0/fsa" all="1" results="1" pollRate="300">
  <FunctionSetAssignments href="/edev/0/fsa/0">
    <mRID>0FB7</mRID>
    <description>Default FSA</description>
    <DERProgramListLink href="/edev/0/fsa/0/derp" all="1"/>
  </FunctionSetAssignments>
</FunctionSetAssignmentsList>`

	var list FunctionSetAssignmentsList
	if err := xml.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("Unmarshal FSAList: %v", err)
	}

	if len(list.FunctionSetAssignments) != 1 {
		t.Fatalf("got %d FSAs, want 1", len(list.FunctionSetAssignments))
	}
	fsa := list.FunctionSetAssignments[0]
	if fsa.DERProgramListLink == nil || fsa.DERProgramListLink.Href != "/edev/0/fsa/0/derp" {
		t.Error("DERProgramListLink missing or wrong")
	}
}

func TestDERProgramListParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<DERProgramList xmlns="urn:ieee:std:2030.5:ns" href="/edev/0/fsa/0/derp" all="1" results="1" pollRate="60">
  <DERProgram href="/derp/0" subscribable="3">
    <mRID>A1B2</mRID>
    <description>Test DER Program</description>
    <primacy>1</primacy>
    <DefaultDERControlLink href="/derp/0/dderc"/>
    <DERControlListLink href="/derp/0/derc" all="2"/>
    <ActiveDERControlListLink href="/derp/0/actderc" all="0"/>
  </DERProgram>
</DERProgramList>`

	var list DERProgramList
	if err := xml.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("Unmarshal DERProgramList: %v", err)
	}

	if len(list.DERProgram) != 1 {
		t.Fatalf("got %d programs, want 1", len(list.DERProgram))
	}
	prog := list.DERProgram[0]
	if prog.Primacy != 1 {
		t.Errorf("Primacy = %d, want 1", prog.Primacy)
	}
	if prog.DefaultDERControlLink == nil || prog.DefaultDERControlLink.Href != "/derp/0/dderc" {
		t.Error("DefaultDERControlLink missing or wrong")
	}
	if prog.DERControlListLink == nil || prog.DERControlListLink.All != 2 {
		t.Errorf("DERControlListLink.All = %d, want 2", prog.DERControlListLink.All)
	}
}

func TestDERControlParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<DERControl xmlns="urn:ieee:std:2030.5:ns" href="/derp/0/derc/0">
  <mRID>C3D4</mRID>
  <description>Limit export to 3kW</description>
  <creationTime>1700000000</creationTime>
  <EventStatus>
    <currentStatus>1</currentStatus>
    <dateTime>1700000100</dateTime>
    <potentiallySuperseded>false</potentiallySuperseded>
  </EventStatus>
  <interval>
    <duration>7200</duration>
    <start>1700000100</start>
  </interval>
  <DERControlBase>
    <opModExpLimW>
      <multiplier>0</multiplier>
      <value>3000</value>
    </opModExpLimW>
  </DERControlBase>
</DERControl>`

	var ctrl DERControl
	if err := xml.Unmarshal([]byte(raw), &ctrl); err != nil {
		t.Fatalf("Unmarshal DERControl: %v", err)
	}

	if ctrl.Href != "/derp/0/derc/0" {
		t.Errorf("Href = %q", ctrl.Href)
	}
	if ctrl.Interval.Duration != 7200 {
		t.Errorf("Duration = %d, want 7200", ctrl.Interval.Duration)
	}
	if ctrl.Interval.Start != 1700000100 {
		t.Errorf("Start = %d", ctrl.Interval.Start)
	}
	if ctrl.DERControlBase.OpModExpLimW == nil {
		t.Fatal("OpModExpLimW is nil")
	}
	if ctrl.DERControlBase.OpModExpLimW.Value != 3000 {
		t.Errorf("OpModExpLimW.Value = %d, want 3000", ctrl.DERControlBase.OpModExpLimW.Value)
	}
	if ctrl.EventStatus == nil || ctrl.EventStatus.CurrentStatus != 1 {
		t.Error("EventStatus missing or wrong")
	}
}

func TestDERControlListParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<DERControlList xmlns="urn:ieee:std:2030.5:ns" href="/derp/0/derc" all="2" results="2" pollRate="60">
  <DERControl href="/derp/0/derc/0">
    <mRID>C3D4</mRID>
    <interval>
      <duration>3600</duration>
      <start>1700000100</start>
    </interval>
    <DERControlBase>
      <opModMaxLimW>
        <multiplier>0</multiplier>
        <value>5000</value>
      </opModMaxLimW>
    </DERControlBase>
  </DERControl>
  <DERControl href="/derp/0/derc/1">
    <mRID>C3D5</mRID>
    <interval>
      <duration>3600</duration>
      <start>1700003700</start>
    </interval>
    <DERControlBase>
      <opModMaxLimW>
        <multiplier>0</multiplier>
        <value>3000</value>
      </opModMaxLimW>
    </DERControlBase>
  </DERControl>
</DERControlList>`

	var list DERControlList
	if err := xml.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("Unmarshal DERControlList: %v", err)
	}
	if list.All != 2 || len(list.DERControl) != 2 {
		t.Errorf("All=%d len=%d", list.All, len(list.DERControl))
	}
}

func TestDefaultDERControlParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<DefaultDERControl xmlns="urn:ieee:std:2030.5:ns" href="/derp/0/dderc">
  <mRID>DD01</mRID>
  <description>Default: export limit 5kW</description>
  <DERControlBase>
    <opModExpLimW>
      <multiplier>0</multiplier>
      <value>5000</value>
    </opModExpLimW>
    <opModConnect>true</opModConnect>
    <opModEnergize>true</opModEnergize>
  </DERControlBase>
</DefaultDERControl>`

	var dderc DefaultDERControl
	if err := xml.Unmarshal([]byte(raw), &dderc); err != nil {
		t.Fatalf("Unmarshal DefaultDERControl: %v", err)
	}
	if dderc.DERControlBase.OpModExpLimW == nil || dderc.DERControlBase.OpModExpLimW.Value != 5000 {
		t.Error("OpModExpLimW missing or wrong value")
	}
	if dderc.DERControlBase.OpModConnect == nil || *dderc.DERControlBase.OpModConnect != true {
		t.Error("OpModConnect should be true")
	}
}

func TestTimeParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<Time xmlns="urn:ieee:std:2030.5:ns" href="/tm" pollRate="900">
  <currentTime>1700000000</currentTime>
  <dstEndTime>1699160400</dstEndTime>
  <dstOffset>3600</dstOffset>
  <tzOffset>-18000</tzOffset>
  <quality>7</quality>
</Time>`

	var tm Time
	if err := xml.Unmarshal([]byte(raw), &tm); err != nil {
		t.Fatalf("Unmarshal Time: %v", err)
	}
	if tm.CurrentTime != 1700000000 {
		t.Errorf("CurrentTime = %d", tm.CurrentTime)
	}
	if tm.TzOffset != -18000 {
		t.Errorf("TzOffset = %d, want -18000", tm.TzOffset)
	}
}

func TestMirrorUsagePointListParse(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<MirrorUsagePointList xmlns="urn:ieee:std:2030.5:ns" href="/mup" all="1" results="1">
  <MirrorUsagePoint href="/mup/0">
    <mRID>5509D69F8B35359500000000000091</mRID>
    <description>Site Meter</description>
    <roleFlags>49</roleFlags>
    <serviceCategoryKind>0</serviceCategoryKind>
    <status>0</status>
    <deviceLFDI>AB12CD34EF56789012345678901234567890ABCD</deviceLFDI>
    <postRate>60</postRate>
  </MirrorUsagePoint>
</MirrorUsagePointList>`

	var list MirrorUsagePointList
	if err := xml.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("Unmarshal MirrorUsagePointList: %v", err)
	}
	if len(list.MirrorUsagePoint) != 1 {
		t.Fatalf("got %d MUPs, want 1", len(list.MirrorUsagePoint))
	}
	mup := list.MirrorUsagePoint[0]
	if mup.PostRate != 60 {
		t.Errorf("PostRate = %d, want 60", mup.PostRate)
	}
}
