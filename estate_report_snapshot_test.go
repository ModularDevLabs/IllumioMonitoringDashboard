package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func estateSnapshotTestFixture(t *testing.T) (EstateHistory, estateReportWindow, EstateOptions) {
	t.Helper()
	oldDir := dataDir
	dataDir = t.TempDir()
	configMutex.Lock()
	oldConfig := config
	config = Config{PCEURL: "https://snapshot-test.example.invalid", OrgID: "1", Timezone: "UTC", Estate: EstateOptions{WeeklyReportEnabled: true}}
	configMutex.Unlock()
	estateMonitor.Lock()
	oldHistory, oldLoaded, oldWarnings := estateMonitor.history, estateMonitor.loaded, estateMonitor.warnings
	oldPolicy, oldAttempt := estateMonitor.policy, estateMonitor.policyAttempt
	estateMonitor.Unlock()
	t.Cleanup(func() {
		dataDir = oldDir
		configMutex.Lock()
		config = oldConfig
		configMutex.Unlock()
		estateMonitor.Lock()
		estateMonitor.history, estateMonitor.loaded, estateMonitor.warnings = oldHistory, oldLoaded, oldWarnings
		estateMonitor.policy, estateMonitor.policyAttempt = oldPolicy, oldAttempt
		estateMonitor.Unlock()
	})
	window := estateCompletedWeek(time.Now(), time.UTC)
	history := EstateHistory{
		Scope: estateScopeForConfig(), Timezone: "UTC", StartedAt: window.Start,
		Days: []EstateSnapshot{{At: window.End.Add(-time.Hour), FirstAt: window.End.Add(-2 * time.Hour), InventoryComplete: true, Total: 12, Managed: 9, Unmanaged: 3}},
	}
	history.Latest = &history.Days[0]
	estateMonitor.Lock()
	estateMonitor.history, estateMonitor.loaded = estateCloneHistory(history), true
	estateMonitor.warnings, estateMonitor.policy = nil, nil
	estateMonitor.policyAttempt = time.Time{}
	estateMonitor.Unlock()
	return history, window, configuredEstateOptions()
}

func TestEstateManualSnapshotImmutableAndIdempotent(t *testing.T) {
	history, window, opts := estateSnapshotTestFixture(t)
	now := time.Now().UTC()
	first, err := saveEstateReportSnapshot(now, opts, history, window, []string{"estate", "quality"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := estateReportRunPath(estateReportRunID("manual:"+history.Scope, window))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("snapshot must be private: %v %v", info, err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || (runtime.GOOS != "windows" && directory.Mode().Perm() != 0o700) {
		t.Fatalf("snapshot directory must be private: %v %v", directory, err)
	}
	history.Days[0].Total = 999
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := saveEstateReportSnapshot(now.Add(time.Hour), opts, history, window, []string{"policy"}, nil)
			if err != nil || got.Estate == nil || got.Estate.Total.Current != 12 || !got.GeneratedAt.Equal(first.GeneratedAt) || !reflect.DeepEqual(got.Sections, first.Sections) {
				t.Errorf("repeat changed the frozen snapshot: %+v %v", got, err)
			}
		}()
	}
	wg.Wait()
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("repeat overwrote saved report: %v", err)
	}
	var run estateReportRun
	if err := json.Unmarshal(after, &run); err != nil || run.State != "generated" || run.DeliveryAttemptedAt != nil || run.Scope != history.Scope || !strings.Contains(run.Message, "Manual weekly snapshot") {
		t.Fatalf("incorrect manual report metadata: %+v %v", run, err)
	}
	if _, err := saveEstateReportSnapshot(now.Add(-time.Millisecond), opts, history, window, estateReportSections, nil); err != nil {
		t.Fatalf("request captured before the first save should still be idempotent: %v", err)
	}
}

