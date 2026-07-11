package main

// cmd_provision.go implements `lexactl provision` — the operator control for
// the BLE re-provision window (ADR-0002, unit B4/GAP-3). UNLIKE every other
// lexactl subcommand, this one does NOT go through the local lexa-api / POST
// /intent: opening a commissioning window is a LOCAL, physical-security control
// (it re-enables the BLE radio on an already-commissioned unit), not a hub
// optimizer intent, and it must work even when lexa-api is down. It therefore
// speaks only to the filesystem — the same /run/lexa/provision-window file
// lexa-provision reads (window.go there) — and is dispatched in main.go BEFORE
// any token/trust/API resolution, exactly like `lexactl fingerprint`.
//
//	lexactl provision --window 10m     open a window for 10 minutes
//	lexactl provision --close          close the window now
//	lexactl provision status           report window + advertising state
//
// The physical re-provision button (a GPIO hold, not built here) is expected to
// invoke `lexactl provision --window 5m` — this is that same control's CLI face.
import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultWindowFile mirrors cmd/provision's defaultWindowFile — the tmpfs file
// holding the re-provision window's Unix-seconds expiry. Kept as a local const
// (not imported) because cmd/provision is package main.
const defaultWindowFile = "/run/lexa/provision-window"

// defaultMarkerFile mirrors cmd/provision's commissioned-marker default, read
// by `provision status` to report whether the unit is commissioned.
const defaultMarkerFile = "/etc/lexa/commissioned"

// dispatchProvision is main.go's entry point; time.Now is injected so tests
// pin the expiry/remaining arithmetic.
func dispatchProvision(args []string, stdout, stderr io.Writer) int {
	return provisionCmd(args, stdout, stderr, time.Now())
}

func provisionCmd(args []string, stdout, stderr io.Writer, now time.Time) int {
	// `status` is a bare positional the spec allows in the natural leading
	// position (`provision status ...`); the rest are flags. Go's flag parser
	// stops at the first non-flag token, so extract the `status` token up front
	// (any position) and hand only flags to the FlagSet. There is at most one
	// positional; a path literally named "status" is not a real case.
	statusReq := false
	var flagArgs []string
	for _, a := range args {
		if a == "status" {
			statusReq = true
			continue
		}
		flagArgs = append(flagArgs, a)
	}

	fs := flag.NewFlagSet("lexactl provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	windowDur := fs.Duration("window", 0, "open a re-provision window for this duration (e.g. 10m)")
	closeWin := fs.Bool("close", false, "close (remove) the re-provision window")
	windowFile := fs.String("window-file", defaultWindowFile, "re-provision window file path")
	markerFile := fs.String("marker-file", defaultMarkerFile, "commissioned marker file path (status)")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	// No other positionals are allowed (a typo'd action is a usage error).
	if len(fs.Args()) > 0 {
		fmt.Fprintln(stderr, provisionUsage)
		return 2
	}

	// Exactly one action: --window <dur>, --close, or `status`.
	actions := 0
	if *windowDur != 0 {
		actions++
	}
	if *closeWin {
		actions++
	}
	if statusReq {
		actions++
	}
	if actions != 1 {
		fmt.Fprintln(stderr, provisionUsage)
		return 2
	}

	switch {
	case *windowDur != 0:
		return provisionOpen(*windowFile, *windowDur, now, *jsonOut, stdout)
	case *closeWin:
		return provisionClose(*windowFile, *jsonOut, stdout)
	default:
		return provisionStatus(*windowFile, *markerFile, now, *jsonOut, stdout)
	}
}

// provisionOpen writes the window file with expiry = now + dur.
func provisionOpen(path string, dur time.Duration, now time.Time, jsonOut bool, stdout io.Writer) int {
	if dur < 0 {
		fmt.Fprintf(stdout, "error: --window duration must be positive (got %s)\n", dur)
		return 1
	}
	expiry := now.Add(dur)
	// Create the parent dir if missing so this works even before the
	// tmpfiles.d drop-in has run (a fresh /run has no lexa dir yet).
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(stdout, "error: create %s: %v\n", dir, err)
			return 1
		}
	}
	line := strconv.FormatInt(expiry.Unix(), 10) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		fmt.Fprintf(stdout, "error: write %s: %v\n", path, err)
		return 1
	}
	if jsonOut {
		writeProvisionJSON(stdout, map[string]any{
			"action":     "open",
			"window":     "open",
			"expires":    expiry.UTC().Format(time.RFC3339),
			"expires_at": expiry.Unix(),
		})
		return 0
	}
	fmt.Fprintf(stdout, "re-provision window OPEN for %s (expires %s)\n",
		dur, expiry.UTC().Format(time.RFC3339))
	return 0
}

// provisionClose removes the window file. A missing file is success (already
// closed), not an error.
func provisionClose(path string, jsonOut bool, stdout io.Writer) int {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stdout, "error: remove %s: %v\n", path, err)
		return 1
	}
	if jsonOut {
		writeProvisionJSON(stdout, map[string]any{"action": "close", "window": "closed"})
		return 0
	}
	fmt.Fprintln(stdout, "re-provision window CLOSED")
	return 0
}

// provisionStatus reports the window + derived advertising state. "Derived"
// because the CLI cannot observe lexa-provision's live in-process brute-force
// throttle backoff — a transient off-radio state that this snapshot does not
// see; that caveat is noted in the human output.
func provisionStatus(windowFile, markerFile string, now time.Time, jsonOut bool, stdout io.Writer) int {
	expiry, present := readWindowExpiry(windowFile)
	windowIsOpen := present && now.Before(expiry)
	commissioned := fileExists(markerFile)
	// Advertising is permitted when uncommissioned OR the window is open
	// (throttle backoff, invisible here, can still suppress it live).
	wouldAdvertise := windowIsOpen || !commissioned

	if jsonOut {
		obj := map[string]any{
			"window":          boolStr(windowIsOpen, "open", "closed"),
			"commissioned":    commissioned,
			"would_advertise": wouldAdvertise,
		}
		if present {
			obj["expires"] = expiry.UTC().Format(time.RFC3339)
			obj["expires_at"] = expiry.Unix()
		}
		writeProvisionJSON(stdout, obj)
		return 0
	}

	if windowIsOpen {
		fmt.Fprintf(stdout, "window:       open (expires %s, %s remaining)\n",
			expiry.UTC().Format(time.RFC3339), expiry.Sub(now).Round(time.Second))
	} else if present {
		fmt.Fprintf(stdout, "window:       closed (expired %s)\n", expiry.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(stdout, "window:       closed")
	}
	fmt.Fprintf(stdout, "commissioned: %s\n", boolStr(commissioned, "yes", "no"))
	fmt.Fprintf(stdout, "advertising:  %s\n", boolStr(wouldAdvertise, "yes (derived)", "no (derived)"))
	fmt.Fprintln(stdout, "note: a live brute-force throttle backoff on the hub is not visible here")
	return 0
}

// readWindowExpiry reads the window file's Unix-seconds expiry. present is
// false for a missing, empty, or unparseable file.
func readWindowExpiry(path string) (expiry time.Time, present bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(ts, 0), true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func writeProvisionJSON(stdout io.Writer, obj map[string]any) {
	b, _ := json.Marshal(obj)
	writeRaw(stdout, b)
}

const provisionUsage = `usage:
  lexactl provision --window <dur>   open a re-provision window (e.g. 10m, 1h)
  lexactl provision --close          close the re-provision window
  lexactl provision status           report window + advertising state
optional: --window-file PATH, --marker-file PATH, -json`
