package main

// cmd_telemetry.go implements `lexactl telemetry [--minutes N]`: GET
// /telemetry/recent, rendered as a compact one-line-per-device table (no
// external table-formatting dependency — GROUND RULES: stdlib only).
import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
)

// telemetrySample mirrors cmd/api/telemetry.go's telemetrySampleResp
// (hand-copied — see client.go's doc), trimmed to the fields this table
// renders.
type telemetrySample struct {
	ArrivedAt string   `json:"arrived_at"`
	Kind      string   `json:"kind"`
	W         *float64 `json:"w,omitempty"`
	SOC       *float64 `json:"soc_pct,omitempty"`
	CurrentA  *float64 `json:"current_a,omitempty"`
	PowerW    *float64 `json:"power_w,omitempty"`
	Status    string   `json:"status,omitempty"`
}

type telemetryRecentResp struct {
	Minutes int                          `json:"minutes"`
	Devices map[string][]telemetrySample `json:"devices"`
}

func cmdTelemetry(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("telemetry", flag.ContinueOnError)
	fs.SetOutput(stdout)
	minutes := fs.Int("minutes", 0, "how many minutes back to show (default: server default, capped at 15)")
	jsonOut := fs.Bool("json", false, "print the raw GET /telemetry/recent body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl telemetry [--minutes N]")
		return 2
	}
	if *minutes < 0 {
		fmt.Fprintf(stdout, "error: --minutes must be >= 0 (got %d)\n", *minutes)
		return 1
	}

	path := "/telemetry/recent"
	if *minutes > 0 {
		path += "?minutes=" + strconv.Itoa(*minutes)
	}

	resp, err := c.get(context.Background(), path)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		writeRaw(stdout, resp.Body)
	}
	if resp.Status != http.StatusOK {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: GET %s: HTTP %d\n", path, resp.Status)
		}
		return 1
	}
	if *jsonOut {
		return 0
	}

	var t telemetryRecentResp
	if err := decodeJSON(resp.Body, &t); err != nil {
		fmt.Fprintf(stdout, "error: decode /telemetry/recent: %v\n", err)
		return 1
	}

	names := make([]string, 0, len(t.Devices))
	for name := range t.Devices {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(stdout, "last %d minute(s):\n", t.Minutes)
	for _, name := range names {
		samples := t.Devices[name]
		if len(samples) == 0 {
			continue
		}
		last := samples[len(samples)-1]
		fmt.Fprintf(stdout, "  %-20s %-14s n=%-4d last=%s%s\n",
			name, last.Kind, len(samples), last.ArrivedAt, formatSampleValue(last))
	}
	return 0
}

func formatSampleValue(s telemetrySample) string {
	switch {
	case s.W != nil:
		return fmt.Sprintf(" W=%.0f", *s.W)
	case s.PowerW != nil:
		return fmt.Sprintf(" power=%.0fW", *s.PowerW)
	case s.CurrentA != nil:
		return fmt.Sprintf(" current=%.1fA", *s.CurrentA)
	case s.SOC != nil:
		return fmt.Sprintf(" soc=%.1f%%", *s.SOC)
	case s.Status != "":
		return " status=" + s.Status
	default:
		return ""
	}
}
