package main

// devices.go implements DEVICE_ROADMAP.md §4.3's GET /devices route: the
// same live device/EVSE projection /status uses, merged with the
// commissioning-time discovery surfaces — the last Modbus scan's hits and
// any OCPP stations awaiting installer approval — so a commissioning wizard
// or dashboard has one place to see "what's configured and live" alongside
// "what's been found but not yet adopted."
import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"lexa-hub/internal/bus"
)

// devicesResp is GET /devices's JSON shape.
type devicesResp struct {
	Devices     map[string]deviceInfo `json:"devices"`
	EVSEs       []evseJSON            `json:"evse_stations,omitempty"`
	ScanResult  *scanResultResp       `json:"scan_result,omitempty"`
	OCPPPending []pendingStationResp  `json:"ocpp_pending,omitempty"`
}

// scanResultResp is the HTTP-facing projection of bus.ScanResult, decoupled
// from the wire envelope (same pattern certStatusJSON/planJSON use for their
// respective bus types, handlers.go).
type scanResultResp struct {
	ID      string        `json:"id"`
	Ts      string        `json:"ts"` // RFC3339
	Devices []scanHitResp `json:"devices"`
}

type scanHitResp struct {
	URL          string   `json:"url"`
	UnitID       uint8    `json:"unit_id"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Serial       string   `json:"serial,omitempty"`
	FwVersion    string   `json:"fw_version,omitempty"`
	Class        string   `json:"class"`
	NameplateW   *float64 `json:"nameplate_w,omitempty"`
}

// pendingStationResp is the HTTP-facing projection of bus.PendingStation.
type pendingStationResp struct {
	StationID   string `json:"station_id"`
	Vendor      string `json:"vendor,omitempty"`
	Model       string `json:"model,omitempty"`
	FirstSeenTs string `json:"first_seen_ts"` // RFC3339
	RemoteAddr  string `json:"remote_addr,omitempty"`
}

// scanResultRespFrom projects a bus.ScanResult into its HTTP shape. Shared by
// devicesHandler and scanHandler's GET path (scan.go).
func scanResultRespFrom(sr *bus.ScanResult) *scanResultResp {
	out := &scanResultResp{ID: sr.ID, Ts: time.Unix(sr.Ts, 0).UTC().Format(time.RFC3339)}
	for _, d := range sr.Devices {
		out.Devices = append(out.Devices, scanHitResp{
			URL: d.URL, UnitID: d.UnitID, Manufacturer: d.Manufacturer, Model: d.Model,
			Serial: d.Serial, FwVersion: d.FwVersion, Class: d.Class, NameplateW: d.NameplateW,
		})
	}
	return out
}

// devicesHandler serves GET /devices.
func devicesHandler(store *stateStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		resp := buildDevicesResp(store.snapshot())
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("lexa-api: /devices encode failed", "err", err)
		}
	}
}

// buildDevicesResp reduces snap into /devices's JSON shape — a pure
// reduction, mirroring buildStatus's shape (handlers.go), kept as a separate
// function since /devices's shape (commissioning-surface merge) and
// /status's shape (live power/CSIP summary) are different HTTP contracts
// over the same underlying snapshot.
func buildDevicesResp(snap snapshot) devicesResp {
	resp := devicesResp{Devices: make(map[string]deviceInfo, len(snap.devices))}

	for name, d := range snap.devices {
		fresh := !d.UpdatedAt.IsZero() && snap.now.Sub(d.UpdatedAt) <= snap.staleAfter
		di := deviceInfo{Role: d.Role, MaxW: d.MaxW, Connected: fresh}
		if d.W != nil {
			di.W = *d.W
		}
		if d.V != nil {
			di.V = *d.V
		}
		if d.Hz != nil {
			di.Hz = *d.Hz
		}
		if d.SOC != nil {
			di.SOC = *d.SOC
		}
		resp.Devices[name] = di
	}

	for _, es := range snap.evses {
		e := es.State
		ej := evseJSON{
			StationID:     e.StationID,
			ConnectorID:   e.ConnectorID,
			Connected:     e.Connected,
			SessionActive: e.SessionActive,
			Status:        e.Status,
			Stale:         es.stale(snap.now),
		}
		if e.CurrentA != nil {
			ej.CurrentA = *e.CurrentA
		}
		if e.MaxCurrentA != nil {
			ej.MaxCurrentA = *e.MaxCurrentA
		}
		if e.VoltageV != nil {
			ej.VoltageV = *e.VoltageV
		}
		if e.PowerW != nil {
			ej.PowerW = *e.PowerW
		}
		if e.EnergyWh != nil {
			ej.EnergyWh = *e.EnergyWh
		}
		if e.SOC != nil {
			soc := *e.SOC
			ej.SOC = &soc
		}
		resp.EVSEs = append(resp.EVSEs, ej)
	}

	if snap.scanResult != nil {
		resp.ScanResult = scanResultRespFrom(snap.scanResult)
	}

	if snap.ocppPending != nil {
		for _, p := range snap.ocppPending.Stations {
			resp.OCPPPending = append(resp.OCPPPending, pendingStationResp{
				StationID:   p.StationID,
				Vendor:      p.Vendor,
				Model:       p.ModelName,
				FirstSeenTs: time.Unix(p.FirstSeenTs, 0).UTC().Format(time.RFC3339),
				RemoteAddr:  p.RemoteAddr,
			})
		}
	}

	return resp
}
