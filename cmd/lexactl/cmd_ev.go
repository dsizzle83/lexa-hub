package main

// cmd_ev.go implements `lexactl ev goal` and `lexactl ev chargenow` — sugar
// over POST /intent kind="evgoal"/"chargenow" (bus.EVGoalIntent /
// bus.ChargeNowIntent). Both validate their arguments LOCALLY before ever
// making a network call, per DEVICE_ROADMAP.md §7's brief — a departure in
// the past or a missing TTL is rejected here, not left for the hub to
// discover and report back as "expired"/"rejected" a network round trip
// later.
import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

func dispatchEV(c *client, args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "usage: lexactl ev goal|chargenow ...")
		return 2
	}
	switch args[0] {
	case "goal":
		return cmdEVGoal(c, args[1:], stdout)
	case "chargenow":
		return cmdEVChargeNow(c, args[1:], stdout)
	default:
		fmt.Fprintf(stdout, "usage: lexactl ev goal|chargenow (got %q)\n", args[0])
		return 2
	}
}

// flagWasSet reports whether name was actually supplied on the command
// line (as opposed to merely defined) — fs.Visit only walks flags that
// were set, unlike fs.VisitAll.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parseDeparture accepts either an RFC3339 timestamp or a "+duration" offset
// from now (e.g. "+2h", "+90m") and returns its Unix-seconds value. A
// departure at or before now is rejected here — never sent to the hub,
// which would just bounce it back as "expired" anyway (DEVICE_ROADMAP.md
// §3.1's adopter validation table) a round trip later.
func parseDeparture(s string, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}

	var t time.Time
	if strings.HasPrefix(s, "+") {
		d, err := time.ParseDuration(s[1:])
		if err != nil {
			return 0, fmt.Errorf("invalid +duration %q: %w", s, err)
		}
		if d <= 0 {
			return 0, fmt.Errorf("+duration must be positive, got %q", s)
		}
		t = now.Add(d)
	} else {
		var err error
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0, fmt.Errorf("not RFC3339 or +duration: %w", err)
		}
	}

	if !t.After(now) {
		return 0, fmt.Errorf("departure %s is not in the future (now %s)", t.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	return t.Unix(), nil
}

func cmdEVGoal(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("ev goal", flag.ContinueOnError)
	fs.SetOutput(stdout)
	targetKwh := fs.Float64("target-kwh", 0, "target EV state of charge, in kWh (required)")
	departure := fs.String("departure", "", "RFC3339 timestamp or +duration (e.g. +2h) (required)")
	initialKwh := fs.Float64("initial-kwh", 0, "user-estimated EV state of charge at plug-in, in kWh (optional)")
	station := fs.String("station", "", "station ID (optional; empty = the default/single station)")
	jsonOut := fs.Bool("json", false, "print the raw POST /intent body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl ev goal --target-kwh N --departure RFC3339|+2h [--initial-kwh N] [--station ID]")
		return 2
	}
	if !flagWasSet(fs, "target-kwh") {
		fmt.Fprintln(stdout, "usage: --target-kwh is required")
		return 2
	}
	if !flagWasSet(fs, "departure") {
		fmt.Fprintln(stdout, "usage: --departure is required")
		return 2
	}
	if *targetKwh < 0 {
		fmt.Fprintf(stdout, "error: --target-kwh must be >= 0 (got %g)\n", *targetKwh)
		return 1
	}

	depUnix, err := parseDeparture(*departure, time.Now())
	if err != nil {
		fmt.Fprintf(stdout, "error: --departure: %v\n", err)
		return 1
	}

	body := map[string]any{
		"target_soc_kwh": *targetKwh,
		"departure_unix": depUnix,
	}
	if *station != "" {
		body["station_id"] = *station
	}
	if flagWasSet(fs, "initial-kwh") {
		if *initialKwh < 0 {
			fmt.Fprintf(stdout, "error: --initial-kwh must be >= 0 (got %g)\n", *initialKwh)
			return 1
		}
		body["initial_soc_kwh"] = *initialKwh
	}

	return postIntentAndReport(c, "evgoal", body, *jsonOut, stdout)
}

func cmdEVChargeNow(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("ev chargenow", flag.ContinueOnError)
	fs.SetOutput(stdout)
	ttl := fs.String("ttl", "", "how long the accelerated-charge override holds, as a Go duration (e.g. 90m, 2h) (required)")
	station := fs.String("station", "", "station ID (optional; empty = the default/single station)")
	jsonOut := fs.Bool("json", false, "print the raw POST /intent body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl ev chargenow --ttl 90m [--station ID]")
		return 2
	}
	if !flagWasSet(fs, "ttl") || *ttl == "" {
		fmt.Fprintln(stdout, "usage: --ttl is required (e.g. --ttl 90m)")
		return 2
	}

	d, err := time.ParseDuration(*ttl)
	if err != nil {
		fmt.Fprintf(stdout, "error: --ttl: invalid duration %q: %v\n", *ttl, err)
		return 1
	}
	if d <= 0 {
		fmt.Fprintf(stdout, "error: --ttl must be positive (got %q)\n", *ttl)
		return 1
	}

	body := map[string]any{"ttl_s": int(d.Seconds())}
	if *station != "" {
		body["station_id"] = *station
	}

	return postIntentAndReport(c, "chargenow", body, *jsonOut, stdout)
}
