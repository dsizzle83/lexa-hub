// Package bus defines MQTT topic names and JSON message types shared across
// all lexa-hub services.
//
// Topic layout:
//
//	lexa/measurements/{device}                modbus       → hub, telemetry
//	lexa/battery/{device}/metrics             modbus       → hub
//	lexa/csip/control                         northbound   → hub (retained)
//	lexa/csip/pricing                         northbound   → hub (retained)
//	lexa/csip/billing                         northbound   → hub (retained)
//	lexa/csip/flowreservation/status          northbound   → hub (retained)
//	lexa/csip/flowreservation/request         hub          → northbound (QoS 1)
//	lexa/csip/rewalk                          hub          → northbound (QoS 1, TASK-042)
//	lexa/northbound/schedule                  northbound   → hub (retained)
//	lexa/northbound/certstatus                northbound   → api (retained, TASK-072)
//	lexa/evse/{station}/state                 ocpp         → hub
//	lexa/desired/{class}/{device}             hub          → modbus/ocpp (retained, AD-013)
//	lexa/hub/plan                             hub          → api (retained)
//	lexa/reconcile/{class}/{device}/report    modbus/ocpp  → hub (retained, TASK-031)
//
// Publish QoS is a per-topic policy owned here (PubQoS), not hardcoded by
// callers: the measurement plane (lexa/measurements/*, lexa/battery/*/metrics,
// lexa/evse/*/state) is QoS 0 — high-frequency, freshness-gated by
// subscribers, a dropped sample is the documented design, not a bug; every
// other topic (commands, CSIP control/pricing/billing/flow-reservation,
// compliance alert, schedule, plan log) is QoS 1 (bounded PUBACK wait,
// mqttutil.publishTimeout). See CLAUDE.md's MQTT topic map for the same table
// (D5 closure: doc and code now agree, enforced by bus.PubQoS).
//
// Every message type below is versioned (AD-006): it embeds Envelope by
// value, giving it an omitempty "v" field, and has a per-schema version
// constant in envelope.go (e.g. MeasurementV). A subscriber calls
// CheckVersion(topic, payload, supportedV) before unmarshalling; the decode
// policy is absent-v accepted as legacy v0 while LegacyV0Accepted is true (the
// transition default), 1..supported accepted, greater-than-supported or
// negative rejected via RejectAndAlarm (counted per topic, logged
// first-plus-every-Nth to stay inside the journald budget). Same-major
// unknown fields are simply ignored by json.Unmarshal, which is what keeps
// additive schema evolution cheap. For a retained control-plane topic, a
// rejected message means hold last-known-good (the scheduler's existing
// fail-closed discipline) rather than running on a zero-valued decode; a
// later task (TASK-042) adds an active re-request instead of waiting out the
// retained message's own expiry. TASK-017 landed the type, constants,
// CheckVersion, and RejectAndAlarm; TASK-018 wired every publisher below to
// stamp V and every subscriber (mqttutil.Subscribe's version gate, plus the
// one raw mc.Subscribe in cmd/northbound) to call CheckVersion via
// SupportedV.
package bus

import (
	"fmt"
	"strings"
)

// QoS byte values, named for readability at PubQoS call sites.
const (
	QoS0 byte = 0
	QoS1 byte = 1
)

// PubQoS returns the publish QoS for topic per the documented policy: QoS 0
// for the measurement plane (device measurements, battery metrics, EVSE
// state), QoS 1 for everything else. Callers publish with
// mqttutil.PublishJSONQoS(client, topic, bus.PubQoS(topic), v) rather than
// hardcoding a QoS, so this function is the single place the policy lives
// (review D5: previously every publish hardcoded QoS 1).
//
// Subscribe QoS is untouched by this policy — Subscribe always requests QoS
// 1, and effective delivery QoS is min(publish, subscribe), so a QoS-0
// publish stays best-effort and a QoS-1 publish stays reliably delivered
// regardless of what a subscriber requests.
func PubQoS(topic string) byte {
	switch {
	case strings.HasPrefix(topic, "lexa/measurements/"):
		return QoS0
	case strings.HasPrefix(topic, "lexa/battery/") && strings.HasSuffix(topic, "/metrics"):
		return QoS0
	case strings.HasPrefix(topic, "lexa/evse/") && strings.HasSuffix(topic, "/state"):
		return QoS0
	default:
		return QoS1
	}
}

