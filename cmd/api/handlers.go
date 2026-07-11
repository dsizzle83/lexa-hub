package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"lexa-hub/internal/buildinfo"
	"lexa-hub/internal/utilitytime"
)

// statusResp mirrors the JSON shape served by the legacy csip-tls-test hub on
// /status, so the existing demo dashboard works unmodified.
type statusResp struct {
	Timestamp    string                `json:"timestamp"`
	ClockOffsetS int64                 `json:"clock_offset_s"`
	CSIPPrograms int                   `json:"csip_programs"`
	CSIPControl  *csipControlInfo      `json:"csip_control,omitempty"`
	Devices      map[string]deviceInfo `json:"devices"`
	Power        powerSummary          `json:"power"`
	LastPlan     planJSON              `json:"last_plan"`
	EVSEs        []evseJSON            `json:"evse_stations,omitempty"`
	// StaleSources names measurement sources the hub has detected as frozen/silent
	// (a hung meter serving a cached value, or a silent charger) — surfaced so the
	// fault is visible, not silently trusted (INV-STALE / INV-EVBLIND).
	StaleSources []string `json:"stale_sources,omitempty"`
	// PlanHeartbeat surfaces whether the retained lexa/hub/plan heartbeat is
	// still advancing (TASK-045): "never" (no PlanLog seen yet — silent by
	// design, not an alarm), "ok", or "stalled". AgeS is the seconds since
	// the last PlanLog ARRIVAL (not the plan's own timestamp — see
	// planHeartbeat's doc in heartbeat.go).
	PlanHeartbeat planHeartbeatJSON `json:"plan_heartbeat"`
	// CertStatus surfaces lexa-northbound's latest client/CA cert-expiry
	// check (TASK-072/§10.5): omitted entirely (nil) until the first
	// retained lexa/northbound/certstatus message arrives — additive field,
	// existing /status consumers that don't know about it are unaffected.
	CertStatus *certStatusJSON `json:"cert_status,omitempty"`
	// APICertFP is this lexa-api instance's own HTTPS server certificate
	// fingerprint (cmd/api/tlscert.go's ensureServerCert, DEVICE_ROADMAP.md
	// §4.1/§4.4) — lowercase hex SHA-256 of the leaf DER, the same value
	// logged at startup and advertised for TOFU comparison. Empty/omitted
	// when TLS is disabled (Config.TLS false). Additive field.
	APICertFP string `json:"api_cert_fp,omitempty"`
	// FW is the build-injected version string (internal/buildinfo.Version,
	// GAP-5) — the same value reported in mDNS TXT "fw=" and GET
	// /site.fw, so all three surfaces agree. Process-static, so it's
	// stamped directly onto the response next to APICertFP rather than
	// threaded through buildStatus's pure reduction. omitempty is
	// defensive symmetry with APICertFP; in practice this is never empty
	// (buildinfo.Version defaults to "dev", never "").
	FW string `json:"fw,omitempty"`
	// Mode is the hub's current plan-author mode (bus.ModeStatus.Mode,
	// TopicHubMode, DEVICE_ROADMAP.md §3.5/§4.3) — "optimizer" or "gateway".
	// Additive: omitted until the first ModeStatus arrives (no mode manager
	// exists in this repo yet, TASK-095+, so today this stays omitted on
	// every deployment).
	Mode string `json:"mode,omitempty"`
	// CloudLink is lexa-cloudlink's latest retained status
	// (bus.CloudlinkStatus, TopicCloudlinkStatus). Additive: nil/omitted
	// until the first message arrives (no cloudlink service exists in this
	// repo yet, TASK-085+, so today this stays omitted on every deployment).
	CloudLink *cloudLinkJSON `json:"cloud_link,omitempty"`
}

