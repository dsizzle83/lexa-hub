package main

import (
	"testing"
	"time"
)

func TestCmdDeduper(t *testing.T) {
	d := cmdDeduper{}
	t0 := time.Now()

	if !d.shouldSend("setpoint=0|true", t0) {
		t.Error("first command must be sent")
	}
	if d.shouldSend("setpoint=0|true", t0.Add(15*time.Second)) {
		t.Error("identical command inside the re-assert window must be suppressed")
	}
	if !d.shouldSend("setpoint=-2000|true", t0.Add(30*time.Second)) {
		t.Error("changed command must be sent immediately")
	}
	if d.shouldSend("setpoint=-2000|true", t0.Add(45*time.Second)) {
		t.Error("repeat of the new command must be suppressed")
	}
	if !d.shouldSend("setpoint=-2000|true", t0.Add(30*time.Second).Add(reassertEvery)) {
		t.Error("identical command must be re-asserted after the watchdog window")
	}
}

// A breach-triggered reset must make the very next identical command publish
// again: during a compliance breach the measured effect contradicts the
// commanded state, so "already sent" is exactly the assumption in doubt (a
// device that reverted behind the hub's back — QA 2026-07-03: a 0 W solar
// ceiling stayed dedupe-suppressed for 30 s against an uncurtailed inverter
// while the hub posted CannotComply about the mismatch).
func TestCmdDeduper_ResetForcesResend(t *testing.T) {
	d := cmdDeduper{}
	t0 := time.Now()

	if !d.shouldSend("curtail=0", t0) {
		t.Fatal("first command must be sent")
	}
	if d.shouldSend("curtail=0", t0.Add(5*time.Second)) {
		t.Fatal("identical command inside the window must be suppressed")
	}
	d.reset() // breach observed: measured effect contradicts the command
	if !d.shouldSend("curtail=0", t0.Add(6*time.Second)) {
		t.Error("after a breach reset, the identical command must publish again")
	}
	if d.shouldSend("curtail=0", t0.Add(7*time.Second)) {
		t.Error("after the forced resend, normal dedupe resumes")
	}
}