// SupportedV returns the highest envelope schema version a subscriber on
// topic currently supports (TASK-018), for use as CheckVersion's `supported`
// argument. Like PubQoS, this is the single place a concrete topic string is
// mapped to policy — here, to the per-schema version constant in
// envelope.go — so callers (mqttutil.Subscribe's version gate, the raw
// FR-request subscribe in cmd/northbound) never hardcode a version.
//
// Every family here is currently at its "born at 1" constant (TASK-017); a
// topic this function does not recognize gets supported=1 rather than 0,
// so an unrecognized-but-legitimate topic still accepts v0 (legacy) and v1
// instead of rejecting everything — recognizing every topic precisely is
// this function's job to keep current, not CheckVersion's to guess at.
func SupportedV(topic string) int {
	switch {
	case strings.HasPrefix(topic, "lexa/measurements/"):
		return MeasurementV
	case strings.HasPrefix(topic, "lexa/battery/") && strings.HasSuffix(topic, "/metrics"):
		return BattMetricsV
	case topic == TopicCSIPControl:
		return ActiveControlV
	case topic == TopicCSIPComplianceAlert:
		return ComplianceAlertV
	case strings.HasPrefix(topic, "lexa/control/battery/"):
		return BattCommandV
	case strings.HasPrefix(topic, "lexa/control/solar/"):
		return SolarCommandV
	case strings.HasPrefix(topic, "lexa/evse/") && strings.HasSuffix(topic, "/state"):
		return EVSEStateV
	case strings.HasPrefix(topic, "lexa/evse/") && strings.HasSuffix(topic, "/command"):
		return EVSECommandV
	case topic == TopicCSIPPricing:
		return PricingUpdateV
	case topic == TopicCSIPBilling:
		return BillingUpdateV
	case topic == TopicCSIPFRRequest:
		return FlowReservationRequestV
	case topic == TopicCSIPRewalk:
		return RewalkRequestV
	case topic == TopicCSIPFRStatus:
		return FlowReservationStatusV
	case topic == TopicNorthboundSchedule:
		return DERScheduleV
	case topic == TopicNorthboundCertStatus:
		return CertStatusV
	case topic == TopicHubPlan:
		return PlanLogV
	case strings.HasPrefix(topic, "lexa/desired/adv/"):
		// WP-9's DesiredAdvanced family occupies the {class}="adv" slot of the
		// desired-topic shape (D6) and versions independently of the scalar
		// DesiredState docs — this arm must stay ABOVE the broader
		// "lexa/desired/" prefix arm below or it would never match.
		return DesiredAdvancedV
	case strings.HasPrefix(topic, "lexa/desired/"):
		// AD-013's DesiredStateV case, deferred from TASK-026 (the reconciler
		// core landed unwired) to TASK-027, whose hub-side publisher is the
		// first thing to ever put a message on this topic family.
		return DesiredStateV
	case strings.HasPrefix(topic, "lexa/reconcile/") && strings.HasSuffix(topic, "/report"):
		return ReconcileReportV
	case strings.HasPrefix(topic, "lexa/intent/"):
		// One arm for the whole "lexa/intent/*" family (TASK-082), dispatching
		// per exact kind — this is the "no wildcard subscription" prefix from
		// TopicIntentMode's doc comment, but SupportedV still needs to name
		// each kind's constant precisely (TopicIntentResult included: it
		// shares the prefix but is a distinct schema, IntentResultV, not one
		// of the seven request kinds).
		switch topic {
		case TopicIntentMode:
			return ModeIntentV
		case TopicIntentEVGoal:
			return EVGoalIntentV
		case TopicIntentReserve:
			return BackupReserveIntentV
		case TopicIntentTariff:
			return TariffIntentV
		case TopicIntentSolarForecast:
			return SolarForecastIntentV
		case TopicIntentLoadProfile:
			return LoadProfileIntentV
		case TopicIntentChargeNow:
			return ChargeNowIntentV
		case TopicIntentResult:
			return IntentResultV
		default:
			return 1
		}
	case topic == TopicHubMode:
		return ModeStatusV
	case topic == TopicHubSettings:
		return HubSettingsV
	case topic == TopicHubSchedule:
		return HubScheduleV
	case topic == TopicCloudlinkStatus:
		return CloudlinkStatusV
	case topic == TopicScanRequest:
		return ScanRequestV
	case topic == TopicScanStatus:
		return ScanStatusV
	case topic == TopicScanResult:
		return ScanResultV
	case topic == TopicOCPPPending:
		return PendingStationsV
	case topic == TopicOCPPPairing:
		return PairingDecisionV
	case topic == TopicHubLogEvent:
		return LogEventV
	case topic == TopicHubDERSite:
		return DERSiteReportV
	case topic == TopicCSIPCurves:
		return CurveSetV
	case topic == TopicOpenADRPrices:
		return OpenADRPricesV
	case topic == TopicOpenADRLimits:
		return OpenADRLimitsV
	case topic == TopicOpenADRStatus:
		return OpenADRStatusV
	default:
		return 1
	}
}

