package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func estateChangeSummaryTestDir(t *testing.T) {
	t.Helper()
	previous := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = previous })
}

func estateChangeSummaryTestRun(t *testing.T, scope, zone, week, generated string) estateReportRun {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	created := estateReportTestTime(t, generated)
	window, err := estateRequestedWeek(week, created, loc)
	if err != nil {
		t.Fatal(err)
	}
	return estateReportRun{
		ID: estateReportRunID(scope, window), Scope: scope, State: "generated", CreatedAt: created,
		Report: estateWeeklyReport{GeneratedAt: created, Timezone: zone, Window: window},
	}
}

func estateChangeSummaryWriteRun(t *testing.T, run estateReportRun) string {
	t.Helper()
	path, err := estateReportRunPath(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFileAtomic(path, run); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEstateChangesSinceReportUsesPeriodEndAndPreservesHistory(t *testing.T) {
	estateChangeSummaryTestDir(t)
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	run := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-26", "2026-11-02T09:00:00Z")
	path := estateChangeSummaryWriteRun(t, run)
	boundary := run.Report.Window.End
	history := EstateHistory{
		Scope: run.Scope, Timezone: "UTC", StartedAt: boundary.AddDate(0, 0, -14),
		Changes: []EstateChange{
			{At: boundary.Add(-time.Nanosecond), Kind: "before-report-end"},
			{At: boundary, Kind: "at-report-end"},
			{At: boundary.Add(time.Hour), Kind: "before-monday-generation"},
			{At: now, Kind: "at-current-time"},
			{At: now.Add(time.Nanosecond), Kind: "future"},
		},
		Days: []EstateSnapshot{{At: boundary.Add(-time.Hour), Total: 100}},
	}
	before := estateCloneHistory(history)
	artifactBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		// The baseline comes from persisted reports, not in-memory state; a
		// freshly deserialized history gives the same result after a restart.
		input := history
		if i > 0 {
			input = estateCloneHistory(history)
		}
		got := estateChangesSinceReport(input, EstateOptions{HistoryDays: 365}, now)
		if got.Status != "available" || got.Count != 3 || !got.Since.Equal(boundary) || got.ReportWeek != "2026-10-26" || got.Partial {
			t.Fatalf("wrong counter on read %d: %+v", i, got)
		}
	}
	if !reflect.DeepEqual(estateCloneHistory(history), before) {
		t.Fatal("reading the counter changed retained estate history")
	}
	artifactAfter, err := os.ReadFile(path)
	if err != nil || string(artifactBefore) != string(artifactAfter) {
		t.Fatalf("reading the counter changed the saved report: %v", err)
	}
}

func TestEstateChangesSinceReportAwaitingFirstSavedReport(t *testing.T) {
	estateChangeSummaryTestDir(t)
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	history := EstateHistory{Scope: "estate-a", Timezone: "UTC", Changes: []EstateChange{{At: now}}}
	for _, emptyDirectory := range []bool{false, true} {
		if emptyDirectory {
			if err := os.MkdirAll(dataFilePath("estate-reports"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		got := estateChangesSinceReport(history, EstateOptions{}, now)
		if got.Status != "awaiting_report" || got.Count != 0 || !got.Since.IsZero() || got.Partial {
			t.Fatalf("must not substitute a retained-lifetime count: %+v", got)
		}
	}
}

func TestEstateChangesSinceReportChoosesLatestWeekNotGeneration(t *testing.T) {
	estateChangeSummaryTestDir(t)
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	latest := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-26", "2026-11-02T09:00:00Z")
	older := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-19", "2026-11-04T09:00:00Z")
	estateChangeSummaryWriteRun(t, latest)
	estateChangeSummaryWriteRun(t, older)
	history := EstateHistory{Scope: "estate-a", Timezone: "UTC", Changes: []EstateChange{
		{At: latest.Report.Window.End.Add(-time.Hour)}, {At: latest.Report.Window.End.Add(time.Hour)},
	}}
	got := estateChangesSinceReport(history, EstateOptions{}, now)
	if got.Status != "available" || got.Count != 1 || got.ReportWeek != latest.Report.Window.FirstDay || !got.Since.Equal(latest.Report.Window.End) {
		t.Fatalf("older regenerated week moved the baseline backwards: %+v", got)
	}
}

func TestEstateChangesSinceReportSeparatesEstateAndTimezone(t *testing.T) {
	estateChangeSummaryTestDir(t)
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	baseline := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-19", "2026-10-26T09:00:00Z")
	estateChangeSummaryWriteRun(t, baseline)
	estateChangeSummaryWriteRun(t, estateChangeSummaryTestRun(t, "other-estate", "UTC", "2026-10-26", "2026-11-02T09:00:00Z"))
	estateChangeSummaryWriteRun(t, estateChangeSummaryTestRun(t, "estate-a", "America/Chicago", "2026-10-26", "2026-11-02T16:00:00Z"))
	got := estateChangesSinceReport(EstateHistory{Scope: "estate-a", Timezone: "UTC"}, EstateOptions{}, now)
	if got.Status != "available" || !got.Since.Equal(baseline.Report.Window.End) {
		t.Fatalf("another estate/timezone supplied the baseline: %+v", got)
	}
}

func TestEstateChangesSinceReportIncludesAllSavedDeliveryStates(t *testing.T) {
	for _, state := range []string{"generated", "delivery_attempted", "delivery_failed", "delivered"} {
		t.Run(state, func(t *testing.T) {
			estateChangeSummaryTestDir(t)
			now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
			run := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-26", "2026-11-02T09:00:00Z")
			run.State = state
			estateChangeSummaryWriteRun(t, run)
			got := estateChangesSinceReport(EstateHistory{Scope: run.Scope, Timezone: "UTC", Changes: []EstateChange{{At: now}}}, EstateOptions{}, now)
			if got.Status != "available" || got.Count != 1 {
				t.Fatalf("saved report with delivery state %s did not establish baseline: %+v", state, got)
			}
		})
	}
}

func TestEstateChangesSinceReportIgnoresInvalidOrFutureReports(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	for _, test := range []struct {
		name   string
		mutate func(*estateReportRun)
	}{
		{"unknown state", func(run *estateReportRun) { run.State = "failed_before_save" }},
		{"zero generation", func(run *estateReportRun) { run.Report.GeneratedAt = time.Time{} }},
		{"future generation", func(run *estateReportRun) { run.Report.GeneratedAt = now.Add(time.Hour) }},
		{"generation before period end", func(run *estateReportRun) { run.Report.GeneratedAt = run.Report.Window.End.Add(-time.Hour) }},
		{"missing first day", func(run *estateReportRun) { run.Report.Window.FirstDay = "" }},
		{"non-Monday first day", func(run *estateReportRun) { run.Report.Window.FirstDay = "2026-10-27" }},
		{"wrong last day", func(run *estateReportRun) { run.Report.Window.LastDay = "2026-11-02" }},
		{"wrong start", func(run *estateReportRun) { run.Report.Window.Start = run.Report.Window.Start.Add(time.Hour) }},
		{"wrong end", func(run *estateReportRun) { run.Report.Window.End = run.Report.Window.End.Add(-time.Hour) }},
		{"incomplete week", func(run *estateReportRun) {
			run.Report.Window = estateWindow(run.Report.Window.Start.AddDate(0, 0, 7), run.Report.Window.End.AddDate(0, 0, 7))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			estateChangeSummaryTestDir(t)
			run := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-26", "2026-11-02T09:00:00Z")
			test.mutate(&run)
			estateChangeSummaryWriteRun(t, run)
			got := estateChangesSinceReport(EstateHistory{Scope: run.Scope, Timezone: "UTC"}, EstateOptions{}, now)
			if got.Status != "awaiting_report" || !got.Since.IsZero() {
				t.Fatalf("invalid report supplied baseline: %+v", got)
			}
		})
	}
}

func TestEstateChangesSinceReportRetentionAndTrackingCoverage(t *testing.T) {
	estateChangeSummaryTestDir(t)
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	run := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-05-04", "2026-05-11T09:00:00Z")
	estateChangeSummaryWriteRun(t, run)
	history := EstateHistory{Scope: run.Scope, Timezone: "UTC", StartedAt: run.Report.Window.Start.AddDate(0, 0, -7), Changes: []EstateChange{{At: now}}}
	for _, days := range []int{0, 90, 365} {
		got := estateChangesSinceReport(history, EstateOptions{HistoryDays: days}, now)
		if got.Status != "available" || got.Count != 1 || got.Partial != (days != 365) || (got.Warning != "") != got.Partial {
			t.Errorf("incorrect retained coverage for %d days: %+v", days, got)
		}
	}
	history.StartedAt = run.Report.Window.End.Add(time.Hour)
	got := estateChangesSinceReport(history, EstateOptions{HistoryDays: 365}, now)
	if !got.Partial || got.Warning == "" || got.Count != 1 {
		t.Fatalf("tracking that began after the report was not marked partial: %+v", got)
	}
}

func TestEstateChangesSinceReportAcceptsDSTCalendarWeeks(t *testing.T) {
	for _, test := range []struct {
		week, generated, now string
		hours                time.Duration
	}{
		{"2026-03-02", "2026-03-09T15:00:00Z", "2026-03-11T18:00:00Z", 167},
		{"2026-10-26", "2026-11-02T16:00:00Z", "2026-11-04T18:00:00Z", 169},
	} {
		t.Run(test.week, func(t *testing.T) {
			estateChangeSummaryTestDir(t)
			run := estateChangeSummaryTestRun(t, "estate-a", "America/Chicago", test.week, test.generated)
			if run.Report.Window.End.Sub(run.Report.Window.Start) != test.hours*time.Hour {
				t.Fatal("test fixture is not a DST transition week")
			}
			estateChangeSummaryWriteRun(t, run)
			history := EstateHistory{Scope: run.Scope, Timezone: run.Report.Timezone, Changes: []EstateChange{{At: run.Report.Window.End}}}
			got := estateChangesSinceReport(history, EstateOptions{}, estateReportTestTime(t, test.now))
			if got.Status != "available" || got.Count != 1 || !got.Since.Equal(run.Report.Window.End) || got.Partial {
				t.Fatalf("DST calendar week was rejected: %+v", got)
			}
		})
	}
}

func TestEstateChangesSinceReportUnreadableBaselineIsUnavailable(t *testing.T) {
	estateChangeSummaryTestDir(t)
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	run := estateChangeSummaryTestRun(t, "estate-a", "UTC", "2026-10-26", "2026-11-02T09:00:00Z")
	path := estateChangeSummaryWriteRun(t, run)
	if err := writeJSONFileAtomic(path, "invalid report structure"); err != nil {
		t.Fatal(err)
	}
	got := estateChangesSinceReport(EstateHistory{Scope: run.Scope, Timezone: "UTC"}, EstateOptions{}, now)
	if got.Status != "unavailable" || got.Warning == "" || !got.Since.IsZero() {
		t.Fatalf("unreadable report was treated as an absent report: %+v", got)
	}
}
