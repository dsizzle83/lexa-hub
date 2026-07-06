// Package publish converts discovered CSIP resource state (schedules,
// pricing, billing, flow reservations, the active DER control) into MQTT bus
// messages and publishes them retained.
//
// Extracted from cmd/northbound/main.go (TASK-068, D12/R5) as a pure move —
// no behavior change, no QoS/retain/topic drift from the original publish*
// functions.
package publish

import (
	"log"
	"math"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/schedule"
	"lexa-hub/internal/northbound/scheduler"
	model "lexa-proto/csipmodel"
)

// CountProgramsWithCurves counts the discovered programs that carry at least
// one curve-linked mode, for the per-cycle discovery-OK log line.
func CountProgramsWithCurves(programs []discovery.ProgramState) int {
	n := 0
	for _, p := range programs {
		if len(p.Curves) > 0 {
			n++
		}
	}
	return n
}

// Schedule converts a DER24hSchedule to a DERScheduleMsg and publishes it
// retained so lexa-hub always has the current 24-hour DER plan.
func Schedule(mc mqtt.Client, sched *schedule.DER24hSchedule) {
	msg := bus.DERScheduleMsg{
		Envelope:    bus.Envelope{V: bus.DERScheduleV},
		WindowStart: sched.WindowStart,
		WindowEnd:   sched.WindowEnd,
		BuildTime:   sched.BuildTime,
		ClockOffset: sched.ClockOffset,
		Ts:          time.Now().Unix(),
	}
	for _, s := range sched.Slots {
		slot := bus.DERScheduleSlot{
			Start:       s.Start,
			End:         s.End,
			Source:      s.Source,
			MRID:        s.MRID,
			Description: s.Description,
			ProgramMRID: s.ProgramMRID,
			Primacy:     s.Primacy,
			RampTms:     s.Base.RampTms,
		}
		// Scalar modes.
		if s.Base.OpModConnect != nil {
			slot.Connect = s.Base.OpModConnect
		}
		if s.Base.OpModEnergize != nil {
			slot.Energize = s.Base.OpModEnergize
		}
		if s.Base.OpModMaxLimW != nil {
			w := apW(s.Base.OpModMaxLimW)
			slot.MaxLimW = &w
		}
		if s.Base.OpModFixedW != nil {
			w := apW(s.Base.OpModFixedW)
			slot.FixedW = &w
		}
		if s.Base.OpModExpLimW != nil {
			w := apW(s.Base.OpModExpLimW)
			slot.ExpLimW = &w
		}
		if s.Base.OpModImpLimW != nil {
			w := apW(s.Base.OpModImpLimW)
			slot.ImpLimW = &w
		}
		if s.Base.OpModGenLimW != nil {
			w := apW(s.Base.OpModGenLimW)
			slot.GenLimW = &w
		}
		if s.Base.OpModLoadLimW != nil {
			w := apW(s.Base.OpModLoadLimW)
			slot.LoadLimW = &w
		}
		if s.Extended != nil {
			if s.Extended.OpModTargetW != nil {
				w := apW(s.Extended.OpModTargetW)
				slot.TargetW = &w
			}
			if s.Extended.OpModFixedVar != nil {
				v := float64(s.Extended.OpModFixedVar.Value.Value) / 100.0
				slot.FixedVarPct = &v
			}
			if s.Extended.OpModFixedPFAbsorbW != nil {
				pf := float64(s.Extended.OpModFixedPFAbsorbW.Value) / 100.0
				slot.FixedPFAbsorb = &pf
			}
			if s.Extended.OpModFixedPFInjectW != nil {
				pf := float64(s.Extended.OpModFixedPFInjectW.Value) / 100.0
				slot.FixedPFInject = &pf
			}
			if fd := s.Extended.OpModFreqDroop; fd != nil {
				slot.FreqDroop = &bus.FreqDroopMsg{
					DBuf: fd.DBuf, DF: fd.DF, DP: fd.DP,
					OpenLoopTms: fd.OpenLoopTms, TResponse: fd.TResponse,
				}
			}
			// Curve-linked modes.
			slot.VoltVar = curveSummary(s.Curves.VoltVar)
			slot.FreqWatt = curveSummary(s.Curves.FreqWatt)
			slot.WattPF = curveSummary(s.Curves.WattPF)
			slot.VoltWatt = curveSummary(s.Curves.VoltWatt)
			slot.HFRTMayTrip = curveSummary(s.Curves.HFRTMayTrip)
			slot.HFRTMustTrip = curveSummary(s.Curves.HFRTMustTrip)
			slot.HVRTMayTrip = curveSummary(s.Curves.HVRTMayTrip)
			slot.HVRTMomentaryCessation = curveSummary(s.Curves.HVRTMomentaryCessation)
			slot.HVRTMustTrip = curveSummary(s.Curves.HVRTMustTrip)
			slot.LFRTMayTrip = curveSummary(s.Curves.LFRTMayTrip)
			slot.LFRTMustTrip = curveSummary(s.Curves.LFRTMustTrip)
			slot.LVRTMayTrip = curveSummary(s.Curves.LVRTMayTrip)
			slot.LVRTMomentaryCessation = curveSummary(s.Curves.LVRTMomentaryCessation)
			slot.LVRTMustTrip = curveSummary(s.Curves.LVRTMustTrip)
		}
		msg.Slots = append(msg.Slots, slot)
	}

	// DER device status summaries.
	for _, rs := range sched.DERResources {
		sum := bus.DERStatusSummary{DERHref: rs.DER.Href}
		if rs.Capability != nil {
			sum.ModesSupported = rs.Capability.ModesSupported
		}
		if rs.Status != nil {
			if rs.Status.GenConnectStatus != nil {
				sum.GenConnectStatus = &rs.Status.GenConnectStatus.Value
			}
			if rs.Status.InverterStatus != nil {
				sum.InverterStatus = &rs.Status.InverterStatus.Value
			}
			if rs.Status.OperationalModeStatus != nil {
				sum.OperationalMode = &rs.Status.OperationalModeStatus.Value
			}
			if rs.Status.StorageModeStatus != nil {
				sum.StorageMode = &rs.Status.StorageModeStatus.Value
			}
			if rs.Status.StateOfChargeStatus != nil {
				pct := float64(rs.Status.StateOfChargeStatus.Value) / 100.0
				sum.StateOfChargePct = &pct
			}
		}
		if rs.Availability != nil && rs.Availability.EstimatedWAvail != nil {
			w := apW(rs.Availability.EstimatedWAvail)
			sum.EstimatedWAvail = &w
		}
		msg.DERStatus = append(msg.DERStatus, sum)
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicNorthboundSchedule, msg); err != nil {
		log.Printf("lexa-northbound: publish schedule: %v", err)
	}
}

