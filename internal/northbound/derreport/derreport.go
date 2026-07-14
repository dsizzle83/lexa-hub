// Package derreport implements the northbound half of the WP-4 DER*
// reporting pipeline (standards-buildout A2 — CORE-009 PUT half, CORE-014,
// BASIC-028): it subscribes the hub's retained lexa/hub/dersite doc
// (bus.DERSiteReport — the D2 GFEMS site aggregate) and PUTs it upstream as
// csipmodel DERCapabilityFull / DERSettingsFull / DERStatusFull /
// DERAvailability, to the hrefs the discovery walker observed on the self
// EndDevice's DERList entry (run.RunOnce feeds them via OnWalk — the same
// walker-owns-the-hrefs shape as flowres.SetRequestPath and the WP-6
// logevent SetPath; no path is ever hardcoded).
//
// Cadences (digest G29/G30, Tables 12/13):
//   - DERCapability + DERSettings: at startup and on dersite CONTENT change,
//     keyed on bus.DERSiteReport.ContentHash (capability/settings-scoped by
//     construction — SoC jitter never re-PUTs nameplate data).
//   - DERStatus + DERAvailability: once per successful discovery walk. The
//     vendored csipmodel.DERList carries no pollRate attribute of its own,
//     and the walk cadence is already the pollRate-honoring cadence
//     (TASK-071/run/pollrate.go effectiveInterval, "honor" product default),
//     so per-walk PUTs inherit exactly the server-advertised rate — that IS
//     the G30 pollRate seam.
//
// Delivery stance is crash-only (AD-011), mirroring the logevent poster: one
// retry per PUT (the fetcher auto-redials a dead session on the first error,
// so the retry covers exactly the stale-keepalive case), then drop-and-count
// — the next cadence point re-sends fresher data anyway; nothing is spooled.
// A 404/405 on a resource means the server doesn't offer it (both DER
// sub-resources and their writability are optional server-side): tolerated
// once — logged + counted — then that resource is skipped until its href
// changes. Egress honors the shared WP-7/D4 PIN-freeze gate: Suspended() is
// checked before every PUT batch.
package derreport