func TestEstateManualSnapshotDoesNotSuppressScheduledDelivery(t *testing.T) {
	history, window, opts := estateSnapshotTestFixture(t)
	opts.WeeklyScheduleEnabled, opts.WeeklyWebhookEnabled = true, true
	opts.WeeklyWebhookURL = "https://delivery.example.invalid/secret-token"
	now := window.End.Add(10 * time.Hour)
	if _, err := saveEstateReportSnapshot(now, opts, history, window, estateReportSections, nil); err != nil {
		t.Fatal(err)
	}
	manualID := estateReportRunID("manual:"+history.Scope, window)
	scheduledID := estateReportRunID(history.Scope, window)
	if manualID == scheduledID {
		t.Fatal("manual snapshot consumed scheduler ID")
	}
	manualPath, _ := estateReportRunPath(manualID)
	before, _ := os.ReadFile(manualPath)
	deliveries := 0
	if err := runEstateReportSchedule(now, opts, time.UTC, history, nil, func(estateWeeklyReport) error { deliveries++; return nil }); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("manual save suppressed scheduled delivery: %d", deliveries)
	}
	scheduledPath, _ := estateReportRunPath(scheduledID)
	if _, err := os.Stat(scheduledPath); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(manualPath)
	if string(before) != string(after) || strings.Contains(string(after), "secret-token") {
		t.Fatal("scheduler changed the manual snapshot or persisted webhook secrets")
	}
}

func TestEstateManualSnapshotRejectsMissingScopeObservationsAndDisabledReports(t *testing.T) {
	history, window, opts := estateSnapshotTestFixture(t)
	for _, test := range []struct {
		name string
		edit func(*EstateOptions, *EstateHistory)
		want error
	}{
		{"disabled", func(o *EstateOptions, h *EstateHistory) { o.WeeklyReportEnabled = false }, errEstateSnapshotDisabled},
		{"scope", func(o *EstateOptions, h *EstateHistory) { h.Scope = "" }, errEstateSnapshotScope},
		{"timezone", func(o *EstateOptions, h *EstateHistory) { h.Timezone = "Invalid/Zone" }, errEstateSnapshotScope},
		{"wrong_timezone", func(o *EstateOptions, h *EstateHistory) { h.Timezone = "America/Chicago" }, errEstateSnapshotScope},
		{"unobserved", func(o *EstateOptions, h *EstateHistory) { h.Days, h.Latest = nil, nil }, errEstateSnapshotUnobserved},
		{"outside_week", func(o *EstateOptions, h *EstateHistory) { h.Days[0].At = window.End.Add(time.Hour) }, errEstateSnapshotUnobserved},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, captured := opts, estateCloneHistory(history)
			test.edit(&options, &captured)
			if _, err := saveEstateReportSnapshot(time.Now(), options, captured, window, estateReportSections, nil); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if _, err := os.Stat(dataFilePath("estate-reports")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid snapshot request wrote report history: %v", err)
			}
		})
	}
}

