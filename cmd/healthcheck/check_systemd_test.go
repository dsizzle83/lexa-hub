package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIsActive(t *testing.T) {
	units := []string{"mosquitto", "lexa-hub", "lexa-api"}

	tests := []struct {
		name    string
		out     string
		wantErr bool
		want    map[string]string
	}{
		{
			name: "all active",
			out:  "active\nactive\nactive\n",
			want: map[string]string{"mosquitto": "active", "lexa-hub": "active", "lexa-api": "active"},
		},
		{
			name: "one inactive, no trailing newline",
			out:  "active\nfailed\nactive",
			want: map[string]string{"mosquitto": "active", "lexa-hub": "failed", "lexa-api": "active"},
		},
		{
			name:    "line count mismatch",
			out:     "active\nactive\n",
			wantErr: true,
		},
		{
			name:    "empty output",
			out:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIsActive(tt.out, units)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseIsActive(%q) = %v, want error", tt.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseIsActive(%q) unexpected error: %v", tt.out, err)
			}
			for u, want := range tt.want {
				if got[u] != want {
					t.Errorf("unit %s: got %q, want %q", u, got[u], want)
				}
			}
		})
	}
}

func TestUnitsToCheck(t *testing.T) {
	dir := t.TempDir()

	units := unitsToCheck(dir)
	for _, u := range units {
		if u == cloudlinkUnit {
			t.Fatalf("unitsToCheck without cloudlink.json included %s: %v", cloudlinkUnit, units)
		}
	}
	if len(units) != len(requiredUnits) {
		t.Fatalf("unitsToCheck without cloudlink.json = %v, want exactly requiredUnits", units)
	}

	if err := os.WriteFile(filepath.Join(dir, "cloudlink.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	units = unitsToCheck(dir)
	found := false
	for _, u := range units {
		if u == cloudlinkUnit {
			found = true
		}
	}
	if !found {
		t.Fatalf("unitsToCheck with cloudlink.json present = %v, want %s included", units, cloudlinkUnit)
	}
	if len(units) != len(requiredUnits)+1 {
		t.Fatalf("unitsToCheck with cloudlink.json = %v, want exactly one extra unit", units)
	}
}

// fakeRunner is the table-testable stand-in for a real Runner: it maps a
// command+args key to a canned (output, error) pair, and records every
// invocation so tests can assert exactly one exec happened where the spec
// requires it ("one exec, parse multi-arg output").
type fakeRunner struct {
	calls   [][]string
	outputs map[string]fakeOutput // key: name + " " + strings.Join(args, " ")
}

type fakeOutput struct {
	out string
	err error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string]fakeOutput{}}
}

func (f *fakeRunner) set(name string, args []string, out string, err error) {
	f.outputs[fakeKey(name, args)] = fakeOutput{out: out, err: err}
}

func fakeKey(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if r, ok := f.outputs[fakeKey(name, args)]; ok {
		return r.out, r.err
	}
	return "", errors.New("fakeRunner: no canned output for " + fakeKey(name, args))
}

func TestCheckSystemd_AllActive_OneExec(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner()
	units := unitsToCheck(dir)
	out := strings.Repeat("active\n", len(units))
	r.set("systemctl", append([]string{"is-active"}, units...), out, nil)

	env := &Environment{ConfigDir: dir, Runner: r}
	res := checkSystemd(context.Background(), env)

	if res.Status != StatusPass {
		t.Fatalf("checkSystemd = %+v, want PASS", res)
	}
	if len(r.calls) != 1 {
		t.Fatalf("systemctl invoked %d times, want exactly 1 (spec: one exec)", len(r.calls))
	}
}

func TestCheckSystemd_OneUnitDown(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner()
	units := unitsToCheck(dir)
	states := make([]string, len(units))
	for i := range states {
		states[i] = "active"
	}
	states[1] = "failed" // lexa-hub
	out := strings.Join(states, "\n") + "\n"
	r.set("systemctl", append([]string{"is-active"}, units...), out, errors.New("exit status 3"))

	env := &Environment{ConfigDir: dir, Runner: r}
	res := checkSystemd(context.Background(), env)

	if res.Status != StatusFail {
		t.Fatalf("checkSystemd = %+v, want FAIL", res)
	}
	if !strings.Contains(res.Detail, "lexa-hub=failed") {
		t.Errorf("detail = %q, want it to name lexa-hub=failed", res.Detail)
	}
}

func TestCheckSystemd_CloudlinkIncludedOnlyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudlink.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newFakeRunner()
	units := unitsToCheck(dir)
	out := strings.Repeat("active\n", len(units))
	r.set("systemctl", append([]string{"is-active"}, units...), out, nil)

	env := &Environment{ConfigDir: dir, Runner: r}
	res := checkSystemd(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkSystemd = %+v, want PASS", res)
	}
	if len(units) != len(requiredUnits)+1 {
		t.Fatalf("expected cloudlink unit to be queried, units=%v", units)
	}
}

func TestCheckSystemd_SystemctlBroken(t *testing.T) {
	dir := t.TempDir()
	r := newFakeRunner() // no canned output at all -> "no canned output" error from fakeRunner.Output
	env := &Environment{ConfigDir: dir, Runner: r}
	res := checkSystemd(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkSystemd = %+v, want FAIL when systemctl can't be run at all", res)
	}
}