func MeasurementTopic(device string) string {
	return fmt.Sprintf("lexa/measurements/%s", device)
}

func BattMetricsTopic(device string) string {
	return fmt.Sprintf("lexa/battery/%s/metrics", device)
}

const TopicCSIPControl = "lexa/csip/control"

// TopicCSIPComplianceAlert is published by the hub (orchestrator) when the
// optimizer cannot meet an active CSIP control limit after exhausting every
// lever (e.g. an import cap with the battery at its SOC reserve). The
// northbound service consumes it and POSTs a 2030.5 CannotComply Response so
// the grid server is told the DER is resource-limited.
const TopicCSIPComplianceAlert = "lexa/csip/compliance/alert"

// Pricing, billing, and flow reservation topics (IEEE 2030.5 §10.5/10.7/10.9).
const (
	TopicCSIPPricing   = "lexa/csip/pricing"
	TopicCSIPBilling   = "lexa/csip/billing"
	TopicCSIPFRStatus  = "lexa/csip/flowreservation/status"
	TopicCSIPFRRequest = "lexa/csip/flowreservation/request"
)

// TopicCSIPRewalk is published by lexa-hub, non-retained at QoS 1, to ask
// lexa-northbound to refresh the retained lexa/csip/control message
// immediately rather than waiting for the next discovery cycle (TASK-042,
// AD-006 extension covering §8.3/GAP-01/GAP-02). The hub publishes a
// RewalkRequest here when it either (a) adopts a retained control whose Ts
// is older than retained_adoption_max_age_s — a possible mosquitto-autosave
// resurrection of a superseded cap — or (b) fails to decode the retained
// payload at all (corrupted retained control, previously a silent drop with
// no recovery path). lexa-northbound subscribes this and, on receipt,
// immediately republishes its last-published ActiveControl (fresh Ts) if it
// has one — repairing the retained value even while the WAN is dark — and
// triggers an out-of-cadence discovery walk to refresh ground truth. Not
// retained: this is a one-shot nudge, not state that should replay on
// reconnect (the hub re-issues it on its own next adoption/decode-error,
// rate-limited independently on both ends — see cmd/hub/state.go's
// rewalkRateLimit and cmd/northbound/main.go's rewalkGate).
const TopicCSIPRewalk = "lexa/csip/rewalk"

// TopicCSIPCurves carries the resolved curve content (bus.CurveSet) for the
// active DER control's curve-linked modes (WP-8, standards-buildout C1/D6):
// northbound → hub, RETAINED, QoS 1 (PubQoS's non-measurement default).
// Curve content deliberately rides its OWN retained doc rather than
// ActiveControl (architecture §2.3/D6): curves are provisioning state that
// changes rarely, keeping the small, staleness-checked lexa/csip/control doc
// (TASK-042 — untouched by WP-8) small. The doc is content-hashed
// (CurveSet.SetID = CurveSetContentHash over the entries), republished only
// on hash change; ActiveControl.CurveSetID names the matching set ("" = the
// active control links no resolvable curves). Retained-doc redelivery on
// rewalk/reconnect covers late subscribers.
const TopicCSIPCurves = "lexa/csip/curves"

// TopicNorthboundSchedule is published by lexa-northbound after each discovery
// walk. It carries the resolved 24-hour DER control schedule (retained, QoS 1).
const TopicNorthboundSchedule = "lexa/northbound/schedule"

