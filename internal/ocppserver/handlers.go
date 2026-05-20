package ocppserver

import (
	"log"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// handler implements provisioning.CSMSHandler and availability.CSMSHandler.
// It logs every incoming message and returns the minimal accepted response.
type handler struct{}

// ── provisioning.CSMSHandler ─────────────────────────────────────────────────

func (h *handler) OnBootNotification(
	csID string, req *provisioning.BootNotificationRequest,
) (*provisioning.BootNotificationResponse, error) {
	log.Printf("[ocpp] BootNotification  cs=%s reason=%s model=%s vendor=%s",
		csID, req.Reason, req.ChargingStation.Model, req.ChargingStation.VendorName)
	resp := provisioning.NewBootNotificationResponse(
		types.NewDateTime(time.Now()),
		60, // heartbeat interval in seconds
		provisioning.RegistrationStatusAccepted,
	)
	return resp, nil
}

func (h *handler) OnNotifyReport(
	csID string, req *provisioning.NotifyReportRequest,
) (*provisioning.NotifyReportResponse, error) {
	log.Printf("[ocpp] NotifyReport cs=%s requestId=%d seqNo=%d", csID, req.RequestID, req.SeqNo)
	return &provisioning.NotifyReportResponse{}, nil
}

// ── availability.CSMSHandler ─────────────────────────────────────────────────

func (h *handler) OnHeartbeat(
	csID string, req *availability.HeartbeatRequest,
) (*availability.HeartbeatResponse, error) {
	now := types.NewDateTime(time.Now())
	log.Printf("[ocpp] Heartbeat cs=%s serverTime=%s", csID, now.FormatTimestamp())
	return availability.NewHeartbeatResponse(*now), nil
}

func (h *handler) OnStatusNotification(
	csID string, req *availability.StatusNotificationRequest,
) (*availability.StatusNotificationResponse, error) {
	log.Printf("[ocpp] StatusNotification cs=%s evse=%d connector=%d status=%s",
		csID, req.EvseID, req.ConnectorID, req.ConnectorStatus)
	return &availability.StatusNotificationResponse{}, nil
}
