package model

import "encoding/xml"

// ───────────────────────────────────────────────────────────────────────
// Billing function set (IEEE 2030.5 §10.7)
// ───────────────────────────────────────────────────────────────────────

// ServiceSupplier describes the utility or retail energy provider.
// Linked from CustomerAccount via ServiceSupplierLink.
type ServiceSupplier struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ServiceSupplier"`
	Resource

	MRID        string `xml:"mRID,omitempty"`
	Description string `xml:"description,omitempty"`
	Email       string `xml:"email,omitempty"`
	Phone       string `xml:"phone,omitempty"`
	ProviderID  uint32 `xml:"providerID,omitempty"`
	Web         string `xml:"web,omitempty"`
}

// CustomerAccount holds account-level billing information. Each account
// can have multiple CustomerAgreements (e.g. different service points or
// commodities).
type CustomerAccount struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns CustomerAccount"`
	Resource

	MRID                      string    `xml:"mRID,omitempty"`
	Description               string    `xml:"description,omitempty"`
	Currency                  uint16    `xml:"currency,omitempty"` // ISO 4217 numeric
	CustomerAccountNumber     string    `xml:"customerAccount,omitempty"`
	CustomerName              string    `xml:"customerName,omitempty"`
	PricePowerOfTenMultiplier int8      `xml:"pricePowerOfTenMultiplier,omitempty"`

	CustomerAgreementListLink *ListLink `xml:"CustomerAgreementListLink,omitempty"`
	ServiceSupplierLink       *Link     `xml:"ServiceSupplierLink,omitempty"`
}

// CustomerAccountList is a collection of CustomerAccount resources.
type CustomerAccountList struct {
	XMLName         xml.Name          `xml:"urn:ieee:std:2030.5:ns CustomerAccountList"`
	Resource

	All             uint32            `xml:"all,attr"`
	Results         uint32            `xml:"results,attr"`
	CustomerAccount []CustomerAccount `xml:"CustomerAccount"`
}

// CustomerAgreement represents a service agreement for one usage point and
// tariff. It links billing periods, historical readings, projections, targets,
// and the associated TariffProfile and UsagePoint together.
type CustomerAgreement struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns CustomerAgreement"`
	Resource

	MRID            string `xml:"mRID,omitempty"`
	Description     string `xml:"description,omitempty"`
	ServiceLocation string `xml:"serviceLocation,omitempty"`

	ActiveBillingPeriodListLink *ListLink `xml:"ActiveBillingPeriodListLink,omitempty"`
	BillingPeriodListLink       *ListLink `xml:"BillingPeriodListLink,omitempty"`
	HistoricalReadingListLink   *ListLink `xml:"HistoricalReadingListLink,omitempty"`
	ProjectionReadingListLink   *ListLink `xml:"ProjectionReadingListLink,omitempty"`
	TargetReadingListLink       *ListLink `xml:"TargetReadingListLink,omitempty"`

	TariffProfileLink *Link `xml:"TariffProfileLink,omitempty"`
	UsagePointLink    *Link `xml:"UsagePointLink,omitempty"`
}

// CustomerAgreementList is a collection of CustomerAgreement resources.
type CustomerAgreementList struct {
	XMLName           xml.Name            `xml:"urn:ieee:std:2030.5:ns CustomerAgreementList"`
	Resource

	All               uint32              `xml:"all,attr"`
	Results           uint32              `xml:"results,attr"`
	Subscribable      uint8               `xml:"subscribable,attr,omitempty"`
	CustomerAgreement []CustomerAgreement `xml:"CustomerAgreement"`
}

// BillingPeriod covers one billing cycle. billToDate and billLastPeriod are
// in units defined by CustomerAccount.pricePowerOfTenMultiplier.
type BillingPeriod struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns BillingPeriod"`
	Resource

	// BillLastPeriod is the amount billed in the previous period.
	BillLastPeriod *int64 `xml:"billLastPeriod,omitempty"`
	// BillToDate is the amount accrued so far in the current period.
	BillToDate *int64 `xml:"billToDate,omitempty"`
	// Interval is the billing period start and duration.
	Interval        DateTimeInterval `xml:"interval"`
	StatusTimeStamp int64            `xml:"statusTimeStamp,omitempty"`
}

// BillingPeriodList is a collection of BillingPeriod resources.
type BillingPeriodList struct {
	XMLName       xml.Name        `xml:"urn:ieee:std:2030.5:ns BillingPeriodList"`
	Resource

	All           uint32          `xml:"all,attr"`
	Results       uint32          `xml:"results,attr"`
	Subscribable  uint8           `xml:"subscribable,attr,omitempty"`
	BillingPeriod []BillingPeriod `xml:"BillingPeriod"`
}

