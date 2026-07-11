package main

// settings.go publishes the retained lexa/hub/settings document (bus.HubSettings,
// GAP-8): the hub's EFFECTIVE backup-reserve floor and its ACTIVE tariff, so the
// app's reserve slider and tariff viewer render hub truth instead of a
// locally-persisted last-submitted value.
//
// Ownership split (why this is a separate component, not inlined in the adopter
// or the plan observer):
//   - The reserve SOURCE and the tariff SPEC/SOURCE/updated_at are known only to
//     the intent adopter (intent.go) — it sees each intent's origin and the
//     compiled tariff.
//   - The EFFECTIVE reserve pct is known only to the engine (Engine.
//     EffectiveReservePct, resolved inside a plan) and is -1 until the first
//     plan runs.
// This publisher holds the intent-sourced fields and reads the engine accessor,
// so both halves land in one message. It is published on three triggers, each
// documented at its call site:
//   1. a SEED at startup (retained, so lexa-api has a value immediately);
//   2. an on-CHANGE publish from the adopter whenever a reserve or tariff intent
//      lands (updates source/spec right away — the effective pct it carries may
//      still be one plan stale, since SetBackupReserve is async);
//   3. a refresh piggybacked on the plan observer, deduped so it republishes
//      ONLY when the effective pct actually moves (catching the -1→real
//      transition and the post-replan settle of an on-change publish).

import (
	"log/slog"
	"math"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
)

// reserveReader is the engine accessor the publisher needs — an interface, not
// *orchestrator.Engine, so settings_test.go can substitute a fake without
// standing up a running engine (the hubEngine/reserveReader split mirrors
// intent.go's). *orchestrator.Engine satisfies it structurally.
type reserveReader interface {
	EffectiveReservePct() float64
}

// settingsPublisher owns the retained lexa/hub/settings document.
type settingsPublisher struct {
	mu sync.Mutex

	mc       mqtt.Client
	eng      reserveReader
	floorPct float64 // configured reserve floor (percent); the value the intent clamps against

	// Intent-sourced state (the adopter's contributions).
	reserveSource   string          // "default" until a reserve intent; then the intent origin
	tariffSpec      *bus.TariffSpec // nil until a tariff intent sets it
	tariffSource    string          // "csip" (no manual override) until a tariff intent → "manual"
	tariffUpdatedAt int64           // Unix s; 0 until a tariff intent

	// Dedupe for the plan-observer refresh: only republish when the effective
	// pct changes. lastPubEffBits is the float64 bits of the effective pct last
	// PUBLISHED; published guards the first publish (before which any refresh
	// must go out).
	published      bool
	lastPubEffBits uint64

	now func() time.Time // seam for tests; time.Now in production
}

// newSettingsPublisher builds the publisher. floorPct is cfg.Planner.
// TerminalReservePct; a non-positive value defaults to 20, matching the engine's
// own floor default (buildPlannerParams) so /status's floor_pct agrees with the
// clamp the engine actually applies.
func newSettingsPublisher(mc mqtt.Client, eng reserveReader, floorPct float64) *settingsPublisher {
	if floorPct <= 0 {
		floorPct = 20
	}
	return &settingsPublisher{
		mc:            mc,
		eng:           eng,
		floorPct:      floorPct,
		reserveSource: "default",
		tariffSource:  "csip",
		now:           time.Now,
	}
}

// settingsLocked builds the current HubSettings for a given effective pct
// (read once by the caller so a single message is internally consistent).
func (p *settingsPublisher) settingsLocked(effPct float64) bus.HubSettings {
	floor := p.floorPct
	hs := bus.HubSettings{
		Envelope: bus.Envelope{V: bus.HubSettingsV},
		Reserve: bus.ReserveSettings{
			FloorPct: &floor,
			Source:   p.reserveSource,
		},
		Tariff: bus.TariffSettings{
			Source:    p.tariffSource,
			UpdatedAt: p.tariffUpdatedAt,
			Spec:      p.tariffSpec,
		},
		Ts: p.now().Unix(),
	}
	// -1 is the engine's "no plan yet" sentinel → leave EffectivePct nil (absent).
	if effPct >= 0 {
		e := effPct
		hs.Reserve.EffectivePct = &e
	}
	return hs
}

// publishLocked builds and publishes the retained settings document, recording
// the effective pct it published for the refresh dedupe. Caller holds p.mu.
func (p *settingsPublisher) publishLocked() {
	eff := p.eng.EffectiveReservePct()
	hs := p.settingsLocked(eff)
	p.lastPubEffBits = math.Float64bits(eff)
	p.published = true
	if err := mqttutil.PublishJSONRetained(p.mc, bus.TopicHubSettings, hs); err != nil {
		slog.Warn("lexa-hub: publish hub settings", "err", err)
	}
}

// publish is the seed/force publish (startup). Takes the lock and emits the
// current settings unconditionally.
func (p *settingsPublisher) publish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishLocked()
}

// onReserveChange records a reserve intent's origin and republishes. Called from
// the adopter (intent.go applyReserve) after a valid SetBackupReserve. The
// effective pct in this publish may be one plan stale (SetBackupReserve is
// async); refreshFromPlan catches the settled value on the next plan.
func (p *settingsPublisher) onReserveChange(origin string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserveSource = reserveSourceFromOrigin(origin)
	p.publishLocked()
}

// onTariffChange records a tariff intent's spec + origin and republishes. A
// tariff intent is always a MANUAL override, so source is "manual" regardless of
// origin (origin is kept only for symmetry / future use). Called from the
// adopter (intent.go applyTariff) after a successful compile.
func (p *settingsPublisher) onTariffChange(spec bus.TariffSpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := spec // defensive copy: the adopter may reuse its decode buffer across redeliveries
	p.tariffSpec = &cp
	p.tariffSource = "manual"
	p.tariffUpdatedAt = p.now().Unix()
	p.publishLocked()
}

// refreshFromPlan republishes ONLY when the engine's effective reserve pct has
// moved since the last publish (or nothing has been published yet). Piggybacked
// on the plan observer, which fires every engine pass — the dedupe keeps this
// from spamming a retained topic every tick while still propagating the
// -1→real transition and the post-replan settle of an on-change publish.
func (p *settingsPublisher) refreshFromPlan() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := math.Float64bits(p.eng.EffectiveReservePct())
	if p.published && cur == p.lastPubEffBits {
		return
	}
	p.publishLocked()
}

// reserveSourceFromOrigin maps an intent's IntentMeta.Origin ("app"|"cloud"|
// "cli") to the reserve source vocabulary ("app"|"cloud"|"lexactl"). An empty
// origin defaults to "app" — a locally-submitted intent that omitted its origin
// stamp is overwhelmingly the local API (which stamps origin:"app").
func reserveSourceFromOrigin(origin string) string {
	switch origin {
	case "cli":
		return "lexactl"
	case "":
		return "app"
	default:
		return origin
	}
}
