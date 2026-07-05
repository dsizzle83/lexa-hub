// Package logutil installs the process-wide slog default used by all six
// lexa-hub services (TASK-045).
//
// Pragmatic adoption, not a full sweep: this package only sets up the
// handler. Existing log.Printf call sites keep working unchanged (slog does
// not touch the standard "log" package's own output), and only
// structured-value call sites the task names are migrated to slog. A full
// rewrite of every log line is churn without payoff and risks breaking the
// QA harness's log-quoting (see docs/refactor/tasks/TASK-045.md in the
// csip-tls-test repo, "Common mistakes").
package logutil

import (
	"log/slog"
	"os"
	"strings"
)

// Setup installs slog.Default() as a text (key=value) handler writing to
// stderr, tagged with svc=<service> on every record.
//
// journald timestamps every line it ingests itself, so the handler must not
// ALSO duplicate an RFC3339 stamp inside the message text; slog's own
// `time=` key (added automatically by TextHandler) is fine — it is a
// separate, greppable key, not a second human-readable stamp — and this
// function does not suppress it.
//
// Call this first thing in main(), before any other setup: every service's
// main() calls it before touching config/MQTT/HTTP so the process has one
// handler installed for its whole life. slog.SetDefault is process-global
// (like wolfssl.Init — but pure Go and safe to call more than once), so
// tests that construct their own *slog.Logger rather than calling Setup are
// unaffected.
func Setup(service string, level slog.Level) {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h).With("svc", service))
}

// ParseLevel maps a config string ("debug"|"info"|"warn"|"error", case
// insensitive, surrounding whitespace ignored) to a slog.Level. An empty or
// unrecognized string returns slog.LevelInfo — a typo'd log_level must fail
// soft (silently running at the default level) rather than making a service
// silent (if a typo parsed as something above Error) or spamming it (if a
// typo parsed as, say, always-Debug), either of which would be a
// self-inflicted incident from an otherwise routine config change.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
