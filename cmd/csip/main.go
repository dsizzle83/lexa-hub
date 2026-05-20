// lexa-csip is the IEEE 2030.5 / CSIP northbound client service.
//
// It maintains a wolfSSL mTLS connection to the utility CSIP server, walks the
// resource tree on a configurable interval, resolves the active DER control via
// the scheduler, and publishes the result to lexa/csip/control (retained).
//
// It also handles the GEN.044 / CORE-022 response POST state machine so the
// server knows when events are received and started.
//
// Usage:
//
//	lexa-csip [-config /etc/lexa/csip.json]
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/csip/discovery"
	"lexa-hub/internal/csip/identity"
	"lexa-hub/internal/csip/model"
	"lexa-hub/internal/csip/scheduler"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/wolfssl"
)

func main() {
	cfgPath := flag.String("config", "/etc/lexa/csip.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-csip: load config: %v", err)
	}

	wolfssl.Init()
	defer wolfssl.Cleanup()

	tlsCfg := tlsclient.Config{
		ServerAddr:     cfg.Server,
		CACertPath:     cfg.CACert,
		ClientCertPath: cfg.ClientCert,
		ClientKeyPath:  cfg.ClientKey,
	}
	fetcherDisc, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-csip: init TLS fetcher (discovery): %v", err)
	}
	defer fetcherDisc.Free()

	fetcherResp, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-csip: init TLS fetcher (response): %v", err)
	}
	defer fetcherResp.Free()

	lfdi := cfg.LFDI
	if lfdi == "" {
		lfdi, err = lfdiFromCert(cfg.ClientCert)
		if err != nil {
			log.Fatalf("lexa-csip: derive LFDI: %v", err)
		}
	}
	log.Printf("lexa-csip: LFDI=%s server=%s", lfdi, cfg.Server)

	mc, err := mqttutil.Connect(cfg.MQTTBroker, cfg.MQTTClientID)
	if err != nil {
		log.Fatalf("lexa-csip: %v", err)
	}
	defer mc.Disconnect(500)

	ctx, cancel := context.WithCancel(context.Background())

	sched := scheduler.New()
	respTracker := newResponseTracker(fetcherResp, lfdi, cfg.ResponseSetPath)

	// Run the first discovery immediately, then loop.
	go func() {
		runDiscovery(mc, fetcherDisc, lfdi, sched, respTracker, cfg)
		ticker := time.NewTicker(cfg.DiscoveryInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runDiscovery(mc, fetcherDisc, lfdi, sched, respTracker, cfg)
			}
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("lexa-csip: shutting down")
	cancel()
}

func runDiscovery(
	mc mqtt.Client,
	fetcher *tlsclient.WolfSSLFetcher,
	lfdi string,
	sched *scheduler.Scheduler,
	rt *responseTracker,
	cfg *Config,
) {
	walker := discovery.NewWalker(fetcher, lfdi)
	tree, err := walker.Discover("/dcap")
	if err != nil {
		log.Printf("lexa-csip: discovery error: %v", err)
		publishNoControl(mc, 0)
		return
	}

	serverNow := scheduler.ServerNow(tree.ClockOffset)
	active := sched.Evaluate(tree.Programs, serverNow)

	msg := toActiveControl(active, tree.ClockOffset)
	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPControl, msg); err != nil {
		log.Printf("lexa-csip: publish control: %v", err)
	}
	log.Printf("lexa-csip: discovery OK programs=%d source=%s mrid=%s clockOffset=%ds",
		len(tree.Programs), msg.Source, msg.MRID, tree.ClockOffset)

	rt.update(tree, active)
}

