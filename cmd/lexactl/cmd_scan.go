package main

// cmd_scan.go implements `lexactl scan run [--cidr X] [--watch]` (POST
// /scan) and `lexactl scan show` (GET /scan) — commissioning-time Modbus/
// SunSpec discovery (DEVICE_ROADMAP.md §5). --watch polls GET /scan until
// the latest status line reaches phase "done" or "refused", capped at 30
// minutes (§7's brief) so a wedged scan or a lost connection can't hang the
// CLI forever.
import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// scanPollInterval/scanWatchCap are vars, not consts, solely so tests can
// shrink them — mirrors cmd/api/intent.go's intentResultWait doc.
var (
	scanPollInterval = 2 * time.Second
	scanWatchCap     = 30 * time.Minute
)

// scanStatusLine/scanHit/scanResult/scanGetResp mirror cmd/api/scan.go's and
// devices.go's response shapes (hand-copied — see client.go's doc).
type scanStatusLine struct {
	ID     string `json:"id"`
	Phase  string `json:"phase"`
	Probed int    `json:"probed"`
	Found  int    `json:"found"`
	Detail string `json:"detail,omitempty"`
	Ts     string `json:"ts"`
}

type scanHit struct {
	URL          string   `json:"url"`
	UnitID       uint8    `json:"unit_id"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Serial       string   `json:"serial,omitempty"`
	FwVersion    string   `json:"fw_version,omitempty"`
	Class        string   `json:"class"`
	NameplateW   *float64 `json:"nameplate_w,omitempty"`
}

type scanResult struct {
	ID      string    `json:"id"`
	Ts      string    `json:"ts"`
	Devices []scanHit `json:"devices"`
}

type scanGetResp struct {
	Status []scanStatusLine `json:"status"`
	Result *scanResult      `json:"result,omitempty"`
}

func dispatchScan(c *client, args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "usage: lexactl scan run|show")
		return 2
	}
	switch args[0] {
	case "run":
		return cmdScanRun(c, args[1:], stdout)
	case "show":
		return cmdScanShow(c, args[1:], stdout)
	default:
		fmt.Fprintf(stdout, "usage: lexactl scan run|show (got %q)\n", args[0])
		return 2
	}
}

func cmdScanRun(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("scan run", flag.ContinueOnError)
	fs.SetOutput(stdout)
	cidr := fs.String("cidr", "", "TCP CIDR to sweep (optional; empty = lexa-modbus's local /24 default)")
	watch := fs.Bool("watch", false, "poll GET /scan every 2s, printing status lines until phase done/refused (30 min cap)")
	jsonOut := fs.Bool("json", false, "print the raw POST /scan body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl scan run [--cidr X] [--watch]")
		return 2
	}

	body := map[string]any{}
	if *cidr != "" {
		body["tcp_cidr"] = *cidr
	}

	resp, err := c.post(context.Background(), "/scan", body)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		writeRaw(stdout, resp.Body)
	}
	if resp.Status != http.StatusAccepted {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: POST /scan: HTTP %d: %s\n", resp.Status, strings.TrimSpace(string(resp.Body)))
		}
		return 1
	}

	var accepted struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(resp.Body, &accepted); err != nil {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: decode POST /scan response: %v\n", err)
		}
		return 1
	}
	if !*jsonOut {
		fmt.Fprintf(stdout, "scan requested: id=%s\n", accepted.ID)
	}
	if !*watch {
		return 0
	}
	return watchScan(c, stdout, scanPollInterval, scanWatchCap)
}

func cmdScanShow(c *client, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("scan show", flag.ContinueOnError)
	fs.SetOutput(stdout)
	jsonOut := fs.Bool("json", false, "print the raw GET /scan body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stdout, "usage: lexactl scan show")
		return 2
	}

	resp, err := c.get(context.Background(), "/scan")
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		writeRaw(stdout, resp.Body)
	}
	if resp.Status != http.StatusOK {
		if !*jsonOut {
			fmt.Fprintf(stdout, "error: GET /scan: HTTP %d\n", resp.Status)
		}
		return 1
	}
	if *jsonOut {
		return 0
	}

	var g scanGetResp
	if err := decodeJSON(resp.Body, &g); err != nil {
		fmt.Fprintf(stdout, "error: decode GET /scan: %v\n", err)
		return 1
	}
	for _, s := range g.Status {
		printScanStatusLine(stdout, s)
	}
	printScanResult(stdout, g.Result)
	return 0
}

// watchScan polls GET /scan every interval, printing each new status line,
// until the ring's latest entry reaches phase "done" (prints the result,
// exit 0) or "refused" (exit 1), or budget elapses without either (exit 1).
func watchScan(c *client, stdout io.Writer, interval, budget time.Duration) int {
	deadline := time.Now().Add(budget)
	var lastPrinted string

	for {
		resp, err := c.get(context.Background(), "/scan")
		if err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
			return 1
		}
		if resp.Status != http.StatusOK {
			fmt.Fprintf(stdout, "error: GET /scan: HTTP %d\n", resp.Status)
			return 1
		}
		var g scanGetResp
		if err := decodeJSON(resp.Body, &g); err != nil {
			fmt.Fprintf(stdout, "error: decode GET /scan: %v\n", err)
			return 1
		}

		if len(g.Status) > 0 {
			latest := g.Status[len(g.Status)-1]
			line := scanStatusLineText(latest)
			if line != lastPrinted {
				fmt.Fprintln(stdout, line)
				lastPrinted = line
			}
			switch latest.Phase {
			case "done":
				printScanResult(stdout, g.Result)
				return 0
			case "refused":
				fmt.Fprintln(stdout, "scan refused (see detail above)")
				return 1
			}
		}

		if !time.Now().Before(deadline) {
			fmt.Fprintln(stdout, "error: scan watch timed out (30 min cap) without reaching done/refused")
			return 1
		}
		time.Sleep(interval)
	}
}

func scanStatusLineText(s scanStatusLine) string {
	line := fmt.Sprintf("[%s] phase=%s probed=%d found=%d", s.Ts, s.Phase, s.Probed, s.Found)
	if s.Detail != "" {
		line += " detail=" + s.Detail
	}
	return line
}

func printScanStatusLine(stdout io.Writer, s scanStatusLine) {
	fmt.Fprintln(stdout, scanStatusLineText(s))
}

func printScanResult(stdout io.Writer, r *scanResult) {
	if r == nil {
		fmt.Fprintln(stdout, "no scan result yet")
		return
	}
	fmt.Fprintf(stdout, "result: id=%s ts=%s devices=%d\n", r.ID, r.Ts, len(r.Devices))
	for _, d := range r.Devices {
		nameplate := ""
		if d.NameplateW != nil {
			nameplate = fmt.Sprintf(" nameplate=%.0fW", *d.NameplateW)
		}
		fmt.Fprintf(stdout, "  %-28s unit=%-3d class=%-16s %s %s%s\n",
			d.URL, d.UnitID, d.Class, d.Manufacturer, d.Model, nameplate)
	}
}