// curveSummary converts a DERCurve pointer to a DERCurveSummary for the bus message.
func curveSummary(c *model.DERCurve) *bus.DERCurveSummary {
	if c == nil {
		return nil
	}
	s := &bus.DERCurveSummary{
		MRID:        c.MRID,
		Description: c.Description,
		CurveType:   c.CurveType,
		XMultiplier: c.XMultiplier,
		YMultiplier: c.YMultiplier,
	}
	for _, pt := range c.CurveData {
		s.Points = append(s.Points, bus.CurvePoint{X: pt.XValue, Y: pt.YValue})
	}
	return s
}

// Pricing converts the discovered pricing state to a PricingUpdate message
// and publishes it retained so the hub always has the latest rates.
func Pricing(mc mqtt.Client, tree *discovery.ResourceTree, serverNow int64) {
	msg := bus.PricingUpdate{Envelope: bus.Envelope{V: bus.PricingUpdateV}, Ts: time.Now().Unix()}

	for _, ts := range tree.PricingProfiles {
		pm := bus.TariffProfileMsg{
			MRID:                      ts.Profile.MRID,
			Description:               ts.Profile.Description,
			Currency:                  ts.Profile.Currency,
			PricePowerOfTenMultiplier: ts.Profile.PricePowerOfTenMultiplier,
			Primacy:                   ts.Profile.Primacy,
			RateCode:                  ts.Profile.RateCode,
		}
		for _, rcs := range ts.RateComponents {
			rcm := bus.RateComponentMsg{
				MRID:        rcs.Component.MRID,
				Description: rcs.Component.Description,
			}
			for _, tti := range rcs.ActiveTimeTariffIntervals {
				rcm.ActiveIntervals = append(rcm.ActiveIntervals, toTimeTariffMsg(tti))
			}
			// Include scheduled intervals that haven't ended yet.
			for _, tti := range rcs.TimeTariffIntervals {
				end := tti.Interval.Start + int64(tti.Interval.Duration)
				if end > serverNow {
					rcm.ScheduledIntervals = append(rcm.ScheduledIntervals, toTimeTariffMsg(tti))
				}
			}
			pm.RateComponents = append(pm.RateComponents, rcm)
		}
		msg.TariffProfiles = append(msg.TariffProfiles, pm)
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPPricing, msg); err != nil {
		log.Printf("lexa-northbound: publish pricing: %v", err)
	}
}

