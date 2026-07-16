package publish

import (
	"encoding/json"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/schedule"
	"lexa-hub/internal/northbound/scheduler"
	model "lexa-proto/csipmodel"
)

// fakeClient is a minimal mqtt.Client double capturing every Publish call
// (topic, QoS, retained, payload), for asserting emitted bus payloads without
// a real broker. Every other method panics if called.
type fakeClient struct {
	publishes []capturedPublish
}

type capturedPublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

func (f *fakeClient) IsConnected() bool      { return true }
func (f *fakeClient) IsConnectionOpen() bool { return true }
func (f *fakeClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeClient) Disconnect(quiesce uint) {
}
func (f *fakeClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.publishes = append(f.publishes, capturedPublish{topic: topic, qos: qos, retained: retained, payload: b})
	return &doneToken{}
}
func (f *fakeClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeClient) Unsubscribe(topics ...string) mqtt.Token { panic("not implemented") }
func (f *fakeClient) AddRoute(topic string, callback mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

type doneToken struct{}

func (t *doneToken) Wait() bool                       { return true }
func (t *doneToken) WaitTimeout(d time.Duration) bool { return true }
func (t *doneToken) Done() <-chan struct{}            { c := make(chan struct{}); close(c); return c }
func (t *doneToken) Error() error                     { return nil }

func (f *fakeClient) only(t *testing.T) capturedPublish {
	t.Helper()
	if len(f.publishes) != 1 {
		t.Fatalf("Publish called %d times, want 1", len(f.publishes))
	}
	return f.publishes[0]
}

func apw(mult int8, val int16) *model.ActivePower {
	return &model.ActivePower{Multiplier: mult, Value: val}
}

func i64(v int64) *int64 { return &v }

// TestSchedule_PublishesRetainedQoS1 verifies the schedule publish uses the
// retained/QoS-1 contract (mqttutil.PublishJSONRetained) and maps a slot's
// scalar fields correctly.
func TestSchedule_PublishesRetainedQoS1(t *testing.T) {
	fc := &fakeClient{}
	sched := &schedule.DER24hSchedule{
		WindowStart: 1000,
		WindowEnd:   2000,
		BuildTime:   1000,
		ClockOffset: 5,
		Slots: []schedule.ScheduleSlot{
			{
				Start: 1000, End: 1500, Source: "event", MRID: "E1", ProgramMRID: "P1", Primacy: 1,
				Base: model.DERControlBase{OpModExpLimW: apw(1, 500)}, // 500 * 10^1 = 5000 W
			},
		},
	}

	Schedule(fc, sched)

	p := fc.only(t)
	if p.topic != bus.TopicNorthboundSchedule {
		t.Errorf("topic = %q, want %q", p.topic, bus.TopicNorthboundSchedule)
	}
	if p.qos != 1 || !p.retained {
		t.Errorf("qos/retained = %d/%v, want 1/true", p.qos, p.retained)
	}

	var msg bus.DERScheduleMsg
	if err := json.Unmarshal(p.payload, &msg); err != nil {
		t.Fatalf("unmarshal DERScheduleMsg: %v", err)
	}
	if msg.ClockOffset != 5 {
		t.Errorf("ClockOffset = %d, want 5", msg.ClockOffset)
	}
	if len(msg.Slots) != 1 {
		t.Fatalf("Slots = %d, want 1", len(msg.Slots))
	}
	if msg.Slots[0].ExpLimW == nil || *msg.Slots[0].ExpLimW != 5000 {
		t.Errorf("ExpLimW = %v, want 5000", msg.Slots[0].ExpLimW)
	}
	if msg.Slots[0].MRID != "E1" || msg.Slots[0].ProgramMRID != "P1" {
		t.Errorf("slot identity = %+v, want mrid=E1 programMRID=P1", msg.Slots[0])
	}
}

// TestPricing_PublishesRetainedAndSplitsActiveScheduled verifies the pricing
// publish keeps active vs. still-live-scheduled intervals separate, dropping
// intervals that have already ended.
func TestPricing_PublishesRetainedAndSplitsActiveScheduled(t *testing.T) {
	fc := &fakeClient{}
	tree := &discovery.ResourceTree{
		PricingProfiles: []discovery.TariffState{{
			Profile: model.TariffProfile{MRID: "TP1", Currency: 840, Primacy: 1, RateCode: "R1"},
			RateComponents: []discovery.RateComponentState{{
				Component: model.RateComponent{MRID: "RC1"},
				ActiveTimeTariffIntervals: []model.TimeTariffInterval{
					{MRID: "TTI-active", Interval: model.DateTimeInterval{Start: 900, Duration: 200}},
				},
				TimeTariffIntervals: []model.TimeTariffInterval{
					{MRID: "TTI-future", Interval: model.DateTimeInterval{Start: 2000, Duration: 100}},
					{MRID: "TTI-past", Interval: model.DateTimeInterval{Start: 100, Duration: 50}}, // ended before serverNow
				},
			}},
		}},
	}

	Pricing(fc, tree, 1000 /* serverNow */)

	p := fc.only(t)
	if p.topic != bus.TopicCSIPPricing || p.qos != 1 || !p.retained {
		t.Fatalf("publish = %+v, want topic=%s qos=1 retained=true", p, bus.TopicCSIPPricing)
	}
	var msg bus.PricingUpdate
	if err := json.Unmarshal(p.payload, &msg); err != nil {
		t.Fatalf("unmarshal PricingUpdate: %v", err)
	}
	if len(msg.TariffProfiles) != 1 {
		t.Fatalf("TariffProfiles = %d, want 1", len(msg.TariffProfiles))
	}
	rc := msg.TariffProfiles[0].RateComponents[0]
	if len(rc.ActiveIntervals) != 1 || rc.ActiveIntervals[0].MRID != "TTI-active" {
		t.Errorf("ActiveIntervals = %+v, want [TTI-active]", rc.ActiveIntervals)
	}
	if len(rc.ScheduledIntervals) != 1 || rc.ScheduledIntervals[0].MRID != "TTI-future" {
		t.Errorf("ScheduledIntervals = %+v, want [TTI-future] (TTI-past must be excluded, already ended)", rc.ScheduledIntervals)
	}
}

// TestBilling_PublishesRetainedAndMapsPeriods verifies the billing publish
// maps CustomerAccount → Agreement → BillingPeriod nesting through unchanged.
func TestBilling_PublishesRetainedAndMapsPeriods(t *testing.T) {
	fc := &fakeClient{}
	tree := &discovery.ResourceTree{
		BillingAccounts: []discovery.CustomerAccountState{{
			Account: model.CustomerAccount{MRID: "CA1", CustomerName: "Alice", Currency: 840},
			Agreements: []discovery.CustomerAgreementState{{
				Agreement: model.CustomerAgreement{MRID: "AG1", ServiceLocation: "loc1"},
				BillingPeriods: []model.BillingPeriod{
					{Interval: model.DateTimeInterval{Start: 1, Duration: 2}, BillLastPeriod: i64(100), BillToDate: i64(50)},
				},
			}},
		}},
	}

	Billing(fc, tree)

	p := fc.only(t)
	if p.topic != bus.TopicCSIPBilling || p.qos != 1 || !p.retained {
		t.Fatalf("publish = %+v, want topic=%s qos=1 retained=true", p, bus.TopicCSIPBilling)
	}
	var msg bus.BillingUpdate
	if err := json.Unmarshal(p.payload, &msg); err != nil {
		t.Fatalf("unmarshal BillingUpdate: %v", err)
	}
	if len(msg.CustomerAccounts) != 1 || msg.CustomerAccounts[0].MRID != "CA1" {
		t.Fatalf("CustomerAccounts = %+v, want one with MRID=CA1", msg.CustomerAccounts)
	}
	ag := msg.CustomerAccounts[0].Agreements[0]
	if ag.MRID != "AG1" || len(ag.BillingPeriods) != 1 {
		t.Fatalf("Agreement = %+v, want MRID=AG1 with one BillingPeriod", ag)
	}
	bp := ag.BillingPeriods[0]
	if bp.BillLastPeriod == nil || *bp.BillLastPeriod != 100 || bp.BillToDate == nil || *bp.BillToDate != 50 {
		t.Errorf("BillingPeriod = %+v, want BillLastPeriod=100 BillToDate=50", bp)
	}
}

// TestFlowReservations_PublishesRetainedAndMapsAvailability verifies the FR
// status publish maps EnergyAvailable/PowerAvailable UnitValues to floats
// (scaled by the power-of-ten multiplier).
func TestFlowReservations_PublishesRetainedAndMapsAvailability(t *testing.T) {
	fc := &fakeClient{}
	tree := &discovery.ResourceTree{
		FlowReservations: &model.FlowReservationResponseList{
			FlowReservationResponse: []model.FlowReservationResponse{{
				MRID:            "FRR1",
				Subject:         "FRQ1",
				EventStatus:     &model.EventStatus{CurrentStatus: 2},
				Interval:        model.DateTimeInterval{Start: 500, Duration: 100},
				EnergyAvailable: &model.UnitValue{Multiplier: 1, Value: 500}, // 500*10^1 = 5000
				PowerAvailable:  &model.UnitValue{Multiplier: 0, Value: 3300},
			}},
		},
	}

	FlowReservations(fc, tree)

	p := fc.only(t)
	if p.topic != bus.TopicCSIPFRStatus || p.qos != 1 || !p.retained {
		t.Fatalf("publish = %+v, want topic=%s qos=1 retained=true", p, bus.TopicCSIPFRStatus)
	}
	var msg bus.FlowReservationStatusMsg
	if err := json.Unmarshal(p.payload, &msg); err != nil {
		t.Fatalf("unmarshal FlowReservationStatusMsg: %v", err)
	}
	if len(msg.Reservations) != 1 {
		t.Fatalf("Reservations = %d, want 1", len(msg.Reservations))
	}
	r := msg.Reservations[0]
	if r.MRID != "FRR1" || r.Subject != "FRQ1" || r.CurrentStatus != 2 {
		t.Errorf("reservation identity = %+v", r)
	}
	if r.EnergyAvailWh == nil || *r.EnergyAvailWh != 5000 {
		t.Errorf("EnergyAvailWh = %v, want 5000", r.EnergyAvailWh)
	}
	if r.PowerAvailW == nil || *r.PowerAvailW != 3300 {
		t.Errorf("PowerAvailW = %v, want 3300", r.PowerAvailW)
	}
}

// TestFlowReservations_NoFlowReservationsPublishesEmpty verifies a nil
// tree.FlowReservations (server doesn't support FR, or nothing granted yet)
// still publishes a valid, empty status message rather than skipping.
func TestFlowReservations_NoFlowReservationsPublishesEmpty(t *testing.T) {
	fc := &fakeClient{}
	FlowReservations(fc, &discovery.ResourceTree{})

	p := fc.only(t)
	var msg bus.FlowReservationStatusMsg
	if err := json.Unmarshal(p.payload, &msg); err != nil {
		t.Fatalf("unmarshal FlowReservationStatusMsg: %v", err)
	}
	if len(msg.Reservations) != 0 {
		t.Errorf("Reservations = %d, want 0", len(msg.Reservations))
	}
}

// TestToActiveControl_NilYieldsNoneSourceButPassesClockOffset verifies the
// nil-active (no valid control) case still carries the walk's ClockOffset
// through — the hub's clock-jitter/expiry logic depends on ClockOffset being
// populated even when Source="none".
func TestToActiveControl_NilYieldsNoneSourceButPassesClockOffset(t *testing.T) {
	msg := ToActiveControl(nil, 42)
	if msg.Source != "none" {
		t.Errorf("Source = %q, want %q", msg.Source, "none")
	}
	if msg.ClockOffset != 42 {
		t.Errorf("ClockOffset = %d, want 42 (must pass through even with nil active)", msg.ClockOffset)
	}
	if msg.MRID != "" || msg.ValidUntil != 0 {
		t.Errorf("nil active must yield zero MRID/ValidUntil, got MRID=%q ValidUntil=%d", msg.MRID, msg.ValidUntil)
	}
}

// TestToActiveControl_FieldMapping verifies every scalar-mode field maps from
// scheduler.ActiveControl to bus.ActiveControl, including the multiplier
// scaling and ClockOffset passthrough for a live control.
func TestToActiveControl_FieldMapping(t *testing.T) {
	connect := true
	ac := &scheduler.ActiveControl{
		Source:     "event",
		MRID:       "EVT-1",
		ValidUntil: 99999,
		Base: model.DERControlBase{
			OpModConnect: &connect,
			OpModExpLimW: apw(0, 1500),
			OpModImpLimW: apw(0, 2500),
			OpModMaxLimW: apw(0, 5000),
			OpModFixedW:  apw(0, -1000),
		},
	}

	msg := ToActiveControl(ac, 7)

	if msg.Source != "event" || msg.MRID != "EVT-1" || msg.ValidUntil != 99999 {
		t.Errorf("identity fields = %+v, want source=event mrid=EVT-1 validUntil=99999", msg)
	}
	if msg.ClockOffset != 7 {
		t.Errorf("ClockOffset = %d, want 7", msg.ClockOffset)
	}
	if msg.Connect == nil || !*msg.Connect {
		t.Errorf("Connect = %v, want true", msg.Connect)
	}
	if msg.ExpLimW == nil || *msg.ExpLimW != 1500 {
		t.Errorf("ExpLimW = %v, want 1500", msg.ExpLimW)
	}
	if msg.ImpLimW == nil || *msg.ImpLimW != 2500 {
		t.Errorf("ImpLimW = %v, want 2500", msg.ImpLimW)
	}
	if msg.MaxLimW == nil || *msg.MaxLimW != 5000 {
		t.Errorf("MaxLimW = %v, want 5000", msg.MaxLimW)
	}
	if msg.FixedW == nil || *msg.FixedW != -1000 {
		t.Errorf("FixedW = %v, want -1000", msg.FixedW)
	}
	// No default carried on this event ⇒ no fallback published.
	if msg.DefaultFallback != nil {
		t.Errorf("DefaultFallback = %+v, want nil (event carried no default)", msg.DefaultFallback)
	}
}

// TestToActiveControl_CarriesDefaultFallback pins H5/ED-3: an active event that
// carried its program's DefaultDERControl publishes it in DefaultFallback so the
// hub can degrade to it on event-end during an outage.
func TestToActiveControl_CarriesDefaultFallback(t *testing.T) {
	ac := &scheduler.ActiveControl{
		Source:      "event",
		MRID:        "EVT-1",
		ValidUntil:  99999,
		Base:        model.DERControlBase{OpModExpLimW: apw(0, 1000)}, // event caps at 1000 W
		Default:     &model.DERControlBase{OpModExpLimW: apw(0, 5000), OpModGenLimW: apw(0, 8000)},
		DefaultMRID: "DEFAULT-1",
	}
	msg := ToActiveControl(ac, 0)

	if msg.ExpLimW == nil || *msg.ExpLimW != 1000 {
		t.Errorf("primary ExpLimW = %v, want 1000 (the active event's cap)", msg.ExpLimW)
	}
	df := msg.DefaultFallback
	if df == nil {
		t.Fatal("DefaultFallback = nil; an event carrying a default must publish it")
	}
	if df.MRID != "DEFAULT-1" {
		t.Errorf("fallback MRID = %q, want DEFAULT-1", df.MRID)
	}
	if df.ExpLimW == nil || *df.ExpLimW != 5000 {
		t.Errorf("fallback ExpLimW = %v, want 5000 (the default's cap)", df.ExpLimW)
	}
	if df.GenLimW == nil || *df.GenLimW != 8000 {
		t.Errorf("fallback GenLimW = %v, want 8000", df.GenLimW)
	}
}

// A default-SOURCED control never carries a self-fallback.
func TestToActiveControl_DefaultSourceNoSelfFallback(t *testing.T) {
	ac := &scheduler.ActiveControl{
		Source: "default", MRID: "DEFAULT-1",
		Base:    model.DERControlBase{OpModExpLimW: apw(0, 5000)},
		Default: &model.DERControlBase{OpModExpLimW: apw(0, 5000)}, // even if set, must not self-carry
	}
	if msg := ToActiveControl(ac, 0); msg.DefaultFallback != nil {
		t.Errorf("a default-sourced control must not carry a DefaultFallback, got %+v", msg.DefaultFallback)
	}
}

// TestCountProgramsWithCurves counts only programs carrying at least one
// curve-linked mode.
func TestCountProgramsWithCurves(t *testing.T) {
	programs := []discovery.ProgramState{
		{Curves: nil},
		{Curves: map[string]model.DERCurve{"href1": {}}},
		{Curves: map[string]model.DERCurve{}},
	}
	if got := CountProgramsWithCurves(programs); got != 1 {
		t.Errorf("CountProgramsWithCurves = %d, want 1", got)
	}
}
