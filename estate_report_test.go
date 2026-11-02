package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func estateReportTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestEstateCompletedWeeksRespectDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		now, start, last string
		hours            time.Duration
	}{
		{"2026-03-09T16:00:00Z", "2026-03-02", "2026-03-08", 167},
		{"2026-11-02T16:00:00Z", "2026-10-26", "2026-11-01", 169},
		{"2026-10-25T16:00:00Z", "2026-10-12", "2026-10-18", 168},
	} {
		window := estateCompletedWeek(estateReportTestTime(t, test.now), loc)
		if window.FirstDay != test.start || window.LastDay != test.last || window.End.Sub(window.Start) != test.hours*time.Hour {
			t.Errorf("wrong calendar week for %s: %+v (%s)", test.now, window, window.End.Sub(window.Start))
		}
	}
}

func TestEstateRequestedWeekValidation(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-02T16:00:00Z")
	for _, value := range []string{"2026-11-02", "2026-10-27", "../../secret", "2026-99-99"} {
		if _, err := estateRequestedWeek(value, now, time.UTC); err == nil {
			t.Errorf("accepted invalid/incomplete week %q", value)
		}
	}
	if got, err := estateRequestedWeek("2026-10-26", now, time.UTC); err != nil || got.LastDay != "2026-11-01" {
		t.Fatalf("valid week: %+v %v", got, err)
	}
	if _, err := estateSelectedSections([]string{"estate", "invalid"}); err == nil {
		t.Fatal("accepted unknown section")
	}
	if got, err := estateSelectedSections([]string{"quality", "estate", "estate"}); err != nil || !reflect.DeepEqual(got, []string{"estate", "quality"}) {
		t.Fatalf("section normalization: %v %v", got, err)
	}
}

func estateReportTestHistory(t *testing.T) (EstateHistory, estateReportWindow) {
	t.Helper()
	now := estateReportTestTime(t, "2026-11-02T16:00:00Z")
	window := estateCompletedWeek(now, time.UTC)
	history := EstateHistory{Scope: "synthetic-estate", StartedAt: window.Start.AddDate(0, 0, -7), Days: []EstateSnapshot{
		{At: window.Start.Add(-time.Hour), FirstAt: window.Start.Add(-23 * time.Hour), InventoryComplete: true, Total: 9, Managed: 7, Unmanaged: 2},
		{At: window.End.Add(-time.Hour), FirstAt: window.End.Add(-23 * time.Hour), InventoryComplete: true, Total: 12, Managed: 9, Unmanaged: 3,
			VENCount: 9, VersionsCollected: true,
			Versions:       []EstateVersionCount{{Version: "25.2", Count: 5}, {Version: "24.5", Count: 2}, {Version: "", Count: 1}},
			HealthComplete: map[string]bool{"warning": true, "stopped": true, "tampering": true},
			Tampered:       map[string]time.Time{"ven-a": window.Start.Add(time.Hour), "too-old": window.Start.Add(-time.Second)}},
	}}
	history.Issues = []EstateIssue{
		{ID: "warning:a", WorkloadID: "ven-a", Kind: "warning", ObservedDays: []string{"2026-10-26", "2026-10-27"}},
		{ID: "warning:a-again", WorkloadID: "ven-a", Kind: "warning", ObservedDays: []string{"2026-11-01"}},
		{ID: "warning:b", WorkloadID: "ven-b", Kind: "warning", ObservedDays: []string{"2026-10-28"}},
		{ID: "stopped:a", WorkloadID: "ven-a", Kind: "stopped", ObservedDays: []string{"2026-10-30"}},
		{ID: "tampering:old", WorkloadID: "too-old", Kind: "tampering", ObservedDays: []string{"2026-10-26"}},
		{ID: "error:previous", WorkloadID: "ven-prior", Kind: "error", ObservedDays: []string{"2026-10-20"}},
	}
	return history, window
}

