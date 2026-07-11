package gatt

import "lexa-hub/internal/provision/sec1"

// ADRCharDefs returns the five characteristics of the ADR-0002 GATT layout, in
// object-path order (char0..char4), with the BlueZ flags the app's contract
// (the ADR §GATT table) implies:
//
//	info    (0002) read              — plaintext info document
//	session (0003) write, indicate   — handshake frames
//	wifi    (0004) write, indicate   — encrypted scan command + results
//	config  (0005) write             — encrypted join / done; responses come
//	                                   back on status, so config never pushes
//	status  (0006) indicate          — encrypted join state machine + handoff
//
// Only session/wifi/status carry indicate: those are the characteristics the
// sec1 peripheral actually sends responses on (config's join/done replies are
// emitted on status). Flags match the app's GATT contract exactly so its
// StartNotify subscriptions never target a characteristic we didn't declare
// notifiable.
func ADRCharDefs() []CharDef {
	return []CharDef{
		{UUID: sec1.UUIDInfo, Flags: []string{"read"}, Info: true},
		{UUID: sec1.UUIDSession, Flags: []string{"write", "indicate"}, Write: true},
		{UUID: sec1.UUIDWifi, Flags: []string{"write", "indicate"}, Write: true},
		{UUID: sec1.UUIDConfig, Flags: []string{"write"}, Write: true},
		{UUID: sec1.UUIDStatus, Flags: []string{"indicate"}},
	}
}
