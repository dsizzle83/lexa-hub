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
