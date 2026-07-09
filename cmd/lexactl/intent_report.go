package main

// intent_report.go is shared by every subcommand that is sugar over
// POST /intent (mode set, ev goal, ev chargenow, reserve set) plus the raw
// `intent` escape hatch: one place that builds the {kind, body} envelope,
// decodes the hub's IntentResult, and maps its Outcome onto this program's
// exit-code contract (0 success / 1 API-or-validation error / 2 usage).
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// intentResultResp mirrors bus.IntentResult / cmd/api's relayed JSON shape
// (the 200 path) and the 202-pending shape (which also carries "outcome").
// Hand-copied rather than importing internal/bus — see client.go's doc for
// why this package never imports it.
type intentResultResp struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	Ts      int64  `json:"ts,omitempty"`
}

// postIntent POSTs {"kind":kind,"body":body} to /intent. body may be a
// map[string]any (the sugar subcommands) or a json.RawMessage (the `intent`
// escape hatch, passing the caller's --json argument through byte-for-byte)
// — encoding/json marshals both correctly as the "body" field.
func (c *client) postIntent(ctx context.Context, kind string, body any) (apiResponse, error) {
	req := struct {
		Kind string `json:"kind"`
		Body any    `json:"body"`
	}{Kind: kind, Body: body}
	return c.post(ctx, "/intent", req)
}

// exitCodeForOutcome maps an IntentResult.Outcome to this program's exit
// contract. "applied"/"clamped"/"duplicate" are all success: the hub did
// what was asked (clamped/duplicate are documented, non-error variants of
// "applied" — see DEVICE_ROADMAP.md §3.1's adopter). "pending" (the 202
// timeout path — cmd/api gave up waiting for the hub's reply, not the same
// as a rejection) is also treated as success: the request was durably
// published, and the eventual result would need a re-query to observe, but
// nothing about a 3s wait failing is itself a fault. "rejected"/"expired" are
// real failures. Anything else (a future outcome string this build doesn't
// know about, or an empty string from a malformed reply) fails closed as an
// API error rather than silently exiting 0.
func exitCodeForOutcome(outcome string) int {
	switch outcome {
	case "applied", "clamped", "duplicate", "pending":
		return 0
	case "rejected", "expired":
		return 1
	default:
		return 1
	}
}

// reportIntentResult renders resp (the outcome of a postIntent call) to
// stdout and returns the exit code. jsonOut prints the raw body verbatim
// (used both for -json and, unconditionally, by the `intent` escape hatch —
// see cmd_intent.go).
func reportIntentResult(resp apiResponse, jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		writeRaw(stdout, resp.Body)
	}

	if resp.Status != http.StatusOK && resp.Status != http.StatusAccepted {
		if !jsonOut {
			fmt.Fprintf(stdout, "error: HTTP %d: %s\n", resp.Status, strings.TrimSpace(string(resp.Body)))
		}
		return 1
	}

	var r intentResultResp
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		if !jsonOut {
			fmt.Fprintf(stdout, "error: decode intent result: %v\n", err)
		}
		return 1
	}
	if !jsonOut {
		line := "outcome: " + r.Outcome
		if r.Detail != "" {
			line += " (" + r.Detail + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	return exitCodeForOutcome(r.Outcome)
}

// postIntentAndReport is the one-line call site every sugar subcommand uses:
// build the kind + body, POST it, render the result.
func postIntentAndReport(c *client, kind string, body any, jsonOut bool, stdout io.Writer) int {
	resp, err := c.postIntent(context.Background(), kind, body)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	return reportIntentResult(resp, jsonOut, stdout)
}
