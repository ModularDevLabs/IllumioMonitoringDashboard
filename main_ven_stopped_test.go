package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const stoppedVENScenarioEnv = "ILLUMIO_STOPPED_VEN_TEST_SCENARIO"

// Isolate the shared collector, configuration, SQLite store, and alert state.
// All PCE/webhook fixtures are loopback servers; no customer data is loaded.
func TestStoppedVENRegression(t *testing.T) {
	for _, scenario := range []string{"status-filter", "persistence", "daily-max", "api-export", "alerts", "zero-baseline"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoppedVENHelperProcess$", "-test.v")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), stoppedVENScenarioEnv+"="+scenario)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("stopped VEN scenario %q: %v\n%s", scenario, err, output)
			}
		})
	}
}

func TestStoppedVENHelperProcess(t *testing.T) {
	scenario := os.Getenv(stoppedVENScenarioEnv)
	if scenario == "" {
		return
	}
	dataDir = t.TempDir()
	config = Config{Timezone: "UTC", BlockedPortStoreBackend: "sqlite", BlockedRollingDedupeBackend: "memory",
		VENMAWindow: 2, DailyMAWindow: 2, VENAnomalyPct: 50, VENAnomalyBaseline: "5m", VENAnomalyDays: 3, VENAnomalyMinPct: 100}
	rollingCache = rollingState{}
	venDaily = map[string]venDailySnapshot{}
	currentStats = DashboardStats{}
	alertState = persistedAlertState{SchemaVersion: 1, Targets: map[string]alertTargetState{}, Metrics: map[string]alertTargetState{}}
	anomalyHistory = nil
	initBlockedPortStore()
	if metricsDB == nil {
		t.Fatal("isolated SQLite test store was not initialized")
	}
	t.Cleanup(func() { _ = metricsDB.Close() })
	switch scenario {
	case "status-filter":
		testStoppedVENStatusFiltering(t)
	case "persistence":
		testStoppedVENPersistence(t)
	case "daily-max":
		testStoppedVENDailyMax(t)
	case "api-export":
		testStoppedVENAPIAndExports(t)
	case "alerts":
		testStoppedVENAlerts(t)
	case "zero-baseline":
		testStoppedVENZeroBaselineAndWarmup(t)
	default:
		t.Fatalf("unknown stopped VEN scenario %q", scenario)
	}
}

func stoppedInt(value int) *int { return &value }

