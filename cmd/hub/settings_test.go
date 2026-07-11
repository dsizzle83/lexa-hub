package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// fakeReserveReader is a settable stand-in for the engine's EffectiveReservePct
// accessor (the reserveReader interface settings.go depends on), so these tests
// need no running orchestrator.Engine.
type fakeReserveReader struct {
	mu  sync.Mutex
	pct float64
}

func (f *fakeReserveReader) EffectiveReservePct() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pct
}

func (f *fakeReserveReader) set(v float64) {
	f.mu.Lock()
	f.pct = v
	f.mu.Unlock()
}

// lastSettingsPublish returns the most recent lexa/hub/settings publish, decoded.
func lastSettingsPublish(t *testing.T, mc *fakeHubMQTTClient) (bus.HubSettings, fakeHubPublish) {
	t.Helper()
	for i := len(mc.publishes) - 1; i >= 0; i-- {
		if mc.publishes[i].topic == bus.TopicHubSettings {
			var hs bus.HubSettings
			if err := json.Unmarshal(mc.publishes[i].payload, &hs); err != nil {
				t.Fatalf("unmarshal HubSettings: %v", err)
			}
			return hs, mc.publishes[i]
		}
	}
	t.Fatal("no lexa/hub/settings message published")
	return bus.HubSettings{}, fakeHubPublish{}
}

func countSettingsPublishes(mc *fakeHubMQTTClient) int {
	n := 0
	for _, p := range mc.publishes {
		if p.topic == bus.TopicHubSettings {
			n++
		}
	}
	return n
}

func TestSettingsPublisher_SeedShape(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	sp := newSettingsPublisher(mc, &fakeReserveReader{pct: -1}, 20) // -1 ⇒ no plan yet
	fixed := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sp.now = func() time.Time { return fixed }

	sp.publish() // startup seed

	hs, pub := lastSettingsPublish(t, mc)
	if !pub.retained {
		t.Error("settings must be published RETAINED (lexa-api re-seed)")
	}
	if hs.V != bus.HubSettingsV {
		t.Errorf("envelope V = %d, want %d", hs.V, bus.HubSettingsV)
	}
	if hs.Reserve.EffectivePct != nil {
		t.Errorf("effective_pct = %v, want nil before the first plan (-1 sentinel)", *hs.Reserve.EffectivePct)
	}
	if hs.Reserve.FloorPct == nil || *hs.Reserve.FloorPct != 20 {
		t.Errorf("floor_pct = %v, want 20", hs.Reserve.FloorPct)
	}
	if hs.Reserve.Source != "default" {
		t.Errorf("reserve.source = %q, want default before any intent", hs.Reserve.Source)
	}
	if hs.Tariff.Source != "csip" {
		t.Errorf("tariff.source = %q, want csip before any tariff intent", hs.Tariff.Source)
	}
	if hs.Tariff.Spec != nil {
		t.Errorf("tariff.spec = %+v, want nil before any tariff intent", hs.Tariff.Spec)
	}
	if hs.Ts != fixed.Unix() {
		t.Errorf("ts = %d, want %d", hs.Ts, fixed.Unix())
	}
	if err := hs.Finite(); err != nil {
		t.Errorf("seed HubSettings.Finite() = %v, want nil", err)
	}
}

func TestSettingsPublisher_ReserveChangeAndFloorDefault(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	rr := &fakeReserveReader{pct: -1}
	sp := newSettingsPublisher(mc, rr, 0) // 0 ⇒ default floor 20 (matches engine)
	if sp.floorPct != 20 {
		t.Fatalf("floorPct = %v, want 20 defaulted from non-positive cfg", sp.floorPct)
	}

	// A plan has now resolved the effective floor to 35 (an app intent raised it).
	rr.set(35)
	sp.onReserveChange("app")

	hs, _ := lastSettingsPublish(t, mc)
	if hs.Reserve.Source != "app" {
		t.Errorf("reserve.source = %q, want app", hs.Reserve.Source)
	}
	if hs.Reserve.EffectivePct == nil || *hs.Reserve.EffectivePct != 35 {
		t.Errorf("effective_pct = %v, want 35", hs.Reserve.EffectivePct)
	}
	if hs.Reserve.FloorPct == nil || *hs.Reserve.FloorPct != 20 {
		t.Errorf("floor_pct = %v, want 20", hs.Reserve.FloorPct)
	}
}

