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
	"sync/atomic"
	"testing"
	"time"
)

func estateNowTestFixture(t *testing.T, now time.Time) (EstateHistory, EstateOptions) {
	t.Helper()
	history, _, opts := estateSnapshotTestFixture(t)
	observation := now.Add(-5 * time.Minute)
	window := estateCurrentWeek(now, time.UTC)
	if observation.Before(window.Start) {
		observation = window.Start
	}
	history.StartedAt = window.Start
	history.Days = []EstateSnapshot{{At: observation, FirstAt: observation, Day: observation.Format("2006-01-02"), InventoryComplete: true, Total: 12, Managed: 9, Unmanaged: 3}}
	history.Latest = &history.Days[0]
	opts.ChangeLogEnabled = true
	estateMonitor.Lock()
	estateMonitor.history = estateCloneHistory(history)
	estateMonitor.Unlock()
	return history, opts
}

func TestEstateOnDemandReportPartialCoverageAndFrozenIdentity(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	history, opts := estateNowTestFixture(t, now)
	window := estateCurrentWeek(now, time.UTC)
	history.Days = append(history.Days,
		EstateSnapshot{At: window.Start.Add(time.Hour), InventoryComplete: true, Total: 10},
		EstateSnapshot{At: window.Start.Add(-time.Hour), InventoryComplete: true, Total: 1})
	history.Changes = []EstateChange{{At: history.Latest.At, Kind: "labels_changed", Name: "example"}}
	before := estateCloneHistory(history)
	previous := 100
	traffic := []estateReportTraffic{{Target: "test", Total: 5, CoveredDays: 1, Previous: &previous, Change: &previous, PreviousCoveredDays: 7}}
	report, err := saveEstateReportNow(now, opts, history, estateReportSections, traffic, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Kind != "on_demand" || !report.PartialWeek || report.SetsChangeBaseline || report.ObservationAt == nil || !report.ObservationAt.Equal(history.Latest.At) || len(report.ID) != 64 {
		t.Fatalf("wrong on-demand metadata: %+v", report)
	}
	if report.Window.FirstDay != "2026-11-02" || report.Window.LastDay != "2026-11-04" || report.Quality.ExpectedDays != 3 || report.Quality.CoveredDays != 2 || !reflect.DeepEqual(report.Quality.MissingDays, []string{"2026-11-03"}) {
		t.Fatalf("partial coverage includes future days or uses a full week: %+v %+v", report.Window, report.Quality)
	}
	if report.Quality.ComparisonAvailable || report.Quality.PreviousCoveredDays != 0 || report.Estate == nil || report.Estate.Total.Current != 12 || report.Estate.Total.Previous != nil || report.Estate.Total.Change != nil || report.PreviousPolicy != nil || report.Traffic[0].Previous != nil || report.Traffic[0].Change != nil {
		t.Fatalf("partial week contains misleading complete-week comparisons: %+v", report)
	}
	if !reflect.DeepEqual(estateCloneHistory(history), before) || traffic[0].Previous == nil || *traffic[0].Previous != previous {
		t.Fatal("saving mutated captured collector history or traffic")
	}
	path, _ := estateReportRunPath(report.ID)
	body, err := os.ReadFile(path)
	var run estateReportRun
	if err != nil || json.Unmarshal(body, &run) != nil || run.ID != report.ID || run.Report.ID != report.ID || run.Scope != history.Scope || run.State != "generated" || run.DeliveryAttemptedAt != nil {
		t.Fatalf("on-demand report was not durably frozen: %+v %v", run, err)
	}
	if summary := estateChangesSinceReport(history, opts, now); summary.Status != "awaiting_report" {
		t.Fatalf("default on-demand save changed the baseline: %+v", summary)
	}
	html, err := renderEstateReportHTML(report, "illumio-light")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"On-demand Estate Report", "Current partial week", "2 of 3 elapsed days", "Latest estate observation", "The estate change baseline is unchanged", `data-theme="illumio-light"`} {
		if !strings.Contains(string(html), required) {
			t.Errorf("HTML missing %q", required)
		}
	}
	for _, unwanted := range []string{"of 7 days", "<th>Previous week</th>", "<th>Previous complete week</th>", "Week-over-week +", "Compared with"} {
		if strings.Contains(string(html), unwanted) {
			t.Errorf("HTML claims full-week comparison: %q", unwanted)
		}
	}
}