// TopicNorthboundCertStatus is published by lexa-northbound's cert-expiry
// monitor (retained, QoS 1, TASK-072/§10.5) after every inspection of the
// configured client + CA PEM files (startup, then every 24h). lexa-api
// subscribes it and folds the latest CertStatus into GET /status's
// "cert_status" field so an expiring certificate is visible to the dashboard
// and QA harness instead of surfacing only as a silent discovery-error loop
// once the utility server starts rejecting the handshake.
const TopicNorthboundCertStatus = "lexa/northbound/certstatus"

// TopicHubPlan carries the optimizer's most recent plan trace (decision log +
// timestamp), published by lexa-hub on every engine pass — economic tick and
// safety tick alike — and retained so lexa-api serves the latest plan across
// its own restarts. This is pure observability (the /status last_plan field):
// before it existed, /status served a hardcoded empty decision list, so the QA
// harness's decision introspection silently never worked ("the hub's plan log
// was empty" appeared in every diagnosis that asked). The per-pass timestamp
// doubles as an engine heartbeat: a hub whose /status last_plan timestamp
// stops advancing has a wedged control loop (QA gaps doc, "wedge detection").
const TopicHubPlan = "lexa/hub/plan"

// TopicHubLogEvent carries CSIP Table 14 DER alarm/RTN occurrences
// (bus.LogEventMsg) from the hub's alarm-edge detector to lexa-northbound's
// LogEvent poster (WP-6, BASIC-027/G31/G32). An EDGE, never retained (a
// retained edge replays as a false edge after a restart — same discipline as
// TopicCSIPComplianceAlert); QoS 1 via PubQoS's non-measurement default, with
// at-least-once redelivery made idempotent by LogEventMsg.DedupeKey. See
// internal/bus/logevent.go for the full contract.
const TopicHubLogEvent = "lexa/hub/logevent"

// TopicHubDERSite carries the hub's GFEMS site-aggregate DER report
// (bus.DERSiteReport — D2 ratings/settings/modes truth-mask plus the live
// status block) from cmd/hub's dersite aggregator to lexa-northbound's
// derreport manager (WP-4, standards-buildout A2). STATE, retained at QoS 1
// (PubQoS's non-measurement default): latest wins, a restarting
// lexa-northbound re-seeds from the broker, and the hub republishes on
// content change (min 60 s apart) plus an unchanged-content heartbeat. See
// internal/bus/dersite.go for the full contract.
const TopicHubDERSite = "lexa/hub/dersite"

// Intent/scan/mode/status topics (TASK-082, docs/DEVICE_ROADMAP.md §1.1/§1.3).
//
// Retained/edge semantics, inherited from the AD-013/TASK-042 disciplines
// (§1.1's design rules):
//   - State-like intents (mode, evgoal, reserve, tariff, solarforecast,
//     loadprofile) are RETAINED, one topic per kind, exactly like
//     lexa/desired/{class}/{device}: a restarting hub re-seeds every user/
//     cloud goal from the broker, and a broker reconnect redelivers them, so
//     adoption is ID-deduped (IntentMeta.ID) rather than re-journaled as a
//     fresh event.
//   - TopicIntentChargeNow is the one EDGE intent: NOT retained, and its
//     IntentMeta.TTLS is mandatory — a chargenow stuck behind a WAN outage
//     must not fire hours late.
//   - TopicIntentResult is NOT retained (one reply per received intent; an
//     edge, not state) and TopicHubMode/TopicCloudlinkStatus ARE retained
//     (authoritative current state, republished on every change and re-served
//     to a restarting subscriber).
//   - TopicScanRequest/TopicScanStatus are NOT retained (one-shot commands and
//     transient progress lines); TopicScanResult and TopicOCPPPending ARE
//     retained (the last completed scan / current pending-station set stay
//     available to a client that connects late, until commissioning
//     supersedes them).
//
// No wildcard subscription on "lexa/intent/+" anywhere: TopicIntentResult
// ("lexa/intent/result") would match it, and a hub subscribing its own
// result topic back to itself is exactly the kind of self-feedback loop the
// per-kind subscribe blocks (seven explicit mqttutil.Subscribe calls, one per
// request kind — never a "lexa/intent/+" catch-all) are designed to avoid.
// This also keeps the mosquitto ACL exact: each topic is its own grant, never
// a prefix wildcard.
const (
	TopicIntentMode          = "lexa/intent/mode"
	TopicIntentEVGoal        = "lexa/intent/evgoal"
	TopicIntentReserve       = "lexa/intent/reserve"
	TopicIntentTariff        = "lexa/intent/tariff"
	TopicIntentSolarForecast = "lexa/intent/solarforecast"
	TopicIntentLoadProfile   = "lexa/intent/loadprofile"
	TopicIntentChargeNow     = "lexa/intent/chargenow"
	TopicIntentResult        = "lexa/intent/result"
	TopicHubMode             = "lexa/hub/mode"
	// TopicHubSettings carries the hub's effective backup-reserve floor and
	// active tariff (bus.HubSettings, retained, GAP-8). Published by lexa-hub,
	// consumed by lexa-api which folds it into /status's "reserve" + "tariff"
	// so the app reads hub truth, not a locally-cached last-submitted value.
	TopicHubSettings = "lexa/hub/settings"
	// TopicHubSchedule carries the hub's most recent 24-hour plan/forecast
	// series (bus.HubSchedule, retained, GAP-7): the solar forecast the
	// optimizer used, the planned per-slot battery setpoint + SOC, and the EV
	// charge plan. Published by lexa-hub on each replan, consumed by lexa-api
	// which projects it into GET /plan for the app's forecast/plan charts.
	TopicHubSchedule     = "lexa/hub/schedule"
	TopicCloudlinkStatus = "lexa/cloudlink/status"
	TopicScanRequest     = "lexa/scan/request"
	TopicScanStatus      = "lexa/scan/status"
	TopicScanResult      = "lexa/scan/result"
	TopicOCPPPending     = "lexa/ocpp/pending"
)

