package main

import (
	"context"
	"os/exec"
)

// Runner executes an external command and returns its captured stdout.
//
// Output may be non-empty even when err != nil: `systemctl is-active`
// exits non-zero whenever ANY of the queried units isn't active, but still
// prints one status word per unit on stdout. Callers must parse Output
// regardless of err, and only treat err as fatal when Output can't be
// parsed at all (see check_systemd.go's parseIsActive).
type Runner interface {
	Output(ctx context.Context, name string, args ...string) (output string, err error)
}

// execRunner is the real, os/exec-backed implementation. Every *_test.go in
// this package must inject a fake instead (see check_systemd_test.go /
// check_clock_test.go) — no test may shell out to a real systemctl or
// timedatectl.
type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	return string(out), err
}
