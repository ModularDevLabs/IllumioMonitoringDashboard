package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// Read frozen reports rather than resetting a counter on page loads or report
// downloads. The baseline survives restarts and never moves backwards when an
// older week is generated again. A saved report counts even if delivery failed.
func estateChangesSinceReport(history EstateHistory, options EstateOptions, now time.Time) EstateChangesSinceReport {
	summary := EstateChangesSinceReport{Status: "awaiting_report"}
	if history.Scope == "" || history.Timezone == "" {
		return summary
	}
	loc, err := time.LoadLocation(history.Timezone)
	if err != nil {
		return EstateChangesSinceReport{Status: "unavailable", Warning: "The estate reporting timezone is unavailable."}
	}
	entries, err := os.ReadDir(dataFilePath("estate-reports"))
	if errors.Is(err, os.ErrNotExist) {
		return summary
	}
	if err != nil {
		return EstateChangesSinceReport{Status: "unavailable", Warning: "Saved weekly reports could not be read; the change baseline is unknown."}
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path, err := estateReportRunPath(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		body, err := os.ReadFile(path)
		var run estateReportRun
		if err != nil || json.Unmarshal(body, &run) != nil {
			// We cannot know whether an unreadable artifact is the newest report
			// for this estate. Never substitute an older baseline silently.
			return EstateChangesSinceReport{Status: "unavailable", Warning: "A saved weekly report could not be read; the change baseline is unknown."}
		}
		if run.Scope != history.Scope || run.Report.Timezone != history.Timezone {
			continue
		}
		switch run.State {
		case "generated", "delivery_attempted", "delivery_failed", "delivered":
		default:
			continue
		}
		window := run.Report.Window
		// Validate a completed calendar week in its reporting timezone. In
		// particular, a DST week need not be exactly 168 hours long.
		expected, err := estateRequestedWeek(window.FirstDay, now, loc)
		if err != nil || window.FirstDay != expected.FirstDay || !window.Start.Equal(expected.Start) || !window.End.Equal(expected.End) || window.LastDay != expected.LastDay {
			continue
		}
		if run.Report.GeneratedAt.IsZero() || run.Report.GeneratedAt.Before(window.End) || run.Report.GeneratedAt.After(now) {
			continue
		}
		if window.End.After(summary.Since) {
			summary.Status, summary.Since, summary.ReportWeek = "available", window.End, window.FirstDay
		}
	}
	if summary.Status != "available" {
		return summary
	}
	for _, change := range history.Changes {
		if !change.At.Before(summary.Since) && !change.At.After(now) {
			summary.Count++
		}
	}
	retention := options.HistoryDays
	if retention <= 0 {
		retention = 90
	}
	cutoff := localDayStart(now, loc).AddDate(0, 0, -retention+1)
	if summary.Since.Before(cutoff) {
		summary.Partial = true
		summary.Warning = "Some changes since this report are outside the retained history window; only retained observations are counted."
	} else if !history.StartedAt.IsZero() && history.StartedAt.After(summary.Since) {
		summary.Partial = true
		summary.Warning = "Estate tracking began after this report period; only changes observed since tracking began are counted."
	}
	return summary
}