// IntentTopic returns lexa/intent/{kind} for a request-kind string ("mode",
// "evgoal", "reserve", "tariff", "solarforecast", "loadprofile", "chargenow")
// — the builder a caller holding only the kind string (e.g. cloudlink's
// downlink dispatch table, or IntentResult.Kind on the way back) uses instead
// of a Topic* constant, mirroring DesiredTopic/ReconcileReportTopic's builder
// shape. It is not meant to be called with "result": lexa/intent/result is a
// distinct reply topic (TopicIntentResult), not a request kind.
func IntentTopic(kind string) string {
	return fmt.Sprintf("lexa/intent/%s", kind)
}

func EVSEStateTopic(stationID string) string {
	return fmt.Sprintf("lexa/evse/%s/state", stationID)
}

// Deprecated: TASK-032 deleted the legacy command path — no producer or
// consumer remains for lexa/evse/{station}/command; the EVSE reconciler
// executes the retained lexa/desired/evse/{station} doc instead. Kept one
// release for external tooling; slated for removal (backlog).
func EVSECommandTopic(stationID string) string {
	return fmt.Sprintf("lexa/evse/%s/command", stationID)
}

// Deprecated: TASK-032 deleted the legacy command path — no producer or
// consumer remains for lexa/control/battery/{device}; the battery reconciler
// executes the retained lexa/desired/battery/{device} doc instead. Kept one
// release for external tooling; slated for removal (backlog).
func CtrlBatteryTopic(device string) string {
	return fmt.Sprintf("lexa/control/battery/%s", device)
}

// Deprecated: TASK-032 deleted the legacy command path — no producer or
// consumer remains for lexa/control/solar/{device}; the solar reconciler
// executes the retained lexa/desired/solar/{device} doc instead. Kept one
// release for external tooling; slated for removal (backlog).
func CtrlSolarTopic(device string) string {
	return fmt.Sprintf("lexa/control/solar/%s", device)
}

// DesiredTopic returns the retained desired-state topic for a device (AD-013):
// lexa/desired/{class}/{device}, class ∈ battery|solar|evse. For EVSE, device
// is the OCPP stationID (the connector rides inside the document). Nothing
// publishes or subscribes to this yet — the reconciler (TASK-026) is the first
// consumer.
func DesiredTopic(class, device string) string {
	return fmt.Sprintf("lexa/desired/%s/%s", class, device)
}

