package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWindowOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provision-window")
	now := time.Unix(1_700_000_000, 0)

	write := func(ts int64) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strconv.FormatInt(ts, 10)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("absent file → closed", func(t *testing.T) {
		_ = os.Remove(path)
		if windowOpen(path, now) {
			t.Fatal("absent window file must be closed (fail-closed)")
		}
	})

	t.Run("unexpired → open", func(t *testing.T) {
		write(now.Add(10 * time.Minute).Unix())
		if !windowOpen(path, now) {
			t.Fatal("future expiry must be open")
		}
	})

	t.Run("expired → closed", func(t *testing.T) {
		write(now.Add(-time.Second).Unix())
		if windowOpen(path, now) {
			t.Fatal("past expiry must be closed")
		}
	})

	t.Run("exactly at expiry → closed (strict before)", func(t *testing.T) {
		write(now.Unix())
		if windowOpen(path, now) {
			t.Fatal("expiry == now must be closed (now.Before(expiry) is strict)")
		}
	})

	t.Run("corrupt contents → closed", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("not-a-timestamp\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if windowOpen(path, now) {
			t.Fatal("unparseable window file must be closed (fail-closed)")
		}
	})

	t.Run("empty file → closed", func(t *testing.T) {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if windowOpen(path, now) {
			t.Fatal("empty window file must be closed")
		}
	})
}