func testStoppedVENStatusFiltering(t *testing.T) {
	rows := []map[string]interface{}{
		{"href": "/vens/1", "hostname": "stopped-string", "status": "stopped"},
		{"href": "/vens/2", "hostname": "stopped-nested", "status": map[string]interface{}{"agent_status": "stopped"}},
		{"href": "/vens/3", "hostname": "Stopped is only a hostname", "status": "active"},
		{"href": "/vens/4", "hostname": "reason-only", "status": map[string]interface{}{"agent_status": "active", "reason": "previously stopped"}},
		{"href": "/vens/5", "hostname": "not-stopped", "status": "not_stopped"},
		{"href": "/vens/6", "hostname": "suspended", "status": "suspended"},
		{"href": "/vens/7", "hostname": "unknown"},
	}
	for _, fallback := range []bool{false, true} {
		filteredRequested := false
		allRequested := false
		var mu sync.Mutex
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/vens" {
				t.Errorf("unexpected fixture endpoint: %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			filter := r.URL.Query().Get("status")
			mu.Lock()
			if filter == "stopped" {
				filteredRequested = true
			} else if filter == "" {
				allRequested = true
			} else {
				t.Errorf("wrong status filter: %q", filter)
			}
			mu.Unlock()
			if fallback && filter != "" {
				http.Error(w, "filter unsupported", http.StatusBadRequest)
				return
			}
			result := rows
			if fallback {
				result = rows[:len(rows)-1]
			}
			_ = json.NewEncoder(w).Encode(result)
		}))
		config.PCEURL = server.URL
		got, err := getAllVENsByStatus(server.URL, "stopped")
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, row := range got {
			names = append(names, row["hostname"].(string))
		}
		want := []string{"stopped-string", "stopped-nested"}
		// The official response schema permits a hostname-only row. For a
		// successful server-filtered query, absent status is not evidence of
		// a different state; only explicitly conflicting states are rejected.
		if !fallback {
			want = append(want, "unknown")
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("fallback=%v admitted unrelated status: %v", fallback, names)
		}
		mu.Lock()
		if !filteredRequested || allRequested != fallback {
			t.Errorf("fallback=%v filtered=%v all=%v", fallback, filteredRequested, allRequested)
		}
		mu.Unlock()
	}
	unknownFallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "" {
			http.Error(w, "filter unsupported", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	config.PCEURL = unknownFallback.URL
	known, unknownErr := getAllVENsByStatus(unknownFallback.URL, "stopped")
	unknownFallback.Close()
	if unknownErr == nil || len(known) != 2 {
		t.Fatalf("unfiltered unknown state reported a complete count: known=%v error=%v", known, unknownErr)
	}
	for _, partialSource := range []string{"filtered", "fallback"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			filtered := r.URL.Query().Get("status") == "stopped"
			if (partialSource == "filtered") != filtered || r.URL.Query().Get("skip") != "0" {
				http.Error(w, "fixture incomplete", http.StatusBadRequest)
				return
			}
			batch := make([]map[string]interface{}, 500)
			for i := range batch {
				status := "active"
				if i < 3 {
					status = "stopped"
				}
				batch[i] = map[string]interface{}{"href": fmt.Sprintf("/vens/partial-%d", i), "hostname": fmt.Sprintf("partial-%d", i), "status": status}
			}
			_ = json.NewEncoder(w).Encode(batch)
		}))
		config.PCEURL = server.URL
		partial, partialErr := getAllVENsByStatus(server.URL, "stopped")
		server.Close()
		if partialErr == nil || len(partial) != 3 {
			t.Fatalf("%s partial evidence lost or marked complete: %d %v", partialSource, len(partial), partialErr)
		}
		for _, row := range partial {
			if row["status"] != "stopped" {
				t.Fatal("partial result admitted non-stopped workload")
			}
		}
	}
	for _, bulkFailure := range []bool{true, false} {
		batch := make([]map[string]interface{}, 500)
		for i := range batch {
			batch[i] = map[string]interface{}{"href": fmt.Sprintf("/vens/repeated-%d", i), "hostname": fmt.Sprintf("repeated-%d", i), "status": "stopped"}
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if bulkFailure && r.URL.Query().Get("max_results") == "200000" {
				http.Error(w, "bulk unavailable", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(batch)
		}))
		config.PCEURL = server.URL
		partial, partialErr := getAllVENsByStatus(server.URL, "stopped")
		server.Close()
		if partialErr == nil || len(partial) != 500 {
			t.Fatalf("repeated full page with bulkFailure=%v marked complete: %d %v", bulkFailure, len(partial), partialErr)
		}
	}
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fixture unavailable", http.StatusBadRequest)
	}))
	defer failure.Close()
	config.PCEURL = failure.URL
	if rows, err := getAllVENsByStatus(failure.URL, "stopped"); err == nil || len(rows) != 0 {
		t.Fatalf("failed query masqueraded as an available zero: %v %v", rows, err)
	}
}

