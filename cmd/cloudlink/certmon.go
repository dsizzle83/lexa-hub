package main

// certmon.go implements DEVICE_ROADMAP.md §2.7: the cloud cert expiry
// monitor. It is a pattern copy of cmd/northbound/certmon.go (that file's own
// doc comment is the canonical rationale for pure crypto/x509 PEM inspection
// — no wolfSSL/cgo involvement, read-only, never a handshake), narrowed to
// cloudlink's own cert family — cfg.CloudCert/cfg.CloudCA, the mTLS identity
// cloud.go's buildTLSConfig dials with — with two deliberate deviations from
// that pattern:
//
//   - ONE flat gauge pair (lexa_cloudlink_cert_expiry_seconds /
//     lexa_cloudlink_cert_expiring), not a client/CA pair each. Northbound
//     exposes four gauges because CertStatus keeps client and CA days-left as
//     separate fields; CloudlinkStatus.CertDaysLeft (this monitor's
//     destination, status.go) is a single binding-constraint scalar, so
//     there is only one meaningful "days left"/"expiring soon" pair to
//     expose here.
//   - No MQTT publish of its own. Northbound's Monitor retained-publishes
//     bus.CertStatus directly; this monitor instead feeds CloudDaysLeft()
//     into main.go, which threads it into statusPublisher (status.go) —
//     CloudlinkStatus already carries CertDaysLeft on the existing retained
//     lexa/cloudlink/status topic, so a second retained topic here would be
//     redundant (§2.7: "No MQTT publish of its own (CloudlinkStatus carries
//     it)").
//
// Runs only when cfg.Enabled (main.go gates construction) — a disabled,
// local-only box has no cloud identity to watch.
import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync/atomic"
	"time"
)

// cloudCertCheckInterval mirrors cmd/northbound/certmon.go's
// certCheckInterval: an immediate startup check, then daily thereafter, so a
// long-running process crosses the warn threshold with an alarm rather than
// silence (northbound's "common mistakes" note applies verbatim here).
const cloudCertCheckInterval = 24 * time.Hour

// cloudCertInfo is the validity window of one inspected leaf certificate —
// only NotAfter matters here (unlike northbound's certInfo, cloudlink has no
// use for NotBefore today).
type cloudCertInfo struct {
	NotAfter time.Time
}

// inspectCloudCertFile parses path as PEM and returns the LEAF certificate's
// NotAfter. Verbatim logic copy of cmd/northbound/certmon.go's
// inspectCertFile (leaf = first CERTIFICATE block, serving both a
// single-cert CA file and a chain-bearing client cert file with one code
// path).
func inspectCloudCertFile(path string) (cloudCertInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cloudCertInfo{}, fmt.Errorf("read %s: %w", path, err)
	}
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return cloudCertInfo{}, fmt.Errorf("no CERTIFICATE PEM block found in %s", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return cloudCertInfo{}, fmt.Errorf("parse certificate in %s: %w", path, err)
		}
		return cloudCertInfo{NotAfter: cert.NotAfter}, nil
	}
}

// cloudDaysUntil mirrors cmd/northbound/certmon.go's daysUntil exactly: a
// still-valid cert always reports >=1 (ceiling), an expired one reports <=0
// (floor), so "<=0" is an unambiguous expired signal.
func cloudDaysUntil(notAfter, now time.Time) int {
	remaining := notAfter.Sub(now)
	if remaining <= 0 {
		return int(math.Floor(remaining.Hours() / 24))
	}
	return int(math.Ceil(remaining.Hours() / 24))
}

type cloudCertLevel int

const (
	cloudCertOK cloudCertLevel = iota
	cloudCertWarn
	cloudCertError
)

// classifyCloudCert mirrors cmd/northbound/certmon.go's classify.
func classifyCloudCert(daysLeft, warnDays int) cloudCertLevel {
	switch {
	case daysLeft <= 0:
		return cloudCertError
	case daysLeft <= warnDays:
		return cloudCertWarn
	default:
		return cloudCertOK
	}
}

// cloudCertMon inspects cfg.CloudCert/cfg.CloudCA at startup and every 24h,
// storing the binding (minimum of the two, when both are readable) days-left
// for status.go's statusPublisher to overlay onto CloudlinkStatus.CertDaysLeft
// via CloudDaysLeft(), and updating the two gauges on m (the service's shared
// *cloudlinkMetrics, same threading convention every other 2.2-2.6 component
// uses — see metrics.go).
type cloudCertMon struct {
	certPath string
	caPath   string
	warnDays int
	m        *cloudlinkMetrics

	daysLeft atomic.Int64

	now     func() time.Time
	checked bool // first-check Info/Debug demotion, mirrors northbound's Monitor.checked
}