func TestEstateWeeklyReportUsesObservedInventoryAndVersionTotals(t *testing.T) {
	history, window := estateReportTestHistory(t)
	report := buildEstateWeeklyReport(history, EstateOptions{WeeklyReportEnabled: true}, window, estateReportSections, window.End, nil)
	if report.Estate == nil || report.Estate.Total.Current != 12 || report.Estate.Total.Change == nil || *report.Estate.Total.Change != 3 {
		t.Fatalf("incorrect inventory comparison: %+v", report.Estate)
	}
	versions := map[string]int{}
	for _, row := range report.Estate.Versions {
		versions[row.Version] = row.Total
	}
	if !reflect.DeepEqual(versions, map[string]int{"25.2": 5, "24.5": 2, "Unknown": 2}) {
		t.Fatalf("weekly versions must be present even with live VENVersionsEnabled false: %v", versions)
	}
	if report.Quality.CoveredDays != 1 || len(report.Quality.MissingDays) != 6 {
		t.Fatalf("missing coverage incorrectly represented: %+v", report.Quality)
	}
	if report.Quality.FirstObservation == nil || !report.Quality.FirstObservation.Equal(window.End.Add(-23*time.Hour)) {
		t.Fatalf("first daily sample was lost: %+v", report.Quality)
	}
	if got := report.Health.UniqueAffected; got["warning"] != 2 || got["stopped"] != 1 || got["tampering"] != 1 {
		t.Fatalf("wrong weekly distinct counts: %v", got)
	}
	if _, ok := report.Health.UniqueAffected["error"]; ok {
		t.Fatal("unobserved health kind fabricated as zero")
	}
}

func TestEstateWeeklyReportDoesNotInventMissingBaselineOrPartialInventory(t *testing.T) {
	history, window := estateReportTestHistory(t)
	history.Days = history.Days[1:]
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate"}, window.End, nil)
	if report.Estate.Total.Previous != nil || report.Estate.Total.Change != nil {
		t.Fatal("missing comparison week converted to zero")
	}
	history.Days[0].InventoryComplete = false
	report = buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate"}, window.End, nil)
	if report.Estate != nil {
		t.Fatal("partial inventory presented as a complete count")
	}
	if !strings.Contains(strings.Join(report.Quality.Warnings, " "), "unavailable, not zero") {
		t.Fatal("missing inventory should be explained")
	}
}

func TestEstateWeeklyFailedOnlyHealthIsUnavailable(t *testing.T) {
	history, window := estateReportTestHistory(t)
	history.Issues = nil
	history.Days[1].HealthComplete = map[string]bool{"error": false, "warning": true}
	history.Days[1].Tampered = nil
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"health"}, window.End, nil)
	if _, ok := report.Health.UniqueAffected["error"]; ok {
		t.Fatal("failed-only health collection became zero")
	}
	if value, ok := report.Health.UniqueAffected["warning"]; !ok || value != 0 {
		t.Fatal("successfully observed zero health count unavailable")
	}
	report.Health.PreviousUniqueAffected = map[string]int{"error": 2}
	body, err := renderEstateReportHTML(report, "dark")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<td>warning</td><td>0</td><td>Unavailable</td>") {
		t.Fatal("missing prior health kind rendered as zero")
	}
}

func TestEstateWeeklyUncollectedVersionsAreUnavailable(t *testing.T) {
	history, window := estateReportTestHistory(t)
	history.Days[1].Versions = nil
	history.Days[1].VersionsCollected = false
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate"}, window.End, nil)
	if report.Estate.VersionsAvailable || len(report.Estate.Versions) != 0 {
		t.Fatalf("missing versions misrepresented: %+v", report.Estate)
	}
	if !strings.Contains(strings.Join(report.Quality.Warnings, " "), "versions were not retained") {
		t.Fatal("uncollected version history was not explained")
	}
}

func TestEstateWeeklySharedVENNotCountedAsUnknownWorkloads(t *testing.T) {
	history, window := estateReportTestHistory(t)
	history.Days[1].VENCount = 2
	history.Days[1].Versions = []EstateVersionCount{{Version: "25.2", Count: 2}}
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate"}, window.End, nil)
	if report.Estate.VENCount != 2 || len(report.Estate.Versions) != 1 || report.Estate.Versions[0].Total != 2 {
		t.Fatalf("shared VEN counted more than once: %+v", report.Estate)
	}
}

