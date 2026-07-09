package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEvalNorthbound(t *testing.T) {
	tests := []struct {
		name         string
		csipPrograms int
		journalFresh bool
		want         Status
	}{
		{"csip programs present -> pass regardless of journal", 3, false, StatusPass},
		{"no programs but fresh journal -> pass", 0, true, StatusPass},
		{"no programs, stale journal -> fail", 0, false, StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalNorthbound(tt.csipPrograms, tt.journalFresh, "detail", nil)
			if got.Status != tt.want {
				t.Errorf("evalNorthbound(%d, %v) = %v, want %v", tt.csipPrograms, tt.journalFresh, got.Status, tt.want)
			}
		})
	}
}

func TestLoadNorthboundConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "northbound.json"), []byte(`{"server":"69.0.0.20:11111"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadNorthboundConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "69.0.0.20:11111" {
		t.Errorf("Server = %q, want 69.0.0.20:11111", cfg.Server)
	}
}

func TestCheckNorthbound_NoServerConfigured_PassesImmediately(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "northbound.json"), []byte(`{"server":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// No api.json, no journal dir at all — must not matter, since an empty
	// server short-circuits before either is consulted.
	env := &Environment{ConfigDir: dir}
	res := checkNorthbound(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkNorthbound with empty server = %+v, want PASS", res)
	}
}

func writeJournalLines(t *testing.T, dir string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "journal.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJournalLastTs_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeJournalLines(t, dir, []string{
		`{"v":1,"ts":1000,"type":"service_start"}`,
		`{"v":1,"ts":2000,"type":"cannot_comply_posted"}`,
	})
	ts, err := journalLastTs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 2000 {
		t.Errorf("journalLastTs = %d, want 2000", ts)
	}
}

func TestJournalLastTs_TornFinalLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: a well-formed line followed by a
	// half-written (invalid-JSON) final line with no trailing newline.
	content := `{"v":1,"ts":1500,"type":"service_start"}` + "\n" + `{"v":1,"ts":9999,"type":"serv`
	if err := os.WriteFile(filepath.Join(dir, "journal.ndjson"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := journalLastTs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 1500 {
		t.Errorf("journalLastTs with torn final line = %d, want 1500 (last COMPLETE line)", ts)
	}
}

func TestJournalLastTs_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := journalLastTs(dir); err == nil {
		t.Fatalf("journalLastTs on a dir with no journal.ndjson should error")
	}
}

func TestJournalLastTs_Empty(t *testing.T) {
	dir := t.TempDir()
	writeJournalLines(t, dir, nil)
	if _, err := journalLastTs(dir); err == nil {
		t.Fatalf("journalLastTs on an empty journal should error")
	}
}

func TestCheckNorthbound_JournalFreshSinceBoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "northbound.json"), []byte(`{"server":"69.0.0.20:11111"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// api.json missing -> defaults to :9100, which nothing listens on in
	// the test sandbox, so /status will fail to fetch and csip_programs
	// stays 0 — exercising the journal-fallback path specifically.
	journalDir := filepath.Join(dir, "journal", "northbound")
	writeJournalLines(t, journalDir, []string{`{"v":1,"ts":5000,"type":"service_start"}`})

	fixedNow := time.Unix(6000, 0)
	env := &Environment{
		ConfigDir:  dir,
		HTTPClient: newProbeHTTPClient(),
		APIScheme:  "http",
		Now:        func() time.Time { return fixedNow },
		Uptime:     func() (time.Duration, error) { return 2000 * time.Second, nil }, // boot = 6000-2000 = 4000 < 5000
		JournalDir: func(svc string) string { return filepath.Join(dir, "journal", svc) },
	}
	res := checkNorthbound(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkNorthbound with fresh journal entry = %+v, want PASS", res)
	}
}

func TestCheckNorthbound_JournalStaleSinceBoot_Fails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "northbound.json"), []byte(`{"server":"69.0.0.20:11111"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	journalDir := filepath.Join(dir, "journal", "northbound")
	writeJournalLines(t, journalDir, []string{`{"v":1,"ts":1000,"type":"service_start"}`})

	fixedNow := time.Unix(6000, 0)
	env := &Environment{
		ConfigDir:  dir,
		HTTPClient: newProbeHTTPClient(),
		APIScheme:  "http",
		Now:        func() time.Time { return fixedNow },
		Uptime:     func() (time.Duration, error) { return 2000 * time.Second, nil }, // boot = 4000 > journal ts 1000
		JournalDir: func(svc string) string { return filepath.Join(dir, "journal", svc) },
	}
	res := checkNorthbound(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkNorthbound with stale-since-boot journal = %+v, want FAIL", res)
	}
}

func TestBootTime(t *testing.T) {
	fixedNow := time.Unix(10_000, 0)
	env := &Environment{
		Now:    func() time.Time { return fixedNow },
		Uptime: func() (time.Duration, error) { return 100 * time.Second, nil },
	}
	got, err := bootTime(env)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(9_900, 0)
	if !got.Equal(want) {
		t.Errorf("bootTime = %v, want %v", got, want)
	}
}

func TestBootTime_UptimeError(t *testing.T) {
	env := &Environment{
		Now:    time.Now,
		Uptime: func() (time.Duration, error) { return 0, errors.New("boom") },
	}
	if _, err := bootTime(env); err == nil {
		t.Fatalf("bootTime should propagate an Uptime error")
	}
}

func TestLoadNorthboundConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadNorthboundConfig(dir); err == nil {
		t.Fatalf("loadNorthboundConfig on a dir with no northbound.json should error")
	}
}

func TestLoadNorthboundConfig_Malformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "northbound.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNorthboundConfig(dir); err == nil {
		t.Fatalf("loadNorthboundConfig on malformed JSON should error")
	}
}
