package main

// certmon.go implements TASK-072 (§10.5): startup + daily inspection of the
// configured client and CA certificate PEM files' expiry, a retained bus
// status, Prometheus gauges, and a WARN/ERROR log alarm as the deadline
// approaches or passes.
//
// lexa-telemetry (cmd/telemetry) is configured with its own ca_cert/
// client_cert paths, but in every deployment those paths point at the SAME
// files this monitor already inspects (configs/*.json both read
// /etc/lexa/certs/{ca,client}.pem — see lexa-hub CLAUDE.md's config table).
// One process watching the file's content is enough; telemetry does not run
// a second monitor. If a future deployment ever points telemetry at a
// genuinely different cert, this note is the trip-wire to revisit that
// assumption.
//
// Pure crypto/x509 PEM inspection — no wolfSSL/cgo involvement. This is
// deliberate (task's "common mistakes" list): the monitor only ever READS
// the file for its NotAfter/NotBefore fields, never performs a handshake, so
// it stays testable on every platform and never touches the wolfSSL context
// TLS actually uses.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/mqttutil"
)

// defaultCertExpiryWarnDays is certMon's days-remaining threshold below
// which it logs a WARN alarm and sets the "expiring" gauge, absent an
// explicit Config.CertExpiryWarnDays override (§10.5's "≥30 days out" gate).
const defaultCertExpiryWarnDays = 30

// certCheckInterval is how often Monitor re-inspects the configured PEM
// files after its immediate startup check. A service that runs for months
// crosses the warn threshold silently if only the startup check ever runs
// (task's "common mistakes" list) — the daily re-check is the point.
const certCheckInterval = 24 * time.Hour

// certInfo is the validity window of one inspected leaf certificate.
type certInfo struct {
	NotBefore time.Time
	NotAfter  time.Time
}

// inspectCertFile parses path as PEM and returns the LEAF certificate's
// validity window. A client cert file may contain a chain (leaf followed by
// intermediates); by PEM chain convention the leaf is the first CERTIFICATE
// block, which is also the correct (and only) block in a single-certificate
// CA file — one code path serves both callers without a chain-aware special
// case for either.
func inspectCertFile(path string) (certInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return certInfo{}, fmt.Errorf("read %s: %w", path, err)
	}
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return certInfo{}, fmt.Errorf("no CERTIFICATE PEM block found in %s", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return certInfo{}, fmt.Errorf("parse certificate in %s: %w", path, err)
		}
		return certInfo{NotBefore: cert.NotBefore, NotAfter: cert.NotAfter}, nil
	}
}

// daysUntil returns the whole number of days remaining until notAfter, as of
// now. A still-valid certificate always reports at least 1 (ceiling of the
// remaining duration) so "days_left <= 0" is an unambiguous expired signal —
// a cert with 12 hours left is not yet expired and must not be misclassified
// as ERROR alongside one that expired months ago. An already-expired
// certificate reports 0 or a negative count (floor of the — negative —
// remaining duration), growing more negative the longer it stays expired.
func daysUntil(notAfter, now time.Time) int {
	remaining := notAfter.Sub(now)
	if remaining <= 0 {
		return int(math.Floor(remaining.Hours() / 24))
	}
	return int(math.Ceil(remaining.Hours() / 24))
}

// certLevel is the alarm severity daysUntil's result maps to.
type certLevel int

const (
	certLevelOK certLevel = iota
	certLevelWarn
	certLevelError
)

// classify maps daysLeft to a certLevel given warnDays as the WARN
// threshold: <=0 is always ERROR (expired) regardless of warnDays.
func classify(daysLeft, warnDays int) certLevel {
	switch {
	case daysLeft <= 0:
		return certLevelError
	case daysLeft <= warnDays:
		return certLevelWarn
	default:
		return certLevelOK
	}
}

