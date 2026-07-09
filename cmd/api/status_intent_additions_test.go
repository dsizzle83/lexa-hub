package main

import (
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestBuildStatus_ModeAndCloudLinkAdditive pins the additive /status fields
// this task adds (DEVICE_ROADMAP.md §4.3: "'/status' gains 'cloud_link' ...
// and 'mode'"): omitted until the first message, populated verbatim once
// one arrives, and never disturbing plan_test.go's existing assertions
// (that file is run unmodified as part of this package's suite).
func TestBuildStatus_ModeAndCloudLinkAdditive(t *testing.T) {
	store := newStateStore(nil, time.Minute)

	t.Run("omitted before any ModeStatus/CloudlinkStatus", func(t *testing.T) {
		resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})
		if resp.Mode != "" {
			t.Errorf("Mode = %q, want empty before any ModeStatus arrives", resp.Mode)
		}
		if resp.CloudLink != nil {
			t.Errorf("CloudLink = %+v, want nil before any CloudlinkStatus arrives", resp.CloudLink)
		}
	})

	store.onModeStatus(bus.TopicHubMode, bus.ModeStatus{Mode: "gateway", Since: 500})
	store.onCloudlinkStatus(bus.TopicCloudlinkStatus, bus.CloudlinkStatus{
		Connected: true, Endpoint: "ssl://example:8883", SpoolBytes: 4096, CertDaysLeft: 45,
	})

	t.Run("populated once messages arrive", func(t *testing.T) {
		resp := buildStatus(store.snapshot(), heartbeatStatus{State: heartbeatNever})
		if resp.Mode != "gateway" {
			t.Errorf("Mode = %q, want %q", resp.Mode, "gateway")
		}
		if resp.CloudLink == nil {
			t.Fatal("CloudLink is nil, want populated")
		}
		if !resp.CloudLink.Connected || resp.CloudLink.Endpoint != "ssl://example:8883" ||
			resp.CloudLink.SpoolBytes != 4096 || resp.CloudLink.CertDaysLeft != 45 {
			t.Errorf("CloudLink = %+v, want the CloudlinkStatus fields projected verbatim", resp.CloudLink)
		}
	})
}