func TestEstateReportSectionsThemesAndEscaping(t *testing.T) {
	history, window := estateReportTestHistory(t)
	history.Days[1].Versions = []EstateVersionCount{{Version: `<script>alert("x")</script>`, Count: 9}}
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate"}, window.End, nil)
	if report.Health != nil || report.Policy != nil || len(report.Traffic) > 0 || len(report.Changes) > 0 {
		t.Fatal("excluded sections still populated")
	}
	for _, theme := range []string{"dark", "illumio-dark", "illumio-light"} {
		body, err := renderEstateReportHTML(report, theme)
		if err != nil {
			t.Fatal(err)
		}
		html := string(body)
		if !strings.Contains(html, `data-theme="`+theme+`"`) || !strings.Contains(html, "VEN version distribution") {
			t.Fatalf("theme or versions missing for %s", theme)
		}
		if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") {
			t.Fatal("untrusted version was not HTML escaped")
		}
		for _, id := range []string{"health", "policy", "traffic", "changes", "quality"} {
			if strings.Contains(html, `id="`+id+`"`) {
				t.Errorf("excluded section %s in export", id)
			}
		}
	}
	body, err := renderEstateReportHTML(report, `"><script>bad</script>`)
	if err != nil || !strings.Contains(string(body), `data-theme="dark"`) {
		t.Fatalf("invalid theme not normalized: %v", err)
	}
}

func TestEstateReportScheduleOptInAndLocalTimezone(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	before := estateReportTestTime(t, "2026-11-02T14:59:00Z")
	after := before.Add(time.Minute)
	opts := EstateOptions{WeeklyReportEnabled: true, WeeklyScheduleEnabled: true, WeeklyDay: "monday", WeeklyTime: "09:00"}
	if _, due := estateReportDue(before, opts, loc); due {
		t.Fatal("scheduled in UTC instead of local time")
	}
	if week, due := estateReportDue(after, opts, loc); !due || week.FirstDay != "2026-10-26" {
		t.Fatalf("report not due: %+v %t", week, due)
	}
	opts.WeeklyScheduleEnabled = false
	if _, due := estateReportDue(after, opts, loc); due {
		t.Fatal("schedule is not opt-in")
	}
	opts.WeeklyScheduleEnabled, opts.WeeklyReportEnabled = true, false
	if _, due := estateReportDue(after, opts, loc); due {
		t.Fatal("report disable did not stop schedule")
	}
}

func TestEstateScheduledReportFrozenAndAtMostOnce(t *testing.T) {
	oldDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = oldDir })
	history, window := estateReportTestHistory(t)
	now := window.End.Add(10 * time.Hour)
	opts := EstateOptions{WeeklyReportEnabled: true, WeeklyScheduleEnabled: true, WeeklyDay: "monday", WeeklyTime: "09:00", WeeklyWebhookEnabled: true, WeeklyWebhookURL: "https://example.invalid/secret-token"}
	deliveries := 0
	deliver := func(report estateWeeklyReport) error {
		deliveries++
		return errors.New("https://example.invalid/secret-token transport failed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runEstateReportSchedule(now, opts, time.UTC, history, nil, deliver); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if deliveries != 1 {
		t.Fatalf("duplicate scheduled delivery: %d", deliveries)
	}
	path, _ := estateReportRunPath(estateReportRunID(history.Scope, window))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatal("webhook secret persisted in report")
	}
	var saved estateReportRun
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State != "delivery_failed" || saved.DeliveryAttemptedAt == nil || saved.Report.Estate.Total.Current != 12 {
		t.Fatalf("wrong frozen report status: %+v", saved)
	}
	history.Days[1].Total = 999
	if err := runEstateReportSchedule(now.Add(time.Hour), opts, time.UTC, history, nil, deliver); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("frozen report was rewritten")
	}
	if deliveries != 1 {
		t.Fatal("failed/uncertain delivery was automatically repeated")
	}
}

