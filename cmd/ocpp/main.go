// lexa-ocpp runs the OCPP 2.0.1 Central System Management System (CSMS) and
// bridges EV charger state to the MQTT bus.
//
// MQTT northbound (publishes):
//
//	lexa/evse/{station}/state   — EVSEState on connect / disconnect / MeterValues
//
// MQTT southbound (subscribes):
//
//	lexa/evse/{station}/command — EVSECommand to set charging current limit
//
// Usage:
//
//	lexa-ocpp [-config /etc/lexa/ocpp.json]
package main

import (
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/ocppserver"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/ocpp.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-ocpp: load config: %v", err)
	}

	mc, err := mqttutil.Connect(cfg.MQTTBroker, cfg.MQTTClientID)
	if err != nil {
		log.Fatalf("lexa-ocpp: %v", err)
	}
	defer mc.Disconnect(500)

	srv := ocppserver.New(ocppserver.Config{
		Port:          cfg.Port,
		CertPath:      cfg.CertPath,
		KeyPath:       cfg.KeyPath,
		BasicAuthUser: cfg.BasicAuthUser,
		BasicAuthPass: cfg.BasicAuthPass,
	})

	bridge := newMQTTBridge(mc, srv.CSMS())

	// Pre-register known station limits.
	for _, sc := range cfg.Stations {
		bridge.setStationConfig(sc.ID, sc.MaxCurrentA, sc.VoltageV)
	}

	// Subscribe to EVSE command topic from the hub (orchestrator).
	if err := mqttutil.Subscribe(mc, bus.SubEVSECommand, func(topic string, cmd bus.EVSECommand) {
		if err := bridge.applyCommand(cmd); err != nil {
			log.Printf("lexa-ocpp: apply command %s: %v", cmd.StationID, err)
		}
	}); err != nil {
		log.Printf("lexa-ocpp: subscribe evse command: %v", err)
	}

	go srv.Start()
	defer srv.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-ocpp: shutting down")
}

// f64ptr returns a pointer to v, or nil if v is NaN.
func f64ptr(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// ── MQTT ↔ OCPP bridge ───────────────────────────────────────────────────────

type connectorStatus string

const (
	statusAvailable   connectorStatus = "Available"
	statusOccupied    connectorStatus = "Occupied"
	statusFaulted     connectorStatus = "Faulted"
	statusUnavailable connectorStatus = "Unavailable"
)

type connState struct {
	connectorID int
	status      connectorStatus
}

type stationState struct {
	id          string
	connected   bool
	connectors  map[int]*connState
	currentA    float64
	maxCurrentA float64
	voltageV    float64
	soc         float64
	energyWh    float64
}

// mqttBridge wraps the OCPP CSMS and publishes EVSE state changes to MQTT.
//
// mu protects stations and all stationState fields. Callers must hold mu
// for any read or write of stations or stationState. publishAll snapshots
// the required state under mu.RLock then publishes outside the lock so
// network I/O never runs while mu is held.
type mqttBridge struct {
	mu       sync.RWMutex
	mc       mqtt.Client
	csms     ocpp2.CSMS
	stations map[string]*stationState
}

func newMQTTBridge(mc mqtt.Client, csms ocpp2.CSMS) *mqttBridge {
	b := &mqttBridge{
		mc:       mc,
		csms:     csms,
		stations: make(map[string]*stationState),
	}

	csms.SetNewChargingStationHandler(func(cs ocpp2.ChargingStationConnection) {
		b.mu.Lock()
		s := b.getOrCreateLocked(cs.ID())
		s.connected = true
		b.mu.Unlock()
		log.Printf("[ocpp] connected: %s addr=%s", cs.ID(), cs.RemoteAddr())
		b.publishAll(cs.ID())
		go b.triggerStatusNotification(cs.ID())
	})

	csms.SetChargingStationDisconnectedHandler(func(cs ocpp2.ChargingStationConnection) {
		b.mu.Lock()
		if s, ok := b.stations[cs.ID()]; ok {
			s.connected = false
		}
		b.mu.Unlock()
		log.Printf("[ocpp] disconnected: %s", cs.ID())
		b.publishAll(cs.ID())
	})

	csms.SetAvailabilityHandler(&availForwarder{bridge: b})
	csms.SetMeterHandler(&meterForwarder{bridge: b})

	return b
}

// getOrCreateLocked returns the stationState for id, creating it if absent.
// Caller must hold mu for writing.
func (b *mqttBridge) getOrCreateLocked(id string) *stationState {
	if s, ok := b.stations[id]; ok {
		return s
	}
	s := &stationState{
		id:          id,
		connectors:  make(map[int]*connState),
		maxCurrentA: 32,
		voltageV:    230,
		soc:         math.NaN(),
	}
	b.stations[id] = s
	return s
}

func (b *mqttBridge) setStationConfig(id string, maxCurrentA, voltageV float64) {
	b.mu.Lock()
	s := b.getOrCreateLocked(id)
	s.maxCurrentA = maxCurrentA
	s.voltageV = voltageV
	b.mu.Unlock()
}

func (b *mqttBridge) publishAll(stationID string) {
	// Snapshot state under the read lock so we never hold the lock
	// during network I/O.
	b.mu.RLock()
	s, ok := b.stations[stationID]
	if !ok {
		b.mu.RUnlock()
		return
	}
	now := time.Now().Unix()
	var msgs []bus.EVSEState
	if len(s.connectors) == 0 {
		msg := bus.EVSEState{
			StationID:   s.id,
			ConnectorID: 0,
			Connected:   s.connected,
			MaxCurrentA: f64ptr(s.maxCurrentA),
			VoltageV:    f64ptr(s.voltageV),
			Status:      string(statusAvailable),
			Ts:          now,
		}
		if !math.IsNaN(s.soc) {
			soc := s.soc
			msg.SOC = &soc
		}
		msgs = append(msgs, msg)
	} else {
		for _, c := range s.connectors {
			active := c.status == statusOccupied
			var powerW float64
			if active {
				powerW = s.currentA * s.voltageV
			}
			msg := bus.EVSEState{
				StationID:     s.id,
				ConnectorID:   c.connectorID,
				Connected:     s.connected,
				SessionActive: active,
				CurrentA:      f64ptr(s.currentA),
				MaxCurrentA:   f64ptr(s.maxCurrentA),
				VoltageV:      f64ptr(s.voltageV),
				PowerW:        f64ptr(powerW),
				EnergyWh:      f64ptr(s.energyWh),
				Status:        string(c.status),
				Ts:            now,
			}
			if !math.IsNaN(s.soc) {
				soc := s.soc
				msg.SOC = &soc
			}
			msgs = append(msgs, msg)
		}
	}
	b.mu.RUnlock()

	for _, msg := range msgs {
		_ = mqttutil.PublishJSON(b.mc, bus.EVSEStateTopic(stationID), msg)
	}
}

func (b *mqttBridge) applyCommand(cmd bus.EVSECommand) error {
	b.mu.RLock()
	s, ok := b.stations[cmd.StationID]
	connected := ok && s.connected
	b.mu.RUnlock()
	if !connected {
		return nil
	}
	evseID := cmd.ConnectorID
	if evseID == 0 {
		evseID = 1
	}
	limit := cmd.MaxCurrentA
	period := types.NewChargingSchedulePeriod(0, limit)
	schedule := types.NewChargingSchedule(1, types.ChargingRateUnitAmperes, period)
	profile := types.NewChargingProfile(
		1, 0,
		types.ChargingProfilePurposeTxDefaultProfile,
		types.ChargingProfileKindAbsolute,
		[]types.ChargingSchedule{*schedule},
	)
	errCh := make(chan error, 1)
	callErr := b.csms.SetChargingProfile(
		cmd.StationID,
		func(resp *smartcharging.SetChargingProfileResponse, err error) {
			errCh <- err
		},
		evseID, profile,
	)
	if callErr != nil {
		return callErr
	}
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case err := <-errCh:
		return err
	case <-t.C:
		return nil
	}
}

