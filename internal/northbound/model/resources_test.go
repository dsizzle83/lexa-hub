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

// ─── Pricing function set tests ──────────────────────────────────────────────

func TestTariffProfileParse(t *testing.T) {
	raw := `<TariffProfile xmlns="urn:ieee:std:2030.5:ns" href="/tp/3">
  <mRID>799794f4620b17e00000e566</mRID>
  <description>PEV TOU Rate</description>
  <currency>840</currency>
  <pricePowerOfTenMultiplier>-6</pricePowerOfTenMultiplier>
  <primacy>0</primacy>
  <rateCode>TOU-D-PEV Baseline 6</rateCode>
  <RateComponentListLink all="1" href="/tp/3/rc"/>
  <serviceCategoryKind>0</serviceCategoryKind>
</TariffProfile>`

	var tp TariffProfile
	if err := xml.Unmarshal([]byte(raw), &tp); err != nil {
		t.Fatalf("Unmarshal TariffProfile: %v", err)
	}
	if tp.MRID != "799794f4620b17e00000e566" {
		t.Errorf("MRID = %q", tp.MRID)
	}
	if tp.Currency != 840 {
		t.Errorf("Currency = %d, want 840", tp.Currency)
	}
	if tp.PricePowerOfTenMultiplier != -6 {
		t.Errorf("PricePowerOfTenMultiplier = %d, want -6", tp.PricePowerOfTenMultiplier)
	}
	if tp.Primacy != 0 {
		t.Errorf("Primacy = %d, want 0", tp.Primacy)
	}
	if tp.RateComponentListLink == nil || tp.RateComponentListLink.Href != "/tp/3/rc" {
		t.Error("RateComponentListLink missing or wrong href")
	}
	if tp.RateComponentListLink.All != 1 {
		t.Errorf("RateComponentListLink.All = %d, want 1", tp.RateComponentListLink.All)
	}
}

func TestTimeTariffIntervalParse(t *testing.T) {
	raw := `<TimeTariffInterval xmlns="urn:ieee:std:2030.5:ns" href="/tp/3/rc/3/tti/5" subscribable="1">
  <mRID>ef06fa23dc0a0f650000e566</mRID>
  <description>Off-Peak 1</description>
  <creationTime>1357430400</creationTime>
  <EventStatus>
    <currentStatus>0</currentStatus>
    <dateTime>1357430400</dateTime>
    <potentiallySuperseded>false</potentiallySuperseded>
  </EventStatus>
  <interval>
    <duration>28800</duration>
    <start>1357516800</start>
  </interval>
  <randomizeDuration>300</randomizeDuration>
  <randomizeStart>300</randomizeStart>
  <ConsumptionTariffIntervalListLink all="1" href="/tp/3/rc/3/tti/5/cti"/>
  <touTier>1</touTier>
</TimeTariffInterval>`

	var tti TimeTariffInterval
	if err := xml.Unmarshal([]byte(raw), &tti); err != nil {
		t.Fatalf("Unmarshal TimeTariffInterval: %v", err)
	}
	if tti.TouTier != 1 {
		t.Errorf("TouTier = %d, want 1", tti.TouTier)
	}
	if tti.Interval.Duration != 28800 {
		t.Errorf("Interval.Duration = %d, want 28800", tti.Interval.Duration)
	}
	if tti.Interval.Start != 1357516800 {
		t.Errorf("Interval.Start = %d, want 1357516800", tti.Interval.Start)
	}
	if tti.RandomizeStart == nil || *tti.RandomizeStart != 300 {
		t.Error("RandomizeStart != 300")
	}
	if tti.RandomizeDuration == nil || *tti.RandomizeDuration != 300 {
		t.Error("RandomizeDuration != 300")
	}
	if tti.ConsumptionTariffIntervalListLink == nil {
		t.Error("ConsumptionTariffIntervalListLink is nil")
	}
	if tti.EventStatus == nil || tti.EventStatus.CurrentStatus != 0 {
		t.Error("EventStatus missing or wrong")
	}
}

