package main

import "testing"

func TestShouldKick(t *testing.T) {
	tests := []struct {
		name      string
		connected bool
		healthy   bool
		want      bool
	}{
		{"connected and healthy", true, true, true},
		{"connected but unhealthy", true, false, false},
		{"disconnected but healthy", false, true, false},
		{"disconnected and unhealthy", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldKick(tt.connected, tt.healthy); got != tt.want {
				t.Errorf("shouldKick(%v, %v) = %v, want %v", tt.connected, tt.healthy, got, tt.want)
			}
		})
	}
}

func TestHealthyDefaultsTrue(t *testing.T) {
	// Today there is nothing else to check (no spool, no cloud session) —
	// see healthy()'s doc comment in main.go for what 2.2 adds. Pinning
	// this makes a future behavior change to the function deliberate
	// rather than an accidental regression.
	if !healthy() {
		t.Error("healthy() = false, want true (nothing wired to make it false yet)")
	}
}
