package schedule_test

import (
	"testing"
	"time"

	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/northbound/schedule"
)

// baseNow is a fixed reference time so test timestamps are readable.
var baseNow = time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC).Unix()

func makeDERControl(mrid string, start, duration int64, creationTime int64, cancelled bool) model.DERControl {
	c := true
	status := uint8(0)
	if cancelled {
		status = 6
	}
	return model.DERControl{
		MRID:         mrid,
		CreationTime: creationTime,
		EventStatus:  &model.EventStatus{CurrentStatus: status},
		Interval:     model.DateTimeInterval{Start: start, Duration: uint32(duration)},
		DERControlBase: model.DERControlBase{
			OpModConnect: &c,
		},
	}
}

func makeProgram(primacy uint8, controls []model.DERControl, hasDefault bool) discovery.ProgramState {
	ps := discovery.ProgramState{
		Program: model.DERProgram{MRID: "prog-1", Primacy: primacy},
	}
	list := &model.DERControlList{}
	list.DERControl = controls
	ps.Controls = list
	if hasDefault {
		t := true
		ps.DefaultControl = &model.DefaultDERControl{
			MRID: "default-ctrl",
			DERControlBase: model.DERControlBase{OpModEnergize: &t},
		}
	}
	return ps
}

func TestBuild_EmptyPrograms(t *testing.T) {
	tree := &discovery.ResourceTree{}
	sched := schedule.Build(tree, baseNow)
	if len(sched.Slots) != 1 {
		t.Fatalf("want 1 none slot, got %d", len(sched.Slots))
	}
	if sched.Slots[0].Source != "none" {
		t.Errorf("want source=none, got %q", sched.Slots[0].Source)
	}
	if sched.Slots[0].Start != baseNow {
		t.Errorf("slot start mismatch")
	}
	windowEnd := baseNow + int64((24 * time.Hour).Seconds())
	if sched.Slots[0].End != windowEnd {
		t.Errorf("slot end mismatch: got %d want %d", sched.Slots[0].End, windowEnd)
	}
}

func TestBuild_NoEventsUsesDefault(t *testing.T) {
	ps := makeProgram(1, nil, true)
	tree := &discovery.ResourceTree{Programs: []discovery.ProgramState{ps}}
	sched := schedule.Build(tree, baseNow)
	if len(sched.Slots) != 1 {
		t.Fatalf("want 1 slot, got %d", len(sched.Slots))
	}
	if sched.Slots[0].Source != "default" {
		t.Errorf("want source=default, got %q", sched.Slots[0].Source)
	}
	if sched.Slots[0].MRID != "default-ctrl" {
		t.Errorf("want mrid=default-ctrl, got %q", sched.Slots[0].MRID)
	}
}

func TestBuild_SingleEvent_WithGaps(t *testing.T) {
	// Event from T+2h to T+4h inside a 24h window.
	evStart := baseNow + 2*3600
	evDur := int64(2 * 3600)
	ctrl := makeDERControl("ev-1", evStart, evDur, baseNow, false)
	ps := makeProgram(1, []model.DERControl{ctrl}, true)
	tree := &discovery.ResourceTree{Programs: []discovery.ProgramState{ps}}
	sched := schedule.Build(tree, baseNow)

	// Expect 3 slots: [default][event][default]
	if len(sched.Slots) != 3 {
		t.Fatalf("want 3 slots, got %d: %+v", len(sched.Slots), sched.Slots)
	}

	s0, s1, s2 := sched.Slots[0], sched.Slots[1], sched.Slots[2]
	if s0.Source != "default" || s0.End != evStart {
		t.Errorf("slot 0: want default ending at %d, got %+v", evStart, s0)
	}
	if s1.Source != "event" || s1.MRID != "ev-1" || s1.Start != evStart || s1.End != evStart+evDur {
		t.Errorf("slot 1 mismatch: %+v", s1)
	}
	if s2.Source != "default" || s2.Start != evStart+evDur {
		t.Errorf("slot 2 mismatch: %+v", s2)
	}
}

func TestBuild_CancelledEventSkipped(t *testing.T) {
	evStart := baseNow + 1*3600
	ctrl := makeDERControl("ev-cancel", evStart, 3600, baseNow, true)
	ps := makeProgram(1, []model.DERControl{ctrl}, true)
	tree := &discovery.ResourceTree{Programs: []discovery.ProgramState{ps}}
	sched := schedule.Build(tree, baseNow)
	if len(sched.Slots) != 1 || sched.Slots[0].Source != "default" {
		t.Errorf("cancelled event should produce single default slot, got %+v", sched.Slots)
	}
}

