package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// northboundConfig is healthcheck's minimal view of northbound.json — see
// status.go's apiConfig doc for why this is a hand-duplicated subset
// rather than an import.
type northboundConfig struct {
	Server string `json:"server"`
}

func loadNorthboundConfig(configDir string) (northboundConfig, error) {
	var cfg northboundConfig
	data, err := os.ReadFile(filepath.Join(configDir, "northbound.json"))
	if err != nil {
		return cfg, fmt.Errorf("read northbound.json: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse northbound.json: %w", err)
	}
	return cfg, nil
}

// journalLastTs reads <dir>/journal.ndjson — internal/journal's Writer
// always uses this exact active-file name (journal.DefaultName) and never
// renames it; rotated siblings are journal.ndjson.1, .2, ... and are by
// definition never the newest — and returns the "ts" field of its last
// DECODABLE line.
//
// A torn final line is tolerated by walking backward from the end: a
// crash mid-write can leave a half-written JSON object as the last line
// on disk (internal/journal's own Writer pads a trailing newline on next
// Open, but healthcheck reads the file independently of that package and
// must not assume it has run first). Skipping backward to the last line
// that actually unmarshals is more robust than trusting "the last line" is
// complete.
func journalLastTs(dir string) (int64, error) {
	f, err := os.Open(filepath.Join(dir, "journal.ndjson"))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("scan journal: %w", err)
	}

	for i := len(lines) - 1; i >= 0; i-- {
		var env struct {
			Ts int64 `json:"ts"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &env); err == nil {
			return env.Ts, nil
		}
	}
	return 0, fmt.Errorf("no decodable line in journal (%d line(s) read)", len(lines))
}

// bootTime derives the system's boot instant from env.Now() - env.Uptime().
func bootTime(env *Environment) (time.Time, error) {
	up, err := env.Uptime()
	if err != nil {
		return time.Time{}, err
	}
	return env.Now().Add(-up), nil
}

// checkNorthbound: an empty "server" in northbound.json means the box
// ships with no CSIP server configured at all — cleanly idle by design
// (the §9 factory profile), and PASSes immediately with no further
// evidence needed. Otherwise it must show either a discovered program set
// (/status csip_programs > 0) or independent evidence the walk loop has
// actually run since boot (a decodable northbound journal entry newer
// than boot time) — /status alone can't distinguish "walking fine, server
// legitimately reports zero programs" from "never walked since boot",
// hence the journal fallback.
func checkNorthbound(ctx context.Context, env *Environment) Result {
	nbCfg, err := loadNorthboundConfig(env.ConfigDir)
	if err != nil {
		return fail("northbound", err.Error())
	}
	if strings.TrimSpace(nbCfg.Server) == "" {
		return pass("northbound", "no server configured — idle by design")
	}

	apiCfg, err := loadAPIConfig(env.ConfigDir)
	if err != nil {
		return fail("northbound", err.Error())
	}
	host, port, err := apiHostPort(apiCfg.ListenAddr)
	if err != nil {
		return fail("northbound", err.Error())
	}
	token, err := loadAPIToken(apiCfg.APITokenFile)
	if err != nil {
		return fail("northbound", err.Error())
	}

	sp, _, statusErr := fetchStatus(ctx, env, host, port, token)
	csipPrograms := 0
	if statusErr == nil {
		csipPrograms = sp.CSIPPrograms
	}

	journalFresh, journalDetail := false, ""
	boot, uptimeErr := bootTime(env)
	if uptimeErr != nil {
		journalDetail = fmt.Sprintf("boot time unavailable: %v", uptimeErr)
	} else if ts, jerr := journalLastTs(env.JournalDir("northbound")); jerr != nil {
		journalDetail = jerr.Error()
	} else {
		journalFresh = !time.Unix(ts, 0).Before(boot)
		journalDetail = fmt.Sprintf("last journal ts=%d boot=%d", ts, boot.Unix())
	}

	return evalNorthbound(csipPrograms, journalFresh, journalDetail, statusErr)
}

// evalNorthbound is the pure decision (table-tested in
// check_northbound_test.go): csipPrograms wins outright when present;
// otherwise journal freshness is the sole fallback signal.
func evalNorthbound(csipPrograms int, journalFresh bool, journalDetail string, statusErr error) Result {
	if csipPrograms > 0 {
		return pass("northbound", fmt.Sprintf("csip_programs=%d", csipPrograms))
	}
	if journalFresh {
		return pass("northbound", "journal entry newer than boot ("+journalDetail+")")
	}
	detail := "no csip_programs and no fresh northbound journal entry since boot"
	if statusErr != nil {
		detail += fmt.Sprintf("; /status: %v", statusErr)
	}
	if journalDetail != "" {
		detail += fmt.Sprintf("; journal: %s", journalDetail)
	}
	return fail("northbound", detail)
}
