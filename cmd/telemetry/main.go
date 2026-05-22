// lexa-telemetry subscribes to device measurements on the MQTT bus and
// forwards them to the CSIP server as MirrorUsagePoint readings (MUP POST).
//
// On startup it registers one MUP per configured device with the server, then
// posts batched meter readings every mup_post_rate_s seconds.
//
// Usage:
//
//	lexa-telemetry [-config /etc/lexa/telemetry.json]
package main

import (
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/identity"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/mqttutil"
	"lexa-hub/internal/southbound/device"
	"lexa-hub/internal/tlsclient"
	"lexa-hub/internal/wolfssl"
)

type mupEntry struct {
	device string
	path   string
	fails  int
}

func main() {
	cfgPath := flag.String("config", "/etc/lexa/telemetry.json", "path to JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("lexa-telemetry: load config: %v", err)
	}

	wolfssl.Init()
	defer wolfssl.Cleanup()

	tlsCfg := tlsclient.Config{
		ServerAddr:     cfg.Server,
		CACertPath:     cfg.CACert,
		ClientCertPath: cfg.ClientCert,
		ClientKeyPath:  cfg.ClientKey,
	}
	fetcher, err := tlsclient.NewWolfSSLFetcher(tlsCfg)
	if err != nil {
		log.Fatalf("lexa-telemetry: init TLS fetcher: %v", err)
	}
	defer fetcher.Free()

	lfdi := cfg.LFDI
	if lfdi == "" {
		lfdi, err = lfdiFromCert(cfg.ClientCert)
		if err != nil {
			log.Fatalf("lexa-telemetry: derive LFDI: %v", err)
		}
	}
	log.Printf("lexa-telemetry: LFDI=%s server=%s", lfdi, cfg.Server)

	mc, err := mqttutil.Connect(cfg.MQTTBroker, cfg.MQTTClientID)
	if err != nil {
		log.Fatalf("lexa-telemetry: %v", err)
	}
	defer mc.Disconnect(500)

	// Register MUPs for each configured device.
	var mups []mupEntry
	for _, dev := range cfg.Devices {
		path, err := registerMUP(fetcher, lfdi, dev, cfg.MUPPostRateS)
		if err != nil {
			log.Printf("lexa-telemetry: MUP register %s: %v — skipping", dev, err)
			continue
		}
		mups = append(mups, mupEntry{device: dev, path: path})
	}
	if len(mups) == 0 {
		log.Fatal("lexa-telemetry: no MUPs registered — exiting")
	}

	// Maintain the latest measurement per device.
	var mu sync.RWMutex
	latest := make(map[string]device.Measurements)

	// Initialise to NaN so we don't post zeros before the first poll.
	for _, dev := range cfg.Devices {
		latest[dev] = device.Measurements{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}
	}

	// Subscribe to measurements from the modbus service.
	if err := mqttutil.Subscribe(mc, bus.SubMeasurements, func(_ string, msg bus.Measurement) {
		mu.Lock()
		m := latest[msg.Device]
		if msg.W != nil {
			m.W = *msg.W
		}
		if msg.V != nil {
			m.V = *msg.V
		}
		if msg.Hz != nil {
			m.Hz = *msg.Hz
		}
		latest[msg.Device] = m
		mu.Unlock()
	}); err != nil {
		log.Fatalf("lexa-telemetry: subscribe measurements: %v", err)
	}

	// Subscribe to clock offset updates from the CSIP service.
	var clockOffset int64
	var clockMu sync.Mutex
	if err := mqttutil.Subscribe(mc, bus.TopicCSIPControl, func(_ string, msg bus.ActiveControl) {
		clockMu.Lock()
		clockOffset = msg.ClockOffset
		clockMu.Unlock()
	}); err != nil {
		log.Printf("lexa-telemetry: subscribe csip control: %v", err)
	}

	ticker := time.NewTicker(cfg.MUPPostRate())
	defer ticker.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-quit:
			log.Println("lexa-telemetry: shutting down")
			return

		case <-ticker.C:
			mu.RLock()
			snap := make(map[string]device.Measurements, len(latest))
			for k, v := range latest {
				snap[k] = v
			}
			mu.RUnlock()

			clockMu.Lock()
			offset := clockOffset
			clockMu.Unlock()

			for i := range mups {
				ep := &mups[i]
				m := snap[ep.device]
				err := postMeasurements(fetcher, ep.device, ep.path, m, offset, cfg.MUPPostRateS)
				if err != nil {
					ep.fails++
					if ep.fails >= 3 {
						log.Printf("lexa-telemetry: re-registering MUP for %s after %d failures", ep.device, ep.fails)
						newPath, rerr := registerMUP(fetcher, lfdi, ep.device, cfg.MUPPostRateS)
						if rerr == nil {
							ep.path = newPath
							ep.fails = 0
						}
					}
				} else {
					ep.fails = 0
				}
			}
		}
	}
}

func registerMUP(fetcher *tlsclient.WolfSSLFetcher, lfdi, devName string, rateS int) (string, error) {
	prefix := lfdi
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	mup := model.MirrorUsagePoint{
		MRID:                prefix + "-" + devName,
		Description:         devName + " Measurements (W/V/Hz)",
		RoleFlags:           0x0002,
		ServiceCategoryKind: 0,
		Status:              1,
		DeviceLFDI:          lfdi,
		PostRate:            uint32(rateS),
	}
	body, err := xml.Marshal(&mup)
	if err != nil {
		return "", err
	}
	_, loc, err := fetcher.Post("/mup", body, "application/sep+xml")
	if err != nil {
		return "", err
	}
	log.Printf("lexa-telemetry: MUP registered: %s → %s", devName, loc)
	return loc, nil
}

func postMeasurements(
	fetcher *tlsclient.WolfSSLFetcher,
	devName, mupPath string,
	m device.Measurements,
	clockOffset int64,
	intervalS int,
) error {
	now := time.Now().Unix() + clockOffset
	dur := uint32(intervalS)
	start := now - int64(dur)

	var readings []model.Reading
	if !math.IsNaN(m.W) {
		readings = append(readings, model.Reading{
			LocalID:    1,
			Value:      int64(math.Round(m.W)),
			TimePeriod: &model.DateTimeInterval{Start: start, Duration: dur},
		})
	}
	if !math.IsNaN(m.V) {
		readings = append(readings, model.Reading{
			LocalID:    2,
			Value:      int64(math.Round(m.V * 100)),
			TimePeriod: &model.DateTimeInterval{Start: start, Duration: dur},
		})
	}
	if !math.IsNaN(m.Hz) {
		readings = append(readings, model.Reading{
			LocalID:    3,
			Value:      int64(math.Round(m.Hz * 100)),
			TimePeriod: &model.DateTimeInterval{Start: start, Duration: dur},
		})
	}
	if len(readings) == 0 {
		return nil
	}

	mmr := model.MirrorMeterReading{
		MirrorReadingSet: []model.MirrorReadingSet{{
			StartTime: start,
			Duration:  dur,
			Reading:   readings,
		}},
	}
	body, err := xml.Marshal(&mmr)
	if err != nil {
		return err
	}
	if _, _, err = fetcher.Post(mupPath, body, "application/sep+xml"); err != nil {
		log.Printf("lexa-telemetry: POST %s: %v", devName, err)
		return err
	}
	log.Printf("lexa-telemetry: posted %s W=%.0f V=%.1f Hz=%.2f", devName, m.W, m.V, m.Hz)
	return nil
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