// cloudLinkJSON is /status's stable, hand-rolled shape for bus.CloudlinkStatus
// — decoupled from the wire envelope, same pattern certStatusJSON uses for
// bus.CertStatus.
type cloudLinkJSON struct {
	Connected    bool   `json:"connected"`
	Endpoint     string `json:"endpoint,omitempty"`
	SpoolBytes   int64  `json:"spool_bytes"`
	LastUplinkTs string `json:"last_uplink_ts,omitempty"` // RFC3339, omitted if never uplinked
	CertDaysLeft int    `json:"cert_days_left,omitempty"`
}

// certStatusJSON is /status's stable, hand-rolled shape for bus.CertStatus —
// decoupled from the wire envelope (same pattern planJSON/decisionJSON use
// for bus.PlanLog) so the bus schema and the HTTP contract can evolve
// independently.
type certStatusJSON struct {
	ClientNotAfter string `json:"client_not_after,omitempty"` // RFC3339, empty if unknown
	CANotAfter     string `json:"ca_not_after,omitempty"`
	ClientDaysLeft int    `json:"client_days_left"`
	CADaysLeft     int    `json:"ca_days_left"`
	DaysLeft       int    `json:"days_left"`
	ClientErr      string `json:"client_err,omitempty"`
	CAErr          string `json:"ca_err,omitempty"`
	CheckedAt      string `json:"checked_at"` // RFC3339 timestamp of the check (bus.CertStatus.Ts)
}

type planHeartbeatJSON struct {
	State string  `json:"state"`
	AgeS  float64 `json:"age_s"`
}

type deviceInfo struct {
	Role      string  `json:"role"`
	W         float64 `json:"W_W"`
	V         float64 `json:"V_V,omitempty"`
	Hz        float64 `json:"Hz_Hz,omitempty"`
	SOC       float64 `json:"soc_pct,omitempty"`
	MaxW      float64 `json:"max_W,omitempty"`
	Connected bool    `json:"connected"`
}

type powerSummary struct {
	SolarW   float64 `json:"solar_W"`
	BatteryW float64 `json:"battery_W"`
	GridW    float64 `json:"grid_W"`
	LoadW    float64 `json:"load_W"`
}

type csipControlInfo struct {
	Source     string      `json:"source"`
	MRID       string      `json:"mrid,omitempty"`
	ValidUntil int64       `json:"valid_until,omitempty"`
	Base       derBaseJSON `json:"base"`
}

type derBaseJSON struct {
	ExpLimW *int64 `json:"exp_lim_W,omitempty"`
	MaxLimW *int64 `json:"max_lim_W,omitempty"`
	ImpLimW *int64 `json:"imp_lim_W,omitempty"`
	FixedW  *int64 `json:"fixed_W,omitempty"`
	Connect *bool  `json:"connect,omitempty"`
}

type planJSON struct {
	Timestamp string         `json:"timestamp"`
	Decisions []decisionJSON `json:"decisions"`
}

type decisionJSON struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
	Impact string `json:"impact"`
}

type evseJSON struct {
	StationID     string   `json:"station_id"`
	ConnectorID   int      `json:"connector_id"`
	Connected     bool     `json:"connected"`
	SessionActive bool     `json:"session_active"`
	Status        string   `json:"status"`
	CurrentA      float64  `json:"current_A"`
	MaxCurrentA   float64  `json:"max_current_A"`
	VoltageV      float64  `json:"voltage_V"`
	PowerW        float64  `json:"power_W"`
	SOC           *float64 `json:"soc_pct,omitempty"`
	EnergyWh      float64  `json:"energy_Wh,omitempty"`
	// Stale is true when an active session's telemetry went silent (MeterValues
	// stopped) — the reported power is a frozen last value, not live (INV-EVBLIND).
	Stale bool `json:"stale,omitempty"`
}

// csipReportGraceS is how many seconds past a control's ValidUntil (in server
// time) /status keeps reporting it. Covers the orchestrator's own
// expiry-confirm debounce plus a clock-jitter margin, so the API never reports
// a control as active meaningfully after the hub stopped enforcing it. Applied
// via utilitytime.ReportGrace (AD-004, TASK-036) — see buildStatus below.
const csipReportGraceS = 15