func TestConsumptionTariffIntervalParse(t *testing.T) {
	raw := `<ConsumptionTariffIntervalList xmlns="urn:ieee:std:2030.5:ns" all="1" href="/tp/3/rc/3/tti/5/cti" results="1">
  <ConsumptionTariffInterval href="/tp/3/rc/3/tti/5/cti/1">
    <consumptionBlock>1</consumptionBlock>
    <price>113000</price>
    <startValue>0</startValue>
  </ConsumptionTariffInterval>
</ConsumptionTariffIntervalList>`

	var ctil ConsumptionTariffIntervalList
	if err := xml.Unmarshal([]byte(raw), &ctil); err != nil {
		t.Fatalf("Unmarshal ConsumptionTariffIntervalList: %v", err)
	}
	if ctil.All != 1 || len(ctil.ConsumptionTariffInterval) != 1 {
		t.Fatalf("expected 1 entry, got all=%d results=%d", ctil.All, len(ctil.ConsumptionTariffInterval))
	}
	cti := ctil.ConsumptionTariffInterval[0]
	if cti.ConsumptionBlock != 1 {
		t.Errorf("ConsumptionBlock = %d, want 1", cti.ConsumptionBlock)
	}
	if cti.Price != 113000 {
		t.Errorf("Price = %d, want 113000", cti.Price)
	}
	if cti.StartValue != 0 {
		t.Errorf("StartValue = %d, want 0", cti.StartValue)
	}
}

// ─── Billing function set tests ──────────────────────────────────────────────

func TestCustomerAccountParse(t *testing.T) {
	raw := `<CustomerAccount xmlns="urn:ieee:std:2030.5:ns" href="/bill/1">
  <mRID>26d0c9722dd639ab0000e566</mRID>
  <currency>840</currency>
  <customerAccount>981273648</customerAccount>
  <CustomerAgreementListLink all="1" href="/bill/1/ca"/>
  <customerName>John Doe</customerName>
  <pricePowerOfTenMultiplier>-6</pricePowerOfTenMultiplier>
  <ServiceSupplierLink href="/ss"/>
</CustomerAccount>`

	var ca CustomerAccount
	if err := xml.Unmarshal([]byte(raw), &ca); err != nil {
		t.Fatalf("Unmarshal CustomerAccount: %v", err)
	}
	if ca.CustomerName != "John Doe" {
		t.Errorf("CustomerName = %q, want John Doe", ca.CustomerName)
	}
	if ca.Currency != 840 {
		t.Errorf("Currency = %d, want 840", ca.Currency)
	}
	if ca.CustomerAgreementListLink == nil || ca.CustomerAgreementListLink.Href != "/bill/1/ca" {
		t.Error("CustomerAgreementListLink missing or wrong")
	}
	if ca.ServiceSupplierLink == nil || ca.ServiceSupplierLink.Href != "/ss" {
		t.Error("ServiceSupplierLink missing or wrong")
	}
}

func TestBillingPeriodParse(t *testing.T) {
	raw := `<BillingPeriodList xmlns="urn:ieee:std:2030.5:ns" all="2" href="/bill/1/ca/1/bp" results="1" subscribable="1">
  <BillingPeriod href="/bill/1/ca/1/bp">
    <billLastPeriod>140730000</billLastPeriod>
    <billToDate>83550000</billToDate>
    <interval>
      <duration>2419200</duration>
      <start>1360195200</start>
    </interval>
    <statusTimeStamp>1361577600</statusTimeStamp>
  </BillingPeriod>
</BillingPeriodList>`

	var bpl BillingPeriodList
	if err := xml.Unmarshal([]byte(raw), &bpl); err != nil {
		t.Fatalf("Unmarshal BillingPeriodList: %v", err)
	}
	if len(bpl.BillingPeriod) != 1 {
		t.Fatalf("expected 1 BillingPeriod, got %d", len(bpl.BillingPeriod))
	}
	bp := bpl.BillingPeriod[0]
	if bp.BillLastPeriod == nil || *bp.BillLastPeriod != 140730000 {
		t.Error("BillLastPeriod wrong")
	}
	if bp.BillToDate == nil || *bp.BillToDate != 83550000 {
		t.Error("BillToDate wrong")
	}
	if bp.Interval.Start != 1360195200 {
		t.Errorf("Interval.Start = %d, want 1360195200", bp.Interval.Start)
	}
	if bp.Interval.Duration != 2419200 {
		t.Errorf("Interval.Duration = %d, want 2419200", bp.Interval.Duration)
	}
}

// ─── Flow Reservation function set tests ─────────────────────────────────────

