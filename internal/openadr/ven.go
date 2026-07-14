package openadr

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"lexa-hub/internal/bus"
)

// VEN ties the client, event store, translator, and report scheduler into
// one poll-driven 3.1 VEN. Owned by cmd/openadr's single poll goroutine
// (single-flight by construction — the northbound run-loop discipline).
type VEN struct {
	Client *Client
	// ProgramIDs filters which programs' events are tracked; matches the
	// program object's id OR programName (utilities hand out names). Empty
	// = every program the token's targets expose.
	ProgramIDs []string
	VenName    string

	store    *EventStore
	programs map[string]Program
	reports  *ReportScheduler

	// venID is the 3.1 ven-object ID, held in MEMORY only (see
	// Client.EnsureVen's restart-behavior doc); venRegDone/venRegSkip stop
	// re-registration churn once resolved or permanently refused.
	venID      string
	venRegDone bool
	venRegSkip bool

	// now is the clock seam for tests.
	now func() time.Time
}

// New constructs a VEN over c.
func New(c *Client, programIDs []string, venName string) *VEN {
	return &VEN{
		Client:     c,
		ProgramIDs: programIDs,
		VenName:    venName,
		store:      NewEventStore(),
		programs:   make(map[string]Program),
		reports:    NewReportScheduler(),
		now:        time.Now,
	}
}

// SetNow overrides the clock (tests).
func (v *VEN) SetNow(now func() time.Time) { v.now = now }

// Store exposes the event store (tests / randomize seam).
func (v *VEN) Store() *EventStore { return v.store }

// PollResult is one successful poll cycle's output.
type PollResult struct {
	Prices       bus.OpenADRPrices
	Limits       bus.OpenADRLimits
	ProgramCount int
	ActiveEvents int
	Diff         Diff
}

// PollOnce runs one full poll cycle: GET /programs (paged), GET /events per
// tracked program (paged), event lifecycle reconciliation, CP translation.
// Any error leaves the store UNCHANGED (fail-closed: a flaky VTN must not
// read as "all events deleted" — the retained bus docs keep their
// last-known-good content until a successful poll says otherwise).
func (v *VEN) PollOnce(ctx context.Context) (PollResult, error) {
	programs, err := v.Client.Programs(ctx)
	if err != nil {
		return PollResult{}, err
	}
	tracked := v.filterPrograms(programs)

	var events []Event
	for _, p := range tracked {
		evs, err := v.Client.Events(ctx, p.ID)
		if err != nil {
			return PollResult{}, err
		}
		events = append(events, evs...)
	}

	// Commit point: only after every fetch succeeded does the store change.
	v.programs = make(map[string]Program, len(tracked))
	for _, p := range tracked {
		v.programs[p.ID] = p
	}
	diff := v.store.Reconcile(events)
	for _, id := range diff.Added {
		slog.Info("openadr: event added", "event", id)
	}
	for _, id := range diff.Updated {
		slog.Info("openadr: event updated", "event", id)
	}
	for _, id := range diff.Deleted {
		slog.Info("openadr: event deleted by VTN", "event", id)
	}

	now := v.now()
	instances := v.store.Instances()
	lookup := NewDescriptorLookup(v.programs)
	return PollResult{
		Prices:       TranslatePrices(instances, lookup, now),
		Limits:       TranslateLimits(instances, lookup, now),
		ProgramCount: len(tracked),
		ActiveEvents: CountActive(instances, now),
		Diff:         diff,
	}, nil
}

func (v *VEN) filterPrograms(programs []Program) []Program {
	if len(v.ProgramIDs) == 0 {
		return programs
	}
	want := make(map[string]bool, len(v.ProgramIDs))
	for _, id := range v.ProgramIDs {
		want[id] = true
	}
	var out []Program
	for _, p := range programs {
		if want[p.ID] || want[p.ProgramName] {
			out = append(out, p)
		}
	}
	return out
}

// EnsureRegistered resolves the ven object once per process lifetime
// (idempotent — safe to call every successful poll cycle). A 403/404/405
// from /vens means this VTN doesn't offer (or doesn't grant) ven-object
// writes; that is logged once and permanently skipped — a CP-profile VEN
// can operate read-only against a public-tariff VTN.
func (v *VEN) EnsureRegistered(ctx context.Context) {
	if v.venRegDone || v.venRegSkip || v.VenName == "" {
		return
	}
	id, err := v.Client.EnsureVen(ctx, v.VenName)
	if err != nil {
		if he, ok := err.(*httpError); ok &&
			(he.Status == http.StatusForbidden || he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
			slog.Warn("openadr: VTN refused ven registration — continuing unregistered (read-only VEN)",
				"venName", v.VenName, "status", he.Status)
			v.venRegSkip = true
			return
		}
		slog.Warn("openadr: ven registration failed — will retry next poll", "venName", v.VenName, "err", err)
		return
	}
	v.venID = id
	v.venRegDone = true
	slog.Info("openadr: ven registered", "venName", v.VenName, "venID", id)
}

// VenID returns the resolved ven-object ID ("" until registration succeeds).
func (v *VEN) VenID() string { return v.venID }

// DueReports returns the report streams due now across active events
// (report.go's scheduler), with defaultPeriod as the cadence floor/fallback.
func (v *VEN) DueReports(now time.Time, defaultPeriod time.Duration) []ReportRequest {
	return v.reports.Due(v.store.Instances(), now, defaultPeriod)
}

// MarkReported records a successful report POST for req's stream.
func (v *VEN) MarkReported(req ReportRequest, now time.Time) {
	v.reports.MarkPosted(req, now)
}
