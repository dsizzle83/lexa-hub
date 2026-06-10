package battery

import (
	"fmt"
	"math"

	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/southbound/sunspec"
)

// Compile-time check: Battery must satisfy BatteryMetricsReader.
var _ orchestrator.BatteryMetricsReader = (*Battery)(nil)

// ReadBatteryMetrics reads battery-specific state and returns it as
// orchestrator.BatteryMetrics. Prefers M713 (DERStorageCapacity, IEEE
// 1547-2018) for SoC, SoH, and energy capacity; falls back to M802
// (LithiumBattery) when M713 is absent.
// Fields not supported by the device are set to math.NaN().
func (b *Battery) ReadBatteryMetrics() (orchestrator.BatteryMetrics, error) {
	m := orchestrator.BatteryMetrics{
		SOC:           math.NaN(),
		SOH:           math.NaN(),
		CapacityWh:    math.NaN(),
		MaxChargeW:    math.NaN(),
		MaxDischargeW: math.NaN(),
	}

	if b.has713 {
		if err := readMetricsFrom713(b, &m); err != nil {
			return m, err
		}
	} else if b.Reader.HasModel(sunspec.ModelLithiumBattery) {
		if err := readMetricsFrom802(b, &m); err != nil {
			return m, err
		}
	} else {
		// No storage model: fall back to nameplate WMax only.
		if !math.IsNaN(b.Wmax) {
			m.MaxChargeW = b.Wmax
			m.MaxDischargeW = b.Wmax
		}
	}

	return m, nil
}

func readMetricsFrom713(b *Battery, m *orchestrator.BatteryMetrics) error {
	regs, err := b.Reader.ReadModel(sunspec.ModelDERStorageCap)
	if err != nil {
		return fmt.Errorf("battery: read M713 for metrics: %w", err)
	}

	s := sunspec.Parse713(regs)
	if !math.IsNaN(s.SoC) && s.SoC >= 0 {
		m.SOC = s.SoC
	}
	if !math.IsNaN(s.SoH) && s.SoH >= 0 {
		m.SOH = s.SoH
	}
	if !math.IsNaN(s.WHRtg) && s.WHRtg > 0 {
		m.CapacityWh = s.WHRtg
	}

	// M713 has no per-direction charge/discharge power rating — use WMax.
	if !math.IsNaN(b.Wmax) {
		m.MaxChargeW = b.Wmax
		m.MaxDischargeW = b.Wmax
	}

	return nil
}

func readMetricsFrom802(b *Battery, m *orchestrator.BatteryMetrics) error {
	regs, err := b.Reader.ReadModel(sunspec.ModelLithiumBattery)
	if err != nil {
		return fmt.Errorf("battery: read Model 802 for metrics: %w", err)
	}

	get := func(offset int) uint16 {
		if offset < len(regs) {
			return regs[offset]
		}
		return 0
	}
	sf := func(sfOffset int) int16 { return int16(get(sfOffset)) }

	if len(regs) > sunspec.M802_SoC {
		soc := sunspec.ApplyScaleUint(get(sunspec.M802_SoC), sf(sunspec.M802_SoC_SF))
		if !math.IsNaN(soc) && soc >= 0 {
			m.SOC = soc
		}
	}

	if len(regs) > sunspec.M802_SoH {
		soh := sunspec.ApplyScaleUint(get(sunspec.M802_SoH), sf(sunspec.M802_SoH_SF))
		if !math.IsNaN(soh) && soh >= 0 {
			m.SOH = soh
		}
	}

	if len(regs) > sunspec.M802_WHRtg {
		cap := sunspec.ApplyScaleUint(get(sunspec.M802_WHRtg), sf(sunspec.M802_WHRtg_SF))
		if !math.IsNaN(cap) && cap > 0 {
			m.CapacityWh = cap
		}
	}

	if len(regs) > sunspec.M802_WDisChaRteMax {
		wSF := sf(sunspec.M802_W_SF)
		cha := sunspec.ApplyScaleUint(get(sunspec.M802_WChaRteMax), wSF)
		dis := sunspec.ApplyScaleUint(get(sunspec.M802_WDisChaRteMax), wSF)
		if !math.IsNaN(cha) && cha > 0 {
			m.MaxChargeW = cha
		} else if !math.IsNaN(b.Wmax) {
			m.MaxChargeW = b.Wmax
		}
		if !math.IsNaN(dis) && dis > 0 {
			m.MaxDischargeW = dis
		} else if !math.IsNaN(b.Wmax) {
			m.MaxDischargeW = b.Wmax
		}
	} else if !math.IsNaN(b.Wmax) {
		m.MaxChargeW = b.Wmax
		m.MaxDischargeW = b.Wmax
	}

	return nil
}
