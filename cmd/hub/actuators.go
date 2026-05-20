package main

import (
	"fmt"
	"math"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/orchestrator"
)

// MQTTBatteryActuator publishes battery commands to lexa/control/battery/{device}.
type MQTTBatteryActuator struct {
	mc     mqtt.Client
	device string
}

func (a *MQTTBatteryActuator) ApplyBatteryCommand(cmd orchestrator.BatteryCommand) error {
	msg := bus.BattCommand{
		Device:  a.device,
		Connect: cmd.Connect,
		Ts:      time.Now().Unix(),
	}
	if !math.IsNaN(cmd.SetpointW) {
		msg.SetpointW = &cmd.SetpointW
	}
	if err := mqttutil.PublishJSON(a.mc, bus.CtrlBatteryTopic(a.device), msg); err != nil {
		return fmt.Errorf("battery actuator %s: %w", a.device, err)
	}
	return nil
}

// MQTTSolarActuator publishes solar curtailment commands to lexa/control/solar/{device}.
type MQTTSolarActuator struct {
	mc     mqtt.Client
	device string
}

func (a *MQTTSolarActuator) ApplySolarCommand(cmd orchestrator.SolarCommand) error {
	msg := bus.SolarCommand{
		Device: a.device,
		Ts:     time.Now().Unix(),
	}
	if !math.IsNaN(cmd.CurtailToW) {
		msg.CurtailToW = &cmd.CurtailToW
	}
	if err := mqttutil.PublishJSON(a.mc, bus.CtrlSolarTopic(a.device), msg); err != nil {
		return fmt.Errorf("solar actuator %s: %w", a.device, err)
	}
	return nil
}

// MQTTEVSEActuator publishes EVSE commands to lexa/evse/{station}/command.
type MQTTEVSEActuator struct {
	mc        mqtt.Client
	stationID string
}

func (a *MQTTEVSEActuator) ApplyEVSECommand(cmd orchestrator.EVSECommand) error {
	msg := bus.EVSECommand{
		StationID:   cmd.StationID,
		ConnectorID: cmd.ConnectorID,
		MaxCurrentA: cmd.MaxCurrentA,
		Ts:          time.Now().Unix(),
	}
	if err := mqttutil.PublishJSON(a.mc, bus.EVSECommandTopic(a.stationID), msg); err != nil {
		return fmt.Errorf("evse actuator %s: %w", a.stationID, err)
	}
	return nil
}
