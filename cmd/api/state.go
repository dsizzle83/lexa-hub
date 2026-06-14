package main

import (
	"sort"
	"sync"
	"time"

	"lexa-hub/internal/bus"
)

// deviceSnap is the per-device state aggregated from MQTT.
type deviceSnap struct {
	Name      string
	Role      string  // from config: inverter | battery | meter
	MaxW      float64 // from config
	W         *float64
	V         *float64
	Hz        *float64
	SOC       *float64 // batteries only
	UpdatedAt time.Time
}

// evseSnap is the per-EVSE state aggregated from MQTT.
type evseSnap struct {
	State     bus.EVSEState
	UpdatedAt time.Time
}

// stateStore is the thread-safe aggregator that backs /status.
//
// It subscribes to MQTT and keeps the latest snapshot per topic, plus enough
// derived fields (CSIP control, DER schedule program count, clock offset) to
// reproduce the JSON shape the demo dashboard expects.
type stateStore struct {
	mu sync.RWMutex

	devices map[string]*deviceSnap // keyed by device name
	evses   map[string]*evseSnap   // keyed by "stationID:connectorID"

	csipControl  *bus.ActiveControl
	csipPrograms int
	clockOffsetS int64

	staleAfter time.Duration
}

func newStateStore(devices []DeviceConfig, staleAfter time.Duration) *stateStore {
	s := &stateStore{
		devices:    make(map[string]*deviceSnap),
		evses:      make(map[string]*evseSnap),
		staleAfter: staleAfter,
	}
	for _, d := range devices {
		s.devices[d.Name] = &deviceSnap{Name: d.Name, Role: d.Role, MaxW: d.MaxW}
	}
	return s
}

// device returns (and lazily creates) the snapshot for a named device.
// Caller must hold mu for writing.
func (s *stateStore) deviceLocked(name string) *deviceSnap {
	d, ok := s.devices[name]
	if !ok {
		d = &deviceSnap{Name: name, Role: "meter"} // unknown → meter
		s.devices[name] = d
	}
	return d
}

func (s *stateStore) onMeasurement(_ string, m bus.Measurement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deviceLocked(m.Device)
	if m.W != nil {
		v := *m.W
		d.W = &v
	}
	if m.V != nil {
		v := *m.V
		d.V = &v
	}
	if m.Hz != nil {
		v := *m.Hz
		d.Hz = &v
	}
	d.UpdatedAt = time.Now()
}

func (s *stateStore) onBattMetrics(_ string, m bus.BattMetrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deviceLocked(m.Device)
	if d.Role == "" {
		d.Role = "battery"
	}
	if m.SOC != nil {
		v := *m.SOC
		d.SOC = &v
	}
	d.UpdatedAt = time.Now()
}

func (s *stateStore) onCSIPControl(_ string, c bus.ActiveControl) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Source == "" || c.Source == "none" {
		s.csipControl = nil
	} else {
		cc := c
		s.csipControl = &cc
	}
	s.clockOffsetS = c.ClockOffset
}

func (s *stateStore) onEVSEState(_ string, e bus.EVSEState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := evseKey(e.StationID, e.ConnectorID)
	s.evses[key] = &evseSnap{State: e, UpdatedAt: time.Now()}
	// The ocpp service publishes a synthetic connector-0 entry while a station
	// has no connectors yet (pre-StatusNotification). Once a real connector
	// reports, drop the placeholder so /status doesn't carry a phantom idle
	// EVSE forever (the dashboard was rendering it as "EV idle, 0 W").
	if e.ConnectorID > 0 {
		delete(s.evses, evseKey(e.StationID, 0))
	}
}

func (s *stateStore) onSchedule(_ string, sched bus.DERScheduleMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progs := map[string]struct{}{}
	for _, slot := range sched.Slots {
		if slot.ProgramMRID != "" {
			progs[slot.ProgramMRID] = struct{}{}
		}
	}
	s.csipPrograms = len(progs)
	if sched.ClockOffset != 0 {
		s.clockOffsetS = sched.ClockOffset
	}
}

// snapshot returns a deep-copy view safe to render without holding the lock.
type snapshot struct {
	devices      map[string]deviceSnap
	evses        []bus.EVSEState
	csipControl  *bus.ActiveControl
	csipPrograms int
	clockOffsetS int64
	staleAfter   time.Duration
	now          time.Time
}

func (s *stateStore) snapshot() snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := snapshot{
		devices:      make(map[string]deviceSnap, len(s.devices)),
		csipPrograms: s.csipPrograms,
		clockOffsetS: s.clockOffsetS,
		staleAfter:   s.staleAfter,
		now:          time.Now(),
	}
	for name, d := range s.devices {
		out.devices[name] = *d
	}
	if s.csipControl != nil {
		cc := *s.csipControl
		out.csipControl = &cc
	}
	// Stable order: sort by stationID, then connector.
	keys := make([]string, 0, len(s.evses))
	for k := range s.evses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.evses = append(out.evses, s.evses[k].State)
	}
	return out
}

func evseKey(stationID string, connectorID int) string {
	return stationID + ":" + itoa(connectorID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
