package discovery

import (
	"context"

	model "lexa-proto/csipmodel"
)

// ───────────────────────────────────────────────────────────────────────
// Billing function set discovery state types
// ───────────────────────────────────────────────────────────────────────

// CustomerAccountState groups a discovered CustomerAccount with its resolved
// CustomerAgreements and billing period summaries.
type CustomerAccountState struct {
	Account    model.CustomerAccount
	Agreements []CustomerAgreementState
}

// CustomerAgreementState holds the agreement and the discovered billing
// periods, historical readings, projection readings, and target readings.
// The walker does not fetch individual BillingReadings — those are large and
// the hub only needs period-level summary data for optimization decisions.
type CustomerAgreementState struct {
	Agreement          model.CustomerAgreement
	BillingPeriods     []model.BillingPeriod
	HistoricalReadings []model.HistoricalReading
	ProjectionReadings []model.ProjectionReading
	TargetReadings     []model.TargetReading
}

// ───────────────────────────────────────────────────────────────────────
// Billing fetch helpers
// ───────────────────────────────────────────────────────────────────────

func (w *Walker) fetchCustomerAccountList(ctx context.Context, path string) (*model.CustomerAccountList, error) {
	var r model.CustomerAccountList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchCustomerAgreementList(ctx context.Context, path string) (*model.CustomerAgreementList, error) {
	var r model.CustomerAgreementList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchBillingPeriodList(ctx context.Context, path string) (*model.BillingPeriodList, error) {
	var r model.BillingPeriodList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchHistoricalReadingList(ctx context.Context, path string) (*model.HistoricalReadingList, error) {
	var r model.HistoricalReadingList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchProjectionReadingList(ctx context.Context, path string) (*model.ProjectionReadingList, error) {
	var r model.ProjectionReadingList
	return &r, w.fetchAndParse(ctx, path, &r)
}

func (w *Walker) fetchTargetReadingList(ctx context.Context, path string) (*model.TargetReadingList, error) {
	var r model.TargetReadingList
	return &r, w.fetchAndParse(ctx, path, &r)
}

// discoverBillingFromFSA walks the CustomerAccountListLink from a single FSA
// and builds CustomerAccountState entries. Failures are non-fatal.
func (w *Walker) discoverBillingFromFSA(ctx context.Context, fsa model.FunctionSetAssignments) []CustomerAccountState {
	if fsa.CustomerAccountListLink == nil {
		return nil
	}

	cal, err := w.fetchCustomerAccountList(ctx, fsa.CustomerAccountListLink.Href)
	if err != nil {
		w.logf("billing: fetch CustomerAccountList %s: %v", fsa.CustomerAccountListLink.Href, err)
		return nil
	}

	states := make([]CustomerAccountState, 0, len(cal.CustomerAccount))
	for _, ca := range cal.CustomerAccount {
		cas := CustomerAccountState{Account: ca}
		if ca.CustomerAgreementListLink == nil {
			states = append(states, cas)
			continue
		}

		agList, err := w.fetchCustomerAgreementList(ctx, ca.CustomerAgreementListLink.Href)
		if err != nil {
			w.logf("billing: fetch CustomerAgreementList %s: %v", ca.CustomerAgreementListLink.Href, err)
			states = append(states, cas)
			continue
		}

		for _, ag := range agList.CustomerAgreement {
			ags := CustomerAgreementState{Agreement: ag}

			// Billing periods — use the active list first, fall back to full list.
			periodLink := ag.ActiveBillingPeriodListLink
			if periodLink == nil {
				periodLink = ag.BillingPeriodListLink
			}
			if periodLink != nil {
				bpl, err := w.fetchBillingPeriodList(ctx, periodLink.Href)
				if err != nil {
					w.logf("billing: fetch BillingPeriodList %s: %v", periodLink.Href, err)
				} else {
					ags.BillingPeriods = bpl.BillingPeriod
				}
			}

			// Historical readings (metadata only — not the individual BillingReadings).
			if ag.HistoricalReadingListLink != nil {
				hrl, err := w.fetchHistoricalReadingList(ctx, ag.HistoricalReadingListLink.Href)
				if err != nil {
					w.logf("billing: fetch HistoricalReadingList %s: %v",
						ag.HistoricalReadingListLink.Href, err)
				} else {
					ags.HistoricalReadings = hrl.HistoricalReading
				}
			}

			// Projection readings (metadata only).
			if ag.ProjectionReadingListLink != nil {
				prl, err := w.fetchProjectionReadingList(ctx, ag.ProjectionReadingListLink.Href)
				if err != nil {
					w.logf("billing: fetch ProjectionReadingList %s: %v",
						ag.ProjectionReadingListLink.Href, err)
				} else {
					ags.ProjectionReadings = prl.ProjectionReading
				}
			}

			// Target readings (metadata only).
			if ag.TargetReadingListLink != nil {
				trl, err := w.fetchTargetReadingList(ctx, ag.TargetReadingListLink.Href)
				if err != nil {
					w.logf("billing: fetch TargetReadingList %s: %v",
						ag.TargetReadingListLink.Href, err)
				} else {
					ags.TargetReadings = trl.TargetReading
				}
			}

			cas.Agreements = append(cas.Agreements, ags)
		}

		states = append(states, cas)
	}

	return states
}