func testStoppedVENPersistence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	legacy := persistedRollingState{SchemaVersion: rollingStateSchemaVersion, Initialized: true, LastCycle: now,
		BaselineBlocked: map[string]persistedTargetBaseline{"Payments": {Count: 19, CapturedUTC: now.Add(-time.Hour)}},
		Buckets: []persistedRollingBucket{
			{EndUTC: now.Add(-15 * time.Minute), VENWarningCount: 8, VENSuspendedCount: 4},
			{EndUTC: now.Add(-10 * time.Minute), VENStoppedCount: stoppedInt(0)},
			{EndUTC: now.Add(-5 * time.Minute), VENStoppedCount: stoppedInt(3)},
		}}
	if err := sqliteKVSetJSON(stateKeyRolling, legacy); err != nil {
		t.Fatal(err)
	}
	day := func(delta int) string { return now.AddDate(0, 0, delta).Format("2006-01-02") }
	records := []venDailyRecord{
		{Day: day(-3), WarningMax: 10, SuspendedMax: 7},
		{Day: day(-2), StoppedMax: stoppedInt(0)},
		{Day: day(-1), StoppedMax: stoppedInt(4)},
	}
	if err := sqliteKVSetJSON(stateKeyVENDaily, records); err != nil {
		t.Fatal(err)
	}
	loadRollingState()
	loadVENHistory()
	series := venTrendSeries("stopped")
	if len(series) != 2 || series[0].Value != 0 || series[1].Value != 3 {
		t.Fatalf("legacy/unavailable rolling points became zero samples: %#v", series)
	}
	daily := venDailyTrendSeries("stopped", 365)
	if len(daily) != 2 || daily[0].Value != 0 || daily[1].Value != 4 {
		t.Fatalf("legacy/unavailable daily points became zero samples: %#v", daily)
	}
	if rollingCache.Buckets[0].VENSuspendedCount != 4 || rollingCache.BaselineBlocked["Payments"].Count != 19 || venDaily[day(-3)].WarningMax != 10 {
		t.Fatal("stopped VEN migration reset unrelated history")
	}
	saveRollingState()
	saveVENHistory()
	var persisted persistedRollingState
	if ok, err := sqliteKVGetJSON(stateKeyRolling, &persisted); err != nil || !ok {
		t.Fatalf("read rolling state: %v %v", ok, err)
	}
	if persisted.Buckets[0].VENStoppedCount != nil || persisted.Buckets[1].VENStoppedCount == nil || *persisted.Buckets[1].VENStoppedCount != 0 {
		t.Fatal("save lost distinction between unavailable and real zero")
	}
	loadRollingState()
	loadVENHistory()
	if !reflect.DeepEqual(venTrendSeries("stopped"), series) || !reflect.DeepEqual(venDailyTrendSeries("stopped", 365), daily) {
		t.Fatal("stopped history changed on restart")
	}
	for _, sample := range []*int{nil, stoppedInt(0)} {
		now = now.Add(time.Minute)
		updateRollingAndBuildView(now, false, 1, 2, 3, sample, 4, 5, 6, 7, 8,
			tamperingWindowResult{Complete: true, FullWindow: true}, nil, nil, nil)
		got := rollingCache.Buckets[len(rollingCache.Buckets)-1].VENStoppedCount
		if (got == nil) != (sample == nil) || (got != nil && *got != *sample) {
			t.Fatalf("collector bucket changed stopped availability: got %v want %v", got, sample)
		}
	}
}

func testStoppedVENDailyMax(t *testing.T) {
	now := time.Now().UTC()
	for _, sample := range []*int{stoppedInt(2), stoppedInt(0), nil, stoppedInt(5), stoppedInt(1)} {
		updateVENDailyHistory(now, 4, 3, 2, sample, 1, 0, 0, 0, 0, 0)
	}
	snapshot := venDaily[now.Format("2006-01-02")]
	if snapshot.StoppedMax == nil || *snapshot.StoppedMax != 5 {
		t.Fatalf("daily stopped maximum lost: %#v", snapshot)
	}
	if snapshot.WarningMax != 4 || snapshot.ErrorMax != 3 || snapshot.SuspendedMax != 2 {
		t.Fatal("stopped daily sampling changed other VEN daily maxima")
	}
	yesterday := now.AddDate(0, 0, -1)
	updateVENDailyHistory(yesterday, 1, 0, 0, nil, 0, 0, 0, 0, 0, 0)
	if venDaily[yesterday.Format("2006-01-02")].StoppedMax != nil {
		t.Fatal("failed-only day acquired an artificial zero baseline")
	}
	updateVENDailyHistory(yesterday, 1, 0, 0, stoppedInt(0), 0, 0, 0, 0, 0, 0)
	if got := venDaily[yesterday.Format("2006-01-02")].StoppedMax; got == nil || *got != 0 {
		t.Fatal("genuine zero sample was not recorded")
	}
	loadVENHistory()
	if got := venDaily[now.Format("2006-01-02")].StoppedMax; got == nil || *got != 5 {
		t.Fatal("daily stopped maximum did not persist")
	}
}

