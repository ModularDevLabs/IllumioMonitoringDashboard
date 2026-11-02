package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

var estateSectionNames = []string{"estate", "health", "policy", "traffic", "changes", "quality"}

func normalizeEstateOptions(o EstateOptions) (EstateOptions, error) {
	if o.HistoryDays == 0 {
		o.HistoryDays = 90
	}
	if o.HistoryDays < 1 || o.HistoryDays > maxHistoryDays {
		return o, fmt.Errorf("estate history days must be between 1 and %d", maxHistoryDays)
	}
	o.WeeklyDay = strings.ToLower(strings.TrimSpace(o.WeeklyDay))
	if o.WeeklyDay == "" {
		o.WeeklyDay = "monday"
	}
	if !slices.Contains([]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}, o.WeeklyDay) {
		return o, fmt.Errorf("invalid weekly report day")
	}
	o.WeeklyTime = strings.TrimSpace(o.WeeklyTime)
	if o.WeeklyTime == "" {
		o.WeeklyTime = "09:00"
	}
	if _, err := time.Parse("15:04", o.WeeklyTime); err != nil {
		return o, fmt.Errorf("weekly report time must use HH:MM (24-hour clock)")
	}
	o.WeeklyWebhookProvider = strings.ToLower(strings.TrimSpace(o.WeeklyWebhookProvider))
	if o.WeeklyWebhookProvider == "" {
		o.WeeklyWebhookProvider = "generic"
	}
	if !slices.Contains([]string{"generic", "slack", "teams"}, o.WeeklyWebhookProvider) {
		return o, fmt.Errorf("invalid weekly webhook provider")
	}
	o.WeeklyWebhookURL = strings.TrimSpace(o.WeeklyWebhookURL)
	if o.WeeklyWebhookURL != "" {
		u, err := parseSafeAbsoluteHTTPURL(o.WeeklyWebhookURL)
		if err != nil || u.Fragment != "" {
			return o, fmt.Errorf("weekly webhook must be an absolute HTTP(S) URL without credentials or fragments")
		}
	}
	if o.WeeklyScheduleEnabled && !o.WeeklyReportEnabled {
		return o, fmt.Errorf("enable Weekly Estate Report before enabling its schedule")
	}
	if o.WeeklyWebhookEnabled && (!o.WeeklyReportEnabled || !o.WeeklyScheduleEnabled || o.WeeklyWebhookURL == "") {
		return o, fmt.Errorf("weekly delivery requires an enabled report, schedule, and webhook URL")
	}
	labels := make([]string, 0, len(o.RequiredLabelTypes))
	for _, s := range o.RequiredLabelTypes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if strings.ContainsAny(s, "\r\n\t") {
			return o, fmt.Errorf("label types cannot contain control characters")
		}
		if !slices.Contains(labels, s) {
			labels = append(labels, s)
		}
	}
	o.RequiredLabelTypes = labels
	sections := make([]string, 0, len(o.WeeklySections))
	if len(o.WeeklySections) == 0 {
		o.WeeklySections = append([]string(nil), estateSectionNames...)
	}
	for _, s := range o.WeeklySections {
		s = strings.ToLower(strings.TrimSpace(s))
		if !slices.Contains(estateSectionNames, s) {
			return o, fmt.Errorf("unknown weekly report section %q", s)
		}
		if !slices.Contains(sections, s) {
			sections = append(sections, s)
		}
	}
	o.WeeklySections = sections
	return o, nil
}

func configuredEstateOptions() EstateOptions {
	configMutex.RLock()
	o := config.Estate
	configMutex.RUnlock()
	o.RequiredLabelTypes = append([]string(nil), o.RequiredLabelTypes...)
	o.WeeklySections = append([]string(nil), o.WeeklySections...)
	normalized, err := normalizeEstateOptions(o)
	if err != nil {
		normalized, _ = normalizeEstateOptions(EstateOptions{})
	}
	return normalized
}

func writeEstateConfig(w http.ResponseWriter) {
	o := configuredEstateOptions()
	configured := o.WeeklyWebhookURL != ""
	o.WeeklyWebhookURL = ""
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		EstateOptions
		WeeklyWebhookConfigured bool   `json:"weekly_webhook_configured"`
		EffectiveTimezone       string `json:"effective_timezone"`
	}{o, configured, configuredDayLocation().String()})
}

func handleConfigEstate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeEstateConfig(w)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, PUT, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		EstateOptions
		WeeklyWebhookURL *string `json:"weekly_webhook_url"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid estate settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "expected one settings object", http.StatusBadRequest)
		return
	}
	configUpdateMu.Lock()
	defer configUpdateMu.Unlock()
	configMutex.RLock()
	previous := config.Estate
	allowed := append([]string(nil), config.WebhookPrivateAllowedOrigins...)
	configMutex.RUnlock()
	request.EstateOptions.WeeklyWebhookURL = previous.WeeklyWebhookURL
	if request.WeeklyWebhookURL != nil {
		request.EstateOptions.WeeklyWebhookURL = *request.WeeklyWebhookURL
	}
	next, err := normalizeEstateOptions(request.EstateOptions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if next.WeeklyWebhookEnabled && (!previous.WeeklyWebhookEnabled || next.WeeklyWebhookURL != previous.WeeklyWebhookURL) {
		if _, err := validateWebhookSettingURL(r.Context(), next.WeeklyWebhookURL, allowed); err != nil {
			http.Error(w, "weekly webhook destination is not permitted: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	configMutex.Lock()
	candidate := config
	candidate.Estate = next
	if err := writeJSONFileAtomic(configFileName, candidate); err != nil {
		configMutex.Unlock()
		http.Error(w, "could not save estate settings; previous settings remain active", http.StatusInternalServerError)
		return
	}
	config = candidate
	if info, err := os.Stat(configFileName); err == nil {
		configModTime = info.ModTime().UTC()
	}
	configMutex.Unlock()
	if estateOptionsKey(previous) != estateOptionsKey(next) {
		resetEstateMonitorContinuity()
	}
	writeEstateConfig(w)
}

func handleEstate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	view := estateView()
	view.Options.WeeklyWebhookURL = ""
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(view)
}

func serveEstate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t, err := template.ParseFS(templateFS, "estate.html")
	if err != nil {
		http.Error(w, "estate page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = t.Execute(w, pageTemplateData())
}
