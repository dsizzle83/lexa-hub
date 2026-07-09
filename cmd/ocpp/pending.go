// Unit 6.1, part 2 (docs/DEVICE_ROADMAP.md §6): the pending-station surface.
//
// Today (and unchanged by this file) getOrCreateLocked auto-creates a
// stationState for ANY inbound station ID, whether or not it appears in
// cfg.Stations — the unconfigured station is tracked-but-uncontrolled:
// measurements flow, but nothing writes to it (no shell, no driver). That
// silent adoption is invisible to an installer. pendingStations surfaces
// the same set of unconfigured-but-seen chargers on the retained
// lexa/ocpp/pending bus doc so a commissioning wizard/installer can see and
// approve them (§4.5) instead of them going unnoticed.
//
// Approval is out-of-band and offline: this codebase has no live config
// reload anywhere, so the wizard writes the station into cfg.Stations and
// restarts lexa-ocpp (§4.5's flow). A freshly-restarted process's
// `configured` set (fixed at construction from cfg.Stations) already
// excludes an approved station from ever being (re-)tracked as pending —
// there is no separate "remove on approval" code path to get wrong.
package main

import (
	"log"
	"sort"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// pendingPublishMinInterval bounds how often pendingStations actually
// republishes the retained doc: a flapping charger (repeated connect/
// disconnect, or a StatusNotification storm) must not hammer the broker on
// every event. Mirrors cmd/hub/state.go's rewalkRateLimit shape exactly —
// same simple "drop the request if one already went out within the window"
// policy, no coalescing/trailing-edge flush: the next event past the window
// (or the next prune-on-publish) catches the broker up.
const pendingPublishMinInterval = 1 * time.Second

// pendingStationTTL: an entry not refreshed (seen) within this long is
// dropped, lazily, the next time anything triggers a publish. 24h is long
// enough that a charger idle over a weekend doesn't fall off the list, short
// enough that a one-off drive-by connection (a demo, a mis-cabled test
// station) doesn't haunt the pending doc indefinitely.
const pendingStationTTL = 24 * time.Hour

// pendingEntry is one unconfigured station's accumulated state. Vendor/
// ModelName start empty (a bare TCP/WS connect carries neither) and are
// filled in when/if a BootNotification arrives — see upsert's merge rule.
type pendingEntry struct {
	Vendor     string
	ModelName  string
	RemoteAddr string
	FirstSeen  time.Time
	LastSeen   time.Time
}

// pendingStations tracks OCPP chargers the CSMS has seen (connect, or send a
// StatusNotification/BootNotification) whose station ID is NOT in
// cfg.Stations, and republishes the full retained bus.PendingStations doc on
// every change (rate-limited). Every entry point takes mu; the actual
// publish call happens outside it, mirroring mqttBridge.publishAll's
// snapshot-under-lock-then-publish-outside-lock discipline.
type pendingStations struct {
	mu         sync.Mutex
	entries    map[string]*pendingEntry
	configured map[string]bool // station IDs pre-registered in cfg.Stations at startup; never tracked as pending

	// publish is the MQTT seam (TASK-082/Unit 6.1 test convention: fake this
	// function directly rather than faking an mqtt.Client). Production
	// wiring (newMQTTBridge) sets it to a closure over
	// mqttutil.PublishJSONRetained(mc, bus.TopicOCPPPending, doc) — QoS 1 is
	// bus.PubQoS's default for this topic (no measurement-plane prefix
	// matches it), which PublishJSONRetained already provides.
	publish func(bus.PendingStations) error

	// gauge mirrors lexa_ocpp_pending_stations (nil-safe: tests without a
	// metrics.Registry pass nil). Updated directly inside buildDocLocked —
	// the single place that computes the authoritative pruned entry set —
	// rather than incremented/decremented at scattered call sites, so it can
	// never drift from what actually got published (same drift concern
	// mqttBridge.connectedStationCount's doc discusses, resolved here by
	// computing fresh in the one function that owns the count instead of a
	// separate scrape-time Collect callback).
	gauge *metrics.Gauge

	lastPublish time.Time
}

// newPendingStations builds a pendingStations component. configuredIDs is
// cfg.Stations' station IDs at startup — anything already in that set is
// never tracked as pending, matching "approval = config-write + restart."
// gauge may be nil (tests without a registry).
func newPendingStations(configuredIDs []string, publish func(bus.PendingStations) error, gauge *metrics.Gauge) *pendingStations {
	configured := make(map[string]bool, len(configuredIDs))
	for _, id := range configuredIDs {
		configured[id] = true
	}
	return &pendingStations{
		entries:    make(map[string]*pendingEntry),
		configured: configured,
		publish:    publish,
		gauge:      gauge,
	}
}

// upsert adds or updates one unconfigured station's pending entry and
// attempts a rate-limited publish. Called from three sites (main.go):
// mqttBridge.onConnect (RemoteAddr known, vendor/model not yet), the
// provisioning forwarder's OnBootNotification (vendor/model known, no
// connection object to read RemoteAddr from), and availForwarder's
// OnStatusNotification (neither known — a defensive twin of onConnect, see
// getOrCreateLocked's doc). Blank vendor/model/remoteAddr never overwrite an
// already-populated field, and FirstSeen is set only once, so the three
// call sites compose into one accumulated entry regardless of arrival order.
//
// A station already in cfg.Stations (the configured set fixed at
// construction) is silently ignored — this component only ever tracks
// stations that are NOT configured.
func (p *pendingStations) upsert(stationID, vendor, model, remoteAddr string, now time.Time) {
	p.mu.Lock()
	if p.configured[stationID] {
		p.mu.Unlock()
		return
	}
	e, ok := p.entries[stationID]
	if !ok {
		e = &pendingEntry{FirstSeen: now}
		p.entries[stationID] = e
	}
	if vendor != "" {
		e.Vendor = vendor
	}
	if model != "" {
		e.ModelName = model
	}
	if remoteAddr != "" {
		e.RemoteAddr = remoteAddr
	}
	e.LastSeen = now
	p.mu.Unlock()
	p.publishRateLimited(now)
}

// publishRateLimited publishes the current doc unless one already went out
// within pendingPublishMinInterval of now, in which case this call is a
// silent no-op (the rewalkRateLimit-style policy — see the const doc).
func (p *pendingStations) publishRateLimited(now time.Time) {
	p.mu.Lock()
	if !p.lastPublish.IsZero() && now.Sub(p.lastPublish) < pendingPublishMinInterval {
		p.mu.Unlock()
		return
	}
	p.lastPublish = now
	doc := p.buildDocLocked(now)
	p.mu.Unlock()
	p.doPublish(doc)
}

// publishStartup unconditionally publishes the current (typically empty)
// doc, bypassing the rate limiter — called once at construction time
// (before anything has connected) so a fresh process clears any stale
// retained lexa/ocpp/pending left over from a prior run (entries that were
// since approved into cfg.Stations, or chargers that never came back)
// instead of leaving a restarting subscriber looking at old state.
func (p *pendingStations) publishStartup(now time.Time) {
	p.mu.Lock()
	p.lastPublish = now
	doc := p.buildDocLocked(now)
	p.mu.Unlock()
	p.doPublish(doc)
}

// buildDocLocked prunes entries not seen within pendingStationTTL, updates
// the gauge, and returns the full retained doc reflecting what remains.
// Caller must hold mu.
func (p *pendingStations) buildDocLocked(now time.Time) bus.PendingStations {
	for id, e := range p.entries {
		if now.Sub(e.LastSeen) > pendingStationTTL {
			delete(p.entries, id)
		}
	}
	ids := make([]string, 0, len(p.entries))
	for id := range p.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic wire order

	doc := bus.PendingStations{
		Envelope: bus.Envelope{V: bus.PendingStationsV},
		Ts:       now.Unix(),
	}
	for _, id := range ids {
		e := p.entries[id]
		doc.Stations = append(doc.Stations, bus.PendingStation{
			StationID:   id,
			Vendor:      e.Vendor,
			ModelName:   e.ModelName,
			FirstSeenTs: e.FirstSeen.Unix(),
			RemoteAddr:  e.RemoteAddr,
		})
	}
	if p.gauge != nil {
		p.gauge.Set(float64(len(p.entries)))
	}
	return doc
}

func (p *pendingStations) doPublish(doc bus.PendingStations) {
	if p.publish == nil {
		return
	}
	if err := p.publish(doc); err != nil {
		log.Printf("lexa-ocpp: publish pending stations: %v", err)
	}
}
