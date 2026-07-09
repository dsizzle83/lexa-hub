package main

// telemetry.go implements DEVICE_ROADMAP.md §4.3's GET /telemetry/recent
// route: a bounded in-memory ring of recent Measurement/BattMetrics/EVSEState
// samples, independent of stateStore's per-device "latest value" map (state.go),
// which only ever keeps the CURRENT reading per device/EVSE and has no memory
// of how it got there.
import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// telemetryRingCap bounds the in-memory telemetry ring (flash/RAM discipline,
// FLASH_BUDGET.md's spirit applied to a pure-RAM structure: nothing here ever
// touches disk, so the bound exists solely to cap process RSS). 4096 entries
// is sized against this bench's device count and poll cadence: a handful of
// inverter/battery/meter devices at roughly 1 Hz measurements + a slower
// battery-metrics cadence, plus occasional EVSE state changes, keeps a full
// 15-minute window (telemetryMaxMinutes) well under this cap even on a
// busier site — the cap is a backstop against a runaway publish rate, not
// the normal operating limit. Oldest entries are evicted first once full,
// exactly like logBroadcaster's ring (logs.go) applied to structs instead of
// strings.
const telemetryRingCap = 4096

// telemetryMaxMinutes caps GET /telemetry/recent's minutes query parameter.
// The ring is not a time-series database — serving an unbounded window would
// silently degrade into "however far back the ring happens to still reach,"
// a moving target as traffic volume changes — so the route's contract is
// capped at the same 15-minute window its default already promises.
const telemetryMaxMinutes = 15

// telemetryKind names which bus type a telemetrySample was captured from.
type telemetryKind string

const (
	telemetryKindMeasurement telemetryKind = "measurement"
	telemetryKindBattMetrics telemetryKind = "batt_metrics"
	telemetryKindEVSE        telemetryKind = "evse"
)

// telemetrySample is one ring entry: a flattened, decoupled projection of
// Measurement/BattMetrics/EVSEState (mirrors certStatusJSON's decoupling of
// the HTTP shape from the bus wire schema, handlers.go), stamped with
// ARRIVAL time — not the message's own Ts — so a publisher's clock skew or a
// warped bench clock has no bearing on which window /telemetry/recent
// considers "recent" (same discipline as planHeartbeat and deviceSnap's
// WChangedAt elsewhere in this package). Only the fields relevant to Kind
// are populated; the rest stay nil/zero.
type telemetrySample struct {
	Kind      telemetryKind
	Device    string // device name, or evseKey(station, connector) for EVSE
	ArrivedAt time.Time

	W             *float64
	VoltageV      *float64
	Hz            *float64
	SOC           *float64
	SOH           *float64
	CapacityWh    *float64
	MaxChargeW    *float64
	MaxDischargeW *float64
	CurrentA      *float64
	MaxCurrentA   *float64
	PowerW        *float64
	EnergyWh      *float64
	Status        string
}

// telemetryRing is a fixed-capacity circular buffer of telemetrySample,
// oldest-first eviction once full. Safe for concurrent use.
type telemetryRing struct {
	mu   sync.Mutex
	buf  []telemetrySample
	head int
	full bool
}

func newTelemetryRing(capacity int) *telemetryRing {
	if capacity <= 0 {
		capacity = telemetryRingCap
	}
	return &telemetryRing{buf: make([]telemetrySample, capacity)}
}

// add appends s, evicting the oldest entry once the ring is full.
func (r *telemetryRing) add(s telemetrySample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = s
	r.head = (r.head + 1) % len(r.buf)
	if r.head == 0 {
		r.full = true
	}
}

// since returns every sample whose ArrivedAt is at or after cutoff, oldest
// first.
func (r *telemetryRing) since(cutoff time.Time) []telemetrySample {
	r.mu.Lock()
	var ordered []telemetrySample
	if r.full {
		ordered = append(ordered, r.buf[r.head:]...)
		ordered = append(ordered, r.buf[:r.head]...)
	} else {
		ordered = append(ordered, r.buf[:r.head]...)
	}
	r.mu.Unlock()

	out := make([]telemetrySample, 0, len(ordered))
	for _, s := range ordered {
		if !s.ArrivedAt.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

// len reports the current number of entries — a test hook pinning the ring's
// eviction bound (e.g. the 4097th add must not grow the ring past capacity).
func (r *telemetryRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.buf)
	}
	return r.head
}

// telemetrySampleResp is one sample's HTTP-facing shape.
type telemetrySampleResp struct {
	ArrivedAt     string   `json:"arrived_at"` // RFC3339
	Kind          string   `json:"kind"`
	W             *float64 `json:"w,omitempty"`
	VoltageV      *float64 `json:"voltage_v,omitempty"`
	Hz            *float64 `json:"hz,omitempty"`
	SOC           *float64 `json:"soc_pct,omitempty"`
	SOH           *float64 `json:"soh_pct,omitempty"`
	CapacityWh    *float64 `json:"capacity_wh,omitempty"`
	MaxChargeW    *float64 `json:"max_charge_w,omitempty"`
	MaxDischargeW *float64 `json:"max_discharge_w,omitempty"`
	CurrentA      *float64 `json:"current_a,omitempty"`
	MaxCurrentA   *float64 `json:"max_current_a,omitempty"`
	PowerW        *float64 `json:"power_w,omitempty"`
	EnergyWh      *float64 `json:"energy_wh,omitempty"`
	Status        string   `json:"status,omitempty"`
}

// telemetryRecentResp is GET /telemetry/recent's JSON shape: the ring,
// grouped per device (or evseKey(station,connector) for EVSE samples), each
// list oldest-first.
type telemetryRecentResp struct {
	Minutes int                              `json:"minutes"`
	Devices map[string][]telemetrySampleResp `json:"devices"`
}

// telemetryRecentHandler serves GET /telemetry/recent?minutes=N (default 15,
// capped at telemetryMaxMinutes) — DEVICE_ROADMAP.md §4.3.
func telemetryRecentHandler(ring *telemetryRing) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		minutes := telemetryMaxMinutes
		if q := r.URL.Query().Get("minutes"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 {
				minutes = v
			}
		}
		if minutes > telemetryMaxMinutes {
			minutes = telemetryMaxMinutes
		}

		cutoff := time.Now().Add(-time.Duration(minutes) * time.Minute)
		samples := ring.since(cutoff)

		resp := telemetryRecentResp{Minutes: minutes, Devices: make(map[string][]telemetrySampleResp)}
		for _, s := range samples {
			resp.Devices[s.Device] = append(resp.Devices[s.Device], telemetrySampleResp{
				ArrivedAt:     s.ArrivedAt.UTC().Format(time.RFC3339),
				Kind:          string(s.Kind),
				W:             s.W,
				VoltageV:      s.VoltageV,
				Hz:            s.Hz,
				SOC:           s.SOC,
				SOH:           s.SOH,
				CapacityWh:    s.CapacityWh,
				MaxChargeW:    s.MaxChargeW,
				MaxDischargeW: s.MaxDischargeW,
				CurrentA:      s.CurrentA,
				MaxCurrentA:   s.MaxCurrentA,
				PowerW:        s.PowerW,
				EnergyWh:      s.EnergyWh,
				Status:        s.Status,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("lexa-api: /telemetry/recent encode failed", "err", err)
		}
	}
}
