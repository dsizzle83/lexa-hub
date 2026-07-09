package main

// cmd_reserve.go implements `lexactl reserve set <pct>` — sugar over
// POST /intent kind="reserve" (bus.BackupReserveIntent). The hub-side
// adopter clamps up to the configured safety floor and can only ever RAISE
// the reserve (DEVICE_ROADMAP.md §3.1) — this subcommand only range-checks
// the value is a plausible percentage before sending it.
import (
	"fmt"
	"io"
	"strconv"
)

func dispatchReserve(c *client, args []string, stdout io.Writer) int {
	if len(args) == 0 || args[0] != "set" {
		fmt.Fprintln(stdout, "usage: lexactl reserve set <pct>")
		return 2
	}
	return cmdReserveSet(c, args[1:], stdout)
}

// cmdReserveSet parses its args by hand rather than via flag.FlagSet: <pct>
// is a bare positional value that must be allowed to start with "-" (a
// negative percentage is a real, if always-rejected, input worth a clean
// validation error rather than flag.FlagSet's "flag provided but not
// defined: -5" — Go's flag package treats any "-"-prefixed token as a flag
// attempt, with no way to tell it "this one's positional").
func cmdReserveSet(c *client, args []string, stdout io.Writer) int {
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
		fmt.Fprintln(stdout, "usage: lexactl reserve set <pct> [-json]")
		return 2
	}

	pct, err := strconv.ParseFloat(positional[0], 64)
	if err != nil {
		fmt.Fprintf(stdout, "error: <pct> must be a number (got %q)\n", positional[0])
		return 1
	}
	if pct < 0 || pct > 100 {
		fmt.Fprintf(stdout, "error: <pct> must be between 0 and 100 (got %g)\n", pct)
		return 1
	}

	body := map[string]any{"reserve_pct": pct}
	return postIntentAndReport(c, "reserve", body, jsonOut, stdout)
}
