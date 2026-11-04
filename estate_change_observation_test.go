package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEstateDelayedProvisioningIsCountedAfterOnDemandBaseline(t *testing.T) {
	baselineAt := estateReportTestTime(t, "2026-11-04T12:10:00Z")
	provisionedAt := baselineAt.Add(-5 * time.Minute)
	observedAt := baselineAt.Add(5 * time.Minute)
	history, opts := estateNowTestFixture(t, baselineAt)
	opts.PolicyEnabled = true
	history.OptionsKey = estateOptionsKey(opts)
	history.Days[0].At, history.Days[0].FirstAt = baselineAt, baselineAt
	history.Days[0].Policy = &EstatePolicy{At: baselineAt.Add(-10 * time.Minute), LastProvisionedAt: baselineAt.Add(-time.Hour)}
	if _, err := saveEstateReportNow(baselineAt, opts, history, estateReportSections, nil, true); err != nil {
		t.Fatal(err)
	}
	next := EstateSnapshot{
		At: observedAt, FirstAt: observedAt, Day: observedAt.Format("2006-01-02"), InventoryComplete: true,
		Policy: &EstatePolicy{At: observedAt, LastProvisionedAt: provisionedAt},
	}
	estateApplySnapshot(&history, next, nil, opts, time.UTC)
	if len(history.Changes) != 1 || history.Changes[0].Kind != "policy_provisioned" || !history.Changes[0].At.Equal(provisionedAt) || history.Changes[0].ObservedAt == nil || !history.Changes[0].ObservedAt.Equal(observedAt) {
		t.Fatalf("delayed provisioning lost either source or observation timestamp: %+v", history.Changes)
	}
	for _, input := range []EstateHistory{history, estateCloneHistory(history)} {
		got := estateChangesSinceReport(input, opts, observedAt)
		if got.Status != "available" || got.Count != 1 || !got.Since.Equal(baselineAt) {
			t.Fatalf("newly observed cached policy event was hidden behind the baseline: %+v", got)
		}
	}
	report, err := saveEstateReportNow(observedAt, opts, history, []string{"changes"}, nil, false)
	if err != nil || len(report.Changes) != 1 {
		t.Fatalf("new report omitted delayed event: %+v %v", report.Changes, err)
	}
	body, err := renderEstateReportHTML(report, "illumio-light")
	if err != nil || !strings.Contains(string(body), "<td>Nov 4, 2026 12:15 UTC</td>") || strings.Contains(string(body), "<td>Nov 4, 2026 12:05 UTC</td>") || !strings.Contains(string(body), provisionedAt.Format(time.RFC3339)) {
		t.Fatalf("Observed column mislabels source event time or hides provisioning time: %s %v", body, err)
	}
	// Re-reading the same cached policy cannot produce another observed event.
	next.At = observedAt.Add(5 * time.Minute)
	estateApplySnapshot(&history, next, nil, opts, time.UTC)
	if len(history.Changes) != 1 {
		t.Fatalf("cached provisioning was counted twice: %+v", history.Changes)
	}
}

func TestEstateChangeReportsAndRetentionUseDiscoveryTime(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-09T12:00:00Z")
	window := estateCurrentWeek(now, time.UTC)
	observed := window.Start.Add(5 * time.Minute)
	oldObserved := now.AddDate(0, 0, -100)
	changes := []EstateChange{
		{At: window.Start.Add(-5 * time.Minute), ObservedAt: &observed, Kind: "policy_provisioned", Name: "discovered_this_week"},
		{At: now.AddDate(0, 0, -100), ObservedAt: &observed, Kind: "policy_provisioned", Name: "old_event_discovered_now"},
		{At: now, ObservedAt: &oldObserved, Name: "old_observation"},
		{At: now, Name: "legacy_current"},
		{At: now.AddDate(0, 0, -100), Name: "legacy_old"},
	}
	history := EstateHistory{Scope: "estate", Timezone: "UTC", Changes: changes}
	current := buildEstateReport(history, EstateOptions{ChangeLogEnabled: true}, window, []string{"changes"}, now, nil, true)
	if len(current.Changes) != 3 || current.Changes[0].Name != "discovered_this_week" || current.Changes[1].Name != "old_event_discovered_now" || current.Changes[2].Name != "legacy_current" {
		t.Fatalf("current report membership used source event time: %+v", current.Changes)
	}
	previous := buildEstateWeeklyReport(history, EstateOptions{ChangeLogEnabled: true}, estateCompletedWeek(now, time.UTC), []string{"changes"}, now, nil)
	if len(previous.Changes) != 0 {
		t.Fatalf("report backdated a subsequently discovered event into the previous week: %+v", previous.Changes)
	}
	estatePruneHistory(&history, now, time.UTC, 90)
	if len(history.Changes) != 3 || history.Changes[0].Name != "discovered_this_week" || history.Changes[1].Name != "old_event_discovered_now" || history.Changes[2].Name != "legacy_current" {
		t.Fatalf("retention discarded a newly discovered old policy event: %+v", history.Changes)
	}
}

func TestEstateChangeObservationFallbackPreservesLegacyHistory(t *testing.T) {
	at := estateReportTestTime(t, "2026-11-04T12:05:00Z")
	observedAt := at.Add(10 * time.Minute)
	zero := time.Time{}
	for _, test := range []struct {
		name   string
		change EstateChange
		want   time.Time
	}{
		{"legacy", EstateChange{At: at}, at},
		{"zero_optional", EstateChange{At: at, ObservedAt: &zero}, at},
		{"discovered", EstateChange{At: at, ObservedAt: &observedAt}, observedAt},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.change)
			var restored EstateChange
			if err != nil || json.Unmarshal(body, &restored) != nil || !estateChangeObservedAt(restored).Equal(test.want) || !restored.At.Equal(at) {
				t.Fatalf("observation/source timestamp changed on disk round trip: %+v %v", restored, err)
			}
		})
	}
}
