package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckClock(t *testing.T) {
	tests := []struct {
		name      string
		year      int
		ntpOut    string
		ntpErr    error
		rtcExists bool
		want      Status
	}{
		{"year implausible fails outright", 2020, "yes", nil, true, StatusFail},
		{"year ok, ntp synced -> pass", 2026, "yes", nil, false, StatusPass},
		{"year ok, ntp not synced, rtc present -> pass (weak fallback)", 2026, "no", nil, true, StatusPass},
		{"year ok, ntp not synced, no rtc -> fail", 2026, "no", nil, false, StatusFail},
		{"year ok, timedatectl missing, rtc present -> pass", 2026, "", errors.New("exec: \"timedatectl\": executable file not found"), true, StatusPass},
		{"year ok, timedatectl missing, no rtc -> fail", 2026, "", errors.New("not found"), false, StatusFail},
		{"future year with ntp synced still passes", 2030, "yes", nil, false, StatusPass},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeRunner()
			r.set("timedatectl", []string{"show", "-p", "NTPSynchronized", "--value"}, tt.ntpOut, tt.ntpErr)
			env := &Environment{
				Runner:    r,
				Now:       func() time.Time { return time.Date(tt.year, 6, 1, 12, 0, 0, 0, time.UTC) },
				RTCExists: func() bool { return tt.rtcExists },
			}
			got := checkClock(context.Background(), env)
			if got.Status != tt.want {
				t.Errorf("checkClock(year=%d ntp=%q rtc=%v) = %v (%s), want %v",
					tt.year, tt.ntpOut, tt.rtcExists, got.Status, got.Detail, tt.want)
			}
		})
	}
}

func TestCheckNTPSynced(t *testing.T) {
	r := newFakeRunner()
	r.set("timedatectl", []string{"show", "-p", "NTPSynchronized", "--value"}, "yes\n", nil)
	ok, detail := checkNTPSynced(context.Background(), r)
	if !ok {
		t.Errorf("checkNTPSynced = false (%s), want true for \"yes\\n\"", detail)
	}

	r2 := newFakeRunner()
	r2.set("timedatectl", []string{"show", "-p", "NTPSynchronized", "--value"}, "no\n", nil)
	ok2, _ := checkNTPSynced(context.Background(), r2)
	if ok2 {
		t.Errorf("checkNTPSynced = true, want false for \"no\\n\"")
	}
}
