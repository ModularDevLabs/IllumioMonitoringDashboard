package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errEstateReportNowUnobserved = errors.New("No estate observations are retained for the current week. Enable estate monitoring and wait for the next dashboard collection, or select a completed week with observations.")

// The current report is explicitly partial, ending at the captured request time.
// The extra nanosecond includes observations captured at exactly that instant
// while retaining the existing exclusive-end report window convention.
func estateCurrentWeek(now time.Time, loc *time.Location) estateReportWindow {
	if loc == nil {
		loc = time.UTC
	}
	today := localDayStart(now, loc)
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	return estateReportWindow{Start: monday, End: now.In(loc).Add(time.Nanosecond), FirstDay: monday.Format("2006-01-02"), LastDay: today.Format("2006-01-02")}
}

func saveEstateReportNow(now time.Time, opts EstateOptions, history EstateHistory, sections []string, traffic []estateReportTraffic, setBaseline bool) (estateWeeklyReport, error) {
	if !opts.WeeklyReportEnabled {
		return estateWeeklyReport{}, errEstateSnapshotDisabled
	}
	if strings.TrimSpace(history.Scope) == "" || strings.TrimSpace(history.Timezone) == "" {
		return estateWeeklyReport{}, errEstateSnapshotScope
	}
	loc, err := time.LoadLocation(history.Timezone)
	if err != nil || now.IsZero() {
		return estateWeeklyReport{}, errEstateSnapshotScope
	}
	window := estateCurrentWeek(now, loc)
	observation := time.Time{}
	for _, snapshot := range history.Days {
		if estateWindowContains(snapshot.At, window) && snapshot.At.After(observation) {
			observation = snapshot.At
		}
	}
	if observation.IsZero() {
		return estateWeeklyReport{}, errEstateReportNowUnobserved
	}
	report := buildEstateReport(history, opts, window, sections, now, traffic, true)
	report.Kind = "on_demand"
	stamp := observation.UTC()
	report.ObservationAt = &stamp
	report.SetsChangeBaseline = setBaseline
	if age := now.Sub(observation); age > 20*time.Minute {
		report.Quality.Warnings = append(report.Quality.Warnings, fmt.Sprintf("The latest estate collection is %s old. This report uses retained observations, not live PCE data; any change baseline is anchored to that observation, not the report creation time.", age.Round(time.Minute)))
	}
	if setBaseline {
		report.Quality.Warnings = append(report.Quality.Warnings, "This saved on-demand report is eligible to establish the estate change baseline at its latest observation timestamp. An existing newer baseline is never moved backwards.")
	} else {
		report.Quality.Warnings = append(report.Quality.Warnings, "This report does not change the estate change baseline. Running, previewing or downloading it does not reset the dashboard counter.")
	}

	// Serialize artifact creation alongside scheduled and completed-week saves,
	// but use a random namespace so every explicit on-demand invocation is an
	// independent frozen artifact and never consumes a weekly delivery marker.
	estateReportScheduleMu.Lock()
	defer estateReportScheduleMu.Unlock()
	var path string
	for {
		var entropy [32]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return estateWeeklyReport{}, err
		}
		report.ID = hex.EncodeToString(entropy[:])
		path, err = estateReportRunPath(report.ID)
		if err != nil {
			return estateWeeklyReport{}, err
		}
		if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return estateWeeklyReport{}, err
		}
	}
	run := estateReportRun{
		ID: report.ID, Scope: history.Scope, CreatedAt: now.UTC(), State: "generated", Report: report,
		Message: "On-demand partial-week report saved locally from retained observations. No webhook delivery was requested; scheduled reporting is unchanged.",
	}
	if setBaseline {
		run.Message += " Selected as an estate change baseline at the report observation timestamp."
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return estateWeeklyReport{}, err
	}
	if err := writeJSONFileAtomic(path, run); err != nil {
		return estateWeeklyReport{}, err
	}
	return report, nil
}

// Registration must use the application's same-origin write guard. Reports
// use the collector's captured data only; no PCE or webhook request is made.
func handleEstateReportNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	switch query.Get("format") {
	case "":
		copy := r.Clone(r.Context())
		urlCopy := *r.URL
		query.Set("format", "preview")
		urlCopy.RawQuery = query.Encode()
		copy.URL = &urlCopy
		r = copy
	case "json", "html", "preview":
	default:
		http.Error(w, "format must be json, html or preview", http.StatusBadRequest)
		return
	}
	setBaseline := false
	switch query.Get("set_baseline") {
	case "", "false":
	case "true":
		setBaseline = true
	default:
		http.Error(w, "set_baseline must be true or false", http.StatusBadRequest)
		return
	}
	if query.Get("week") != "" || query.Get("week_start") != "" {
		http.Error(w, "Run report now uses the current partial week; use Save weekly snapshot for a selected completed week.", http.StatusBadRequest)
		return
	}
	configUpdateMu.Lock()
	opts := configuredEstateOptions()
	if !opts.WeeklyReportEnabled {
		configUpdateMu.Unlock()
		http.Error(w, errEstateSnapshotDisabled.Error(), http.StatusForbidden)
		return
	}
	values := opts.WeeklySections
	if raw, ok := query["sections"]; ok {
		values = strings.Split(strings.Join(raw, ","), ",")
	}
	sections, err := estateSelectedSections(values)
	if err != nil {
		configUpdateMu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	window := estateCurrentWeek(now, configuredDayLocation())
	history, traffic := estateHistoryCopy(), estateTrafficForWeek(window)
	configUpdateMu.Unlock()
	report, err := saveEstateReportNow(now, opts, history, sections, traffic, setBaseline)
	if err != nil {
		status, message := http.StatusInternalServerError, "Unable to save the on-demand report. The estate change baseline has not been changed."
		switch {
		case errors.Is(err, errEstateSnapshotDisabled):
			status, message = http.StatusForbidden, err.Error()
		case errors.Is(err, errEstateSnapshotScope), errors.Is(err, errEstateReportNowUnobserved):
			status, message = http.StatusConflict, err.Error()
		}
		http.Error(w, message, status)
		return
	}
	writeEstateReport(w, r, report)
}
