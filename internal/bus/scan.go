package bus

// Commissioning-scan and pending-station message types (TASK-082,
// docs/DEVICE_ROADMAP.md §1.2). Same house rules as intent.go: Envelope
// embedded by value, *float64 for optional quantities, Finite() in
// finite.go for the one type here that carries one (ScanResult's
// NameplateW, via ScanHit).

// ScanRequest — lexa/scan/request. Honored by lexa-modbus ONLY when
// uncommissioned (see §5.2); otherwise answered with a refused ScanStatus.
type ScanRequest struct {
	Envelope
	ID      string  `json:"id"`
	TCPCidr string  `json:"tcp_cidr,omitempty"` // e.g. "192.168.1.0/24"; empty = local /24
	TCPPort int     `json:"tcp_port,omitempty"` // default 502
	RTUDev  string  `json:"rtu_dev,omitempty"`  // e.g. "/dev/ttyUSB0"; empty = skip RTU
	Bauds   []int   `json:"bauds,omitempty"`    // default {9600, 19200}
	UnitIDs []uint8 `json:"unit_ids,omitempty"` // default 1..247 (RTU), {1,2,3,126} (TCP)
	Ts      int64   `json:"ts"`
}

// ScanStatus — lexa/scan/status. Progress lines while a sweep runs.
type ScanStatus struct {
	Envelope
	ID     string `json:"id"`
	Phase  string `json:"phase"` // "refused" | "tcp" | "rtu" | "identify" | "done"
	Probed int    `json:"probed"`
	Found  int    `json:"found"`
	Detail string `json:"detail,omitempty"`
	Ts     int64  `json:"ts"`
}

// ScanResult — lexa/scan/result (retained until commissioning completes).
type ScanResult struct {
	Envelope
	ID      string    `json:"id"`
	Devices []ScanHit `json:"devices"`
	Ts      int64     `json:"ts"`
}

// ScanHit is one identified (or unidentified-but-responsive) Modbus device
// found during a commissioning scan.
type ScanHit struct {
	URL          string   `json:"url"` // "tcp://192.168.1.40:502" | "rtu:///dev/ttyUSB0"
	UnitID       uint8    `json:"unit_id"`
	Manufacturer string   `json:"manufacturer,omitempty"` // SunSpec model 1
	Model        string   `json:"model,omitempty"`
	Serial       string   `json:"serial,omitempty"`
	FwVersion    string   `json:"fw_version,omitempty"`
	Class        string   `json:"class"`            // "inverter"|"battery"|"meter"|"unknown-sunspec"|"unknown-modbus"
	Models       []uint16 `json:"models,omitempty"` // SunSpec model IDs present
	NameplateW   *float64 `json:"nameplate_w,omitempty"`
}

// PendingStations — lexa/ocpp/pending (retained). Unknown chargers that
// dialed the CSMS; surfaced for installer approval instead of silent adoption.
type PendingStations struct {
	Envelope
	Stations []PendingStation `json:"stations"`
	Ts       int64            `json:"ts"`
}

// PendingStation is one OCPP charger the CSMS has seen but not yet approved.
type PendingStation struct {
	StationID   string `json:"station_id"`
	Vendor      string `json:"vendor,omitempty"` // from BootNotification if seen
	ModelName   string `json:"model,omitempty"`
	FirstSeenTs int64  `json:"first_seen_ts"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
}
