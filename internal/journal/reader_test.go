package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScanNonexistentDirIsNotAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	skipped, err := Scan(dir, "j.ndjson", func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Scan on a nonexistent dir should not error, got: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
}

func TestScanSkipsSubdirectoriesAndNonNumericSuffixes(t *testing.T) {
	dir := t.TempDir()
	// A stray subdirectory that happens to live alongside the journal files
	// (e.g. a coincidentally-named data dir) must not confuse orderedFiles.
	if err := os.Mkdir(filepath.Join(dir, "somedir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// A file that matches the "name." prefix but has a non-numeric suffix
	// (e.g. an editor backup) must be ignored, not mistaken for a rotation.
	if err := os.WriteFile(filepath.Join(dir, "j.ndjson.bak"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w, err := Open(Config{Dir: dir, Name: "j.ndjson"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Append(mustEvent(t, "control_released")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var got int
	skipped, err := Scan(dir, "j.ndjson", func(Event) error { got++; return nil })
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if skipped != 0 || got != 1 {
		t.Errorf("got decoded=%d skipped=%d, want decoded=1 skipped=0 (stray dir/file must be ignored)", got, skipped)
	}
}

func TestOrderedFilesActiveStatErrorNotExist(t *testing.T) {
	dir := t.TempDir()
	// dir/sub is a regular file, not a directory: Stat(dir/sub/active) must
	// fail with something other than ErrNotExist (ENOTDIR), exercising the
	// branch distinct from "the active file simply doesn't exist yet".
	if err := os.WriteFile(filepath.Join(dir, "sub"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := orderedFiles(dir, "sub/active")
	if err == nil {
		t.Fatal("expected an error statting through a non-directory path component")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected an ENOTDIR-shaped error, not ErrNotExist: %v", err)
	}
}

func TestScanPropagatesOrderedFilesError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not block reads")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o755)

	_, err := Scan(dir, "j.ndjson", func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected Scan to propagate an os.ReadDir failure")
	}
}

func TestScanPropagatesPerFileOpenError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not block reads")
	}
	dir := t.TempDir()
	w, err := Open(Config{Dir: dir, Name: "j.ndjson", MaxBytes: 1, MaxFiles: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force a rotation so there is a rotated file (j.ndjson.1) to poison.
	if err := w.Append(mustEvent(t, "control_released")); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := w.Append(mustEvent(t, "control_released")); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rotated := filepath.Join(dir, "j.ndjson.1")
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected %s to exist from rotation: %v", rotated, err)
	}
	if err := os.Chmod(rotated, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(rotated, 0o644)

	_, err = Scan(dir, "j.ndjson", func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected Scan to propagate a per-file open failure")
	}
}

func TestScanSkipsBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.ndjson")
	content := "\n" + `{"v":1,"ts":1,"seq":1,"type":"x","svc":"hub"}` + "\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got []Event
	skipped, err := Scan(dir, "j.ndjson", func(e Event) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if skipped != 0 {
		t.Errorf("blank lines must not count as skipped/corrupt lines, got skipped=%d", skipped)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
}

func TestScanStopsOnCallbackError(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Config{Dir: dir, Name: "j.ndjson"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Append(mustEvent(t, "control_released")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stopErr := errors.New("stop here")
	var got int
	_, err = Scan(dir, "j.ndjson", func(Event) error {
		got++
		return stopErr
	})
	if !errors.Is(err, stopErr) {
		t.Errorf("expected Scan to propagate the callback's error, got %v", err)
	}
	if got != 1 {
		t.Errorf("expected exactly 1 callback invocation before stopping, got %d", got)
	}
}

func TestScanTooLongLineErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.ndjson")
	// One line far bigger than scanFile's 8 MiB scan buffer, no newline:
	// bufio.Scanner must surface bufio.ErrTooLong via sc.Err(), and Scan
	// must propagate it rather than silently truncating or hanging.
	huge := bytes.Repeat([]byte("a"), 9<<20)
	if err := os.WriteFile(path, huge, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Scan(dir, "j.ndjson", func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected Scan to error on an oversized line")
	}
}