// reserveSourceFromOrigin maps intent origin → source vocabulary.
func TestReserveSourceFromOrigin(t *testing.T) {
	cases := map[string]string{"app": "app", "cloud": "cloud", "cli": "lexactl", "": "app"}
	for origin, want := range cases {
		if got := reserveSourceFromOrigin(origin); got != want {
			t.Errorf("reserveSourceFromOrigin(%q) = %q, want %q", origin, got, want)
		}
	}
}

func TestSettingsPublisher_TariffChange(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	sp := newSettingsPublisher(mc, &fakeReserveReader{pct: 20}, 20)
	fixed := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	sp.now = func() time.Time { return fixed }

	spec := bus.TariffSpec{
		Currency: "USD",
		Periods: []bus.TariffPeriod{
			{Label: "peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38},
			{Label: "off-peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 0, EndHH: 16, ImportPerKwh: 0.12},
		},
	}
	sp.onTariffChange(spec)

	hs, _ := lastSettingsPublish(t, mc)
	if hs.Tariff.Source != "manual" {
		t.Errorf("tariff.source = %q, want manual", hs.Tariff.Source)
	}
	if hs.Tariff.UpdatedAt != fixed.Unix() {
		t.Errorf("tariff.updated_at = %d, want %d", hs.Tariff.UpdatedAt, fixed.Unix())
	}
	if hs.Tariff.Spec == nil {
		t.Fatal("tariff.spec is nil, want the submitted spec echoed")
	}
	if hs.Tariff.Spec.Currency != "USD" || len(hs.Tariff.Spec.Periods) != 2 {
		t.Errorf("tariff.spec = %+v, want the 2-period USD spec verbatim", hs.Tariff.Spec)
	}
	if hs.Tariff.Spec.Periods[0].Label != "peak" || hs.Tariff.Spec.Periods[0].ImportPerKwh != 0.38 {
		t.Errorf("period[0] = %+v, want peak/0.38", hs.Tariff.Spec.Periods[0])
	}
}

// TestSettingsPublisher_TariffChange_RoundTripsDeliveryAndFixed proves the new
// PR-C fields (per-period delivery_per_kwh + top-level fixed_daily_charge) round
// -trip through the published lexa/hub/settings doc for free — settings.go
// stores the adopted spec as-is (a shallow struct copy) and JSON-marshals it, so
// /status.tariff echoes exactly what was submitted, including the new fields.
func TestSettingsPublisher_TariffChange_RoundTripsDeliveryAndFixed(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	sp := newSettingsPublisher(mc, &fakeReserveReader{pct: 20}, 20)

	spec := bus.TariffSpec{
		Currency:         "USD",
		FixedDailyCharge: ptr(0.42),
		Periods: []bus.TariffPeriod{
			{Label: "peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 16, EndHH: 21, ImportPerKwh: 0.38, DeliveryPerKwh: ptr(0.06)},
			{Label: "off-peak", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 0, EndHH: 16, ImportPerKwh: 0.12},
		},
	}
	sp.onTariffChange(spec)

	hs, _ := lastSettingsPublish(t, mc)
	if hs.Tariff.Spec == nil {
		t.Fatal("tariff.spec is nil, want the submitted spec echoed")
	}
	if hs.Tariff.Spec.FixedDailyCharge == nil || *hs.Tariff.Spec.FixedDailyCharge != 0.42 {
		t.Errorf("fixed_daily_charge round-trip = %v, want 0.42", hs.Tariff.Spec.FixedDailyCharge)
	}
	if hs.Tariff.Spec.Periods[0].DeliveryPerKwh == nil || *hs.Tariff.Spec.Periods[0].DeliveryPerKwh != 0.06 {
		t.Errorf("period[0].delivery_per_kwh round-trip = %v, want 0.06", hs.Tariff.Spec.Periods[0].DeliveryPerKwh)
	}
	if hs.Tariff.Spec.Periods[1].DeliveryPerKwh != nil {
		t.Errorf("period[1].delivery_per_kwh = %v, want nil (unset)", hs.Tariff.Spec.Periods[1].DeliveryPerKwh)
	}
}

// refreshFromPlan republishes ONLY when the effective pct moves.
func TestSettingsPublisher_RefreshDedupe(t *testing.T) {
	mc := &fakeHubMQTTClient{}
	rr := &fakeReserveReader{pct: -1}
	sp := newSettingsPublisher(mc, rr, 20)

	sp.refreshFromPlan() // first ever publish (nothing published yet) → emits
	if n := countSettingsPublishes(mc); n != 1 {
		t.Fatalf("after first refresh: %d publishes, want 1", n)
	}

	sp.refreshFromPlan() // still -1, unchanged → no publish
	if n := countSettingsPublishes(mc); n != 1 {
		t.Fatalf("after unchanged refresh: %d publishes, want still 1 (deduped)", n)
	}

	rr.set(40) // plan resolved a new floor
	sp.refreshFromPlan()
	if n := countSettingsPublishes(mc); n != 2 {
		t.Fatalf("after changed refresh: %d publishes, want 2", n)
	}
	hs, _ := lastSettingsPublish(t, mc)
	if hs.Reserve.EffectivePct == nil || *hs.Reserve.EffectivePct != 40 {
		t.Errorf("effective_pct = %v, want 40 after the plan moved it", hs.Reserve.EffectivePct)
	}

	sp.refreshFromPlan() // 40 unchanged → no publish
	if n := countSettingsPublishes(mc); n != 2 {
		t.Fatalf("after unchanged refresh: %d publishes, want still 2 (deduped)", n)
	}
}

// The adopter publishes a HubSettings on a reserve intent and on a tariff
// intent (through the full adopt funnel, alongside the IntentResult).
func TestIntentAdopter_PublishesSettingsOnChange(t *testing.T) {
	f := newTestAdopter(t, nil)
	rr := &fakeReserveReader{pct: 25}
	f.adopter.settings = newSettingsPublisher(f.mc, rr, 20)

	// reserve intent (origin cloud) through adopt → one HubSettings, source cloud.
	f.adopter.adopt("reserve", bus.IntentMeta{ID: "r1", Origin: "cloud"}, func() (string, string) {
		return f.adopter.applyReserve(bus.BackupReserveIntent{
			IntentMeta: bus.IntentMeta{ID: "r1", Origin: "cloud"},
			ReservePct: ptr(30.0),
		})
	})
	hs, _ := lastSettingsPublish(t, f.mc)
	if hs.Reserve.Source != "cloud" {
		t.Errorf("reserve.source = %q, want cloud", hs.Reserve.Source)
	}
	if hs.Reserve.EffectivePct == nil || *hs.Reserve.EffectivePct != 25 {
		t.Errorf("effective_pct = %v, want 25 (engine's current resolved floor)", hs.Reserve.EffectivePct)
	}

	// tariff intent through adopt → HubSettings with source manual + spec.
	spec := bus.TariffSpec{Currency: "USD", Periods: []bus.TariffPeriod{
		{Label: "flat", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 0, EndHH: 24, ImportPerKwh: 0.20},
	}}
	f.adopter.adopt("tariff", bus.IntentMeta{ID: "t1", Origin: "app"}, func() (string, string) {
		return f.adopter.applyTariff(bus.TariffIntent{
			IntentMeta: bus.IntentMeta{ID: "t1", Origin: "app"},
			Tariff:     spec,
		})
	})
	hs, _ = lastSettingsPublish(t, f.mc)
	if hs.Tariff.Source != "manual" {
		t.Errorf("tariff.source = %q, want manual", hs.Tariff.Source)
	}
	if hs.Tariff.Spec == nil || hs.Tariff.Spec.Periods[0].ImportPerKwh != 0.20 {
		t.Errorf("tariff.spec = %+v, want the submitted flat 0.20 spec", hs.Tariff.Spec)
	}
}

// A nil settings publisher (the Unit 3.3 tests' default) must not panic — the
// nil-guards in applyReserve/applyTariff keep the adopter usable without it.
func TestIntentAdopter_NilSettingsNoPanic(t *testing.T) {
	f := newTestAdopter(t, nil)
	if f.adopter.settings != nil {
		t.Fatal("settings should be nil by default in the test fixture")
	}
	if out, _ := f.adopter.applyReserve(bus.BackupReserveIntent{ReservePct: ptr(50.0)}); out != "applied" {
		t.Errorf("applyReserve outcome = %q, want applied with nil settings", out)
	}
	if out, _ := f.adopter.applyTariff(bus.TariffIntent{Tariff: bus.TariffSpec{
		Currency: "USD",
		Periods:  []bus.TariffPeriod{{Label: "flat", Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHH: 0, EndHH: 24, ImportPerKwh: 0.2}},
	}}); out != "applied" {
		t.Errorf("applyTariff outcome = %q, want applied with nil settings", out)
	}
}
