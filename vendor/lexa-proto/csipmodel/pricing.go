package csipmodel

import "encoding/xml"

// ───────────────────────────────────────────────────────────────────────
// Pricing function set (IEEE 2030.5 §10.5)
// ───────────────────────────────────────────────────────────────────────

// UnitValue represents a quantity with a unit-of-measure code and power-of-ten
// multiplier. Used for flow rate limits in RateComponent and for energy/power
// values in FlowReservation. Unit is an IEC 61968-9 UOM code (e.g. 38=W, 72=Wh).
type UnitValue struct {
	Multiplier int8   `xml:"multiplier"`
	Unit       uint8  `xml:"unit,omitempty"`
	Value      int64  `xml:"value"`
}

// TariffProfile is the root resource of the Pricing function set.
// The server exposes one TariffProfile per commodity/service-provider combo.
type TariffProfile struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns TariffProfile"`
	Resource

	Subscribable              uint8     `xml:"subscribable,attr,omitempty"`
	MRID                      string    `xml:"mRID,omitempty"`
	Description               string    `xml:"description,omitempty"`
	Currency                  uint16    `xml:"currency,omitempty"` // ISO 4217 numeric code
	PricePowerOfTenMultiplier int8      `xml:"pricePowerOfTenMultiplier,omitempty"`
	Primacy                   uint8     `xml:"primacy"`
	RateCode                  string    `xml:"rateCode,omitempty"`
	ServiceCategoryKind       uint8     `xml:"serviceCategoryKind,omitempty"` // 0=electricity

	RateComponentListLink *ListLink `xml:"RateComponentListLink,omitempty"`
}

// TariffProfileList is a collection of TariffProfile resources.
type TariffProfileList struct {
	XMLName       xml.Name        `xml:"urn:ieee:std:2030.5:ns TariffProfileList"`
	Resource

	All           uint32          `xml:"all,attr"`
	Results       uint32          `xml:"results,attr"`
	PollRate      uint32          `xml:"pollRate,attr,omitempty"`
	TariffProfile []TariffProfile `xml:"TariffProfile"`
}

// RateComponent aggregates the TimeTariffIntervals for one rate direction
// (e.g. forward/delivered). Each TariffProfile has one or more RateComponents.
type RateComponent struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns RateComponent"`
	Resource

	MRID        string `xml:"mRID,omitempty"`
	Description string `xml:"description,omitempty"`

	// Flow rate limits let the server target a specific demand range
	// (e.g. for a PEV-specific rate).
	FlowRateEndLimit   *UnitValue `xml:"flowRateEndLimit,omitempty"`
	FlowRateStartLimit *UnitValue `xml:"flowRateStartLimit,omitempty"`

	ReadingTypeLink *Link `xml:"ReadingTypeLink,omitempty"`
	// RoleFlags bit field: bit 2 = isPrimary (forward), bit 3 = isReverse
	RoleFlags uint16 `xml:"roleFlags,omitempty"`

	TimeTariffIntervalListLink       *ListLink `xml:"TimeTariffIntervalListLink,omitempty"`
	ActiveTimeTariffIntervalListLink *ListLink `xml:"ActiveTimeTariffIntervalListLink,omitempty"`
}

// RateComponentList is a collection of RateComponent resources.
type RateComponentList struct {
	XMLName       xml.Name        `xml:"urn:ieee:std:2030.5:ns RateComponentList"`
	Resource

	All           uint32          `xml:"all,attr"`
	Results       uint32          `xml:"results,attr"`
	RateComponent []RateComponent `xml:"RateComponent"`
}

// TimeTariffInterval is a time-bound event that specifies which TOU tier is
// active during its Effective Scheduled Period. The spec defines it as an
// Event subtype so it carries EventStatus and randomization attributes.
type TimeTariffInterval struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns TimeTariffInterval"`
	Resource

	Subscribable     uint8        `xml:"subscribable,attr,omitempty"`
	MRID             string       `xml:"mRID,omitempty"`
	Description      string       `xml:"description,omitempty"`
	CreationTime     int64        `xml:"creationTime,omitempty"`
	EventStatus      *EventStatus `xml:"EventStatus,omitempty"`
	Interval         DateTimeInterval `xml:"interval"`
	RandomizeDuration *int32      `xml:"randomizeDuration,omitempty"`
	RandomizeStart   *int32       `xml:"randomizeStart,omitempty"`
	// TouTier identifies which pricing tier this interval belongs to.
	// Higher values = higher price (mandatory per §10.5.3.8).
	TouTier uint8 `xml:"touTier"`

	ConsumptionTariffIntervalListLink *ListLink `xml:"ConsumptionTariffIntervalListLink,omitempty"`
}

// TimeTariffIntervalList is a collection of TimeTariffInterval resources.
type TimeTariffIntervalList struct {
	XMLName            xml.Name             `xml:"urn:ieee:std:2030.5:ns TimeTariffIntervalList"`
	Resource

	All                uint32               `xml:"all,attr"`
	Results            uint32               `xml:"results,attr"`
	Subscribable       uint8                `xml:"subscribable,attr,omitempty"`
	TimeTariffInterval []TimeTariffInterval `xml:"TimeTariffInterval"`
}

// ConsumptionTariffInterval specifies the price for a given consumption block
// within a TimeTariffInterval. For flat/TOU rates, there is typically one
// ConsumptionTariffInterval with startValue=0; for block rates, there is one
// per block.
type ConsumptionTariffInterval struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns ConsumptionTariffInterval"`
	Resource

	// ConsumptionBlock is the block index (1-based; 0 = all blocks).
	ConsumptionBlock uint8 `xml:"consumptionBlock"`
	// Price in units defined by TariffProfile.pricePowerOfTenMultiplier.
	Price int32 `xml:"price"`
	// StartValue is the cumulative consumption (commodity units) at which
	// this block becomes active.
	StartValue int64 `xml:"startValue"`
}

// ConsumptionTariffIntervalList is a collection of ConsumptionTariffInterval resources.
type ConsumptionTariffIntervalList struct {
	XMLName                   xml.Name                    `xml:"urn:ieee:std:2030.5:ns ConsumptionTariffIntervalList"`
	Resource

	All                       uint32                      `xml:"all,attr"`
	Results                   uint32                      `xml:"results,attr"`
	ConsumptionTariffInterval []ConsumptionTariffInterval `xml:"ConsumptionTariffInterval"`
}

// PriceResponseCfg allows the server to configure price-responsive thresholds
// for a specific end device. Optional; only present if the server supports it.
type PriceResponseCfg struct {
	XMLName xml.Name `xml:"urn:ieee:std:2030.5:ns PriceResponseCfg"`
	Resource

	// ConsumeThreshold: below this price, the device SHOULD consume more.
	ConsumeThreshold int32 `xml:"consumeThreshold"`
	// MaxReductionThreshold: above this price, device SHOULD reduce to max extent.
	MaxReductionThreshold int32 `xml:"maxReductionThreshold"`
	// RateComponentLink identifies which RateComponent these thresholds apply to.
	RateComponentLink *Link `xml:"RateComponentLink,omitempty"`
}

// PriceResponseCfgList is a collection of PriceResponseCfg resources.
type PriceResponseCfgList struct {
	XMLName          xml.Name           `xml:"urn:ieee:std:2030.5:ns PriceResponseCfgList"`
	Resource

	All              uint32             `xml:"all,attr"`
	Results          uint32             `xml:"results,attr"`
	PriceResponseCfg []PriceResponseCfg `xml:"PriceResponseCfg"`
}
