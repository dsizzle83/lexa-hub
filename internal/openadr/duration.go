package openadr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// unboundedCutoff: an ISO 8601 duration at or beyond ~100 years is treated
// as the 3.1 "P9999Y" infinity sentinel rather than a real length.
const unboundedCutoff = 100 * 365 * 24 * time.Hour

// ParseDuration parses the ISO 8601 duration subset OpenADR 3.1 uses
// (PnYnMnWnDTnHnMnS, fractional seconds allowed). Returns unbounded=true for
// the "P9999Y"-style infinity sentinel (any duration >= 100 years).
//
// Calendar components are approximated (Y=365d, M=30d) — acceptable because
// CP-profile intervals are minutes/hours and the only calendar-unit duration
// seen in practice is the infinity sentinel, which the cutoff absorbs before
// the approximation could matter.
func ParseDuration(s string) (d time.Duration, unbounded bool, err error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, fmt.Errorf("openadr: empty duration")
	}
	if s[0] == '-' {
		return 0, false, fmt.Errorf("openadr: negative duration %q", orig)
	}
	if s[0] != 'P' {
		return 0, false, fmt.Errorf("openadr: duration %q missing P", orig)
	}
	s = s[1:]
	datePart, timePart := s, ""
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		datePart, timePart = s[:i], s[i+1:]
		if timePart == "" {
			return 0, false, fmt.Errorf("openadr: duration %q has trailing T", orig)
		}
	}
	var total float64 // seconds
	consume := func(part string, units map[byte]float64) error {
		for len(part) > 0 {
			j := 0
			for j < len(part) && (part[j] == '.' || (part[j] >= '0' && part[j] <= '9')) {
				j++
			}
			if j == 0 || j == len(part) {
				return fmt.Errorf("openadr: malformed duration %q", orig)
			}
			n, perr := strconv.ParseFloat(part[:j], 64)
			if perr != nil {
				return fmt.Errorf("openadr: malformed duration %q: %v", orig, perr)
			}
			mult, ok := units[part[j]]
			if !ok {
				return fmt.Errorf("openadr: duration %q: unexpected designator %q", orig, string(part[j]))
			}
			total += n * mult
			part = part[j+1:]
		}
		return nil
	}
	if err := consume(datePart, map[byte]float64{
		'Y': 365 * 24 * 3600, 'M': 30 * 24 * 3600, 'W': 7 * 24 * 3600, 'D': 24 * 3600,
	}); err != nil {
		return 0, false, err
	}
	if err := consume(timePart, map[byte]float64{
		'H': 3600, 'M': 60, 'S': 1,
	}); err != nil {
		return 0, false, err
	}
	// Compare in float seconds BEFORE converting: P9999Y-scale totals
	// overflow time.Duration's int64 nanoseconds.
	if total >= unboundedCutoff.Seconds() {
		return 0, true, nil
	}
	return time.Duration(total * float64(time.Second)), false, nil
}

// ParseStart parses a 3.1 intervalPeriod start. RFC3339; the spec's
// "0001-01-01T00:00:00Z" sentinel (any year <= 1) means "now" — the caller
// substitutes its own clock (nowSentinel=true, t is the zero value then).
func ParseStart(s string) (t time.Time, nowSentinel bool, err error) {
	t, err = time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("openadr: bad start %q: %w", s, err)
	}
	if t.Year() <= 1 {
		return time.Time{}, true, nil
	}
	return t, false, nil
}

// FormatDurationISO renders d as a plain seconds-based ISO 8601 duration
// ("PT900S") for report interval periods.
func FormatDurationISO(d time.Duration) string {
	return fmt.Sprintf("PT%dS", int64(d.Round(time.Second)/time.Second))
}
