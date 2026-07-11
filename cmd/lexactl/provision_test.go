package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Unix(1_700_000_000, 0) }

// readExpiry reads the window file and returns its parsed Unix-seconds expiry.
func readExpiry(t *testing.T, path string) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read window file: %v", err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("parse window file %q: %v", data, err)
	}
	return ts
}

func TestProvision_WindowWritesCorrectExpiry(t *testing.T) {
	dir := t.TempDir()
	win := filepath.Join(dir, "run", "provision-window") // parent must be auto-created
	var out, errOut bytes.Buffer

	code := provisionCmd([]string{"--window", "10m", "--window-file", win}, &out, &errOut, fixedNow())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut.String())
	}
	want := fixedNow().Add(10 * time.Minute).Unix()
	if got := readExpiry(t, win); got != want {
		t.Fatalf("expiry = %d, want now+10m = %d", got, want)
	}
	if !strings.Contains(out.String(), "OPEN for 10m0s") {
		t.Errorf("output missing confirmation: %s", out.String())
	}
}

func TestProvision_WindowRejectsNegative(t *testing.T) {
	win := filepath.Join(t.TempDir(), "provision-window")
	var out, errOut bytes.Buffer
	code := provisionCmd([]string{"--window", "-5m", "--window-file", win}, &out, &errOut, fixedNow())
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for negative duration", code)
	}
	if _, err := os.Stat(win); !os.IsNotExist(err) {
		t.Error("negative window must not write the file")
	}
}

func TestProvision_CloseRemovesFile(t *testing.T) {
	win := filepath.Join(t.TempDir(), "provision-window")
	if err := os.WriteFile(win, []byte("9999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := provisionCmd([]string{"--close", "--window-file", win}, &out, &errOut, fixedNow())
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(win); !os.IsNotExist(err) {
		t.Error("--close must remove the window file")
	}
	if !strings.Contains(out.String(), "CLOSED") {
		t.Errorf("output missing confirmation: %s", out.String())
	}
	// --close on an already-absent file is success (idempotent).
	out.Reset()
	if code := provisionCmd([]string{"--close", "--window-file", win}, &out, &errOut, fixedNow()); code != 0 {
		t.Fatalf("--close on absent file exit = %d, want 0", code)
	}
}

func TestProvision_StatusOpenWindow(t *testing.T) {
	dir := t.TempDir()
	win := filepath.Join(dir, "provision-window")
	marker := filepath.Join(dir, "commissioned") // absent → uncommissioned
	now := fixedNow()
	if err := os.WriteFile(win, []byte(strconv.FormatInt(now.Add(9*time.Minute).Unix(), 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := provisionCmd([]string{"--window-file", win, "--marker-file", marker, "status"}, &out, &errOut, now)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut.String())
	}
	s := out.String()
	for _, want := range []string{"window:", "open", "9m0s remaining", "commissioned: no", "advertising:  yes"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

func TestProvision_StatusCommissionedNoWindow(t *testing.T) {
	dir := t.TempDir()
	win := filepath.Join(dir, "provision-window") // absent → closed
	marker := filepath.Join(dir, "commissioned")
	if err := os.WriteFile(marker, nil, 0o644); err != nil { // present → commissioned
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := provisionCmd([]string{"--window-file", win, "--marker-file", marker, "status"}, &out, &errOut, fixedNow())
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, "window:       closed") {
		t.Errorf("want closed window:\n%s", s)
	}
	if !strings.Contains(s, "commissioned: yes") || !strings.Contains(s, "advertising:  no") {
		t.Errorf("commissioned unit with no window must not advertise:\n%s", s)
	}
}

func TestProvision_StatusJSON(t *testing.T) {
	dir := t.TempDir()
	win := filepath.Join(dir, "provision-window")
	marker := filepath.Join(dir, "commissioned")
	now := fixedNow()
	if err := os.WriteFile(win, []byte(strconv.FormatInt(now.Add(time.Minute).Unix(), 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := provisionCmd([]string{"--window-file", win, "--marker-file", marker, "-json", "status"}, &out, &errOut, now)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, `"window":"open"`) || !strings.Contains(s, `"would_advertise":true`) {
		t.Errorf("json status = %s", s)
	}
}

func TestProvision_UsageErrors(t *testing.T) {
	win := filepath.Join(t.TempDir(), "provision-window")
	cases := [][]string{
		{},                             // no action
		{"--window", "10m", "--close"}, // two actions
		{"--window", "10m", "status"},  // window + status
		{"bogus"},                      // unknown positional
		{"status", "extra"},            // extra positional
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		full := append([]string{"--window-file", win}, args...)
		if code := provisionCmd(full, &out, &errOut, fixedNow()); code != 2 {
			t.Errorf("args %v: exit = %d, want 2 (usage)", args, code)
		}
	}
}

// TestProvision_DispatchedBeforeTokenAndTrust proves `lexactl provision` is
// handled locally (no API, no token, no cert) — a garbage -addr/-token-file
// that would fail their own resolution steps are never reached.
func TestProvision_DispatchedBeforeTokenAndTrust(t *testing.T) {
	dir := t.TempDir()
	win := filepath.Join(dir, "provision-window")
	marker := filepath.Join(dir, "commissioned")
	var out, errOut bytes.Buffer
	code := run([]string{
		"-addr", "not-a-url",
		"-token-file", "/definitely/not/real",
		"provision", "--window-file", win, "--marker-file", marker, "status",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (provision is local, no addr/token resolution); stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "window:") {
		t.Errorf("expected status output, got:\n%s", out.String())
	}
}
