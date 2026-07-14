package ocppserver16

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// Handlers is the consumer-facing registration seam: optional callbacks for
// the charge-point-initiated Core messages a CSMS bridge cares about. A nil
// callback keeps the package default (log + minimal accepted response), so a
// consumer overrides only what it consumes — unlike ocpp-go's
// core.CentralSystemHandler, which would force all eight Core methods onto
// every consumer.
type Handlers struct {
	OnBootNotification   func(cpID string, req *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error)
	OnStatusNotification func(cpID string, req *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error)
	OnStartTransaction   func(cpID string, req *core.StartTransactionRequest) (*core.StartTransactionConfirmation, error)
	OnStopTransaction    func(cpID string, req *core.StopTransactionRequest) (*core.StopTransactionConfirmation, error)
	OnMeterValues        func(cpID string, req *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error)
}

// handler implements the core (and empty smartcharging) CSMS handler
// interfaces. Seam-covered messages delegate to the consumer's Handlers when
// set; everything else logs and returns the minimal accepted response.
type handler struct {
	user Handlers

	// nextTxID assigns transaction IDs for the DEFAULT StartTransaction
	// response only. A consumer that owns session lifecycle (the mqttBridge)
	// installs OnStartTransaction and assigns its own IDs.
	nextTxID int32
}

// ── core.CentralSystemHandler ────────────────────────────────────────────────

func (h *handler) OnAuthorize(
	cpID string, req *core.AuthorizeRequest,
) (*core.AuthorizeConfirmation, error) {
	log.Printf("[ocpp16] Authorize cs=%s idTag=%s", cpID, req.IdTag)
	return core.NewAuthorizationConfirmation(types.NewIdTagInfo(types.AuthorizationStatusAccepted)), nil
}

func (h *handler) OnBootNotification(
	cpID string, req *core.BootNotificationRequest,
) (*core.BootNotificationConfirmation, error) {
	if h.user.OnBootNotification != nil {
		return h.user.OnBootNotification(cpID, req)
	}
	log.Printf("[ocpp16] BootNotification  cs=%s model=%s vendor=%s",
		cpID, req.ChargePointModel, req.ChargePointVendor)
	return core.NewBootNotificationConfirmation(
		types.NewDateTime(time.Now()),
		60, // heartbeat interval in seconds
		core.RegistrationStatusAccepted,
	), nil
}

func (h *handler) OnDataTransfer(
	cpID string, req *core.DataTransferRequest,
) (*core.DataTransferConfirmation, error) {
	log.Printf("[ocpp16] DataTransfer cs=%s vendorId=%s messageId=%s", cpID, req.VendorId, req.MessageId)
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}

func (h *handler) OnHeartbeat(
	cpID string, req *core.HeartbeatRequest,
) (*core.HeartbeatConfirmation, error) {
	now := types.NewDateTime(time.Now())
	log.Printf("[ocpp16] Heartbeat cs=%s serverTime=%s", cpID, now.FormatTimestamp())
	return core.NewHeartbeatConfirmation(now), nil
}

func (h *handler) OnMeterValues(
	cpID string, req *core.MeterValuesRequest,
) (*core.MeterValuesConfirmation, error) {
	if h.user.OnMeterValues != nil {
		return h.user.OnMeterValues(cpID, req)
	}
	log.Printf("[ocpp16] MeterValues cs=%s connector=%d values=%d", cpID, req.ConnectorId, len(req.MeterValue))
	return core.NewMeterValuesConfirmation(), nil
}

func (h *handler) OnStatusNotification(
	cpID string, req *core.StatusNotificationRequest,
) (*core.StatusNotificationConfirmation, error) {
	if h.user.OnStatusNotification != nil {
		return h.user.OnStatusNotification(cpID, req)
	}
	log.Printf("[ocpp16] StatusNotification cs=%s connector=%d status=%s errorCode=%s",
		cpID, req.ConnectorId, req.Status, req.ErrorCode)
	return core.NewStatusNotificationConfirmation(), nil
}

func (h *handler) OnStartTransaction(
	cpID string, req *core.StartTransactionRequest,
) (*core.StartTransactionConfirmation, error) {
	if h.user.OnStartTransaction != nil {
		return h.user.OnStartTransaction(cpID, req)
	}
	txID := int(atomic.AddInt32(&h.nextTxID, 1))
	log.Printf("[ocpp16] StartTransaction cs=%s connector=%d idTag=%s meterStart=%d → tx=%d",
		cpID, req.ConnectorId, req.IdTag, req.MeterStart, txID)
	return core.NewStartTransactionConfirmation(
		types.NewIdTagInfo(types.AuthorizationStatusAccepted), txID), nil
}

func (h *handler) OnStopTransaction(
	cpID string, req *core.StopTransactionRequest,
) (*core.StopTransactionConfirmation, error) {
	if h.user.OnStopTransaction != nil {
		return h.user.OnStopTransaction(cpID, req)
	}
	log.Printf("[ocpp16] StopTransaction cs=%s tx=%d meterStop=%d reason=%s",
		cpID, req.TransactionId, req.MeterStop, req.Reason)
	return core.NewStopTransactionConfirmation(), nil
}

// ── smartcharging.CentralSystemHandler ───────────────────────────────────────
//
// The 1.6 SmartCharging CSMS handler interface has no methods (all
// SmartCharging messages are CSMS-initiated); handler satisfies it as-is.