import (
	"encoding/xml"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"sync"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/northbound/egress"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// Putter is the subset of tlsclient.WolfSSLFetcher the Manager needs
// (defined at the point of consumption, 05 §2 — same shape as
// logevent.Poster), keeping every PUT path unit-testable without a live TLS
// session.
type Putter interface {
	Put(path string, body []byte, contentType string) ([]byte, error)
}

// resource indexes the four DER sub-resources the Manager PUTs.
type resource int

const (
	resCapability resource = iota
	resSettings
	resStatus
	resAvailability
	numResources
)

var resourceNames = [numResources]string{
	"DERCapability", "DERSettings", "DERStatus", "DERAvailability",
}

const contentTypeSepXML = "application/sep+xml"

// Manager subscribes lexa/hub/dersite and PUTs DER* resources.
type Manager struct {
	mu      sync.Mutex
	fetcher Putter
	// clk is the shared single-owner utility Clock (AD-004): the report's
	// hub-local timestamps are converted to server time before stamping
	// readingTime/updatedTime, exactly as the logevent poster does.
	clk *utilitytime.Clock
	// gate is the WP-7/D4 shared egress freeze (nil-safe: nil = open).
	gate *egress.Gate

	// hrefs/skip/failing are per-resource: the walker-observed PUT target
	// ("" until a walk finds one, or when the server exposes no link), the
	// 404/405 "server doesn't offer it" latch (reset when the href changes),
	// and the consecutive-failure edge for transition-only logging.
	hrefs   [numResources]string
	skip    [numResources]bool
	failing [numResources]bool

	// report is the latest dersite doc; lastCapSetHash is the ContentHash of
	// the report whose capability+settings most recently PUT clean (every
	// non-skipped resource succeeded) — the G29 on-change dedupe.
	report         *bus.DERSiteReport
	lastCapSetHash string

	puts   *metrics.Counter // lexa_nb_derreport_puts_total; nil-safe
	errors *metrics.Counter // lexa_nb_derreport_errors_total; nil-safe
}

// New constructs a Manager that PUTs via f, reading utility time from clk,
// counting successful PUTs on puts and failed/404-skipped ones on errors
// (both nil-safe, matching metrics.Counter's contract).
func New(f Putter, clk *utilitytime.Clock, puts, errors *metrics.Counter) *Manager {
	return &Manager{fetcher: f, clk: clk, puts: puts, errors: errors}
}

// SetEgressGate wires the shared WP-7/D4 egress gate. Call once at wiring
// time (cmd/northbound/main.go), before any walk or subscription delivers.
func (m *Manager) SetEgressGate(g *egress.Gate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

// HandleDERSite is bus.TopicHubDERSite's subscription handler: wire it
// directly into mqttutil.Subscribe from main() (the version gate, decode,
// and Finite check live there). Runs on the MQTT subscription goroutine; a
// triggered capability/settings PUT is synchronous here, which is fine —
// this is not the hub's tick path, and the fetcher's own ReadTimeout bounds
// it (the same stance as logevent.HandleLogEvent).
func (m *Manager) HandleDERSite(_ string, msg bus.DERSiteReport) {
	m.mu.Lock()
	m.report = &msg
	changed := msg.ContentHash != m.lastCapSetHash
	m.mu.Unlock()
	if changed {
		m.putCapabilitySettings()
	}
}

// OnWalk is run.Discovery's per-walk callback (run.DERReportSink): it
// refreshes the four PUT targets from the walker's observations and drives
// the per-walk cadence — DERStatus+DERAvailability every walk (G30, see the
// package doc's pollRate note), DERCapability+DERSettings whenever the
// current report's ContentHash has not yet PUT clean (which covers startup:
// the retained dersite doc usually arrives before the first walk completes).
// An href that CHANGED since the last walk clears that resource's 404/405
// skip latch — a re-laid-out server deserves a fresh attempt.
//
// Runs on the single discovery walk goroutine. Never called while the PIN
// verifier has the walk frozen (run.RunOnce returns before the tree
// consumers on a freeze), and the egress gate is re-checked here too — the
// MQTT-driven path above does not ride the walk's freeze.
func (m *Manager) OnWalk(capabilityHref, settingsHref, statusHref, availabilityHref string) {
	m.mu.Lock()
	for res, href := range map[resource]string{
		resCapability:   capabilityHref,
		resSettings:     settingsHref,
		resStatus:       statusHref,
		resAvailability: availabilityHref,
	} {
		if m.hrefs[res] != href {
			m.hrefs[res] = href
			m.skip[res] = false
		}
	}
	needCapSet := m.report != nil && m.report.ContentHash != m.lastCapSetHash
	m.mu.Unlock()

	m.putStatusAvailability()
	if needCapSet {
		m.putCapabilitySettings()
	}
}

// putCapabilitySettings PUTs DERCapability + DERSettings for the current
// report, recording its ContentHash only when every non-skipped resource
// succeeded — a transient failure leaves the hash unrecorded so the next
// walk/change retries the identical content (same "late/dropped is retried"
// contract as everything else on this bus).
func (m *Manager) putCapabilitySettings() {
	m.mu.Lock()
	rep := m.report
	suspended := m.gate.Suspended()
	m.mu.Unlock()
	if rep == nil {
		return
	}
	if suspended {
		slog.Warn("lexa-northbound: derreport capability/settings PUT skipped — egress suspended (PIN freeze)",
			"content_hash", rep.ContentHash)
		return
	}

	ok := m.putResource(resCapability, buildCapability(rep))
	ok = m.putResource(resSettings, m.buildSettings(rep)) && ok
	if ok {
		m.mu.Lock()
		m.lastCapSetHash = rep.ContentHash
		m.mu.Unlock()
		slog.Info("lexa-northbound: DERCapability/DERSettings PUT",
			"content_hash", rep.ContentHash, "modes", rep.ModesSupported, "rtg_max_w", rep.RtgMaxW)
	}
}

// putStatusAvailability PUTs DERStatus + DERAvailability from the current
// report — the per-walk (pollRate-paced) cadence. No hash bookkeeping:
// status is a fresh snapshot every time.
func (m *Manager) putStatusAvailability() {
	m.mu.Lock()
	rep := m.report
	suspended := m.gate.Suspended()
	m.mu.Unlock()
	if rep == nil {
		return // no dersite doc seen yet — nothing truthful to report
	}
	if suspended {
		// The walk path normally never reaches here during a PIN freeze
		// (RunOnce returns early), but the gate is authoritative regardless.
		slog.Warn("lexa-northbound: derreport status PUT skipped — egress suspended (PIN freeze)")
		return
	}

	m.putResource(resStatus, m.buildStatus(rep))
	if av := m.buildAvailability(rep); av != nil {
		m.putResource(resAvailability, av)
	}
}

// putResource marshals and PUTs one resource, with the retry/skip semantics
// from the package doc. Returns whether this resource should be considered
// settled — success, or a permanent "server doesn't offer it" skip; false
// means retry-worthy (no href yet, or a transient failure).
func (m *Manager) putResource(res resource, v any) bool {
	m.mu.Lock()
	href := m.hrefs[res]
	skipped := m.skip[res]
	m.mu.Unlock()

	if skipped {
		return true // server doesn't offer it — settled, don't block the hash
	}
	if href == "" {
		// No walk has observed this link yet (or the server exposes none on
		// the DER entry). Not settled: the next walk may discover it.
		return false
	}

	body, err := xml.Marshal(v)
	if err != nil {
		// Cannot happen for these hand-built structs; treat as transient.
		slog.Warn("lexa-northbound: derreport marshal failed", "resource", resourceNames[res], "err", err)
		m.errors.Inc()
		return false
	}

	_, err = m.fetcher.Put(href, body, contentTypeSepXML)
	if err != nil && putStatusCode(err) == 0 {
		// Transport-level failure (no HTTP status): retry once — the fetcher
		// auto-redials a dead session on the first error, so this covers
		// exactly the stale-keepalive case (logevent's reasoning).
		_, err = m.fetcher.Put(href, body, contentTypeSepXML)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.puts.Inc()
		if m.failing[res] {
			m.failing[res] = false
			slog.Info("lexa-northbound: derreport PUT recovered", "resource", resourceNames[res], "href", href)
		}
		return true
	}

	m.errors.Inc()
	if code := putStatusCode(err); code == 404 || code == 405 {
		// The server answered: it does not offer this resource (or not for
		// writing). Tolerated once, then skipped until the href changes.
		m.skip[res] = true
		slog.Warn("lexa-northbound: derreport resource not offered by server — skipping henceforth",
			"resource", resourceNames[res], "href", href, "status", code)
		return true
	}
	// Transient/other failure: edge-logged (one WARN per failure episode,
	// not one per walk — flash budget), retried at the next cadence point.
	if !m.failing[res] {
		m.failing[res] = true
		slog.Warn("lexa-northbound: derreport PUT failed — will retry next cadence",
			"resource", resourceNames[res], "href", href, "err", err)
	}
	return false
}

// putStatusRe extracts the trailing HTTP status from tlsclient's
// putResult error format ("PUT %s: status %d" — pinned there by WP-3's
// tests). Parsing the message is deliberate lane-discipline: derreport's
// only alternative is a typed error added to internal/tlsclient, which is
// outside this package's WP; if putResult ever grows one, switch to
// errors.As here and delete this.
var putStatusRe = regexp.MustCompile(`status (\d+)$`)

// putStatusCode returns the HTTP status carried in a Put error, or 0 for a
// transport-level failure (dial/read/redirect errors carry no status).
func putStatusCode(err error) int {
	if err == nil {
		return 0
	}
	mch := putStatusRe.FindStringSubmatch(err.Error())
	if mch == nil {
		return 0
	}
	n, _ := strconv.Atoi(mch[1])
	return n
}

// serverTime converts a hub-local Unix timestamp to utility/server time by
// adding the discovery walk's accumulated clock offset; before the first
// successful walk the local timestamp is the best available value (CSIP
// §5.2.1.3's 30 s discipline makes it close) — same policy as the logevent
// poster.
func (m *Manager) serverTime(localTs int64) int64 {
	if off, ok := m.clk.Offset(); ok {
		return localTs + off
	}
	return localTs
}

// buildCapability converts the report's ratings/modes into a
// csipmodel.DERCapabilityFull. rtgMaxWh has no field on the vendored type
// (the WP-1 pin is frozen; the value still rides the bus for /status and a
// future pin), so it is not PUT. Zero-valued storage ratings (a site with no
// batteries) are omitted rather than asserted as 0 (G27).
func buildCapability(rep *bus.DERSiteReport) *model.DERCapabilityFull {
	cap := &model.DERCapabilityFull{
		Type:           rep.DERType,
		ModesSupported: rep.ModesSupported,
		RtgMaxW:        *apWatts(rep.RtgMaxW),
	}
	if rep.RtgMaxChargeRateW > 0 {
		cap.RtgMaxChargeRateW = apWatts(rep.RtgMaxChargeRateW)
	}
	if rep.RtgMaxDischargeRateW > 0 {
		cap.RtgMaxDischargeRateW = apWatts(rep.RtgMaxDischargeRateW)
	}
	if rep.RtgMaxVA != nil {
		cap.RtgMaxVA = apWatts(*rep.RtgMaxVA)
	}
	if rep.RtgMaxVar != nil {
		cap.RtgMaxVar = apWatts(*rep.RtgMaxVar)
	}
	return cap
}

// buildSettings converts the report's settings (≤ ratings by construction,
// hub-side) into a csipmodel.DERSettingsFull. setMaxWh mirrors rtgMaxWh's
// vendored-model gap.
func (m *Manager) buildSettings(rep *bus.DERSiteReport) *model.DERSettingsFull {
	set := &model.DERSettingsFull{
		UpdatedTime: m.serverTime(rep.Ts),
		SetMaxW:     apWatts(rep.SetMaxW),
	}
	if rep.SetMaxChargeRateW > 0 {
		set.SetMaxChargeRateW = apWatts(rep.SetMaxChargeRateW)
	}
	if rep.SetMaxDischargeRateW > 0 {
		set.SetMaxDischargeRateW = apWatts(rep.SetMaxDischargeRateW)
	}
	if rep.SetMaxVA != nil {
		set.SetMaxVA = apWatts(*rep.SetMaxVA)
	}
	if rep.SetMaxVar != nil {
		set.SetMaxVar = apWatts(*rep.SetMaxVar)
	}
	return set
}

// buildStatus converts the report's live status block into a
// csipmodel.DERStatusFull (Table 13/G30). alarmStatus has no field on the
// vendored type (pin frozen — same note as rtgMaxWh); the category bitmap
// still rides the bus, and the alarm OCCURRENCES reach the server through
// the WP-6 LogEvent pipeline regardless.
func (m *Manager) buildStatus(rep *bus.DERSiteReport) *model.DERStatusFull {
	rt := m.serverTime(rep.Status.ReadingTs)
	st := &model.DERStatusFull{
		ReadingTime:           rt,
		GenConnectStatus:      &model.DERStatusValue{DateTime: rt, Value: rep.Status.GenConnectStatus},
		OperationalModeStatus: &model.DERStatusValue{DateTime: rt, Value: rep.Status.OperationalMode},
	}
	if rep.Status.StorageMode != nil {
		st.StorageModeStatus = &model.DERStatusValue{DateTime: rt, Value: *rep.Status.StorageMode}
	}
	if rep.Status.SocPct != nil {
		soc := *rep.Status.SocPct
		if soc < 0 {
			soc = 0
		}
		if soc > 100 {
			soc = 100
		}
		st.StateOfChargeStatus = &struct {
			DateTime int64 `xml:"dateTime"`
			Value    int16 `xml:"value"`
		}{DateTime: rt, Value: int16(math.Round(soc * 100))} // percent × 100
	}
	return st
}

// buildAvailability converts the report's availability block, or nil when
// the hub derived nothing (G27 — an empty DERAvailability asserts nothing
// worth a PUT).
func (m *Manager) buildAvailability(rep *bus.DERSiteReport) *model.DERAvailability {
	if rep.Avail == nil {
		return nil
	}
	av := &model.DERAvailability{ReadingTime: m.serverTime(rep.Status.ReadingTs)}
	if rep.Avail.EstimatedWAvailW != nil {
		av.EstimatedWAvail = apWatts(*rep.Avail.EstimatedWAvailW)
	}
	if rep.Avail.AvailabilityDurationS != nil {
		d := *rep.Avail.AvailabilityDurationS
		av.AvailabilityDuration = &d
	}
	if rep.Avail.MaxChargeDurationS != nil {
		d := *rep.Avail.MaxChargeDurationS
		av.MaxChargeDuration = &d
	}
	return av
}

// apWatts encodes w into an IEEE 2030.5 ActivePower, scaling the multiplier
// up until the value fits in int16 — a bare int16 conversion is
// implementation-defined for out-of-range floats, silently corrupting any
// value ≥ 32.768 kW. Precision loss is bounded by half the final scale step.
// (A deliberate small copy of cmd/hub/state.go's wattsToActivePower: cmd/*
// packages are not importable, 05 §1.)
func apWatts(w float64) *model.ActivePower {
	mult := int8(0)
	for (w > math.MaxInt16 || w < math.MinInt16) && mult < 9 {
		w /= 10
		mult++
	}
	return &model.ActivePower{Value: int16(math.Round(w)), Multiplier: mult}
}
