package bus

// Measurement is published by the modbus service for each device poll.
// Pointer fields are omitted when the device does not report that quantity.
type Measurement struct {
	Device string   `json:"device"`
	W      *float64 `json:"w,omitempty"`  // net power (W): + discharge/gen, - charge/load
	V      *float64 `json:"v,omitempty"`  // voltage (V)
	Hz     *float64 `json:"hz,omitempty"` // frequency (Hz)
	Ts     int64    `json:"ts"`           // Unix seconds
}

// BattMetrics is published by the modbus service for battery-role devices after
// each successful SunSpec battery metrics read.
type BattMetrics struct {
	Device        string   `json:"device"`
	SOC           *float64 `json:"soc_pct,omitempty"`
	SOH           *float64 `json:"soh_pct,omitempty"`
	CapacityWh    *float64 `json:"capacity_wh,omitempty"`
	MaxChargeW    float64  `json:"max_charge_w"`
	MaxDischargeW float64  `json:"max_discharge_w"`
	Ts            int64    `json:"ts"`
}

// ActiveControl is published by the csip service after every discovery walk.
// Watt values already have the IEEE 2030.5 ActivePower multiplier applied.
// Source is "event", "default", or "none" (no programs / no active control).
type ActiveControl struct {
	Source      string   `json:"source"`
	MRID        string   `json:"mrid,omitempty"`
	Connect     *bool    `json:"connect,omitempty"`
	ExpLimW     *float64 `json:"exp_lim_w,omitempty"` // export limit (W)
	ImpLimW     *float64 `json:"imp_lim_w,omitempty"` // import limit (W)
	MaxLimW     *float64 `json:"max_lim_w,omitempty"` // generation cap (W)
	FixedW      *float64 `json:"fixed_w,omitempty"`   // fixed dispatch (W)
	ClockOffset int64    `json:"clock_offset"`         // server_time − local_time (s)
	ValidUntil  int64    `json:"valid_until,omitempty"` // Unix seconds; 0 = no expiry
	Ts          int64    `json:"ts"`
}

// BattCommand is published by the hub (orchestrator) to the modbus service.
// Nil SetpointW means "leave unchanged".
type BattCommand struct {
	Device    string   `json:"device"`
	SetpointW *float64 `json:"setpoint_w,omitempty"` // + discharge, − charge (W)
	Connect   *bool    `json:"connect,omitempty"`
	Ts        int64    `json:"ts"`
}

// SolarCommand is published by the hub to the modbus service.
// Nil CurtailToW means "restore to full nameplate output".
type SolarCommand struct {
	Device     string   `json:"device"`
	CurtailToW *float64 `json:"curtail_to_w,omitempty"` // nil = uncurtailed
	Ts         int64    `json:"ts"`
}

// EVSEState is published by the ocpp service whenever connector state changes.
type EVSEState struct {
	StationID     string   `json:"station_id"`
	ConnectorID   int      `json:"connector_id"`
	Connected     bool     `json:"connected"`
	SessionActive bool     `json:"session_active"`
	CurrentA      float64  `json:"current_a"`
	MaxCurrentA   float64  `json:"max_current_a"`
	VoltageV      float64  `json:"voltage_v"`
	PowerW        float64  `json:"power_w"`
	SOC           *float64 `json:"soc_pct,omitempty"`
	EnergyWh      float64  `json:"energy_wh"`
	Status        string   `json:"status"`
	Ts            int64    `json:"ts"`
}

// EVSECommand is published by the hub to the ocpp service.
// MaxCurrentA == 0 means suspend the charging session.
type EVSECommand struct {
	StationID   string  `json:"station_id"`
	ConnectorID int     `json:"connector_id"`
	MaxCurrentA float64 `json:"max_current_a"`
	Ts          int64   `json:"ts"`
}
