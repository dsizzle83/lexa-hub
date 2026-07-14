package main

import (
	"fmt"
	"testing"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ws"

	"lexa-proto/ocppserver16"
)

// TestOCPP16SecurityProfile2_BasicAuth is the sp2_auth_test.go analog for the
// WP-12 1.6J listener: the exact ocppserver16.Config wiring cmd/ocpp/main.go
// uses in production (the SAME BasicAuthUser/BasicAuthPass fields as the
// 2.0.1 listener → ocppserver16.New → ws.Server.SetBasicAuthHandler,
// constant-time compare) rejects wrong credentials and accepts right ones.
// Runs over plain ws:// for the same reason as the 2.0.1 test: Basic Auth is
// enabled whenever BasicAuthUser is non-empty, independent of TLS.
func TestOCPP16SecurityProfile2_BasicAuth(t *testing.T) {
	const (
		authUser = "evse-bench"
		authPass = "correct-horse-battery-staple"
	)

	port := freeSP2Port(t)
	srv := ocppserver16.New(ocppserver16.Config{
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
		cp := ocpp16.NewChargePoint(id, nil, client)
		err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d/ocpp", port))
		if err == nil {
			cp.Stop()
		}
		return err
	}

	t.Run("wrong password rejected", func(t *testing.T) {
		if err := dial("test-cp16-badpass", authUser, "wrong-password"); err == nil {
			t.Fatal("1.6 charge point with wrong Basic Auth password connected; want rejection")
		}
	})

	t.Run("wrong username rejected", func(t *testing.T) {
		if err := dial("test-cp16-baduser", "not-"+authUser, authPass); err == nil {
			t.Fatal("1.6 charge point with wrong Basic Auth username connected; want rejection")
		}
	})

	t.Run("correct credentials accepted", func(t *testing.T) {
		if err := dial("test-cp16-ok", authUser, authPass); err != nil {
			t.Fatalf("1.6 charge point with correct Basic Auth credentials rejected: %v", err)
		}
	})
}
