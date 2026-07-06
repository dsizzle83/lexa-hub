// Package sunspecsweep has no production code of its own — it exists solely
// to make TASK-053's generative int16/scale-factor boundary sweep an actual
// CI-executed test in this repo.
//
// The sweep's real implementation lives in the shared lexa-proto/sunspec
// package (single-sourced there so both consumer repos test the identical
// contract, not two copies that can drift the way the pre-021 forks did —
// see docs/refactor/tasks/TASK-053.md). lexa-proto is unhosted with no CI of
// its own, and its _test.go files are (correctly) not vendored by `go mod
// vendor` — only production source is. So the exported Sweep* functions in
// lexa-proto/sunspec/sweep.go are called from here, against THIS repo's own
// vendored copy of the codec, to get real signal in this repo's hosted CI:
// a codec bug, OR a vendor gone stale relative to lexa-proto, fails this
// test on the next PR.
package sunspecsweep

import (
	"testing"

	"lexa-proto/sunspec"
)

func TestSweep_RoundTrip_Signed(t *testing.T) {
	for _, msg := range sunspec.SweepRoundTripSigned(sunspec.ScaleFactors) {
		t.Error(msg)
	}
}

func TestSweep_RoundTrip_Uint(t *testing.T) {
	for _, msg := range sunspec.SweepRoundTripUint(sunspec.ScaleFactors) {
		t.Error(msg)
	}
}

func TestSweep_WrapGuard_Signed(t *testing.T) {
	for _, msg := range sunspec.SweepWrapGuardSigned(sunspec.ScaleFactors) {
		t.Error(msg)
	}
}

func TestSweep_WrapGuard_Uint(t *testing.T) {
	for _, msg := range sunspec.SweepWrapGuardUint(sunspec.ScaleFactors) {
		t.Error(msg)
	}
}

func TestSweep_Sentinel(t *testing.T) {
	for _, msg := range sunspec.SweepSentinel() {
		t.Error(msg)
	}
}

func TestSweep_NaNInfEncode(t *testing.T) {
	for _, msg := range sunspec.SweepNaNInfEncode() {
		t.Error(msg)
	}
}