// Monitor owns the cert-expiry inspection goroutine: a startup check plus a
// daily re-check of the configured client and CA PEM files, publishing a
// retained bus.CertStatus, updating Prometheus gauges, and logging a WARN
// (<=warnDays remaining) or ERROR (already expired) alarm.
//
// Gauges are two independently-named pairs (lexa_cert_expiry_client_seconds/
// lexa_cert_expiry_ca_seconds, lexa_cert_expiring_client/
// lexa_cert_expiring_ca) rather than a single lexa_cert_expiry_seconds gauge
// with a {cert="client|ca"} label: internal/metrics (TASK-044) is
// deliberately label-free (Registry.Gauge is keyed by a flat string name —
// see its package doc), so a literal Prometheus label pair on a metric name
// would render as part of the metric name itself in the exposition text
// rather than a real label, which promtool/Prometheus's parser rejects
// (metric names must match [a-zA-Z_:][a-zA-Z0-9_:]*, no braces). Two flat
// gauge names stay valid exposition text under this repo's library, at the
// cost of the task's literal metric name — noted as a deviation.
type Monitor struct {
	clientPath string
	caPath     string
	warnDays   int
	mc         mqtt.Client

	clientExpirySeconds *metrics.Gauge // lexa_cert_expiry_client_seconds
	caExpirySeconds     *metrics.Gauge // lexa_cert_expiry_ca_seconds
	clientExpiring      *metrics.Gauge // lexa_cert_expiring_client (1 if <=warnDays or unreadable, else 0)
	caExpiring          *metrics.Gauge // lexa_cert_expiring_ca

	now func() time.Time // seam for tests; defaults to time.Now

	// pinOK (WP-7, D4) is the optional provider for the additive
	// CertStatus.PinOK field — run.PinVerifier.PinOK in production. nil, or
	// a provider returning nil, omits the field (check disabled / no
	// verdict yet). Set via SetPinOK before any CheckOnce caller starts
	// (the monitor goroutine, the rotation controller's onCommit, and the
	// verifier's own onChange all invoke CheckOnce).
	pinOK func() *bool

	// checked is set after the first CheckOnce: a healthy result logs at Info
	// on the very first (startup) check, then demotes to Debug on later
	// checks that are still healthy (steady-state, not a transition — same
	// convention run.Discovery.RunOnce uses for its per-walk success log).
	// WARN/ERROR always log at their own level regardless of checked, because
	// the daily re-alarm is the point (task's "common mistakes": alerting
	// once at startup only misses a cert that crosses the threshold mid-run).
	checked bool
}

// NewMonitor constructs a Monitor. reg may be nil (tests exercising CheckOnce
// without a metrics registry); warnDays<=0 falls back to
// defaultCertExpiryWarnDays.
func NewMonitor(mc mqtt.Client, clientPath, caPath string, warnDays int, reg *metrics.Registry) *Monitor {
	if warnDays <= 0 {
		warnDays = defaultCertExpiryWarnDays
	}
	m := &Monitor{
		clientPath: clientPath,
		caPath:     caPath,
		warnDays:   warnDays,
		mc:         mc,
		now:        time.Now,
	}
	if reg != nil {
		m.clientExpirySeconds = reg.Gauge("lexa_cert_expiry_client_seconds")
		m.caExpirySeconds = reg.Gauge("lexa_cert_expiry_ca_seconds")
		m.clientExpiring = reg.Gauge("lexa_cert_expiring_client")
		m.caExpiring = reg.Gauge("lexa_cert_expiring_ca")
	}
	return m
}

// SetPinOK wires the WP-7/D4 pin_ok provider (see the pinOK field doc).
// Call during wiring, before Run/CheckOnce goroutines start.
func (m *Monitor) SetPinOK(provider func() *bool) {
	m.pinOK = provider
}

// Run performs an immediate check, then re-checks every interval until ctx
// is cancelled. Callers run this in its own goroutine — an owned goroutine
// with its own ticker and its own ctx.Done() shutdown path (05 §4), sharing
// no state with the discovery walk loop. Production wiring (cmd/northbound/
// main.go) passes certCheckInterval (24h); interval is a parameter (mirrors
// run.Discovery.Loop(ctx, interval)'s shape) so tests can inject a short
// interval and observe multiple ticks without waiting a day.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	m.CheckOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckOnce()
		}
	}
}

