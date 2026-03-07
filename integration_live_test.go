package main

import (
	"strings"
	"testing"
	"time"
)

func TestLiveIntegrationFromConfig(t *testing.T) {
	if !loadConfig() {
		t.Fatalf("config.json is missing or incomplete")
	}

	start := time.Now()
	stats := getIllumioStats()
	elapsed := time.Since(start)

	if stats.Timestamp.IsZero() {
		t.Fatalf("stats timestamp was not populated")
	}
	if elapsed > 5*time.Minute {
		t.Fatalf("live integration call exceeded 5 minutes: %v", elapsed)
	}

	if stats.Workloads.Status.Error != "" && strings.Contains(strings.ToLower(stats.Workloads.Status.Error), "404") {
		t.Fatalf("workloads endpoint returned 404: %s", stats.Workloads.Status.Error)
	}
	if stats.Tampering.Status.Error != "" && strings.Contains(strings.ToLower(stats.Tampering.Status.Error), "404") {
		t.Fatalf("events endpoint returned 404: %s", stats.Tampering.Status.Error)
	}
	if stats.Blocked.Status.Error != "" && strings.Contains(strings.ToLower(stats.Blocked.Status.Error), "404") {
		t.Fatalf("blocked traffic endpoints returned 404: %s", stats.Blocked.Status.Error)
	}

	t.Logf("workloads: success=%v total=%d err=%q", stats.Workloads.Status.Success, stats.Workloads.Total, stats.Workloads.Status.Error)
	t.Logf("ven: success=%v warnings=%d errors=%d err=%q", stats.VENStatus.Status.Success, len(stats.VENStatus.Warning), len(stats.VENStatus.Error), stats.VENStatus.Status.Error)
	t.Logf("tampering: success=%v count=%d err=%q", stats.Tampering.Status.Success, stats.Tampering.Count, stats.Tampering.Status.Error)
	t.Logf("blocked: success=%v prod=%d nonprod=%d err=%q", stats.Blocked.Status.Success, stats.Blocked.PROD, stats.Blocked.NONPROD, stats.Blocked.Status.Error)
}