func (b *mqttBridge) triggerStatusNotification(stationID string) {
	time.Sleep(500 * time.Millisecond)
	_ = b.csms.TriggerMessage(
		stationID,
		func(_ *remotecontrol.TriggerMessageResponse, _ error) {},
		remotecontrol.MessageTriggerStatusNotification,
	)
}

// ── OCPP handler forwarders ───────────────────────────────────────────────────

type availForwarder struct{ bridge *mqttBridge }

func (h *availForwarder) OnHeartbeat(csID string, _ *availability.HeartbeatRequest) (*availability.HeartbeatResponse, error) {
	now := types.NewDateTime(time.Now())
	return availability.NewHeartbeatResponse(*now), nil
}

func (h *availForwarder) OnStatusNotification(csID string, req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
	status := connectorStatus(req.ConnectorStatus)
	h.bridge.mu.Lock()
	s := h.bridge.getOrCreateLocked(csID)
	s.connectors[req.ConnectorID] = &connState{connectorID: req.ConnectorID, status: status}
	h.bridge.mu.Unlock()
	log.Printf("[ocpp] StatusNotification cs=%s connector=%d status=%s", csID, req.ConnectorID, status)
	h.bridge.publishAll(csID)
	return &availability.StatusNotificationResponse{}, nil
}

type meterForwarder struct{ bridge *mqttBridge }

func (h *meterForwarder) OnMeterValues(csID string, req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
	h.bridge.mu.Lock()
	s := h.bridge.getOrCreateLocked(csID)
	for _, mv := range req.MeterValue {
		for _, sv := range mv.SampledValue {
			v := sv.Value
			switch sv.Measurand {
			case types.MeasurandCurrentImport:
				s.currentA = v
			case types.MeasurandSoC:
				s.soc = v
			case types.MeasurandEnergyActiveImportRegister:
				mult := 0
				if sv.UnitOfMeasure != nil && sv.UnitOfMeasure.Multiplier != nil {
					mult = *sv.UnitOfMeasure.Multiplier
				}
				if mult != 0 {
					v *= math.Pow10(mult)
				}
				s.energyWh = v
			case types.MeasurandVoltage:
				if v > 0 {
					s.voltageV = v
				}
			}
		}
	}
	currentA, soc, energyWh := s.currentA, s.soc, s.energyWh
	h.bridge.mu.Unlock()
	log.Printf("[ocpp] MeterValues cs=%s evse=%d current=%.1fA soc=%.1f%% energy=%.0fWh",
		csID, req.EvseID, currentA, soc, energyWh)
	h.bridge.publishAll(csID)
	return meter.NewMeterValuesResponse(), nil
}