func TestEstateOnDemandBaselineUsesObservationStrictlyAfterAndSurvivesRestart(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	history, opts := estateNowTestFixture(t, now)
	observation := history.Latest.At
	report, err := saveEstateReportNow(now, opts, history, estateReportSections, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	history.Changes = []EstateChange{{At: observation.Add(-time.Nanosecond)}, {At: observation}, {At: observation.Add(time.Nanosecond)}, {At: now}, {At: now.Add(time.Nanosecond)}}
	for _, input := range []EstateHistory{history, estateCloneHistory(history)} {
		summary := estateChangesSinceReport(input, opts, now)
		if summary.Status != "available" || summary.Count != 2 || summary.BaselineKind != "on_demand" || summary.ReportID != report.ID || !summary.Since.Equal(observation) || summary.Partial || summary.ObservationAt == nil || !summary.ObservationAt.Equal(observation) {
			t.Fatalf("on-demand observation boundary was wrong: %+v", summary)
		}
	}
	// A newer save without explicit opt-in and an older completed-week report
	// must both leave this baseline intact.
	history.Days[0].At = now
	if _, err := saveEstateReportNow(now.Add(time.Hour), opts, history, estateReportSections, nil, false); err != nil {
		t.Fatal(err)
	}
	estateChangeSummaryWriteRun(t, estateChangeSummaryTestRun(t, history.Scope, "UTC", "2026-10-26", "2026-11-04T18:00:00Z"))
	if summary := estateChangesSinceReport(history, opts, now.Add(time.Hour)); !summary.Since.Equal(observation) {
		t.Fatalf("an unrelated save moved baseline: %+v", summary)
	}
	// The next completed reporting week advances the boundary and includes
	// events at its exclusive period end, preserving existing weekly behavior.
	weekly := estateChangeSummaryTestRun(t, history.Scope, "UTC", "2026-11-02", "2026-11-09T09:00:00Z")
	estateChangeSummaryWriteRun(t, weekly)
	history.Changes = append(history.Changes, EstateChange{At: weekly.Report.Window.End})
	if summary := estateChangesSinceReport(history, opts, weekly.CreatedAt); summary.BaselineKind != "weekly" || summary.Count != 1 || !summary.Since.Equal(weekly.Report.Window.End) {
		t.Fatalf("new completed week failed to advance baseline: %+v", summary)
	}
}

func TestEstateOnDemandStaleObservationWarnsAndDoesNotSkipChanges(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	history, opts := estateNowTestFixture(t, now)
	history.Days[0].At = now.Add(-48 * time.Hour)
	history.Days[0].FirstAt = history.Days[0].At
	report, err := saveEstateReportNow(now, opts, history, estateReportSections, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(report.Quality.Warnings, " "), "48h0m0s old") || !report.ObservationAt.Equal(history.Days[0].At) {
		t.Fatalf("stale observation was hidden or baseline used request time: %+v", report)
	}
	history.Changes = []EstateChange{{At: now.Add(-time.Hour)}}
	if summary := estateChangesSinceReport(history, opts, now); summary.Count != 1 {
		t.Fatalf("changes since stale collection were skipped: %+v", summary)
	}
}

func TestEstateOnDemandRejectsInvalidUnobservedOrUnsaveableReports(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	for _, test := range []struct {
		name string
		edit func(*EstateHistory, *EstateOptions)
		want error
	}{
		{"disabled", func(h *EstateHistory, o *EstateOptions) { o.WeeklyReportEnabled = false }, errEstateSnapshotDisabled},
		{"missing_scope", func(h *EstateHistory, o *EstateOptions) { h.Scope = "" }, errEstateSnapshotScope},
		{"invalid_timezone", func(h *EstateHistory, o *EstateOptions) { h.Timezone = "bad/timezone" }, errEstateSnapshotScope},
		{"missing_observations", func(h *EstateHistory, o *EstateOptions) { h.Days = nil }, errEstateReportNowUnobserved},
		{"only_future_observations", func(h *EstateHistory, o *EstateOptions) { h.Days[0].At = now.Add(time.Hour) }, errEstateReportNowUnobserved},
		{"only_previous_week", func(h *EstateHistory, o *EstateOptions) {
			h.Days[0].At = estateCurrentWeek(now, time.UTC).Start.Add(-time.Nanosecond)
		}, errEstateReportNowUnobserved},
	} {
		t.Run(test.name, func(t *testing.T) {
			history, opts := estateNowTestFixture(t, now)
			test.edit(&history, &opts)
			if _, err := saveEstateReportNow(now, opts, history, estateReportSections, nil, true); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if _, err := os.Stat(dataFilePath("estate-reports")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid report wrote a baseline: %v", err)
			}
		})
	}
	t.Run("save_failure", func(t *testing.T) {
		history, opts := estateNowTestFixture(t, now)
		if err := os.WriteFile(dataFilePath("estate-reports"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := saveEstateReportNow(now, opts, history, estateReportSections, nil, true); err == nil {
			t.Fatal("failed write reported success")
		}
		if summary := estateChangesSinceReport(history, opts, now); summary.Status == "available" {
			t.Fatalf("failed write established a baseline: %+v", summary)
		}
	})
}

func TestEstateOnDemandConcurrentRunsCreateIndependentFrozenReports(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	history, opts := estateNowTestFixture(t, now)
	const count = 8
	results := make(chan estateWeeklyReport, count)
	errors := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			report, err := saveEstateReportNow(now, opts, history, estateReportSections, nil, false)
			results <- report
			errors <- err
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for report := range results {
		if ids[report.ID] || len(report.ID) != 64 {
			t.Fatalf("on-demand invocations reused identity %q", report.ID)
		}
		ids[report.ID] = true
	}
	entries, err := os.ReadDir(dataFilePath("estate-reports"))
	if err != nil || len(entries) != count {
		t.Fatalf("concurrent artifacts missing: %d %v", len(entries), err)
	}
}

func TestEstateOnDemandHTTPValidationAndFrozenDownloads(t *testing.T) {
	now := time.Now()
	history, _ := estateNowTestFixture(t, now)
	var externalRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	configMutex.Lock()
	config.PCEURL = server.URL
	config.Estate.WeeklyScheduleEnabled, config.Estate.WeeklyWebhookEnabled = true, true
	config.Estate.WeeklyWebhookURL = server.URL + "/secret"
	configMutex.Unlock()
	history.Scope = estateScopeForConfig()
	estateMonitor.Lock()
	estateMonitor.history = estateCloneHistory(history)
	estateMonitor.Unlock()
	for _, test := range []struct {
		method, query string
		want          int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "?format=pdf", http.StatusBadRequest},
		{http.MethodPost, "?set_baseline=maybe", http.StatusBadRequest},
		{http.MethodPost, "?sections=invalid", http.StatusBadRequest},
		{http.MethodPost, "?week=2026-11-02", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		handleEstateReportNow(w, httptest.NewRequest(test.method, "/api/estate/report/now"+test.query, nil))
		if w.Code != test.want {
			t.Fatalf("%s %s: got %d: %s", test.method, test.query, w.Code, w.Body.String())
		}
	}
	if _, err := os.Stat(dataFilePath("estate-reports")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid request persisted artifact: %v", err)
	}
	w := httptest.NewRecorder()
	handleEstateReportNow(w, httptest.NewRequest(http.MethodPost, "/api/estate/report/now?theme=illumio-light&set_baseline=true", nil))
	var preview struct {
		Report estateWeeklyReport `json:"report"`
		HTML   string             `json:"html"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &preview) != nil || preview.Report.Kind != "on_demand" || !preview.Report.SetsChangeBaseline || !strings.Contains(preview.HTML, `data-theme="illumio-light"`) {
		t.Fatalf("default POST did not return saved report preview: %d %s", w.Code, w.Body.String())
	}
	path, _ := estateReportRunPath(preview.Report.ID)
	before, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(before), "/secret") {
		t.Fatalf("snapshot missing or contains webhook secret: %v", err)
	}
	w = httptest.NewRecorder()
	handleEstateReportRuns(w, httptest.NewRequest(http.MethodGet, "/api/estate/report/runs?id="+preview.Report.ID+"&format=preview&theme=dark", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), preview.Report.ID) {
		t.Fatalf("saved preview could not be reopened: %d %s", w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) || externalRequests.Load() != 0 {
		t.Fatal("download changed artifact or report contacted PCE/webhook")
	}
	configMutex.Lock()
	config.OrgID = "different-org"
	configMutex.Unlock()
	w = httptest.NewRecorder()
	handleEstateReportRuns(w, httptest.NewRequest(http.MethodGet, "/api/estate/report/runs?id="+preview.Report.ID, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("another organization can read report: %d", w.Code)
	}
}

func TestEstateOnDemandCurrentWeekCalendarAndDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		now, first, last string
		days             int
	}{
		{"2026-11-02T06:00:00Z", "2026-11-02", "2026-11-02", 1},
		{"2026-11-01T18:00:00Z", "2026-10-26", "2026-11-01", 7},
		{"2026-03-08T18:00:00Z", "2026-03-02", "2026-03-08", 7},
	} {
		now := estateReportTestTime(t, test.now)
		window := estateCurrentWeek(now, loc)
		if window.FirstDay != test.first || window.LastDay != test.last || estateReportExpectedDays(window) != test.days || !estateWindowContains(now, window) || estateWindowContains(now.Add(2*time.Nanosecond), window) {
			t.Fatalf("incorrect partial calendar window: %+v", window)
		}
	}
}

func TestEstateOnDemandInvalidSavedBaselineIsNeverSelected(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	for _, test := range []struct {
		name string
		edit func(*estateReportRun)
	}{
		{"unknown_kind", func(r *estateReportRun) { r.Report.Kind = "unknown" }},
		{"not_opted_in", func(r *estateReportRun) { r.Report.SetsChangeBaseline = false }},
		{"missing_observation", func(r *estateReportRun) { r.Report.ObservationAt = nil }},
		{"zero_observation", func(r *estateReportRun) { r.Report.ObservationAt = &time.Time{} }},
		{"future_observation", func(r *estateReportRun) { at := now.Add(time.Hour); r.Report.ObservationAt = &at }},
		{"different_observation", func(r *estateReportRun) { at := now.Add(-time.Minute); r.Report.ObservationAt = &at }},
		{"future_creation", func(r *estateReportRun) { r.Report.GeneratedAt = now.Add(time.Hour) }},
		{"creation_mismatch", func(r *estateReportRun) { r.CreatedAt = now.Add(-time.Hour) }},
		{"missing_quality", func(r *estateReportRun) { r.Report.Quality.LastObservation = nil }},
		{"full_week_flag", func(r *estateReportRun) { r.Report.PartialWeek = false }},
		{"report_id_mismatch", func(r *estateReportRun) { r.Report.ID = strings.Repeat("0", 64) }},
		{"run_id_mismatch", func(r *estateReportRun) { r.ID = strings.Repeat("0", 64) }},
		{"invalid_window", func(r *estateReportRun) { r.Report.Window.End = r.Report.Window.End.Add(time.Hour) }},
		{"wrong_calendar_day", func(r *estateReportRun) { r.Report.Window.LastDay = "2026-11-05" }},
		{"other_scope", func(r *estateReportRun) { r.Scope = "another-estate" }},
		{"other_timezone", func(r *estateReportRun) { r.Report.Timezone = "America/Chicago" }},
		{"delivery_attempted", func(r *estateReportRun) { r.DeliveryAttemptedAt = &now }},
		{"unknown_state", func(r *estateReportRun) { r.State = "failed" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			history, opts := estateNowTestFixture(t, now)
			report, err := saveEstateReportNow(now, opts, history, estateReportSections, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			path, _ := estateReportRunPath(report.ID)
			body, _ := os.ReadFile(path)
			var run estateReportRun
			if err := json.Unmarshal(body, &run); err != nil {
				t.Fatal(err)
			}
			test.edit(&run)
			if err := writeJSONFileAtomic(path, run); err != nil {
				t.Fatal(err)
			}
			if summary := estateChangesSinceReport(history, opts, now); summary.Status != "awaiting_report" || !summary.Since.IsZero() {
				t.Fatalf("invalid saved report changed baseline: %+v", summary)
			}
		})
	}
}

func TestEstatePendingPolicyReportHTMLSeparatesInventoryFromPendingCounts(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	counts := &EstatePendingPolicyCounts{
		Rulesets: EstatePendingPolicyDelta{Added: 1, Modified: 2, Deleted: 3, Total: 6, Complete: true},
		Rules:    EstatePendingPolicyDelta{Added: 4, Modified: 5, Deleted: 6, Total: 15, Complete: true},
	}
	report := estateWeeklyReport{Timezone: "UTC", Window: estateCurrentWeek(now, time.UTC), Sections: []string{"policy"}, Policy: &EstatePolicy{
		At: now, Active: EstatePolicyCount{Rules: 1100, Rulesets: 569, Complete: true}, Draft: EstatePolicyCount{Rules: 1105, Rulesets: 570, Complete: true}, PendingCounts: counts,
	}}
	body, err := renderEstateReportHTML(report, "illumio-light")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Pending policy changes", "<td>Rulesets</td><td>6</td><td>1</td><td>2</td><td>3</td>", "<td>Rules</td><td>15</td><td>4</td><td>5</td><td>6</td>", "<td>Draft</td><td>570</td><td>1105</td>"} {
		if !strings.Contains(string(body), required) {
			t.Errorf("policy report missing %q", required)
		}
	}
	counts.Rules.Complete = false
	counts.Warnings = []string{"Incomplete comparison <escaped>"}
	body, err = renderEstateReportHTML(report, "dark")
	if err != nil || !strings.Contains(string(body), "<td>Rules</td><td colspan=\"4\">Unavailable / incomplete</td>") || !strings.Contains(string(body), "Incomplete comparison &lt;escaped&gt;") {
		t.Fatalf("partial pending count reported as an exact numeric total: %s %v", body, err)
	}
	report.Policy.PendingCounts = nil
	body, err = renderEstateReportHTML(report, "dark")
	if err != nil || !strings.Contains(string(body), "Pending change counts were not collected for this observation") {
		t.Fatalf("legacy report invents pending zero count: %s %v", body, err)
	}
}

func TestEstateReportHTMLPreservesWarningsWithoutQualitySection(t *testing.T) {
	now := estateReportTestTime(t, "2026-11-04T18:00:00Z")
	history, opts := estateNowTestFixture(t, now)
	history.Days[0].At = now.Add(-2 * time.Hour)
	history.Days[0].FirstAt = history.Days[0].At
	history.Days[0].InventoryComplete = false
	history.Days[0].Warnings = []string{"Source detail <must remain escaped>"}
	report, err := saveEstateReportNow(now, opts, history, []string{"estate"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, includeQuality := range []bool{false, true} {
		report.Sections = []string{"estate"}
		if includeQuality {
			report.Sections = append(report.Sections, "quality")
		}
		body, err := renderEstateReportHTML(report, "illumio-light")
		if err != nil {
			t.Fatal(err)
		}
		html := string(body)
		for _, required := range []string{
			"The latest estate collection is 2h0m0s old.",
			"At least one inventory observation was incomplete; partial inventory is not used as a weekly inventory total.",
			"Source detail &lt;must remain escaped&gt;",
		} {
			if strings.Count(html, required) != 1 {
				t.Errorf("quality selected=%v: expected warning once, got %d occurrences of %q", includeQuality, strings.Count(html, required), required)
			}
		}
		if strings.Contains(html, "Important data notes") == includeQuality {
			t.Errorf("quality selected=%v: warning fallback absent or duplicated", includeQuality)
		}
	}
}