func TestFlowReservationResponseParse(t *testing.T) {
	raw := `<FlowReservationResponseList xmlns="urn:ieee:std:2030.5:ns" all="1" href="/edev/3/frp" results="1" subscribable="1">
  <FlowReservationResponse href="/edev/3/frp/1" subscribable="1">
    <mRID>f8afa6fde40db98d0000ea75</mRID>
    <description>Charge from 1:00 AM to 5:20 AM</description>
    <creationTime>1379869260</creationTime>
    <EventStatus>
      <currentStatus>0</currentStatus>
      <dateTime>1379869260</dateTime>
      <potentiallySuperseded>false</potentiallySuperseded>
    </EventStatus>
    <interval>
      <duration>15600</duration>
      <start>1379898000</start>
    </interval>
    <energyAvailable>
      <multiplier>0</multiplier>
      <value>12000</value>
    </energyAvailable>
    <powerAvailable>
      <multiplier>0</multiplier>
      <value>3000</value>
    </powerAvailable>
    <subject>68512866203db3b10000e566</subject>
  </FlowReservationResponse>
</FlowReservationResponseList>`

	var frrl FlowReservationResponseList
	if err := xml.Unmarshal([]byte(raw), &frrl); err != nil {
		t.Fatalf("Unmarshal FlowReservationResponseList: %v", err)
	}
	if len(frrl.FlowReservationResponse) != 1 {
		t.Fatalf("expected 1 response, got %d", len(frrl.FlowReservationResponse))
	}
	frr := frrl.FlowReservationResponse[0]
	if frr.MRID != "f8afa6fde40db98d0000ea75" {
		t.Errorf("MRID = %q", frr.MRID)
	}
	if frr.Interval.Start != 1379898000 {
		t.Errorf("Interval.Start = %d, want 1379898000", frr.Interval.Start)
	}
	if frr.Interval.Duration != 15600 {
		t.Errorf("Interval.Duration = %d, want 15600", frr.Interval.Duration)
	}
	if frr.Subject != "68512866203db3b10000e566" {
		t.Errorf("Subject = %q", frr.Subject)
	}
	if frr.EnergyAvailable == nil || frr.EnergyAvailable.Value != 12000 {
		t.Error("EnergyAvailable wrong")
	}
	if frr.PowerAvailable == nil || frr.PowerAvailable.Value != 3000 {
		t.Error("PowerAvailable wrong")
	}
	if frr.EventStatus == nil || frr.EventStatus.CurrentStatus != 0 {
		t.Error("EventStatus missing or wrong")
	}
}

func TestFlowReservationRequestRoundTrip(t *testing.T) {
	req := FlowReservationRequest{
		MRID:              "68512866203db3b10000e566",
		Description:       "Charge from 12:00 AM to 8:00 AM",
		CreationTime:      1379869200,
		DurationRequested: 7371,
		EnergyRequested: &UnitValue{
			Multiplier: 3,
			Value:      12,
		},
		IntervalRequested: DateTimeInterval{
			Duration: 28800,
			Start:    1379894400,
		},
		PowerRequested: &UnitValue{
			Multiplier: 3,
			Value:      7,
		},
		RequestStatus: RequestStatus{
			DateTime:      1379869200,
			RequestStatus: FlowReqStatusRequested,
		},
	}

	data, err := xml.MarshalIndent(req, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed FlowReservationRequest
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if parsed.MRID != req.MRID {
		t.Errorf("MRID = %q, want %q", parsed.MRID, req.MRID)
	}
	if parsed.RequestStatus.RequestStatus != FlowReqStatusRequested {
		t.Errorf("RequestStatus = %d, want %d", parsed.RequestStatus.RequestStatus, FlowReqStatusRequested)
	}
	if parsed.EnergyRequested == nil || parsed.EnergyRequested.Value != 12 {
		t.Error("EnergyRequested wrong after round-trip")
	}
}

// ─── ReadingType extension tests ─────────────────────────────────────────────

func TestReadingTypeWithTouFields(t *testing.T) {
	raw := `<ReadingType xmlns="urn:ieee:std:2030.5:ns" href="/rt/1">
  <accumulationBehaviour>4</accumulationBehaviour>
  <commodity>1</commodity>
  <dataQualifier>12</dataQualifier>
  <flowDirection>1</flowDirection>
  <intervalLength>3600</intervalLength>
  <kind>12</kind>
  <numberOfConsumptionBlocks>1</numberOfConsumptionBlocks>
  <numberOfTouTiers>3</numberOfTouTiers>
  <phase>0</phase>
  <powerOfTenMultiplier>3</powerOfTenMultiplier>
  <tieredConsumptionBlocks>false</tieredConsumptionBlocks>
  <uom>72</uom>
</ReadingType>`

	var rt ReadingType
	if err := xml.Unmarshal([]byte(raw), &rt); err != nil {
		t.Fatalf("Unmarshal ReadingType: %v", err)
	}
	if rt.NumberOfConsumptionBlocks != 1 {
		t.Errorf("NumberOfConsumptionBlocks = %d, want 1", rt.NumberOfConsumptionBlocks)
	}
	if rt.NumberOfTouTiers != 3 {
		t.Errorf("NumberOfTouTiers = %d, want 3", rt.NumberOfTouTiers)
	}
	if rt.TieredConsumptionBlocks == nil || *rt.TieredConsumptionBlocks != false {
		t.Error("TieredConsumptionBlocks should be false")
	}
	if rt.IntervalLength != 3600 {
		t.Errorf("IntervalLength = %d, want 3600", rt.IntervalLength)
	}
}