// Charge is an individual line-item charge within a BillingReading.
// Kind: 0=consumption, 1=demand, 2=auxiliary.
type Charge struct {
	Kind  uint8 `xml:"kind"`
	Value int64 `xml:"value"`
}

// BillingReading is an interval-metered or block-metered reading with
// associated charges. Used inside BillingReadingSet.
type BillingReading struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns BillingReading"`

	ConsumptionBlock uint8             `xml:"consumptionBlock,omitempty"`
	QualityFlags     string            `xml:"qualityFlags,omitempty"`
	TimePeriod       *DateTimeInterval `xml:"timePeriod,omitempty"`
	TouTier          uint8             `xml:"touTier,omitempty"`
	Value            int64             `xml:"value,omitempty"`
	Charge           []Charge          `xml:"Charge,omitempty"`
}

// BillingReadingList is a collection of BillingReading resources.
type BillingReadingList struct {
	XMLName        xml.Name         `xml:"urn:ieee:std:2030.5:ns BillingReadingList"`
	Resource

	All            uint32           `xml:"all,attr"`
	Results        uint32           `xml:"results,attr"`
	BillingReading []BillingReading `xml:"BillingReading"`
}

// BillingReadingSet groups BillingReadings for a time period (e.g. one day
// or one billing period segment). Used under HistoricalReading,
// ProjectionReading, and TargetReading.
type BillingReadingSet struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns BillingReadingSet"`
	Resource

	MRID        string           `xml:"mRID,omitempty"`
	Description string           `xml:"description,omitempty"`
	TimePeriod  DateTimeInterval `xml:"timePeriod"`

	BillingReadingListLink *ListLink `xml:"BillingReadingListLink,omitempty"`
}

// BillingReadingSetList is a collection of BillingReadingSet resources.
type BillingReadingSetList struct {
	XMLName           xml.Name            `xml:"urn:ieee:std:2030.5:ns BillingReadingSetList"`
	Resource

	All               uint32              `xml:"all,attr"`
	Results           uint32              `xml:"results,attr"`
	Subscribable      uint8               `xml:"subscribable,attr,omitempty"`
	BillingReadingSet []BillingReadingSet `xml:"BillingReadingSet"`
}

// HistoricalReading provides verified past consumption or cost data from
// the service provider's billing system.
type HistoricalReading struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns HistoricalReading"`
	Resource

	MRID        string `xml:"mRID,omitempty"`
	Description string `xml:"description,omitempty"`

	BillingReadingSetListLink *ListLink `xml:"BillingReadingSetListLink,omitempty"`
	ReadingTypeLink           *Link     `xml:"ReadingTypeLink,omitempty"`
}

// HistoricalReadingList is a collection of HistoricalReading resources.
type HistoricalReadingList struct {
	XMLName           xml.Name            `xml:"urn:ieee:std:2030.5:ns HistoricalReadingList"`
	Resource

	All               uint32              `xml:"all,attr"`
	Results           uint32              `xml:"results,attr"`
	HistoricalReading []HistoricalReading `xml:"HistoricalReading"`
}

// ProjectionReading provides the service provider's projected future consumption
// or cost if current habits continue.
type ProjectionReading struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ProjectionReading"`
	Resource

	MRID        string `xml:"mRID,omitempty"`
	Description string `xml:"description,omitempty"`

	BillingReadingSetListLink *ListLink `xml:"BillingReadingSetListLink,omitempty"`
	ReadingTypeLink           *Link     `xml:"ReadingTypeLink,omitempty"`
}

// ProjectionReadingList is a collection of ProjectionReading resources.
type ProjectionReadingList struct {
	XMLName           xml.Name            `xml:"urn:ieee:std:2030.5:ns ProjectionReadingList"`
	Resource

	All               uint32              `xml:"all,attr"`
	Results           uint32              `xml:"results,attr"`
	ProjectionReading []ProjectionReading `xml:"ProjectionReading"`
}

// TargetReading provides an absolute consumption or cost target set by the
// service provider (e.g. a 10% reduction challenge expressed as an absolute kWh).
type TargetReading struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns TargetReading"`
	Resource

	MRID        string `xml:"mRID,omitempty"`
	Description string `xml:"description,omitempty"`

	BillingReadingSetListLink *ListLink `xml:"BillingReadingSetListLink,omitempty"`
	ReadingTypeLink           *Link     `xml:"ReadingTypeLink,omitempty"`
}

// TargetReadingList is a collection of TargetReading resources.
type TargetReadingList struct {
	XMLName       xml.Name        `xml:"urn:ieee:std:2030.5:ns TargetReadingList"`
	Resource

	All           uint32          `xml:"all,attr"`
	Results       uint32          `xml:"results,attr"`
	TargetReading []TargetReading `xml:"TargetReading"`
}
