package discovery

import (
	"context"

	model "lexa-proto/csipmodel"
)

// ───────────────────────────────────────────────────────────────────────
// Pricing function set discovery state types
// ───────────────────────────────────────────────────────────────────────

// TariffState groups a discovered TariffProfile with its resolved
// RateComponents and their TimeTariffIntervals. This is the pricing
// analogue of ProgramState.
type TariffState struct {
	Profile        model.TariffProfile
	RateComponents []RateComponentState
}

// RateComponentState groups a RateComponent with its discovered intervals.
// ActiveTimeTariffIntervals contains only currently-active intervals (from
// the server's ActiveTimeTariffIntervalListLink); TimeTariffIntervals
// contains the full upcoming schedule.
type RateComponentState struct {
	Component                 model.RateComponent
	ActiveTimeTariffIntervals []model.TimeTariffInterval
	TimeTariffIntervals       []model.TimeTariffInterval
}

// ───────────────────────────────────────────────────────────────────────
// Pricing fetch helpers
// ───────────────────────────────────────────────────────────────────────

func (w *Walker) fetchTariffProfileList(ctx context.Context, path string) (*model.TariffProfileList, error) {
	var r model.TariffProfileList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchRateComponentList(ctx context.Context, path string) (*model.RateComponentList, error) {
	var r model.RateComponentList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchTimeTariffIntervalList(ctx context.Context, path string) (*model.TimeTariffIntervalList, error) {
	var r model.TimeTariffIntervalList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchConsumptionTariffIntervalList(ctx context.Context, path string) (*model.ConsumptionTariffIntervalList, error) {
	var r model.ConsumptionTariffIntervalList
	return &r, w.fetchAndParse(ctx, path, &r)
}

// discoverPricingFromFSA walks the TariffProfileListLink from a single FSA
// and builds TariffState entries for each discovered TariffProfile.
//
// Failures at any step are logged but do not abort discovery — pricing is an
// optional function set and its absence must not prevent DER control.
func (w *Walker) discoverPricingFromFSA(ctx context.Context, fsa model.FunctionSetAssignments) []TariffState {
	if fsa.TariffProfileListLink == nil {
		return nil
	}

	tpl, err := w.fetchTariffProfileList(ctx, fsa.TariffProfileListLink.Href)
	if err != nil {
		w.logf("pricing: fetch TariffProfileList %s: %v", fsa.TariffProfileListLink.Href, err)
		return nil
	}

	states := make([]TariffState, 0, len(tpl.TariffProfile))
	for _, tp := range tpl.TariffProfile {
		ts := TariffState{Profile: tp}
		if tp.RateComponentListLink == nil {
			states = append(states, ts)
			continue
		}

		rcl, err := w.fetchRateComponentList(ctx, tp.RateComponentListLink.Href)
		if err != nil {
			w.logf("pricing: fetch RateComponentList %s: %v", tp.RateComponentListLink.Href, err)
			states = append(states, ts)
			continue
		}

		for _, rc := range rcl.RateComponent {
			rcs := RateComponentState{Component: rc}

			// Active intervals — what the device is acting on right now.
			if rc.ActiveTimeTariffIntervalListLink != nil {
				attil, err := w.fetchTimeTariffIntervalList(ctx, rc.ActiveTimeTariffIntervalListLink.Href)
				if err != nil {
					w.logf("pricing: fetch ActiveTimeTariffIntervalList %s: %v",
						rc.ActiveTimeTariffIntervalListLink.Href, err)
				} else {
					rcs.ActiveTimeTariffIntervals = attil.TimeTariffInterval
				}
			}

			// Full schedule — needed for look-ahead price-responsive dispatch.
			if rc.TimeTariffIntervalListLink != nil {
				ttil, err := w.fetchTimeTariffIntervalList(ctx, rc.TimeTariffIntervalListLink.Href)
				if err != nil {
					w.logf("pricing: fetch TimeTariffIntervalList %s: %v",
						rc.TimeTariffIntervalListLink.Href, err)
				} else {
					rcs.TimeTariffIntervals = ttil.TimeTariffInterval
				}
			}

			ts.RateComponents = append(ts.RateComponents, rcs)
		}

		states = append(states, ts)
	}

	return states
}
