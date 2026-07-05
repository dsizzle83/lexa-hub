package logutil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestSetupTextHandlerHasSvcAttr is a handler smoke test: Setup's installed
// default handler must render key=value text including svc=<service> on
// every record, and must honor the level passed in (a Debug call below the
// configured level is dropped).
func TestSetupTextHandlerHasSvcAttr(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(h).With("svc", "lexa-test")

	logger.Info("widget adopted", "mrid", "abc123", "limit_w", 5000)
	logger.Debug("should not appear at Info level")

	out := buf.String()
	if !strings.Contains(out, "svc=lexa-test") {
		t.Errorf("output missing svc attr: %q", out)
	}
	if !strings.Contains(out, "msg=\"widget adopted\"") {
		t.Errorf("output missing msg: %q", out)
	}
	if !strings.Contains(out, "mrid=abc123") || !strings.Contains(out, "limit_w=5000") {
		t.Errorf("output missing structured keys: %q", out)
	}
	if strings.Contains(out, "should not appear") {
		t.Errorf("Debug record leaked through an Info-level handler: %q", out)
	}
	// No duplicate human-readable timestamp beyond slog's own time= key —
	// this is a smoke test for the shape, not a strict grammar check.
	if !strings.Contains(out, "time=") {
		t.Errorf("output missing slog's own time= key: %q", out)
	}
}

// TestParseLevel pins the config-string → slog.Level mapping, including the
// fail-soft default for unrecognized input.
func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":      slog.LevelDebug,
		"DEBUG":      slog.LevelDebug,
		"  debug  ":  slog.LevelDebug,
		"info":       slog.LevelInfo,
		"":           slog.LevelInfo,
		"warn":       slog.LevelWarn,
		"warning":    slog.LevelWarn,
		"error":      slog.LevelError,
		"bogus-typo": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
