package main

import (
	"fmt"
	"net"
	"testing"
	"time"

	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ws"

	"lexa-proto/ocppserver"
)

// freeSP2Port returns an unused TCP port on localhost (mirrors
// lexa-proto/ocppserver's own freePort test helper — kept local here since
// that helper is unexported in its package).
func freeSP2Port(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeSP2Port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// waitSP2Listening blocks until port accepts TCP connections or 3s elapse.
func waitSP2Listening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d never started accepting connections", port)
}

// TestOCPPSecurityProfile2_BasicAuth is the negative-auth regression named by
// TASK-074 (review §10.1/W7; 09 Security hard gate "OCPP: security profile
// ≥2 enabled"). It exercises the exact ocppserver.Config wiring
// cmd/ocpp/main.go uses in production — BasicAuthUser/BasicAuthPass ->
// ocppserver.New -> ws.Server.SetBasicAuthHandler, constant-time compare
// (lexa-proto/ocppserver/server.go) — and asserts:
//   - a charging station with the wrong password is rejected
//   - a charging station with the wrong username is rejected
//   - a charging station with the right credentials connects
//
// Runs over plain ws:// (no TLS cert fixture needed): ocppserver.New enables
// Basic Auth whenever BasicAuthUser is non-empty, independent of whether
// CertPath/KeyPath are set, so this covers the auth-handler logic itself —
// the TLS half already has coverage via evsim's -tls-ca path and is proven
// on the bench per docs/BENCH.md's OCPP SP2 runbook.
func TestOCPPSecurityProfile2_BasicAuth(t *testing.T) {
	const (
		authUser = "evse-bench"
		authPass = "correct-horse-battery-staple"
	)

	port := freeSP2Port(t)
	srv := ocppserver.New(ocppserver.Config{
		Port:          port,
		BasicAuthUser: authUser,
		BasicAuthPass: authPass,
	})
	go srv.Start()
	defer srv.Stop()
	waitSP2Listening(t, port)

	dial := func(id, user, pass string) error {
		client := ws.NewClient()
		client.SetBasicAuth(user, pass)
		cs := ocpp2.NewChargingStation(id, nil, client)
		err := cs.Start(fmt.Sprintf("ws://127.0.0.1:%d/ocpp", port))
		if err == nil {
			cs.Stop()
		}
		return err
	}

	t.Run("wrong password rejected", func(t *testing.T) {
		if err := dial("test-cs-sp2-badpass", authUser, "wrong-password"); err == nil {
			t.Fatal("charging station with wrong Basic Auth password connected; want rejection")
		}
	})

	t.Run("wrong username rejected", func(t *testing.T) {
		if err := dial("test-cs-sp2-baduser", "not-"+authUser, authPass); err == nil {
			t.Fatal("charging station with wrong Basic Auth username connected; want rejection")
		}
	})

	t.Run("correct credentials accepted", func(t *testing.T) {
		if err := dial("test-cs-sp2-ok", authUser, authPass); err != nil {
			t.Fatalf("charging station with correct Basic Auth credentials rejected: %v", err)
		}
	})
}
