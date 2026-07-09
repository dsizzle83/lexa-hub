package main

// cmd_intent.go implements the `lexactl intent <kind> --json '<body>'`
// escape hatch: any whitelisted /intent kind (cmd/api/intent.go's
// localIntentKinds — mode/evgoal/reserve/tariff/chargenow at the time of
// writing), body passed through byte-for-byte. This exists for kinds this
// program has no dedicated sugar for (e.g. "tariff") and for scripting/
// debugging the other kinds directly.
import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

func cmdIntent(c *client, args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "usage: lexactl intent <kind> --json '<body>'")
		return 2
	}
	kind := args[0]

	fs := flag.NewFlagSet("intent "+kind, flag.ContinueOnError)
	fs.SetOutput(stdout)
	bodyStr := fs.String("json", "", `the intent body as a raw JSON object, e.g. '{"mode":"gateway"}' (required)`)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl intent <kind> --json '<body>'")
		return 2
	}
	if *bodyStr == "" {
		fmt.Fprintln(stdout, "usage: lexactl intent <kind> --json '<body>' (--json is required)")
		return 2
	}
	if !json.Valid([]byte(*bodyStr)) {
		fmt.Fprintln(stdout, "error: --json is not valid JSON")
		return 1
	}

	resp, err := c.postIntent(context.Background(), kind, json.RawMessage(*bodyStr))
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	// Always verbatim — that's this subcommand's entire purpose, regardless
	// of any -json flag (there isn't one here; --json is the request body).
	return reportIntentResult(resp, true, stdout)
}
