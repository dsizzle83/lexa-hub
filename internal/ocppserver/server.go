// Package ocppserver wraps the lorenzodonini/ocpp-go library to provide an
// OCPP 2.0.1 Central System Management System (CSMS).
//
// TLS support follows Security Profile 2: TLS on the WebSocket transport plus
// HTTP Basic Authentication for identity.  The wolfSSL bridge is intentionally
// not used here; this package is pure-Go and decoupled from the grid-compliance
// mTLS layer.
package ocppserver

import (
	"crypto/tls"
	"log"

	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ws"
)

const (
	DefaultPort       = 8887
	// DefaultListenPath is the gorilla/mux path template the CSMS handler is
	// registered on.  The {id} segment captures the charging station identifier
	// that the client appends to its URL (e.g. ws://host:8887/ocpp/cs-001).
	DefaultListenPath = "/ocpp/{id}"
)

// Config holds all options for the OCPP CSMS.
type Config struct {
	// Port to listen on.  Defaults to DefaultPort if zero.
	Port int

	// TLS — both must be non-empty to enable TLS (Security Profile 2).
	CertPath string
	KeyPath  string

	// BasicAuth credentials.  Ignored when both are empty.
	BasicAuthUser string
	BasicAuthPass string
}

// Server is a running OCPP 2.0.1 CSMS instance.
type Server struct {
	csms ocpp2.CSMS
	cfg  Config
}

// New creates a CSMS, wires the default handler set, and returns a ready-to-start Server.
// Call Start to begin accepting WebSocket connections.
func New(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}

	var wsServer ws.WsServer
	if cfg.CertPath != "" && cfg.KeyPath != "" {
		wsServer = ws.NewTLSServer(cfg.CertPath, cfg.KeyPath, &tls.Config{
			MinVersion: tls.VersionTLS12,
		})
		log.Printf("[ocpp] TLS enabled (cert=%s)", cfg.CertPath)
	} else {
		wsServer = ws.NewServer()
	}

	if cfg.BasicAuthUser != "" {
		wsServer.SetBasicAuthHandler(func(user, pass string) bool {
			ok := user == cfg.BasicAuthUser && pass == cfg.BasicAuthPass
			if !ok {
				log.Printf("[ocpp] basic-auth rejected user=%q", user)
			}
			return ok
		})
	}

	csms := ocpp2.NewCSMS(nil, wsServer)

	h := &handler{}
	csms.SetProvisioningHandler(h)
	csms.SetAvailabilityHandler(h)

	csms.SetNewChargingStationHandler(func(cs ocpp2.ChargingStationConnection) {
		log.Printf("[ocpp] charging station connected  id=%s addr=%s", cs.ID(), cs.RemoteAddr())
	})
	csms.SetChargingStationDisconnectedHandler(func(cs ocpp2.ChargingStationConnection) {
		log.Printf("[ocpp] charging station disconnected id=%s", cs.ID())
	})

	return &Server{csms: csms, cfg: cfg}
}

// Start begins listening.  It blocks until the underlying WebSocket server
// exits, so callers should invoke it in a goroutine.
func (s *Server) Start() {
	log.Printf("[ocpp] CSMS listening on :%d%s", s.cfg.Port, DefaultListenPath)
	s.csms.Start(s.cfg.Port, DefaultListenPath)
}

// Stop shuts down the CSMS.
func (s *Server) Stop() {
	s.csms.Stop()
}

// CSMS returns the underlying ocpp2.CSMS for callers that need to send
// CSMS-initiated requests (e.g. ReserveNow, RemoteStart).
func (s *Server) CSMS() ocpp2.CSMS {
	return s.csms
}