func toTimeTariffMsg(tti model.TimeTariffInterval) bus.TimeTariffMsg {
	m := bus.TimeTariffMsg{
		MRID:          tti.MRID,
		Description:   tti.Description,
		TouTier:       tti.TouTier,
		IntervalStart: tti.Interval.Start,
		Duration:      tti.Interval.Duration,
	}
	return m
}

// Billing converts the discovered billing state to a BillingUpdate message
// and publishes it retained.
func Billing(mc mqtt.Client, tree *discovery.ResourceTree) {
	msg := bus.BillingUpdate{Envelope: bus.Envelope{V: bus.BillingUpdateV}, Ts: time.Now().Unix()}

	for _, cas := range tree.BillingAccounts {
		cam := bus.CustomerAccountMsg{
			MRID:         cas.Account.MRID,
			CustomerName: cas.Account.CustomerName,
			Currency:     cas.Account.Currency,
		}
		for _, ags := range cas.Agreements {
			agm := bus.CustomerAgreementMsg{
				MRID:            ags.Agreement.MRID,
				Description:     ags.Agreement.Description,
				ServiceLocation: ags.Agreement.ServiceLocation,
			}
			for _, bp := range ags.BillingPeriods {
				agm.BillingPeriods = append(agm.BillingPeriods, bus.BillingPeriodMsg{
					IntervalStart:  bp.Interval.Start,
					Duration:       bp.Interval.Duration,
					BillLastPeriod: bp.BillLastPeriod,
					BillToDate:     bp.BillToDate,
				})
			}
			cam.Agreements = append(cam.Agreements, agm)
		}
		msg.CustomerAccounts = append(msg.CustomerAccounts, cam)
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPBilling, msg); err != nil {
		log.Printf("lexa-northbound: publish billing: %v", err)
	}
}

// FlowReservations converts discovered FlowReservationResponses to a
// FlowReservationStatusMsg and publishes it retained.
func FlowReservations(mc mqtt.Client, tree *discovery.ResourceTree) {
	msg := bus.FlowReservationStatusMsg{Envelope: bus.Envelope{V: bus.FlowReservationStatusV}, Ts: time.Now().Unix()}

	if tree.FlowReservations != nil {
		for _, frr := range tree.FlowReservations.FlowReservationResponse {
			status := uint8(0)
			if frr.EventStatus != nil {
				status = frr.EventStatus.CurrentStatus
			}
			rm := bus.ReservationMsg{
				MRID:          frr.MRID,
				Subject:       frr.Subject,
				CurrentStatus: status,
				IntervalStart: frr.Interval.Start,
				Duration:      frr.Interval.Duration,
			}
			if frr.EnergyAvailable != nil {
				v := unitValueToFloat(frr.EnergyAvailable)
				rm.EnergyAvailWh = &v
			}
			if frr.PowerAvailable != nil {
				v := unitValueToFloat(frr.PowerAvailable)
				rm.PowerAvailW = &v
			}
			msg.Reservations = append(msg.Reservations, rm)
		}
	}

	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPFRStatus, msg); err != nil {
		log.Printf("lexa-northbound: publish flow reservation status: %v", err)
	}
}

func unitValueToFloat(uv *model.UnitValue) float64 {
	if uv == nil {
		return 0
	}
	return float64(uv.Value) * math.Pow10(int(uv.Multiplier))
}

// ToActiveControl converts a scheduler.ActiveControl to the MQTT bus message.
func ToActiveControl(ac *scheduler.ActiveControl, clockOffset int64) bus.ActiveControl {
	msg := bus.ActiveControl{
		Envelope:    bus.Envelope{V: bus.ActiveControlV},
		Source:      "none",
		ClockOffset: clockOffset,
		Ts:          time.Now().Unix(),
	}
	if ac == nil {
		return msg
	}
	msg.Source = ac.Source
	msg.MRID = ac.MRID
	msg.ValidUntil = ac.ValidUntil
	msg.Connect = ac.Base.OpModConnect
	if ac.Base.OpModExpLimW != nil {
		w := apW(ac.Base.OpModExpLimW)
		msg.ExpLimW = &w
	}
	if ac.Base.OpModImpLimW != nil {
		w := apW(ac.Base.OpModImpLimW)
		msg.ImpLimW = &w
	}
	if ac.Base.OpModMaxLimW != nil {
		w := apW(ac.Base.OpModMaxLimW)
		msg.MaxLimW = &w
	}
	if ac.Base.OpModFixedW != nil {
		w := apW(ac.Base.OpModFixedW)
		msg.FixedW = &w
	}
	return msg
}

func apW(ap *model.ActivePower) float64 {
	return float64(ap.Value) * math.Pow10(int(ap.Multiplier))
}