// toActiveControl converts a scheduler.ActiveControl to the MQTT bus message.
func toActiveControl(ac *scheduler.ActiveControl, clockOffset int64) bus.ActiveControl {
	msg := bus.ActiveControl{
		Source:      "none",
		ClockOffset: clockOffset,
		Ts:          time.Now().Unix(),
	}
	if ac == nil {
		return msg
	}
	msg.Source = ac.Source
	msg.MRID = ac.MRID
	msg.ValidUntil = ac.ValidUntil
	msg.Connect = ac.Base.OpModConnect
	if ac.Base.OpModExpLimW != nil {
		w := apW(ac.Base.OpModExpLimW)
		msg.ExpLimW = &w
	}
	if ac.Base.OpModImpLimW != nil {
		w := apW(ac.Base.OpModImpLimW)
		msg.ImpLimW = &w
	}
	if ac.Base.OpModMaxLimW != nil {
		w := apW(ac.Base.OpModMaxLimW)
		msg.MaxLimW = &w
	}
	if ac.Base.OpModFixedW != nil {
		w := apW(ac.Base.OpModFixedW)
		msg.FixedW = &w
	}
	return msg
}

func publishNoControl(mc mqtt.Client, clockOffset int64) {
	msg := bus.ActiveControl{Source: "none", ClockOffset: clockOffset, Ts: time.Now().Unix()}
	if err := mqttutil.PublishJSONRetained(mc, bus.TopicCSIPControl, msg); err != nil {
		log.Printf("lexa-csip: publish no-control: %v", err)
	}
}

func apW(ap *model.ActivePower) float64 {
	return float64(ap.Value) * math.Pow10(int(ap.Multiplier))
}

func lfdiFromCert(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	l, _ := identity.FromCertificate(cert)
	return l.String(), nil
}

// ── Response tracker (GEN.044 / CORE-022) ────────────────────────────────────

type responseTracker struct {
	fetcher         *tlsclient.WolfSSLFetcher
	lfdi            string
	responseSetPath string
	clockOffset     int64
	received        map[string]bool
	activeMRID      string
}

func newResponseTracker(f *tlsclient.WolfSSLFetcher, lfdi, path string) *responseTracker {
	return &responseTracker{
		fetcher:         f,
		lfdi:            lfdi,
		responseSetPath: path,
		received:        make(map[string]bool),
	}
}

func (rt *responseTracker) update(tree *discovery.ResourceTree, active *scheduler.ActiveControl) {
	rt.clockOffset = tree.ClockOffset

	for _, ps := range tree.Programs {
		if ps.Controls == nil {
			continue
		}
		for _, ctrl := range ps.Controls.DERControl {
			if ctrl.EventStatus != nil && ctrl.EventStatus.CurrentStatus == 6 {
				continue
			}
			if !rt.received[ctrl.MRID] {
				rt.postResponse(ctrl.MRID, model.ResponseEventReceived)
				rt.received[ctrl.MRID] = true
			}
		}
	}

	if active == nil || active.Source == "default" {
		if rt.activeMRID != "" {
			rt.postResponse(rt.activeMRID, model.ResponseEventCompleted)
			rt.activeMRID = ""
		}
		return
	}

	if active.MRID != rt.activeMRID {
		if rt.activeMRID != "" {
			rt.postResponse(rt.activeMRID, model.ResponseEventCompleted)
		}
		rt.postResponse(active.MRID, model.ResponseEventStarted)
		rt.activeMRID = active.MRID
	}

	if active.ValidUntil > 0 && scheduler.ServerNow(tree.ClockOffset) >= active.ValidUntil {
		rt.postResponse(active.MRID, model.ResponseEventCompleted)
		rt.activeMRID = ""
	}
}

func (rt *responseTracker) postResponse(mrid string, status uint8) {
	resp := model.Response{
		CreatedDateTime: scheduler.ServerNow(rt.clockOffset),
		EndDeviceLFDI:   rt.lfdi,
		Status:          status,
		Subject:         mrid,
	}
	body, err := xml.Marshal(&resp)
	if err != nil {
		log.Printf("lexa-csip: marshal Response: %v", err)
		return
	}
	if _, _, err = rt.fetcher.Post(rt.responseSetPath, body, "application/sep+xml"); err != nil {
		log.Printf("lexa-csip: POST response (mrid=%s status=%d): %v", mrid, status, err)
		return
	}
	names := map[uint8]string{1: "Received", 2: "Started", 3: "Completed"}
	log.Printf("lexa-csip: response posted: %s mrid=%s", names[status], mrid)
}
