package main

// cmd_status.go implements `lexactl status`: GET /status, rendered as a
// short human summary (mode, plan heartbeat, cloud link, device count, cert
// fingerprint) — or the raw body verbatim with -json.
import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// statusResp is the subset of cmd/api's statusResp (handlers.go) this
// subcommand renders. Hand-copied, not imported — see client.go's doc.
type statusResp struct {
	Mode          string `json:"mode,omitempty"`
	PlanHeartbeat struct {
		State string  `json:"state"`
		AgeS  float64 `json:"age_s"`
	} `json:"plan_heartbeat"`
	CloudLink *struct {
		Connected bool   `json:"connected"`
		Endpoint  string `json:"endpoint,omitempty"`
	} `json:"cloud_link,omitempty"`
	Devices map[string]struct {
		Connected bool `json:"connected"`
	} `json:"devices"`
	APICertFP string `json:"api_cert_fp,omitempty"`
}

func cmdStatus(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stdout)
	jsonOut := fs.Bool("json", false, "print the raw GET /status body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl status [-json]")
		return 2
	}

	resp, err := c.get(context.Background(), "/status")
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		writeRaw(stdout, resp.Body)
	}
	if resp.Status != http.StatusOK {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: GET /status: HTTP %d: %s\n", resp.Status, strings.TrimSpace(string(resp.Body)))
		}
		return 1
	}
	if *jsonOut {
		return 0
	}

	var s statusResp
	if err := decodeJSON(resp.Body, &s); err != nil {
		fmt.Fprintf(stdout, "error: decode /status: %v\n", err)
		return 1
	}

	mode := s.Mode
	if mode == "" {
		mode = "unknown (no mode manager reporting yet)"
	}
	fmt.Fprintf(stdout, "mode: %s\n", mode)

	hbState := s.PlanHeartbeat.State
	if hbState == "" {
		hbState = "unknown"
	}
	fmt.Fprintf(stdout, "plan heartbeat: %s (age %.1fs)\n", hbState, s.PlanHeartbeat.AgeS)

	switch {
	case s.CloudLink == nil:
		fmt.Fprintln(stdout, "cloud link: not reporting (no lexa-cloudlink, or none seen yet)")
	case s.CloudLink.Connected:
		fmt.Fprintf(stdout, "cloud link: connected (%s)\n", s.CloudLink.Endpoint)
	default:
		fmt.Fprintf(stdout, "cloud link: disconnected (%s)\n", s.CloudLink.Endpoint)
	}

	connected := 0
	for _, d := range s.Devices {
		if d.Connected {
			connected++
		}
	}
	fmt.Fprintf(stdout, "devices: %d (%d connected)\n", len(s.Devices), connected)

	certFP := s.APICertFP
	if certFP == "" {
		certFP = "(tls disabled)"
	}
	fmt.Fprintf(stdout, "cert fp: %s\n", certFP)

	return 0
}
