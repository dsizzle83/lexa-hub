package bus

import "testing"

// Scratch test for TASK-002 acceptance: proves CI turns red on a failing
// test. Reverted immediately after the red check is observed.
func TestCIRedCheckScratch(t *testing.T) {
	t.Fatal("deliberate failure — TASK-002 red-check validation, revert me")
}
