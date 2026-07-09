package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type cloudlinkConfig struct {
	Enabled bool `json:"enabled"`
}

// cloudlinkMetricsHost/Port: DEVICE_ROADMAP.md §1.5's fixed port map entry
// for lexa-cloudlink (127.0.0.1:9106) — there is no config knob for this
// today, matching the house convention that metrics_addr is loopback-only
// by product default.
const (
	cloudlinkMetricsHost = "127.0.0.1"
	cloudlinkMetricsPort = "9106"
)

// checkCloudlink: absent or disabled cloudlink.json SKIPs — this never
// gates OTA commit on a service that isn't even supposed to be running
// (cloudlink, TASK-085, may not have landed on a given fleet unit at all,
// or may be deliberately disabled). When enabled, PASS requires its
// /metrics to be reachable AND either lexa_cloudlink_connected==1 OR a
// lexa_cloudlink_spool_bytes gauge is present at all — spool activity is
// evidence of life even while fully offline. This deliberately NEVER gates
// on WAN/cloud reachability: an air-gapped or not-yet-online install must
// still commit (spec, explicit).
func checkCloudlink(ctx context.Context, env *Environment) Result {
	data, err := os.ReadFile(filepath.Join(env.ConfigDir, "cloudlink.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return skip("cloudlink", "not configured")
		}
		return fail("cloudlink", fmt.Sprintf("read cloudlink.json: %v", err))
	}
	var cfg cloudlinkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fail("cloudlink", fmt.Sprintf("parse cloudlink.json: %v", err))
	}
	if !cfg.Enabled {
		return skip("cloudlink", "disabled")
	}

	// Plain http, no scheme fallback: /metrics is Prometheus text
	// exposition, never TLS, on every service in this codebase (see
	// internal/metrics' package doc).
	res, err := probeGET(ctx, env.HTTPClient, "http", cloudlinkMetricsHost, cloudlinkMetricsPort, "/metrics", nil)
	if err != nil {
		return fail("cloudlink", fmt.Sprintf("GET %s:%s/metrics: %v", cloudlinkMetricsHost, cloudlinkMetricsPort, err))
	}
	if res.StatusCode != 200 {
		return fail("cloudlink", fmt.Sprintf("GET %s:%s/metrics: HTTP %d", cloudlinkMetricsHost, cloudlinkMetricsPort, res.StatusCode))
	}
	return evalCloudlink(parsePromGauges(res.Body))
}

// evalCloudlink is the pure decision (table-tested in
// check_cloudlink_test.go) over the parsed gauge set.
func evalCloudlink(gauges map[string]float64) Result {
	if connected, ok := gauges["lexa_cloudlink_connected"]; ok && connected == 1 {
		return pass("cloudlink", "connected=1")
	}
	if spoolBytes, ok := gauges["lexa_cloudlink_spool_bytes"]; ok {
		return pass("cloudlink", fmt.Sprintf("spool_bytes=%v gauge present (offline-safe)", spoolBytes))
	}
	return fail("cloudlink", "neither lexa_cloudlink_connected==1 nor lexa_cloudlink_spool_bytes present")
}

// parsePromGauges does the minimum text-exposition parsing this tool
// needs. internal/metrics' hand-rolled Handler (no label dimension — see
// its package doc) emits "# TYPE <name> counter|gauge" comment lines then
// bare "<name> <value>" pairs; this reads only the value lines.
func parsePromGauges(body []byte) map[string]float64 {
	out := make(map[string]float64)
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		out[fields[0]] = v
	}
	return out
}