func stoppedFixtureSeries(values ...int) {
	now := time.Now().UTC()
	rollingCache.Buckets = nil
	for i, value := range values {
		rollingCache.Buckets = append(rollingCache.Buckets, rollingBucket{EndUTC: now.Add(time.Duration(i-len(values)+1) * 5 * time.Minute), VENStoppedCount: stoppedInt(value)})
	}
}

func testStoppedVENAPIAndExports(t *testing.T) {
	stoppedFixtureSeries(1, 1, 5)
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		venDaily[now.AddDate(0, 0, -i).Format("2006-01-02")] = venDailySnapshot{StoppedMax: stoppedInt(2)}
	}
	currentStats.VENStatus.Stopped = []string{"stopped-b", "stopped-a"}
	currentStats.VENStatus.StoppedStatus = FetchStatus{Success: true}
	for _, source := range []string{"5m", "daily"} {
		config.VENAnomalyBaseline = source
		recorder := httptest.NewRecorder()
		handleDrilldown(recorder, httptest.NewRequest(http.MethodGet, "/api/drilldown?metric=ven_stopped", nil))
		var response DrilldownResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || response.Count != 2 || !reflect.DeepEqual(response.Items, []string{"stopped-a", "stopped-b"}) || response.Status == nil || !response.Status.Success {
			t.Fatalf("stopped drilldown response = %#v", response)
		}
		wantMA24h := 1
		if source == "daily" {
			wantMA24h = 3
		}
		if len(response.Trend24h) != 3 || len(response.TrendDaily) != 3 || len(response.TrendMA24h) != wantMA24h || len(response.TrendMADaily) != 1 {
			t.Fatalf("stopped drilldown missing trend/MA: %#v", response)
		}
		if response.AnomalySource != source || response.AnomalyPct != 50 || !response.Anomalous {
			t.Fatalf("stopped metric did not use shared VEN anomaly settings: %#v", response)
		}
	}
	currentStats.VENStatus.StoppedStatus = FetchStatus{Success: false, Error: "PCE unavailable"}
	recorder := httptest.NewRecorder()
	handleDrilldown(recorder, httptest.NewRequest(http.MethodGet, "/api/drilldown?metric=ven_stopped", nil))
	var failed DrilldownResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status == nil || failed.Status.Success || failed.Status.Error != "PCE unavailable" {
		t.Fatal("failed stopped collection was not surfaced")
	}
	if failed.Anomalous {
		t.Fatal("unavailable stopped drilldown advertised a current anomaly")
	}
	otherRecorder := httptest.NewRecorder()
	handleDrilldown(otherRecorder, httptest.NewRequest(http.MethodGet, "/api/drilldown?metric=ven_warning", nil))
	var otherResponse DrilldownResponse
	if err := json.Unmarshal(otherRecorder.Body.Bytes(), &otherResponse); err != nil {
		t.Fatal(err)
	}
	if otherResponse.Status != nil {
		t.Fatal("stopped availability metadata leaked into an unrelated metric")
	}
	statsRecorder := httptest.NewRecorder()
	handleStats(statsRecorder, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if !strings.Contains(statsRecorder.Body.String(), "\"stopped_status\"") || !strings.Contains(statsRecorder.Body.String(), "stopped-a") {
		t.Fatal("stats response omitted stopped data/status")
	}
	for _, endpoint := range []struct {
		handler http.HandlerFunc
		path    string
	}{
		{handleExportDrilldownCSV, "/api/export/drilldown.csv?metric=ven_stopped"},
		{handleExportReportCSV, "/api/export/report.csv"},
	} {
		recorder := httptest.NewRecorder()
		endpoint.handler(recorder, httptest.NewRequest(http.MethodGet, endpoint.path, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "stopped-a") || !strings.Contains(strings.ToLower(recorder.Body.String()), "stopped") {
			t.Fatalf("stopped CSV missing workload: %d %s", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "Unavailable") || !strings.Contains(recorder.Body.String(), "PCE unavailable") {
			t.Fatalf("failed stopped collection exported as a valid current count: %s", recorder.Body.String())
		}
	}
	currentStats.VENStatus.StoppedStatus = FetchStatus{Success: true}
	currentStats.VENStatus.Stopped = nil
	for _, endpoint := range []struct {
		handler       http.HandlerFunc
		path, zeroRow string
	}{
		{handleExportDrilldownCSV, "/api/export/drilldown.csv?metric=ven_stopped", "Item Count,0"},
		{handleExportReportCSV, "/api/export/report.csv", "Stopped VENs,0"},
	} {
		recorder := httptest.NewRecorder()
		endpoint.handler(recorder, httptest.NewRequest(http.MethodGet, endpoint.path, nil))
		if !strings.Contains(recorder.Body.String(), endpoint.zeroRow) || strings.Contains(recorder.Body.String(), "Unavailable") {
			t.Fatalf("genuine zero exported as unavailable: %s", recorder.Body.String())
		}
	}
}

func testStoppedVENAlerts(t *testing.T) {
	var mu sync.Mutex
	deliveries := []map[string]interface{}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook: %v", err)
			http.Error(w, "bad JSON", 400)
			return
		}
		mu.Lock()
		deliveries = append(deliveries, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	config.WebhookEnabled = true
	config.WebhookURL = server.URL + "/hook"
	config.WebhookProvider = "generic"
	config.WebhookPrivateAllowedOrigins = []string{server.URL}
	stats := DashboardStats{}
	stats.VENStatus.StoppedStatus = FetchStatus{Success: true}
	stoppedFixtureSeries(5)
	processWebhookAlerts(stats)
	if alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 0 {
		t.Fatal("stopped warmup triggered an alert")
	}
	stoppedFixtureSeries(1, 1, 5)
	processWebhookAlerts(stats)
	if !alertState.Metrics["ven_stopped"].Active {
		t.Fatal("stopped spike did not activate alert")
	}
	processWebhookAlerts(stats)
	if len(anomalyHistory) != 1 {
		t.Fatal("unchanged active alert duplicated history")
	}
	stoppedFixtureSeries(1, 1, 0)
	stats.VENStatus.StoppedStatus = FetchStatus{Success: false, Error: "query failed"}
	processWebhookAlerts(stats)
	if !alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 1 {
		t.Fatal("failed stopped fetch falsely resolved alert")
	}
	stats.VENStatus.StoppedStatus = FetchStatus{Success: true}
	processWebhookAlerts(stats)
	if alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 2 {
		t.Fatal("successful zero sample did not resolve stopped alert")
	}
	for i, state := range []string{"triggered", "resolved"} {
		if event := anomalyHistory[i]; event.Event != "ven_stopped_anomaly" || event.Metric != "ven_stopped" || event.State != state {
			t.Fatalf("wrong stopped anomaly history: %#v", event)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("stopped webhook deliveries = %d, want trigger and resolve", len(deliveries))
	}
	for i, state := range []string{"triggered", "resolved"} {
		if deliveries[i]["event"] != "ven_stopped_anomaly" || deliveries[i]["state"] != state {
			t.Fatalf("wrong stopped webhook payload: %#v", deliveries[i])
		}
	}
	loadAlertState()
	if alertState.Metrics["ven_stopped"].LastEventType != "resolved" {
		t.Fatal("stopped alert state did not persist")
	}
	loadAnomalyHistory()
	if len(anomalyHistory) != 2 || anomalyHistory[0].Event != "ven_stopped_anomaly" || anomalyHistory[1].State != "resolved" {
		t.Fatal("stopped trigger/resolve history did not persist")
	}
	config.VENAnomalyBaseline = "daily"
	stoppedFixtureSeries(7)
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		venDaily[now.AddDate(0, 0, -i).Format("2006-01-02")] = venDailySnapshot{StoppedMax: stoppedInt(2)}
	}
	for _, metric := range anomalyMetricSnapshots() {
		if metric.Key == "ven_stopped" && (!metric.Eval.Anomalous || metric.Source != "daily" || metric.Eval.Baseline != 2) {
			t.Fatalf("daily stopped baseline evaluation failed: %#v", metric)
		}
	}
	venDaily[now.AddDate(0, 0, -1).Format("2006-01-02")] = venDailySnapshot{}
	for _, metric := range anomalyMetricSnapshots() {
		if metric.Key == "ven_stopped" && (metric.Eval.Anomalous || metric.Eval.CoveragePct >= 100) {
			t.Fatalf("unavailable daily history counted toward baseline coverage: %#v", metric)
		}
	}
}

func testStoppedVENZeroBaselineAndWarmup(t *testing.T) {
	for _, source := range []string{"5m", "daily"} {
		t.Run(source, func(t *testing.T) {
			config.VENAnomalyBaseline = source
			config.VENMAWindow = 2
			config.VENAnomalyDays = 3
			alertState = persistedAlertState{SchemaVersion: 1, Targets: map[string]alertTargetState{}, Metrics: map[string]alertTargetState{}}
			anomalyHistory = nil
			venDaily = map[string]venDailySnapshot{}
			now := time.Now().UTC()
			for i := 1; i <= 3; i++ {
				venDaily[now.AddDate(0, 0, -i).Format("2006-01-02")] = venDailySnapshot{StoppedMax: stoppedInt(0)}
			}
			stoppedFixtureSeries(0, 0, 1)
			found := false
			for _, metric := range anomalyMetricSnapshots() {
				if metric.Key != "ven_stopped" {
					continue
				}
				found = true
				if !metric.Eval.Anomalous || metric.Eval.Baseline != 0 || metric.Eval.Latest != 1 {
					t.Fatalf("healthy-zero to one-stopped transition did not trigger: %#v", metric)
				}
			}
			if !found {
				t.Fatal("stopped anomaly snapshot missing")
			}
			stats := DashboardStats{}
			stats.VENStatus.StoppedStatus = FetchStatus{Success: false, Error: "query unavailable"}
			processWebhookAlerts(stats)
			if alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 0 {
				t.Fatal("failed collection triggered a stopped alert")
			}
			stats.VENStatus.StoppedStatus = FetchStatus{Success: true}
			processWebhookAlerts(stats)
			if !alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 1 {
				t.Fatal("positive stopped count over healthy baseline did not activate alert")
			}
			if source == "5m" {
				config.VENMAWindow = 12
			} else {
				config.VENAnomalyDays = 5
			}
			processWebhookAlerts(stats)
			if !alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 1 {
				t.Fatal("increased baseline window/warmup falsely resolved active alert")
			}
			config.VENMAWindow = 2
			config.VENAnomalyDays = 3
			if source == "daily" {
				venDaily[now.AddDate(0, 0, -1).Format("2006-01-02")] = venDailySnapshot{}
				processWebhookAlerts(stats)
				if !alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 1 {
					t.Fatal("missing daily coverage falsely resolved active alert")
				}
				venDaily[now.AddDate(0, 0, -1).Format("2006-01-02")] = venDailySnapshot{StoppedMax: stoppedInt(0)}
			}
			stoppedFixtureSeries(0, 0, 0)
			processWebhookAlerts(stats)
			if alertState.Metrics["ven_stopped"].Active || len(anomalyHistory) != 2 || anomalyHistory[1].State != "resolved" {
				t.Fatal("confirmed healthy-zero recovery did not resolve alert")
			}
		})
	}
}
