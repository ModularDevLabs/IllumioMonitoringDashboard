package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	errEstateSnapshotDisabled   = errors.New("Weekly Estate Reports are disabled. Enable them in Settings before saving a weekly snapshot.")
	errEstateSnapshotUnobserved = errors.New("No estate observations are retained for this week. Choose a completed week with observations before saving a snapshot.")
	errEstateSnapshotConflict   = errors.New("The saved weekly snapshot could not be validated. It has not been overwritten.")
	errEstateSnapshotScope      = errors.New("The estate or reporting timezone is not available. Wait for an estate collection before saving a snapshot.")
)

// Manual snapshots deliberately use a different ID namespace from scheduled
// reports. Saving a local baseline must never consume the scheduler's durable
// delivery marker or trigger a webhook itself.
func saveEstateReportSnapshot(now time.Time, opts EstateOptions, history EstateHistory, window estateReportWindow, sections []string, traffic []estateReportTraffic) (estateWeeklyReport, error) {
	if !opts.WeeklyReportEnabled {
		return estateWeeklyReport{}, errEstateSnapshotDisabled
	}
	if strings.TrimSpace(history.Scope) == "" || strings.TrimSpace(history.Timezone) == "" {
		return estateWeeklyReport{}, errEstateSnapshotScope
	}
	loc, err := time.LoadLocation(history.Timezone)
	if err != nil || window.Start.Location().String() != history.Timezone {
		return estateWeeklyReport{}, errEstateSnapshotScope
	}
	expected, err := estateRequestedWeek(window.FirstDay, now, loc)
	if err != nil || window.FirstDay != expected.FirstDay || !window.Start.Equal(expected.Start) || !window.End.Equal(expected.End) || window.LastDay != expected.LastDay {
		return estateWeeklyReport{}, errEstateSnapshotScope
	}

	estateReportScheduleMu.Lock()
	defer estateReportScheduleMu.Unlock()
	id := estateReportRunID("manual:"+history.Scope, window)
	path, err := estateReportRunPath(id)
	if err != nil {
		return estateWeeklyReport{}, err
	}
	if body, err := os.ReadFile(path); err == nil {
		var run estateReportRun
		// Another request may have saved after this request captured its clock
		// but before it acquired the lock. That is still an idempotent repeat.
		validationNow := time.Now()
		if now.After(validationNow) {
			validationNow = now
		}
		if json.Unmarshal(body, &run) != nil || run.ID != id || run.Scope != history.Scope || run.State != "generated" || run.Report.Timezone != history.Timezone || run.Report.GeneratedAt.IsZero() || run.Report.GeneratedAt.Before(window.End) || run.Report.GeneratedAt.After(validationNow) || !run.Report.Window.Start.Equal(window.Start) || !run.Report.Window.End.Equal(window.End) || run.Report.Window.FirstDay != window.FirstDay || run.Report.Window.LastDay != window.LastDay || run.DeliveryAttemptedAt != nil {
			return estateWeeklyReport{}, errEstateSnapshotConflict
		}
		// Idempotency also preserves the original section selection and source
		// observations if the collector has advanced since the first save.
		return run.Report, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return estateWeeklyReport{}, err
	}
	covered, _ := estateWeekCoverage(history, window)
	if covered == 0 {
		return estateWeeklyReport{}, errEstateSnapshotUnobserved
	}
	report := buildEstateWeeklyReport(history, opts, window, sections, now, traffic)
	run := estateReportRun{
		ID: id, Scope: history.Scope, CreatedAt: now.UTC(), State: "generated", Report: report,
		Message: "Manual weekly snapshot saved locally. No webhook delivery was requested; scheduled delivery is unchanged.",
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return estateWeeklyReport{}, err
	}
	if err := writeJSONFileAtomic(path, run); err != nil {
		return estateWeeklyReport{}, err
	}
	return report, nil
}

// Saving is an explicit mutation; previews and downloads remain read-only.
// The route is registered behind the application's same-origin write guard.
func handleEstateReportSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	format := r.URL.Query().Get("format")
	switch format {
	case "":
		copy := r.Clone(r.Context())
		urlCopy := *r.URL
		query := urlCopy.Query()
		query.Set("format", "preview")
		urlCopy.RawQuery = query.Encode()
		copy.URL = &urlCopy
		r = copy
	case "json", "html", "preview":
	default:
		http.Error(w, "format must be json, html or preview", http.StatusBadRequest)
		return
	}
	configUpdateMu.Lock()
	opts := configuredEstateOptions()
	if !opts.WeeklyReportEnabled {
		configUpdateMu.Unlock()
		http.Error(w, errEstateSnapshotDisabled.Error(), http.StatusForbidden)
		return
	}
	now := time.Now()
	loc := configuredDayLocation()
	week := r.URL.Query().Get("week")
	if week == "" {
		week = r.URL.Query().Get("week_start")
	}
	window, err := estateRequestedWeek(week, now, loc)
	if err != nil {
		configUpdateMu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	values := opts.WeeklySections
	if raw, ok := r.URL.Query()["sections"]; ok {
		values = strings.Split(strings.Join(raw, ","), ",")
	}
	sections, err := estateSelectedSections(values)
	if err != nil {
		configUpdateMu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	history, traffic := estateHistoryCopy(), estateTrafficForWeek(window)
	configUpdateMu.Unlock()
	report, err := saveEstateReportSnapshot(now, opts, history, window, sections, traffic)
	if err != nil {
		status := http.StatusInternalServerError
		message := "Unable to save the weekly snapshot. No existing snapshot has been replaced."
		switch {
		case errors.Is(err, errEstateSnapshotDisabled):
			status, message = http.StatusForbidden, err.Error()
		case errors.Is(err, errEstateSnapshotScope), errors.Is(err, errEstateSnapshotUnobserved), errors.Is(err, errEstateSnapshotConflict):
			status, message = http.StatusConflict, err.Error()
		}
		http.Error(w, message, status)
		return
	}
	// The artifact is durable before the response can announce success.
	writeEstateReport(w, r, report)
}
