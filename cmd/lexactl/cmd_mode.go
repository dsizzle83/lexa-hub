package main

// cmd_mode.go implements `lexactl mode get` (GET /mode) and
// `lexactl mode set optimizer|gateway` (POST /intent kind="mode").
import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// modeGetResp mirrors cmd/api/mode.go's modeResp on success, plus the
// {"error":"unknown"} shape it serves (503) before the hub's first
// ModeStatus arrives — that 503 is a documented, expected steady state
// (DEVICE_ROADMAP.md §3.5), not a failure, so it's rendered as "unknown"
// rather than an error.
type modeGetResp struct {
	Mode     string `json:"mode"`
	Since    int64  `json:"since"`
	Actor    string `json:"actor,omitempty"`
	IntentID string `json:"intent_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

func dispatchMode(c *client, args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "usage: lexactl mode get|set optimizer|gateway")
		return 2
	}
	switch args[0] {
	case "get":
		return cmdModeGet(c, args[1:], stdout)
	case "set":
		return cmdModeSet(c, args[1:], stdout)
	default:
		fmt.Fprintf(stdout, "usage: lexactl mode get|set optimizer|gateway (got %q)\n", args[0])
		return 2
	}
}

func cmdModeGet(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("mode get", flag.ContinueOnError)
	fs.SetOutput(stdout)
	jsonOut := fs.Bool("json", false, "print the raw GET /mode body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl mode get [-json]")
		return 2
	}

	resp, err := c.get(context.Background(), "/mode")
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		writeRaw(stdout, resp.Body)
	}

	var m modeGetResp
	if decErr := decodeJSON(resp.Body, &m); decErr != nil {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: decode /mode: %v\n", decErr)
		}
		return 1
	}

	if resp.Status == http.StatusServiceUnavailable && m.Error == "unknown" {
		if !*jsonOut {
			fmt.Fprintln(stdout, "mode: unknown (hub has not reported a mode yet)")
		}
		return 0
	}
	if resp.Status != http.StatusOK {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: GET /mode: HTTP %d\n", resp.Status)
		}
		return 1
	}
	if *jsonOut {
		return 0
	}

	fmt.Fprintf(stdout, "mode: %s\n", m.Mode)
	if m.Actor != "" {
		fmt.Fprintf(stdout, "actor: %s\n", m.Actor)
	}
	if m.IntentID != "" {
		fmt.Fprintf(stdout, "intent: %s\n", m.IntentID)
	}
	if m.Since != 0 {
		fmt.Fprintf(stdout, "since: %s\n", time.Unix(m.Since, 0).UTC().Format(time.RFC3339))
	}
	return 0
}

// cmdModeSet parses its args by hand rather than via flag.FlagSet: Go's
// flag package stops recognizing flags after the first positional argument,
// which would silently break the entirely natural `lexactl mode set
// gateway -json` ordering (flag AFTER the positional). Same reasoning as
// cmd_reserve.go's cmdReserveSet.
func cmdModeSet(c *client, args []string, stdout io.Writer) int {
	jsonOut := false
	var positional []string
	for _, a := range args {
		if a == "-json" || a == "--json" {
			jsonOut = true
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stdout, "usage: lexactl mode set optimizer|gateway")
		return 2
	}
	mode := positional[0]
	if mode != "optimizer" && mode != "gateway" {
		fmt.Fprintf(stdout, "usage: lexactl mode set optimizer|gateway (got %q)\n", mode)
		return 2
	}

	body := map[string]string{"mode": mode}
	return postIntentAndReport(c, "mode", body, jsonOut, stdout)
}