// buildStatus reduces the aggregated snapshot into the dashboard-facing
// JSON. hb is the plan heartbeat's current state (TASK-045), evaluated by
// the caller (statusHandler) so this function stays a pure reduction, as it
// was before this field existed.
func buildStatus(snap snapshot, hb heartbeatStatus) statusResp {
	resp := statusResp{
		Timestamp:    snap.now.UTC().Format(time.RFC3339),
		ClockOffsetS: snap.clockOffsetS,
		CSIPPrograms: snap.csipPrograms,
		Devices:      make(map[string]deviceInfo, len(snap.devices)),
		LastPlan: planJSON{
			Timestamp: snap.now.UTC().Format(time.RFC3339),
			Decisions: []decisionJSON{},
		},
		PlanHeartbeat: planHeartbeatJSON{State: string(hb.State), AgeS: hb.AgeS},
	}

	if cs := snap.certStatus; cs != nil {
		csj := &certStatusJSON{
			ClientDaysLeft: cs.ClientDaysLeft,
			CADaysLeft:     cs.CADaysLeft,
			DaysLeft:       cs.DaysLeft,
			ClientErr:      cs.ClientErr,
			CAErr:          cs.CAErr,
			CheckedAt:      time.Unix(cs.Ts, 0).UTC().Format(time.RFC3339),
		}
		if cs.ClientNotAfter != 0 {
			csj.ClientNotAfter = time.Unix(cs.ClientNotAfter, 0).UTC().Format(time.RFC3339)
		}
		if cs.CANotAfter != 0 {
			csj.CANotAfter = time.Unix(cs.CANotAfter, 0).UTC().Format(time.RFC3339)
		}
		resp.CertStatus = csj
	}

	if ms := snap.modeStatus; ms != nil {
		resp.Mode = ms.Mode
	}

	if cl := snap.cloudLinkStatus; cl != nil {
		clj := &cloudLinkJSON{
			Connected:    cl.Connected,
			Endpoint:     cl.Endpoint,
			SpoolBytes:   cl.SpoolBytes,
			CertDaysLeft: cl.CertDaysLeft,
		}
		if cl.LastUplinkTs != 0 {
			clj.LastUplinkTs = time.Unix(cl.LastUplinkTs, 0).UTC().Format(time.RFC3339)
		}
		resp.CloudLink = clj
	}

	// Relay the hub's actual plan trace (TopicHubPlan). The timestamp is the
	// PLAN's evaluation time, not now — a frozen timestamp here is the wedge
	// signal the QA gaps doc asked for. Until 2026-07-03 this field was a
	// hardcoded empty stub, so every harness diagnosis that inspected hub
	// decisions read "plan log empty" regardless of what the optimizer did.
	if snap.lastPlan != nil {
		resp.LastPlan.Timestamp = time.Unix(snap.lastPlan.Ts, 0).UTC().Format(time.RFC3339)
		for _, dec := range snap.lastPlan.Decisions {
			resp.LastPlan.Decisions = append(resp.LastPlan.Decisions, decisionJSON{
				Rule: dec.Rule, Reason: dec.Reason, Impact: dec.Impact,
			})
		}
	}

	// Apply the same local-expiry discipline lexa-hub applies (cmd/hub state.go):
	// the retained control message outlives its authority when the northbound is
	// dark (a WAN outage means nobody republishes or clears it), and /status is
	// the operator's — and the QA harness's — view of what the hub is enforcing.
	// Reporting a control the orchestrator has already dropped makes the hub
	// look like it is acting on withdrawn authority when it is not (QA
	// 2026-07-02: wan-outage-expiry INV-EXPIRED was this artifact). The hub
	// debounces with expiryConfirmTicks before RELEASING; for pure reporting a
	// small fixed grace is enough and needs no tick cadence.
	//
	// Delegates to utilitytime.ReportGrace (AD-004, TASK-036): identical
	// semantics to the removed inline comparison (ValidUntil==0 never expires;
	// otherwise reportable while serverNow < ValidUntil+GraceS).
	grace := utilitytime.ReportGrace{GraceS: csipReportGraceS}
	serverNow := utilitytime.ServerNowAt(snap.now, snap.clockOffsetS)
	expired := snap.csipControl != nil && !grace.Reportable(snap.csipControl.ValidUntil, serverNow)
	if snap.csipControl != nil && !expired {
		c := snap.csipControl
		info := &csipControlInfo{
			Source:     c.Source,
			MRID:       c.MRID,
			ValidUntil: c.ValidUntil,
			Base:       derBaseJSON{Connect: c.Connect},
		}
		if c.ExpLimW != nil {
			v := int64(*c.ExpLimW)
			info.Base.ExpLimW = &v
		}
		if c.ImpLimW != nil {
			v := int64(*c.ImpLimW)
			info.Base.ImpLimW = &v
		}
		if c.MaxLimW != nil {
			v := int64(*c.MaxLimW)
			info.Base.MaxLimW = &v
		}
		if c.FixedW != nil {
			v := int64(*c.FixedW)
			info.Base.FixedW = &v
		}
		resp.CSIPControl = info
	}

	var solarW, batteryW, gridW float64
	for name, d := range snap.devices {
		di := deviceInfo{
			Role: d.Role,
			MaxW: d.MaxW,
		}
		fresh := !d.UpdatedAt.IsZero() && snap.now.Sub(d.UpdatedAt) <= snap.staleAfter
		di.Connected = fresh
		if d.W != nil {
			di.W = *d.W
			if fresh {
				switch d.Role {
				case "inverter":
					solarW += *d.W
				case "battery":
					batteryW += *d.W
				case "meter":
					gridW += *d.W
				}
			}
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

	// Site load = solar + battery + grid_import − ev_charging  (≥ 0).
	loadW := solarW + batteryW + gridW
	var evW float64
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
			evW += *e.PowerW
		}
		if e.EnergyWh != nil {
			ej.EnergyWh = *e.EnergyWh
		}
		if e.SOC != nil {
			soc := *e.SOC
			ej.SOC = &soc
		}
		if ej.Stale {
			resp.StaleSources = append(resp.StaleSources, "evse:"+e.StationID)
		}
		resp.EVSEs = append(resp.EVSEs, ej)
	}
	loadW -= evW
	if loadW < 0 {
		loadW = 0
	}

	resp.StaleSources = append(resp.StaleSources, snap.staleMeters()...)

	resp.Power = powerSummary{
		SolarW:   solarW,
		BatteryW: batteryW,
		GridW:    gridW,
		LoadW:    loadW,
	}
	return resp
}

// statusHandler serves GET /status as JSON. hb supplies the plan heartbeat
// field (TASK-045), evaluated fresh on every request (not cached from the
// periodic ticker) so /status always reflects the live state. certFP is
// this process's TLS cert fingerprint (empty when TLS is disabled) —
// static for the process lifetime, so it's stamped directly onto the
// response rather than threaded through buildStatus's pure reduction. FW
// (buildinfo.Version, GAP-5) is stamped the same way, for the same reason.
func statusHandler(store *stateStore, hb *planHeartbeat, certFP string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		resp := buildStatus(store.snapshot(), hb.evaluate(time.Now()))
		resp.APICertFP = certFP
		resp.FW = buildinfo.Version
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("lexa-api: /status encode: %v", err)
		}
	}
}

// logsHandler serves GET /logs as text/event-stream.
//
// The lexa-api log broadcaster captures every MQTT event the API observes
// (measurements, controls, EVSE state, schedule). On subscribe we replay the
// recent backlog so the dashboard fills in immediately.
func logsHandler(lb *logBroadcaster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		ch, backlog := lb.subscribe()
		defer lb.unsubscribe(ch)
		for _, line := range backlog {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", line)
				flusher.Flush()
			}
		}
	}
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}