// ReconcileReportTopic returns the retained reconciler-report topic for a device
// (TASK-031): lexa/reconcile/{class}/{device}/report, class ∈ battery|solar|evse.
// The reconciler shells publish their device-level non-convergence state here
// (NonConvergedBegin/End), RETAINED so the hub re-seeds current convergence
// state after a restart. This is STATE (latest level wins), never an edge — the
// CannotComply alert edge lives on TopicCSIPComplianceAlert and stays
// non-retained (a retained edge would replay as a false edge after restarts).
func ReconcileReportTopic(class, device string) string {
	return fmt.Sprintf("lexa/reconcile/%s/%s/report", class, device)
}

// Wildcard subscription topics used by subscribers.
const (
	SubMeasurements = "lexa/measurements/+"
	SubBattMetrics  = "lexa/battery/+/metrics"
	SubEVSEState    = "lexa/evse/+/state"
	// Deprecated (TASK-032): the legacy command subscriptions were deleted from
	// lexa-modbus/lexa-ocpp; nothing subscribes these anymore. Kept one release
	// for external tooling; slated for removal (backlog).
	SubEVSECommand = "lexa/evse/+/command"
	SubCtrlBattery = "lexa/control/battery/+"
	SubCtrlSolar   = "lexa/control/solar/+"
	// SubDesired matches every retained desired-state document across all
	// device classes (AD-013). The reconciler (TASK-026) subscribes this.
	SubDesired = "lexa/desired/+/+"
	// SubReconcileReport matches every retained reconciler report across all
	// device classes (TASK-031). The hub's breach-episode component subscribes
	// this to merge device-level non-convergence into CannotComply episodes.
	SubReconcileReport = "lexa/reconcile/+/+/report"
)

// DeviceFromMeasurementTopic extracts the device name from a topic like
// "lexa/measurements/inverter-0".
func DeviceFromMeasurementTopic(topic string) string {
	return lastSegment(topic)
}

// DeviceFromBattMetricsTopic extracts the device name from
// "lexa/battery/{device}/metrics".
func DeviceFromBattMetricsTopic(topic string) string {
	// lexa/battery/<device>/metrics — 4 segments, device is index 2
	return nthSegment(topic, 2)
}

// StationFromEVSEStateTopic extracts the station ID from "lexa/evse/{id}/state".
func StationFromEVSEStateTopic(topic string) string {
	return nthSegment(topic, 2)
}

// StationFromEVSECommandTopic extracts the station ID from "lexa/evse/{id}/command".
func StationFromEVSECommandTopic(topic string) string {
	return nthSegment(topic, 2)
}

// DeviceFromCtrlBatteryTopic extracts the device name from
// "lexa/control/battery/{device}".
func DeviceFromCtrlBatteryTopic(topic string) string {
	return lastSegment(topic)
}

// DeviceFromCtrlSolarTopic extracts the device name from
// "lexa/control/solar/{device}".
func DeviceFromCtrlSolarTopic(topic string) string {
	return lastSegment(topic)
}

// ClassFromDesiredTopic extracts the device class from
// "lexa/desired/{class}/{device}" (the segment at index 2).
func ClassFromDesiredTopic(topic string) string {
	return nthSegment(topic, 2)
}

// DeviceFromDesiredTopic extracts the device (or EVSE stationID) from
// "lexa/desired/{class}/{device}" (the segment at index 3).
func DeviceFromDesiredTopic(topic string) string {
	return nthSegment(topic, 3)
}

// ClassFromReconcileReportTopic extracts the device class from
// "lexa/reconcile/{class}/{device}/report" (the segment at index 1).
func ClassFromReconcileReportTopic(topic string) string {
	return nthSegment(topic, 1)
}

// DeviceFromReconcileReportTopic extracts the device (or EVSE stationID) from
// "lexa/reconcile/{class}/{device}/report" (the segment at index 2).
func DeviceFromReconcileReportTopic(topic string) string {
	return nthSegment(topic, 2)
}

func lastSegment(topic string) string {
	for i := len(topic) - 1; i >= 0; i-- {
		if topic[i] == '/' {
			return topic[i+1:]
		}
	}
	return topic
}

func nthSegment(topic string, n int) string {
	seg := 0
	start := 0
	for i := 0; i <= len(topic); i++ {
		if i == len(topic) || topic[i] == '/' {
			if seg == n {
				return topic[start:i]
			}
			seg++
			start = i + 1
		}
	}
	return ""
}
