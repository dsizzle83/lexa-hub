// Package ocppserver16 wraps the lorenzodonini/ocpp-go library to provide an
// OCPP 1.6J Central System (CSMS) — the compatibility-mode sibling of this
// module's ocppserver package (OCPP 2.0.1). Scope is the Core and
// SmartCharging profiles only.
//
// TLS support follows the Security Profile 2 analog: TLS on the WebSocket
// transport plus HTTP Basic Authentication for identity.  The wolfSSL bridge
// is intentionally not used here; this package is pure-Go and decoupled from
// the grid-compliance mTLS layer.
package ocppserver16

import (
	"crypto/subtle"
	"crypto/tls"
	"log"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ws"
)

const (
	// DefaultPort is the 1.6J listener default. It deliberately differs from
	// ocppserver.DefaultPort (8887): ocpp-go gives each protocol version its
	// own ws.WsServer with its own Start(port, path), so a dual-stack
	// consumer runs two listeners side by side.
	DefaultPort = 8886
	// DefaultListenPath is the gorilla/mux path template the CSMS handler is
	// registered on.  The {id} segment captures the charge point identifier
	// that the client appends to its URL (e.g. ws://host:8886/ocpp/cp-001).
	DefaultListenPath = "/ocpp/{id}"
)

// Config holds all options for the OCPP 1.6 CSMS. Field-for-field the same
// shape as ocppserver.Config so a consumer can populate both listeners from
// one config block.
type Config struct {
	// Port to listen on.  Defaults to DefaultPort if zero.
	Port int

	// TLS — both must be non-empty to enable TLS (Security Profile 2 analog).
	CertPath string
	KeyPath  string

	// BasicAuth credentials.  Ignored when both are empty.
	BasicAuthUser string
	BasicAuthPass string
}

// Server is a running OCPP 1.6 CSMS instance.
type Server struct {
	cs  ocpp16.CentralSystem
	cfg Config
	h   *handler
}

// New creates a 1.6 CSMS, wires the default handler set (minimal accepted
// responses for every Core message), and returns a ready-to-start Server.
// Call SetHandlers to install consumer callbacks, then Start to begin
// accepting WebSocket connections.
func New(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}

	var wsServer ws.WsServer
	if cfg.CertPath != "" && cfg.KeyPath != "" {
		wsServer = ws.NewTLSServer(cfg.CertPath, cfg.KeyPath, &tls.Config{
			MinVersion: tls.VersionTLS12,
		})
		log.Printf("[ocpp16] TLS enabled (cert=%s)", cfg.CertPath)
	} else {
		wsServer = ws.NewServer()
	}

	if cfg.BasicAuthUser != "" {
		wsServer.SetBasicAuthHandler(func(user, pass string) bool {
			// Constant-time comparison so credential checking leaks no
			// timing signal (& to avoid short-circuiting between the two).
			ok := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.BasicAuthUser)) &
				subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.BasicAuthPass))
			if ok != 1 {
				log.Printf("[ocpp16] basic-auth rejected user=%q", user)
			}
			return ok == 1
		})
	}

	cs := ocpp16.NewCentralSystem(nil, wsServer)

	h := &handler{}
	cs.SetCoreHandler(h)
	// SmartCharging messages are all CSMS-initiated in 1.6 (SetChargingProfile /
	// ClearChargingProfile / GetCompositeSchedule); the CSMS-side handler
	// interface is empty, but registering it documents the profile scope.
	cs.SetSmartChargingHandler(h)

	cs.SetNewChargePointHandler(func(cp ocpp16.ChargePointConnection) {
		log.Printf("[ocpp16] charge point connected  id=%s addr=%s", cp.ID(), cp.RemoteAddr())
	})
	cs.SetChargePointDisconnectedHandler(func(cp ocpp16.ChargePointConnection) {
		log.Printf("[ocpp16] charge point disconnected id=%s", cp.ID())
	})

	return &Server{cs: cs, cfg: cfg, h: h}
}

// SetHandlers installs the consumer's message callbacks (see Handlers). Any
// callback left nil keeps this package's default minimal-accept behavior.
// Must be called before Start — the handler fields are read from the
// WebSocket serving goroutines without further synchronization.
func (s *Server) SetHandlers(h Handlers) {
	s.h.user = h
}

// Start begins listening.  It blocks until the underlying WebSocket server
// exits, so callers should invoke it in a goroutine.
func (s *Server) Start() {
	log.Printf("[ocpp16] CSMS listening on :%d%s", s.cfg.Port, DefaultListenPath)
	s.cs.Start(s.cfg.Port, DefaultListenPath)
}

// Stop shuts down the CSMS.
func (s *Server) Stop() {
	s.cs.Stop()
}

// CentralSystem returns the underlying ocpp16.CentralSystem for callers that
// need to send CSMS-initiated requests (e.g. SetChargingProfile,
// ClearChargingProfile, TriggerMessage, RemoteStart/StopTransaction).
func (s *Server) CentralSystem() ocpp16.CentralSystem {
	return s.cs
}
