package flowres

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"testing"

	"lexa-hub/internal/bus"
	model "lexa-proto/csipmodel"
)

// fakePoster records every POST the Manager makes, decoding the XML body
// back into a FlowReservationRequest so tests can assert on it.
type fakePoster struct {
	calls []model.FlowReservationRequest
	err   error
}

func (f *fakePoster) Post(path string, body []byte, contentType string) ([]byte, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	var req model.FlowReservationRequest
	_ = xml.Unmarshal(body, &req)
	f.calls = append(f.calls, req)
	return nil, "/frr/0/frq/0", nil
}

func f64(v float64) *float64 { return &v }

// TestHandleRequest_HappyPath verifies a well-formed FlowReservationRequestMsg
// is decoded and POSTed to the manager's current request path with the
// expected field mapping.
func TestHandleRequest_HappyPath(t *testing.T) {
	fp := &fakePoster{}
	m := New(fp, "LFDI-1")
	m.SetRequestPath("/edev/0/frq")

	msg := bus.FlowReservationRequestMsg{
		MRID:              "abc123",
		Description:       "evening charge",
		EnergyRequestedWh: f64(5000),
		PowerRequestedW:   f64(3300),
		DurationRequested: 3600,
		IntervalStart:     1700000000,
		IntervalDuration:  7200,
		Ts:                1700000000,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	m.HandleRequest(body)

	if len(fp.calls) != 1 {
		t.Fatalf("Post called %d times, want 1", len(fp.calls))
	}
	got := fp.calls[0]
	if got.MRID != "abc123" {
		t.Errorf("MRID = %q, want %q", got.MRID, "abc123")
	}
	if got.EnergyRequested == nil || got.EnergyRequested.Value != 5000 {
		t.Errorf("EnergyRequested = %+v, want Value=5000", got.EnergyRequested)
	}
	if got.PowerRequested == nil || got.PowerRequested.Value != 3300 {
		t.Errorf("PowerRequested = %+v, want Value=3300", got.PowerRequested)
	}
	if got.IntervalRequested.Start != 1700000000 || got.IntervalRequested.Duration != 7200 {
		t.Errorf("IntervalRequested = %+v, want Start=1700000000 Duration=7200", got.IntervalRequested)
	}
	if got.RequestStatus.RequestStatus != model.FlowReqStatusRequested {
		t.Errorf("RequestStatus = %d, want %d", got.RequestStatus.RequestStatus, model.FlowReqStatusRequested)
	}
}

// TestHandleRequest_MalformedPayload is a table of malformed/edge-case
// payloads that must never panic or POST anything malformed.
func TestHandleRequest_MalformedPayload(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"not json", []byte("{not json")},
		{"empty", []byte("")},
		{"wrong type", []byte(`"just a string"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakePoster{}
			m := New(fp, "LFDI-1")
			m.SetRequestPath("/edev/0/frq")

			m.HandleRequest(tc.body) // must not panic

			if len(fp.calls) != 0 {
				t.Fatalf("malformed payload produced a POST: %+v", fp.calls)
			}
		})
	}
}

// TestHandleRequest_NoRequestPathYet verifies a well-formed request is
// dropped (no POST) when SetRequestPath has never been called — the server
// may not support FR, or discovery hasn't completed a walk yet.
func TestHandleRequest_NoRequestPathYet(t *testing.T) {
	fp := &fakePoster{}
	m := New(fp, "LFDI-1")

	msg := bus.FlowReservationRequestMsg{MRID: "xyz"}
	body, _ := json.Marshal(msg)
	m.HandleRequest(body)

	if len(fp.calls) != 0 {
		t.Fatalf("Post called with no request path set: %+v", fp.calls)
	}
}

// TestHandleRequest_PosterError verifies a POST failure is swallowed (logged,
// not panicked/retried here) — matches the pre-extraction behavior.
func TestHandleRequest_PosterError(t *testing.T) {
	fp := &fakePoster{err: errors.New("timeout")}
	m := New(fp, "LFDI-1")
	m.SetRequestPath("/edev/0/frq")

	msg := bus.FlowReservationRequestMsg{MRID: "abc"}
	body, _ := json.Marshal(msg)
	m.HandleRequest(body) // must not panic despite the POST erroring
}
