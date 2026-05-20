package discovery

import (
	"fmt"

	"lexa-hub/internal/csip/model"
)

// VerifyRegistration fetches the Registration resource and validates
// the PIN. Per BASIC-001 and CORE-009, the client must verify the PIN.
// For CSIP conformance testing, PIN is always 111115 (spec section 3.2.3).
func (w *Walker) VerifyRegistration(ed *model.EndDevice, expectedPIN uint32) (*model.Registration, error) {
	if ed.RegistrationLink == nil {
		return nil, fmt.Errorf("EndDevice has no RegistrationLink")
	}
	var reg model.Registration
	if err := w.fetchAndParse(ed.RegistrationLink.Href, &reg); err != nil {
		return nil, fmt.Errorf("fetch Registration: %w", err)
	}
	if reg.PIN != expectedPIN {
		return nil, fmt.Errorf("PIN mismatch: server=%d expected=%d", reg.PIN, expectedPIN)
	}
	return &reg, nil
}

// HighestPriorityProgram returns the ProgramState with the lowest primacy
// value (highest priority). Per CSIP, lower primacy = higher priority.
// Secondary sort key is mRID (lexicographic) per IEEE 2030.5.
func HighestPriorityProgram(programs []ProgramState) *ProgramState {
	if len(programs) == 0 {
		return nil
	}
	best := 0
	for i := 1; i < len(programs); i++ {
		if programs[i].Program.Primacy < programs[best].Program.Primacy ||
			(programs[i].Program.Primacy == programs[best].Program.Primacy &&
				programs[i].Program.MRID < programs[best].Program.MRID) {
			best = i
		}
	}
	return &programs[best]
}

// ActiveDefaultControl returns the DefaultDERControl from the highest
// priority DERProgram. Per BASIC-016 and CORE-012, this is what the
// client should apply when no DERControl event is active.
func ActiveDefaultControl(programs []ProgramState) *model.DefaultDERControl {
	hp := HighestPriorityProgram(programs)
	if hp == nil {
		return nil
	}
	return hp.DefaultControl
}
