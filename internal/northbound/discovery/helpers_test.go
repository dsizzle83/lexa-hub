package discovery

import (
	"testing"

	model "lexa-proto/csipmodel"
)

func TestVerifyRegistrationPIN(t *testing.T) {
	m := newMockFetcher()
	buildFullResourceTree(m)

	w := NewWalker(m, testLFDI)
	tree, err := w.Discover("/dcap")
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	reg, err := w.VerifyRegistration(tree.SelfDevice, 111115)
	if err != nil {
		t.Fatalf("VerifyRegistration failed: %v", err)
	}
	if reg.PIN != 111115 {
		t.Errorf("PIN = %d, want 111115", reg.PIN)
	}

	_, err = w.VerifyRegistration(tree.SelfDevice, 999999)
	if err == nil {
		t.Fatal("expected error for wrong PIN")
	}
}

func TestVerifyRegistrationNoLink(t *testing.T) {
	m := newMockFetcher()
	w := NewWalker(m, testLFDI)
	ed := &model.EndDevice{}
	_, err := w.VerifyRegistration(ed, 111115)
	if err == nil {
		t.Fatal("expected error when RegistrationLink missing")
	}
}

func TestHighestPriorityProgram(t *testing.T) {
	programs := []ProgramState{
		{Program: model.DERProgram{MRID: "SY01", Primacy: 10}},
		{Program: model.DERProgram{MRID: "SP01", Primacy: 1}},
		{Program: model.DERProgram{MRID: "FD01", Primacy: 5}},
	}
	hp := HighestPriorityProgram(programs)
	if hp == nil {
		t.Fatal("returned nil")
	}
	if hp.Program.MRID != "SP01" {
		t.Errorf("got %q, want SP01", hp.Program.MRID)
	}
}

func TestHighestPriorityProgramTieBreaker(t *testing.T) {
	programs := []ProgramState{
		{Program: model.DERProgram{MRID: "BBB", Primacy: 1}},
		{Program: model.DERProgram{MRID: "AAA", Primacy: 1}},
	}
	hp := HighestPriorityProgram(programs)
	if hp.Program.MRID != "AAA" {
		t.Errorf("got %q, want AAA", hp.Program.MRID)
	}
}

func TestActiveDefaultControl(t *testing.T) {
	programs := []ProgramState{
		{
			Program: model.DERProgram{MRID: "SY01", Primacy: 10},
			DefaultControl: &model.DefaultDERControl{MRID: "SY_DDERC",
				DERControlBase: model.DERControlBase{
					OpModExpLimW: &model.ActivePower{Value: 10000}}},
		},
		{
			Program: model.DERProgram{MRID: "SP01", Primacy: 1},
			DefaultControl: &model.DefaultDERControl{MRID: "SP_DDERC",
				DERControlBase: model.DERControlBase{
					OpModExpLimW: &model.ActivePower{Value: 5000}}},
		},
	}
	dderc := ActiveDefaultControl(programs)
	if dderc == nil {
		t.Fatal("returned nil")
	}
	if dderc.MRID != "SP_DDERC" {
		t.Errorf("got %q, want SP_DDERC", dderc.MRID)
	}
	if dderc.DERControlBase.OpModExpLimW.Value != 5000 {
		t.Errorf("export limit = %d, want 5000", dderc.DERControlBase.OpModExpLimW.Value)
	}
}

func TestActiveDefaultControlEmpty(t *testing.T) {
	dderc := ActiveDefaultControl(nil)
	if dderc != nil {
		t.Error("expected nil for empty list")
	}
}