func TestEstateManualSnapshotDoesNotOverwriteConflictingArtifact(t *testing.T) {
	history, window, opts := estateSnapshotTestFixture(t)
	now := time.Now().UTC()
	_, err := saveEstateReportSnapshot(now, opts, history, window, estateReportSections, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := estateReportRunID("manual:"+history.Scope, window)
	path, _ := estateReportRunPath(id)
	body, _ := os.ReadFile(path)
	var original estateReportRun
	if err := json.Unmarshal(body, &original); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*estateReportRun)
	}{
		{"scope", func(r *estateReportRun) { r.Scope = "other-estate" }},
		{"timezone", func(r *estateReportRun) { r.Report.Timezone = "America/Chicago" }},
		{"id", func(r *estateReportRun) { r.ID = "other-id" }},
		{"state", func(r *estateReportRun) { r.State = "delivered" }},
		{"window", func(r *estateReportRun) { r.Report.Window.End = r.Report.Window.End.Add(time.Hour) }},
		{"unobserved_generation", func(r *estateReportRun) { r.Report.GeneratedAt = time.Time{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := original
			test.edit(&run)
			if err := writeJSONFileAtomic(path, run); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if _, err := saveEstateReportSnapshot(now.Add(time.Hour), opts, history, window, estateReportSections, nil); !errors.Is(err, errEstateSnapshotConflict) {
				t.Fatalf("conflicting snapshot accepted: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("conflicting artifact was overwritten")
			}
		})
	}
	if err := os.WriteFile(path, []byte("not valid JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveEstateReportSnapshot(now.Add(time.Hour), opts, history, window, estateReportSections, nil); !errors.Is(err, errEstateSnapshotConflict) {
		t.Fatalf("corrupt artifact accepted: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != "not valid JSON" {
		t.Fatal("corrupt artifact was overwritten")
	}
}

func TestEstateManualSnapshotRejectsMissingWeekStart(t *testing.T) {
	history, window, opts := estateSnapshotTestFixture(t)
	window.FirstDay = ""
	if _, err := saveEstateReportSnapshot(time.Now(), opts, history, window, estateReportSections, nil); !errors.Is(err, errEstateSnapshotScope) {
		t.Fatalf("missing week start was accepted: %v", err)
	}
	if _, err := os.Stat(dataFilePath("estate-reports")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid window wrote report history: %v", err)
	}
}

func TestEstateManualSnapshotHTTPValidationAndReadOnlyPreviews(t *testing.T) {
	_, _, _ = estateSnapshotTestFixture(t)
	for _, test := range []struct {
		method, query string
		want          int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "?format=pdf", http.StatusBadRequest},
		{http.MethodPost, "?sections=invalid", http.StatusBadRequest},
		{http.MethodPost, "?week=invalid", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		handleEstateReportSnapshot(w, httptest.NewRequest(test.method, "/api/estate/report/snapshot"+test.query, nil))
		if w.Code != test.want {
			t.Fatalf("%s %s: got %d: %s", test.method, test.query, w.Code, w.Body.String())
		}
		if test.method == http.MethodGet && w.Header().Get("Allow") != "POST" {
			t.Fatal("GET did not advertise POST-only route")
		}
	}
	w := httptest.NewRecorder()
	handleEstateReport(w, httptest.NewRequest(http.MethodGet, "/api/estate/report?format=preview", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("preview failed: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(dataFilePath("estate-reports")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid mutation or read-only preview wrote a snapshot: %v", err)
	}
	configMutex.Lock()
	config.Estate.WeeklyReportEnabled = false
	configMutex.Unlock()
	w = httptest.NewRecorder()
	handleEstateReportSnapshot(w, httptest.NewRequest(http.MethodPost, "/api/estate/report/snapshot", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled reports accepted save: %d %s", w.Code, w.Body.String())
	}
}

func TestEstateManualSnapshotHTTPReturnsSavedPreviewWithoutNetworkOrCrossScope(t *testing.T) {
	history, window, _ := estateSnapshotTestFixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	configMutex.Lock()
	config.PCEURL = server.URL
	config.Estate.WeeklyScheduleEnabled, config.Estate.WeeklyWebhookEnabled = true, true
	config.Estate.WeeklyWebhookURL = server.URL
	configMutex.Unlock()
	history.Scope = estateScopeForConfig()
	estateMonitor.Lock()
	estateMonitor.history = estateCloneHistory(history)
	estateMonitor.Unlock()
	w := httptest.NewRecorder()
	handleEstateReportSnapshot(w, httptest.NewRequest(http.MethodPost, "/api/estate/report/snapshot?theme=illumio-light", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		Report estateWeeklyReport `json:"report"`
		HTML   string             `json:"html"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview.Report.Estate == nil || preview.Report.Estate.Total.Current != 12 || !strings.Contains(preview.HTML, `data-theme="illumio-light"`) {
		t.Fatalf("default format did not return matching preview: %+v %v", preview, err)
	}
	path, _ := estateReportRunPath(estateReportRunID("manual:"+history.Scope, window))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("save announced success before persisting: %v", err)
	}
	configMutex.Lock()
	config.OrgID = "2"
	configMutex.Unlock()
	w = httptest.NewRecorder()
	handleEstateReportSnapshot(w, httptest.NewRequest(http.MethodPost, "/api/estate/report/snapshot", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown new estate accepted or leaked previous report: %d %s", w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || requests.Load() != 0 {
		t.Fatalf("save contacted a webhook/PCE or modified another estate: %d requests", requests.Load())
	}
}