func TestEstateScheduledReportNoDeliveryUnlessExplicitlyEnabled(t *testing.T) {
	oldDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = oldDir })
	history, window := estateReportTestHistory(t)
	opts := EstateOptions{WeeklyReportEnabled: true, WeeklyScheduleEnabled: true, WeeklyDay: "monday", WeeklyTime: "09:00"}
	if err := runEstateReportSchedule(window.End.Add(10*time.Hour), opts, time.UTC, history, nil, func(estateWeeklyReport) error { t.Fatal("webhook not enabled"); return nil }); err != nil {
		t.Fatal(err)
	}
	path, _ := estateReportRunPath(estateReportRunID(history.Scope, window))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved estateReportRun
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State != "generated" || saved.DeliveryAttemptedAt != nil {
		t.Fatalf("wrong local-only status: %+v", saved)
	}
	if _, err := estateReportRunPath("../../config.json"); err == nil {
		t.Fatal("accepted unsafe report ID")
	}
}

func TestEstateScheduleDoesNotWriteBeforeEstateKnown(t *testing.T) {
	oldDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = oldDir })
	opts := EstateOptions{WeeklyReportEnabled: true, WeeklyScheduleEnabled: true, WeeklyDay: "monday", WeeklyTime: "09:00", WeeklyWebhookEnabled: true, WeeklyWebhookURL: "https://example.invalid"}
	if err := runEstateReportSchedule(estateReportTestTime(t, "2026-11-02T16:00:00Z"), opts, time.UTC, EstateHistory{}, nil, func(estateWeeklyReport) error { t.Fatal("empty estate delivered"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataFilePath("estate-reports")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty estate report created: %v", err)
	}
}

func TestEstateReportHTTPMethodAndFormat(t *testing.T) {
	w := httptest.NewRecorder()
	handleEstateReport(w, httptest.NewRequest(http.MethodPost, "/api/estate/report", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST accepted: %d", w.Code)
	}
	history, window := estateReportTestHistory(t)
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate"}, window.End, nil)
	w = httptest.NewRecorder()
	writeEstateReport(w, httptest.NewRequest(http.MethodGet, "/api/estate/report?format=html&theme=illumio-light", nil), report)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Body.String(), `data-theme="illumio-light"`) {
		t.Fatalf("HTML response: %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	writeEstateReport(w, httptest.NewRequest(http.MethodGet, "/api/estate/report?format=invalid", nil), report)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid format: %d", w.Code)
	}
}

func TestEstateReportPreviewReturnsOneConsistentSnapshot(t *testing.T) {
	history, window := estateReportTestHistory(t)
	report := buildEstateWeeklyReport(history, EstateOptions{}, window, []string{"estate", "health"}, window.End.Add(9*time.Hour), nil)
	report.Version = "preview-version-unique"
	report.Estate.Total.Current = 24681
	w := httptest.NewRecorder()
	writeEstateReport(w, httptest.NewRequest(http.MethodGet, "/api/estate/report?format=preview&theme=illumio-light", nil), report)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("preview response: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("Content-Disposition") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview should be uncached JSON, not an attachment: %v", w.Header())
	}
	var preview struct {
		Report estateWeeklyReport `json:"report"`
		HTML   string             `json:"html"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	expectedReport, _ := json.Marshal(report)
	actualReport, _ := json.Marshal(preview.Report)
	if string(expectedReport) != string(actualReport) {
		t.Fatal("preview metadata differs from the captured report")
	}
	expectedHTML, err := renderEstateReportHTML(report, "illumio-light")
	if err != nil {
		t.Fatal(err)
	}
	if preview.HTML != string(expectedHTML) {
		t.Fatal("preview HTML differs from the same captured metadata snapshot")
	}
	if !strings.Contains(preview.HTML, "24681") || !strings.Contains(preview.HTML, report.Version) || !strings.Contains(preview.HTML, `data-theme="illumio-light"`) {
		t.Fatal("preview did not preserve the captured data/version/theme")
	}
	if strings.Contains(preview.HTML, `id="traffic"`) {
		t.Fatal("preview did not preserve section selection")
	}
	// The shared writer also serves frozen-run exports and retains both existing
	// JSON contracts: neither the default nor format=json gains a wrapper.
	for _, format := range []string{"", "json"} {
		out := httptest.NewRecorder()
		writeEstateReport(out, httptest.NewRequest(http.MethodGet, "/api/estate/report/runs?id=frozen&format="+format, nil), report)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(out.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["report"]; ok {
			t.Fatal("existing JSON format was wrapped")
		}
		if _, ok := raw["generated_at"]; !ok {
			t.Fatal("existing JSON metadata was lost")
		}
	}
	// A later update cannot alter either representation already returned.
	report.Estate.Total.Current = 99999
	if preview.Report.Estate.Total.Current != 24681 || strings.Contains(preview.HTML, "99999") {
		t.Fatal("preview changed after capture")
	}
}

func TestEstateWeeklyDeliveryRejectsSettingsOrScopeChange(t *testing.T) {
	configMutex.Lock()
	oldConfig := config
	config = Config{PCEURL: "https://pce-one.example.invalid", OrgID: "1", Timezone: "UTC", Estate: EstateOptions{WeeklyReportEnabled: true, WeeklyScheduleEnabled: true, WeeklyWebhookEnabled: true, WeeklyWebhookURL: "https://example.invalid/webhook"}}
	configMutex.Unlock()
	t.Cleanup(func() { configMutex.Lock(); config = oldConfig; configMutex.Unlock() })
	opts := configuredEstateOptions()
	scope := estateScopeForConfig()
	delivered := 0
	deliver := func() error {
		if !configUpdateMu.TryLock() {
			t.Fatal("network delivery holds configuration/collector lock")
		}
		configUpdateMu.Unlock()
		delivered++
		return nil
	}
	if err := estateDeliverReportIfCurrent(opts, scope, "UTC", deliver); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatal("unchanged settings rejected")
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Estate.WeeklyWebhookEnabled = false },
		func(c *Config) { c.Estate.WeeklyWebhookURL = "https://elsewhere.example.invalid/new" },
		func(c *Config) { c.PCEURL = "https://pce-two.example.invalid" },
		func(c *Config) { c.Timezone = "America/Chicago" },
	} {
		configUpdateMu.Lock()
		configMutex.Lock()
		original := config
		change(&config)
		configMutex.Unlock()
		configUpdateMu.Unlock()
		if err := estateDeliverReportIfCurrent(opts, scope, "UTC", deliver); err == nil {
			t.Fatal("changed settings allowed stale report delivery")
		}
		configMutex.Lock()
		config = original
		configMutex.Unlock()
	}
	if delivered != 1 {
		t.Fatal("stale or disabled report was delivered")
	}
}

func TestEstateWeeklyTrafficRequiresVerifiedScopeAndFilters(t *testing.T) {
	oldDir := dataDir
	dataDir = t.TempDir()
	configMutex.Lock()
	oldConfig := config
	target := TrafficTarget{Name: "weekly-report-target", Kind: "label"}
	config = Config{PCEURL: "https://estate-one.example.invalid", OrgID: "1", Timezone: "UTC", TrafficTargets: []TrafficTarget{target}}
	configMutex.Unlock()
	historyMu.Lock()
	oldBlockedDaily := blockedDaily
	window := estateCompletedWeek(time.Now(), time.UTC)
	blockedDaily = map[string]map[string]int{window.FirstDay: {target.Name: 999999}}
	historyMu.Unlock()
	t.Cleanup(func() {
		dataDir = oldDir
		configMutex.Lock()
		config = oldConfig
		configMutex.Unlock()
		historyMu.Lock()
		blockedDaily = oldBlockedDaily
		historyMu.Unlock()
	})
	base := "https://estate-one.example.invalid/api/v2/orgs/1"
	path := estateVerifiedTrafficPath(estateScopeForConfig(), "UTC")
	estateRecordVerifiedTraffic(base, target, window.FirstDay, 12, true)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled weekly reports wrote traffic history")
	}
	configMutex.Lock()
	config.Estate.WeeklyReportEnabled = true
	configMutex.Unlock()
	rows := estateTrafficForWeek(window)
	if len(rows) != 1 || rows[0].CoveredDays != 0 || rows[0].Total != 0 {
		t.Fatalf("unattributed legacy traffic included: %+v", rows)
	}
	fingerprint := estateVerifiedTrafficFingerprint(target)
	estateRecordVerifiedTraffic("https://estate-two.example.invalid/api/v2/orgs/1", target, window.FirstDay, 500, true, fingerprint)
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 0 {
		t.Fatal("wrong PCE traffic included")
	}
	estateRecordVerifiedTraffic(base, target, window.FirstDay, 12, true, fingerprint)
	estateRecordVerifiedTraffic(base, target, window.FirstDay, 999, false, fingerprint)
	rows = estateTrafficForWeek(window)
	if rows[0].Total != 12 || rows[0].CoveredDays != 1 {
		t.Fatalf("verified count replaced by partial result: %+v", rows)
	}
	today := localDayStart(time.Now(), time.UTC).Format("2006-01-02")
	estateRecordVerifiedTraffic(base, target, today, 500, true, fingerprint)
	stored := estateReadVerifiedTraffic(estateScopeForConfig(), "UTC")
	if len(stored.Records) != 1 {
		t.Fatal("today-so-far was persisted as a complete day")
	}
	configMutex.Lock()
	config.OrgID = "2"
	configMutex.Unlock()
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 0 {
		t.Fatal("previous organization traffic leaked into new organization")
	}
	configMutex.Lock()
	config.OrgID = "1"
	config.SourceExclusions = []TrafficTarget{{Name: "excluded-source", Kind: "label"}}
	configMutex.Unlock()
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 0 {
		t.Fatal("traffic from superseded exclusions included")
	}
	estateRecordVerifiedTraffic(base, target, window.FirstDay, 700, true, fingerprint)
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 0 {
		t.Fatal("in-flight superseded query was accepted")
	}
	estateRecordVerifiedTraffic(base, target, window.FirstDay, 3, true, estateVerifiedTrafficFingerprint(target))
	if rows := estateTrafficForWeek(window); rows[0].Total != 3 || rows[0].CoveredDays != 1 {
		t.Fatalf("new verified query not accepted: %+v", rows)
	}
}

func TestEstateWeeklyTrafficComparesOnlyCompleteWeeks(t *testing.T) {
	oldDir := dataDir
	dataDir = t.TempDir()
	configMutex.Lock()
	oldConfig := config
	target := TrafficTarget{Name: "weekly-comparison", Kind: "label"}
	config = Config{PCEURL: "https://estate.example.invalid", OrgID: "1", Timezone: "UTC", TrafficTargets: []TrafficTarget{target}, Estate: EstateOptions{WeeklyReportEnabled: true}}
	configMutex.Unlock()
	t.Cleanup(func() { dataDir = oldDir; configMutex.Lock(); config = oldConfig; configMutex.Unlock() })
	window := estateCompletedWeek(time.Now(), time.UTC)
	base := "https://estate.example.invalid/api/v2/orgs/1"
	for day := window.Start.AddDate(0, 0, -7); day.Before(window.End); day = day.AddDate(0, 0, 1) {
		count := 1
		if !day.Before(window.Start) {
			count = 2
		}
		estateRecordVerifiedTraffic(base, target, day.Format("2006-01-02"), count, true, estateVerifiedTrafficFingerprint(target))
	}
	rows := estateTrafficForWeek(window)
	if len(rows) != 1 || rows[0].Total != 14 || rows[0].Previous == nil || *rows[0].Previous != 7 || rows[0].Change == nil || *rows[0].Change != 7 {
		t.Fatalf("complete weekly comparison: %+v", rows)
	}
	scope := estateScopeForConfig()
	store := estateReadVerifiedTraffic(scope, "UTC")
	delete(store.Records, estateVerifiedTrafficKey(window.Start.AddDate(0, 0, -1).Format("2006-01-02"), target.Name))
	if err := writeJSONFileAtomic(estateVerifiedTrafficPath(scope, "UTC"), store); err != nil {
		t.Fatal(err)
	}
	rows = estateTrafficForWeek(window)
	if rows[0].Previous != nil || rows[0].Change != nil {
		t.Fatal("incomplete previous week was compared as a complete baseline")
	}
}

func TestEstateWeeklyTrafficNamedServiceContentChangeInvalidatesHistory(t *testing.T) {
	oldDir := dataDir
	dataDir = t.TempDir()
	configMutex.Lock()
	oldConfig := config
	target := TrafficTarget{Name: "named-service-report", Kind: "label", ServiceExclusions: []string{"Service: Search"}}
	config = Config{PCEURL: "https://estate.example.invalid", OrgID: "1", Timezone: "UTC", TrafficTargets: []TrafficTarget{target}, Estate: EstateOptions{WeeklyReportEnabled: true}}
	configMutex.Unlock()
	oldService := resolvedServiceExclusionFingerprint([]trafficServiceFilter{{Port: 9300, Proto: 6}})
	newService := resolvedServiceExclusionFingerprint([]trafficServiceFilter{{Port: 9400, Proto: 6}})
	rollingMu.Lock()
	oldFingerprints := rollingCache.ServiceFilterFingerprints
	oldPending := rollingCache.ServiceHistoryPending
	rollingCache.ServiceFilterFingerprints = map[string]string{target.Name: oldService}
	rollingCache.ServiceHistoryPending = map[string]string{}
	rollingMu.Unlock()
	t.Cleanup(func() {
		dataDir = oldDir
		configMutex.Lock()
		config = oldConfig
		configMutex.Unlock()
		rollingMu.Lock()
		rollingCache.ServiceFilterFingerprints = oldFingerprints
		rollingCache.ServiceHistoryPending = oldPending
		rollingMu.Unlock()
	})
	window := estateCompletedWeek(time.Now(), time.UTC)
	base := "https://estate.example.invalid/api/v2/orgs/1"
	prepared := target
	prepared.resolvedServiceFingerprint = oldService
	captured := estateVerifiedTrafficFingerprint(prepared)
	if captured == "" || captured != estateVerifiedTrafficFingerprint(target) {
		t.Fatal("query provenance does not match current resolved service")
	}
	estateRecordVerifiedTraffic(base, prepared, window.FirstDay, 12, true, captured)
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 1 || rows[0].Total != 12 {
		t.Fatalf("initial verified traffic missing: %+v", rows)
	}
	// The PCE object retains its name but now excludes TCP 9400. Simulate the
	// collector detecting this change and a reconciliation clearing pending.
	rollingMu.Lock()
	rollingCache.ServiceFilterFingerprints[target.Name] = newService
	rollingCache.ServiceHistoryPending[target.Name] = newService
	delete(rollingCache.ServiceHistoryPending, target.Name)
	rollingMu.Unlock()
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 0 || rows[0].Total != 0 {
		t.Fatalf("old named-service contents reappeared after pending cleared: %+v", rows)
	}
	// A stale in-flight query must not overwrite data under the new semantics.
	estateRecordVerifiedTraffic(base, prepared, window.FirstDay, 999, true, captured)
	store := estateReadVerifiedTraffic(estateScopeForConfig(), "UTC")
	if stored := store.Records[estateVerifiedTrafficKey(window.FirstDay, target.Name)]; stored.Count != 12 {
		t.Fatalf("stale resolved service query overwrote verified history: %+v", stored)
	}
	prepared.resolvedServiceFingerprint = newService
	newCaptured := estateVerifiedTrafficFingerprint(prepared)
	if newCaptured == captured {
		t.Fatal("named service content did not change provenance")
	}
	estateRecordVerifiedTraffic(base, prepared, window.FirstDay, 5, true, newCaptured)
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 1 || rows[0].Total != 5 {
		t.Fatalf("new verified semantics not shown: %+v", rows)
	}
	rollingMu.Lock()
	delete(rollingCache.ServiceFilterFingerprints, target.Name)
	rollingMu.Unlock()
	if estateVerifiedTrafficFingerprint(target) != "" {
		t.Fatal("unresolved named service accepted as current provenance")
	}
	if rows := estateTrafficForWeek(window); rows[0].CoveredDays != 0 {
		t.Fatal("unresolved current service displayed historical traffic")
	}
}