// newCloudCertMon constructs the monitor from cfg's cloud_cert/cloud_ca/
// cert_expiry_warn_days fields (all already loaded by config.go — no new
// config surface needed for this unit) and the service's shared metrics.
func newCloudCertMon(cfg *Config, m *cloudlinkMetrics) *cloudCertMon {
	warnDays := cfg.CertExpiryWarnDays
	if warnDays <= 0 {
		warnDays = defaultCertWarnDays
	}
	return &cloudCertMon{
		certPath: cfg.CloudCert,
		caPath:   cfg.CloudCA,
		warnDays: warnDays,
		m:        m,
		now:      time.Now,
	}
}

// Run performs an immediate check, then re-checks every interval until ctx
// is cancelled — same shape as cmd/northbound/certmon.go's Monitor.Run
// (interval is a parameter so tests can inject a short one).
func (c *cloudCertMon) Run(ctx context.Context, interval time.Duration) {
	c.CheckOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.CheckOnce()
		}
	}
}

// CloudDaysLeft returns the last-computed binding days-left. 0 before the
// first CheckOnce runs — status.go's overlay treats that identically to the
// "no cert monitor has run yet" zero value CloudlinkStatus.CertDaysLeft
// already carries via its omitempty json tag (bus/intent.go), so a box that
// hasn't completed its first check yet reports the same as one running with
// certmon disabled, rather than a distinguishable sentinel.
func (c *cloudCertMon) CloudDaysLeft() int {
	return int(c.daysLeft.Load())
}

// CheckOnce inspects both PEM files, updates the binding days-left + gauges,
// and logs at the level the worse of the two certs' state demands. Never
// returns an error and never panics on an inspection failure: a missing or
// unparsable cert file is reported (ERROR log, "expiring" gauge forced to 1)
// rather than swallowed — same fail-closed reporting posture as
// cmd/northbound/certmon.go's CheckOnce.
func (c *cloudCertMon) CheckOnce() {
	now := c.now()
	certInfo, certErr := inspectCloudCertFile(c.certPath)
	caInfo, caErr := inspectCloudCertFile(c.caPath)

	var certDays, caDays int
	if certErr == nil {
		certDays = cloudDaysUntil(certInfo.NotAfter, now)
	}
	if caErr == nil {
		caDays = cloudDaysUntil(caInfo.NotAfter, now)
	}

	// DaysLeft is the binding constraint: whichever cert expires first is
	// what actually breaks the mTLS dial first (same reasoning as
	// northbound's CertStatus.DaysLeft).
	var daysLeft int
	switch {
	case certErr == nil && caErr == nil:
		daysLeft = certDays
		if caDays < daysLeft {
			daysLeft = caDays
		}
	case certErr == nil:
		daysLeft = certDays
	case caErr == nil:
		daysLeft = caDays
	default:
		daysLeft = 0
	}
	c.daysLeft.Store(int64(daysLeft))

	c.logAlarm(daysLeft, certErr, caErr)
	c.setGauges(certInfo, caInfo, certErr, caErr, daysLeft, now)
	c.checked = true
}

func (c *cloudCertMon) logAlarm(daysLeft int, certErr, caErr error) {
	if certErr != nil {
		slog.Error("lexa-cloudlink: cloud cert expiry check: cloud_cert unreadable/unparsable",
			"path", c.certPath, "err", certErr)
	}
	if caErr != nil {
		slog.Error("lexa-cloudlink: cloud cert expiry check: cloud_ca unreadable/unparsable",
			"path", c.caPath, "err", caErr)
	}
	if certErr != nil || caErr != nil {
		return
	}

	switch classifyCloudCert(daysLeft, c.warnDays) {
	case cloudCertError:
		slog.Error("lexa-cloudlink: cloud certificate EXPIRED", "days_left", daysLeft)
	case cloudCertWarn:
		slog.Warn("lexa-cloudlink: cloud certificate expiring soon",
			"days_left", daysLeft, "warn_threshold_days", c.warnDays)
	default:
		if !c.checked {
			slog.Info("lexa-cloudlink: cloud certificate expiry OK", "days_left", daysLeft)
		} else {
			slog.Debug("lexa-cloudlink: cloud certificate expiry OK", "days_left", daysLeft)
		}
	}
}

func (c *cloudCertMon) setGauges(certInfo, caInfo cloudCertInfo, certErr, caErr error, daysLeft int, now time.Time) {
	if c.m == nil {
		return
	}
	// The seconds gauge must describe the SAME binding constraint daysLeft
	// does: recompute from whichever NotAfter actually produced the minimum,
	// rather than assuming cert-over-CA.
	var notAfter time.Time
	switch {
	case certErr == nil && caErr == nil:
		if certInfo.NotAfter.Before(caInfo.NotAfter) {
			notAfter = certInfo.NotAfter
		} else {
			notAfter = caInfo.NotAfter
		}
	case certErr == nil:
		notAfter = certInfo.NotAfter
	case caErr == nil:
		notAfter = caInfo.NotAfter
	}
	if !notAfter.IsZero() {
		c.m.certExpirySeconds.Set(notAfter.Sub(now).Seconds())
	}
	expiring := certErr != nil || caErr != nil || classifyCloudCert(daysLeft, c.warnDays) != cloudCertOK
	c.m.certExpiring.Set(boolToGauge(expiring))
}
