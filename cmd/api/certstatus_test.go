package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestOnCertStatus_MergesIntoSnapshot verifies stateStore.onCertStatus stores
// the latest bus.CertStatus and snapshot() copies it out (TASK-072/§10.5's
// "lexa-api aggregates retained topics into GET /status" pattern, same shape
// as onPlanLog/lastPlan just above it in state.go).
func TestOnCertStatus_MergesIntoSnapshot(t *testing.T) {
	s := newStateStore(nil, 5*time.Second)

	// Before any message arrives, /status must omit the field rather than
	// report a fabricated OK.
	if got := s.snapshot().certStatus; got != nil {
		t.Fatalf("certStatus before any message = %+v, want nil", got)
	}

	cs := bus.CertStatus{
		Envelope:       bus.Envelope{V: bus.CertStatusV},
		ClientNotAfter: 1234567890,
		CANotAfter:     1999999999,
		ClientDaysLeft: 12,
		CADaysLeft:     900,
		DaysLeft:       12,
		Ts:             1111111111,
	}
	s.onCertStatus(bus.TopicNorthboundCertStatus, cs)

	got := s.snapshot().certStatus
	if got == nil {
		t.Fatal("certStatus after onCertStatus = nil, want the stored message")
	}
	if got.DaysLeft != 12 || got.ClientNotAfter != 1234567890 || got.CANotAfter != 1999999999 {
		t.Errorf("certStatus = %+v, want the message just published", got)
	}
}

// TestBuildStatus_CertStatus verifies buildStatus's translation into the
// /status JSON shape: present only once a message has arrived, RFC3339
// timestamps for the *NotAfter fields, and the raw day counts + error
// strings carried through unchanged.
func TestBuildStatus_CertStatus(t *testing.T) {
	now := time.Now()

	// Nil certStatus (no message received yet, or an older northbound build)
	// → the JSON field is omitted (nil), not a zero-valued struct.
	snap := snapshot{now: now}
	if got := buildStatus(snap, heartbeatStatus{State: heartbeatNever}); got.CertStatus != nil {
		t.Errorf("CertStatus = %+v, want nil when no message has arrived", got.CertStatus)
	}

	notAfter := now.Add(10 * 24 * time.Hour).Unix()
	caNotAfter := now.Add(3650 * 24 * time.Hour).Unix()
	cs := &bus.CertStatus{
		Envelope:       bus.Envelope{V: bus.CertStatusV},
		ClientNotAfter: notAfter,
		CANotAfter:     caNotAfter,
		ClientDaysLeft: 10,
		CADaysLeft:     3650,
		DaysLeft:       10,
		Ts:             now.Unix(),
	}
	snap = snapshot{now: now, certStatus: cs}
	got := buildStatus(snap, heartbeatStatus{State: heartbeatNever})
	if got.CertStatus == nil {
		t.Fatal("CertStatus = nil, want the populated field")
	}
	if got.CertStatus.DaysLeft != 10 || got.CertStatus.ClientDaysLeft != 10 || got.CertStatus.CADaysLeft != 3650 {
		t.Errorf("CertStatus day counts = %+v, want {10,10,3650}", got.CertStatus)
	}
	wantClientNotAfter := time.Unix(notAfter, 0).UTC().Format(time.RFC3339)
	if got.CertStatus.ClientNotAfter != wantClientNotAfter {
		t.Errorf("CertStatus.ClientNotAfter = %q, want %q", got.CertStatus.ClientNotAfter, wantClientNotAfter)
	}

	// An error state (unreadable file) must carry through, not be dropped.
	errStatus := &bus.CertStatus{
		Envelope:   bus.Envelope{V: bus.CertStatusV},
		ClientErr:  "read /etc/lexa/certs/client.pem: no such file or directory",
		CADaysLeft: 900,
		DaysLeft:   0,
		Ts:         now.Unix(),
	}
	snap = snapshot{now: now, certStatus: errStatus}
	got = buildStatus(snap, heartbeatStatus{State: heartbeatNever})
	if got.CertStatus == nil || got.CertStatus.ClientErr == "" {
		t.Errorf("CertStatus error state not carried through: %+v", got.CertStatus)
	}
	if got.CertStatus.ClientNotAfter != "" {
		t.Errorf("ClientNotAfter = %q, want empty when ClientNotAfter is 0 (inspection failed)", got.CertStatus.ClientNotAfter)
	}
}