func TestBuild_OverlappingEvents_LaterCreationTimeWins(t *testing.T) {
	// Two events both starting at T+1h, duration 2h.
	// ev-b has a later creationTime and should win.
	evStart := baseNow + 3600
	a := makeDERControl("ev-a", evStart, 7200, baseNow-10, false)
	b := makeDERControl("ev-b", evStart, 7200, baseNow, false)
	ps := makeProgram(1, []model.DERControl{a, b}, false)
	tree := &discovery.ResourceTree{Programs: []discovery.ProgramState{ps}}
	sched := schedule.Build(tree, baseNow)

	found := false
	for _, s := range sched.Slots {
		if s.Source == "event" {
			if s.MRID != "ev-b" {
				t.Errorf("want ev-b to win, got %q", s.MRID)
			}
			found = true
		}
	}
	if !found {
		t.Error("no event slot found")
	}
}

func TestBuild_EventOutsideWindow_Ignored(t *testing.T) {
	// Event entirely before the window.
	evStart := baseNow - 10*3600
	ctrl := makeDERControl("ev-old", evStart, 3600, baseNow-11*3600, false)
	ps := makeProgram(1, []model.DERControl{ctrl}, true)
	tree := &discovery.ResourceTree{Programs: []discovery.ProgramState{ps}}
	sched := schedule.Build(tree, baseNow)

	for _, s := range sched.Slots {
		if s.Source == "event" {
			t.Errorf("event before window should be ignored, got slot %+v", s)
		}
	}
}

func TestBuild_CurveResolution(t *testing.T) {
	// Program has an extended DERControlList with a VoltVar curve link.
	curveHref := "/edev/0/derp/0/derc/42"
	curve := model.DERCurve{
		Resource:  model.Resource{Href: curveHref},
		MRID:      "curve-vv",
		CurveType: model.CurveTypeVoltVar,
		CurveData: []model.DERCurveData{{XValue: 9500, YValue: 2000}, {XValue: 10500, YValue: -2000}},
	}
	link := &model.CurveLink{Href: curveHref}
	extBase := model.ExtendedDERControlBase{OpModVoltVar: link}
	extCtrl := model.ExtendedDERControl{
		MRID:           "ev-vv",
		CreationTime:   baseNow,
		Interval:       model.DateTimeInterval{Start: baseNow + 3600, Duration: 7200},
		DERControlBase: extBase,
	}
	extList := &model.ExtendedDERControlList{DERControl: []model.ExtendedDERControl{extCtrl}}

	ps := discovery.ProgramState{
		Program:          model.DERProgram{MRID: "prog-1", Primacy: 1},
		ExtendedControls: extList,
		Curves:           map[string]model.DERCurve{curveHref: curve},
	}
	tree := &discovery.ResourceTree{Programs: []discovery.ProgramState{ps}}
	sched := schedule.Build(tree, baseNow)

	var eventSlot *schedule.ScheduleSlot
	for i := range sched.Slots {
		if sched.Slots[i].Source == "event" {
			eventSlot = &sched.Slots[i]
			break
		}
	}
	if eventSlot == nil {
		t.Fatal("no event slot found")
	}
	if eventSlot.Curves.VoltVar == nil {
		t.Fatal("VoltVar curve not resolved")
	}
	if eventSlot.Curves.VoltVar.MRID != "curve-vv" {
		t.Errorf("wrong curve mrid: %q", eventSlot.Curves.VoltVar.MRID)
	}
	if len(eventSlot.Curves.VoltVar.CurveData) != 2 {
		t.Errorf("wrong curve data length: %d", len(eventSlot.Curves.VoltVar.CurveData))
	}
}

func TestBuild_WindowStartEnd(t *testing.T) {
	tree := &discovery.ResourceTree{}
	sched := schedule.Build(tree, baseNow)
	want := baseNow + int64((24 * time.Hour).Seconds())
	if sched.WindowStart != baseNow || sched.WindowEnd != want {
		t.Errorf("window mismatch: got [%d, %d] want [%d, %d]",
			sched.WindowStart, sched.WindowEnd, baseNow, want)
	}
}