// CheckOnce inspects both PEM files, publishes the retained CertStatus,
// updates gauges, and logs at the level the worse of the two certs' state
// demands. It never returns an error and never panics on an inspection
// failure: a missing or unparsable cert file is REPORTED (fail-closed —
// publish the error state onto the bus and alarm at ERROR) rather than
// swallowed or crashing the process.
func (m *Monitor) CheckOnce() bus.CertStatus {
	now := m.now()
	status := bus.CertStatus{Envelope: bus.Envelope{V: bus.CertStatusV}, Ts: now.Unix()}

	clientInfo, clientErr := inspectCertFile(m.clientPath)
	caInfo, caErr := inspectCertFile(m.caPath)

	if clientErr != nil {
		status.ClientErr = clientErr.Error()
	} else {
		status.ClientNotAfter = clientInfo.NotAfter.Unix()
		status.ClientDaysLeft = daysUntil(clientInfo.NotAfter, now)
	}
	if caErr != nil {
		status.CAErr = caErr.Error()
	} else {
		status.CANotAfter = caInfo.NotAfter.Unix()
		status.CADaysLeft = daysUntil(caInfo.NotAfter, now)
	}

	// DaysLeft is the binding constraint: whichever inspected cert expires
	// first is what actually breaks the mTLS handshake first, so the
	// top-level alarm tracks the minimum among the certs successfully read,
	// not an average or the client alone. If both failed to parse, DaysLeft
	// stays 0 (classify treats that as ERROR, matching "unknown state is
	// worst case" — fail-closed reporting, not a silent gap).
	switch {
	case clientErr == nil && caErr == nil:
		status.DaysLeft = status.ClientDaysLeft
		if status.CADaysLeft < status.DaysLeft {
			status.DaysLeft = status.CADaysLeft
		}
	case clientErr == nil:
		status.DaysLeft = status.ClientDaysLeft
	case caErr == nil:
		status.DaysLeft = status.CADaysLeft
	default:
		status.DaysLeft = 0
	}

	// WP-7 (D4): fold the registration-PIN verdict into the retained doc so
	// lexa-api's /status can surface it; nil (disabled/no verdict) omits it.
	if m.pinOK != nil {
		status.PinOK = m.pinOK()
	}

	m.logAlarm(status, clientErr, caErr)
	m.setGauges(status, clientErr, caErr, now)
	m.checked = true

	if err := mqttutil.PublishJSONRetained(m.mc, bus.TopicNorthboundCertStatus, status); err != nil {
		slog.Warn("lexa-northbound: publish cert status", "err", err)
	}
	return status
}

func (m *Monitor) logAlarm(status bus.CertStatus, clientErr, caErr error) {
	if clientErr != nil {
		slog.Error("lexa-northbound: cert expiry check: client cert unreadable/unparsable",
			"path", m.clientPath, "err", clientErr)
	}
	if caErr != nil {
		slog.Error("lexa-northbound: cert expiry check: CA cert unreadable/unparsable",
			"path", m.caPath, "err", caErr)
	}
	if clientErr != nil || caErr != nil {
		return
	}

	switch classify(status.DaysLeft, m.warnDays) {
	case certLevelError:
		slog.Error("lexa-northbound: certificate EXPIRED",
			"client_days_left", status.ClientDaysLeft, "ca_days_left", status.CADaysLeft, "days_left", status.DaysLeft)
	case certLevelWarn:
		slog.Warn("lexa-northbound: certificate expiring soon",
			"client_days_left", status.ClientDaysLeft, "ca_days_left", status.CADaysLeft,
			"days_left", status.DaysLeft, "warn_threshold_days", m.warnDays)
	default:
		if !m.checked {
			slog.Info("lexa-northbound: certificate expiry OK",
				"client_days_left", status.ClientDaysLeft, "ca_days_left", status.CADaysLeft)
		} else {
			slog.Debug("lexa-northbound: certificate expiry OK",
				"client_days_left", status.ClientDaysLeft, "ca_days_left", status.CADaysLeft)
		}
	}
}

func (m *Monitor) setGauges(status bus.CertStatus, clientErr, caErr error, now time.Time) {
	if clientErr == nil {
		m.clientExpirySeconds.Set(time.Unix(status.ClientNotAfter, 0).Sub(now).Seconds())
		m.clientExpiring.Set(boolToGauge(classify(status.ClientDaysLeft, m.warnDays) != certLevelOK))
	} else {
		m.clientExpiring.Set(1) // unreadable/unparsable is itself an alarm condition
	}
	if caErr == nil {
		m.caExpirySeconds.Set(time.Unix(status.CANotAfter, 0).Sub(now).Seconds())
		m.caExpiring.Set(boolToGauge(classify(status.CADaysLeft, m.warnDays) != certLevelOK))
	} else {
		m.caExpiring.Set(1)
	}
}

func boolToGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
