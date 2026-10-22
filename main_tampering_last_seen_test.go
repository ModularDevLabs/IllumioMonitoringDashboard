package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func isolateTamperingState(t *testing.T) {
	t.Helper()
	configMutex.Lock()
	previousConfig := config
	config.Timezone = "UTC"
	config.BlockedPortStoreBackend = "sqlite"
	config.BlockedRollingDedupeBackend = "memory"
	configMutex.Unlock()
	rollingMu.Lock()
	previousRolling := rollingCache
	rollingCache = rollingState{}
	rollingMu.Unlock()
	statsMutex.Lock()
	previousStats := currentStats
	currentStats = DashboardStats{}
	statsMutex.Unlock()
	previousDataDir := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() {
		configMutex.Lock()
		config = previousConfig
		configMutex.Unlock()
		rollingMu.Lock()
		rollingCache = previousRolling
		rollingMu.Unlock()
		statsMutex.Lock()
		currentStats = previousStats
		statsMutex.Unlock()
		dataDir = previousDataDir
	})
}

func tamperEvent(name, field, timestamp string) map[string]interface{} {
	return map[string]interface{}{"event_type": "agent.tampering", field: timestamp, "workload": map[string]interface{}{"hostname": name}}
}

func TestTamperingActualTimestampParsingAndLatestDeduplication(t *testing.T) {
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	latest := tamperEvent("alpha", "event_timestamp", "2026-10-15T12:30:00-05:00")
	events := []map[string]interface{}{
		latest,
		tamperEvent("alpha", "timestamp", "2026-10-15T16:00:00.123Z"),
		latest,
		{"event_type": "agent.tampering", "payload": map[string]interface{}{"occurred_at": "2026-10-15T17:45:00Z", "workload": map[string]interface{}{"name": "beta"}}},
		tamperEvent("expired", "timestamp", "2026-10-14T17:00:00Z"),
		tamperEvent("future", "timestamp", "2026-10-16T17:00:00Z"),
		tamperEvent("unknown-time", "timestamp", "not-a-date"),
	}
	result := summarizeTamperingEvents(events, now.Add(-24*time.Hour), now)
	if result.EventCount != 3 || !reflect.DeepEqual(result.Names, []string{"alpha", "beta"}) {
		t.Fatalf("deduplicated events/names = %#v", result)
	}
	if want := now.Add(-30 * time.Minute); !result.LastSeen["alpha"].Equal(want) {
		t.Fatalf("latest alpha event = %v, want %v (not query time)", result.LastSeen["alpha"], want)
	}
	if result.LastSeen["alpha"].Location() != time.UTC {
		t.Fatal("event offset was not normalized to UTC")
	}
}

func updateTamperingFixture(now time.Time, baseline bool, result tamperingWindowResult) []string {
	_, names, _, _, _, _, _ := updateRollingAndBuildView(now, baseline, 2, 3, 0, nil, 4, 5, 6, 7, 8,
		result, map[string]int{"Payments": 2}, map[string]int{}, []TrafficTarget{{Name: "Payments"}})
	return names
}

func TestTamperingBaselineExpiresByEventTimeAndIncrementalNeverMovesBackwards(t *testing.T) {
	isolateTamperingState(t)
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	old := now.Add(-24*time.Hour + time.Minute)
	recent := now.Add(-time.Hour)
	result := tamperingWindowResult{Complete: true, FullWindow: true, IntervalStart: now.Add(-24 * time.Hour),
		Names: []string{"old", "recent"}, LastSeen: map[string]time.Time{"old": old, "recent": recent}}
	if names := updateTamperingFixture(now, true, result); !reflect.DeepEqual(names, []string{"old", "recent"}) {
		t.Fatal(names)
	}
	if rollingCache.Buckets[0].TamperingCount != 0 {
		t.Fatal("startup baseline became a 5-minute spike")
	}
	next := now.Add(5 * time.Minute)
	result = tamperingWindowResult{Complete: true, IntervalStart: now, LastSeen: map[string]time.Time{"recent": recent.Add(-time.Minute)}}
	if names := updateTamperingFixture(next, false, result); !reflect.DeepEqual(names, []string{"recent"}) {
		t.Fatalf("expired baseline survived: %v", names)
	}
	if !rollingTamperingLastSeen(next)["recent"].Equal(recent) {
		t.Fatal("out-of-order event overwrote newer timestamp")
	}
	if rollingCache.Buckets[1].TamperingCount != 0 {
		t.Fatal("old repeated event was counted in current interval")
	}
	last := next.Add(4 * time.Minute)
	result = tamperingWindowResult{Complete: true, IntervalStart: next, LastSeen: map[string]time.Time{"recent": last}}
	updateTamperingFixture(next.Add(5*time.Minute), false, result)
	if !rollingTamperingLastSeen(next.Add(5 * time.Minute))["recent"].Equal(last) {
		t.Fatal("new event did not advance last seen")
	}
	if rollingCache.Buckets[2].TamperingCount != 1 {
		t.Fatal("current interval count changed meaning")
	}
}

func TestTamperingFailurePreservesEvidenceAndRequestsRecoveryBackfill(t *testing.T) {
	isolateTamperingState(t)
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	rollingCache = rollingState{Initialized: true, LastCycle: now.Add(-5 * time.Minute), TamperingTimestampsReady: true,
		TamperingCollectionVersion: tamperingCollectionMethodVersion,
		TamperingLastSeen:          map[string]time.Time{"known": now.Add(-time.Hour)}, BaselineBlocked: map[string]targetBaseline{}}
	result := tamperingWindowResult{Complete: false, IntervalStart: now.Add(-5 * time.Minute), LastSeen: map[string]time.Time{"partial": now.Add(-time.Minute)}}
	if names := updateTamperingFixture(now, false, result); !reflect.DeepEqual(names, []string{"known", "partial"}) {
		t.Fatalf("partial read lost known events: %v", names)
	}
	start, _, backfill := tamperingCollectionWindow(now.Add(5*time.Minute), now, now.Add(5*time.Minute), false)
	if !backfill || !start.Equal(now.Add(5*time.Minute-24*time.Hour)) {
		t.Fatal("failed query cursor did not request 24h recovery")
	}
	updateTamperingFixture(now.Add(5*time.Minute), false, tamperingWindowResult{Complete: true, FullWindow: true, IntervalStart: now})
	if rollingCache.TamperingBackfillPending {
		t.Fatal("successful recovery did not clear pending status")
	}
	if got := rollingTamperingLastSeen(now.Add(5 * time.Minute)); len(got) != 2 {
		t.Fatalf("empty recovery erased known recent evidence: %v", got)
	}
	rollingCache.LastCycle = now.Add(-2 * time.Hour)
	if _, _, needed := tamperingCollectionWindow(now, now.Add(-5*time.Minute), now, false); !needed {
		t.Fatal("restart gap did not trigger tampering-only backfill")
	}
}

func TestTamperingLegacyMigrationPreservesOtherHistoryAndAvoidsIntervalSpike(t *testing.T) {
	isolateTamperingState(t)
	now := time.Now().UTC().Truncate(time.Minute)
	legacy := persistedRollingState{SchemaVersion: rollingStateSchemaVersion, Initialized: true, LastCycle: now.Add(-5 * time.Minute),
		BaselineCapturedUTC: now.Add(-time.Hour), BaselineWorkloads: []string{"legacy-unknown"},
		BaselineBlocked: map[string]persistedTargetBaseline{"Payments": {Count: 22, CapturedUTC: now.Add(-time.Hour)}},
		Buckets: []persistedRollingBucket{{EndUTC: now.Add(-5 * time.Minute), VENWarningCount: 9, TamperingCount: 1,
			TamperingWorkloads: []string{"legacy-unknown"}, BlockedByTarget: map[string]int{"Payments": 7}}}}
	if err := writeJSONFileAtomic(dataFilePath(rollingStateFileName), legacy); err != nil {
		t.Fatal(err)
	}
	previousDB := metricsDB
	metricsDB = nil
	initBlockedPortStore()
	if metricsDB == nil {
		t.Fatal("isolated SQLite test store was not initialized")
	}
	t.Cleanup(func() { _ = metricsDB.Close(); metricsDB = previousDB })
	loadRollingState()
	if rollingCache.TamperingTimestampsReady || len(rollingCache.TamperingLastSeen) != 0 {
		t.Fatal("migration fabricated timestamps")
	}
	names, lastSeen := rollingTamperingViewLocked(now)
	if !reflect.DeepEqual(names, []string{"legacy-unknown"}) || len(lastSeen) != 0 {
		t.Fatalf("unknown legacy name must have unavailable timestamp: %v %v", names, lastSeen)
	}
	start, _, needed := tamperingCollectionWindow(now, now.Add(-5*time.Minute), now, false)
	if !needed || !start.Equal(now.Add(-24*time.Hour)) {
		t.Fatal("legacy state was not scheduled for tampering-only backfill")
	}
	result := tamperingWindowResult{Complete: true, FullWindow: true, IntervalStart: now.Add(-5 * time.Minute),
		LastSeen: map[string]time.Time{"backfilled": now.Add(-2 * time.Hour), "current": now.Add(-time.Minute)}}
	if got := updateTamperingFixture(now, false, result); !reflect.DeepEqual(got, []string{"backfilled", "current"}) {
		t.Fatal(got)
	}
	if rollingCache.BaselineBlocked["Payments"].Count != 22 || rollingCache.Buckets[0].VENWarningCount != 9 || rollingCache.Buckets[0].BlockedByTarget["Payments"] != 7 {
		t.Fatal("tampering migration reset unrelated history")
	}
	if rollingCache.Buckets[1].TamperingCount != 1 {
		t.Fatal("24h backfill was used as the current 5-minute bucket")
	}
	want := rollingTamperingLastSeen(now)
	saveRollingState()
	loadRollingState()
	if !rollingCache.TamperingTimestampsReady || !reflect.DeepEqual(rollingTamperingLastSeen(now), want) {
		t.Fatal("timestamp map/readiness did not survive restart")
	}
	if got := tamperingTodaySoFarUniqueCount(now); got != len(tamperingNamesSince(want, localDayStart(now, time.UTC), now)) {
		t.Fatal("daily unique count did not use event dates")
	}
}

func TestTamperingAPIAndCSVExposeLastSeenWithoutChangingOtherMetrics(t *testing.T) {
	isolateTamperingState(t)
	at := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)
	currentStats.Tampering.Workloads = []string{"known", "unknown"}
	currentStats.Tampering.Count = 2
	currentStats.Tampering.LastSeen = map[string]time.Time{"known": at}
	for _, metric := range []string{"tampering", "ven_warning"} {
		recorder := httptest.NewRecorder()
		handleDrilldown(recorder, httptest.NewRequest(http.MethodGet, "/api/drilldown?metric="+metric, nil))
		var response map[string]interface{}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if metric == "tampering" {
			if got := response["item_last_seen"].(map[string]interface{})["known"]; got != at.Format(time.RFC3339) {
				t.Fatal(got)
			}
			if !reflect.DeepEqual(response["items"], []interface{}{"known", "unknown"}) {
				t.Fatal("legacy names API changed")
			}
		} else if _, present := response["item_last_seen"]; present {
			t.Fatal("timestamps leaked into unrelated metric")
		}
	}
	recorder := httptest.NewRecorder()
	handleExportDrilldownCSV(recorder, httptest.NewRequest(http.MethodGet, "/api/export/drilldown.csv?metric=tampering", nil))
	reader := csv.NewReader(strings.NewReader(recorder.Body.String()))
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if reflect.DeepEqual(row, []string{"known", at.Format(time.RFC3339)}) {
			found = true
		}
		if len(row) > 0 && row[0] == "unknown" && !reflect.DeepEqual(row, []string{"unknown", ""}) {
			t.Fatal("unknown timestamp was fabricated")
		}
	}
	if !found || !strings.Contains(recorder.Body.String(), "Last tamper UTC") {
		t.Fatal("CSV missing actual tamper timestamp")
	}
}

func TestTamperingPaginationPartialErrorsAndFlexiblePageIdentity(t *testing.T) {
	isolateTamperingRetrieval(t)
	start := time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC)
	events := tamperingRetrievalFixture(1601, start, false)
	// Distinct events without hrefs still use flexible timestamps and names.
	for i, event := range events {
		delete(event, "href")
		event["workload"] = map[string]interface{}{"hostname": fmt.Sprintf("host-%d", i)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveTamperingSyncFixture(t, w, req, events)
	}))
	defer server.Close()
	useTestPCEOrigin(t, server.URL)
	batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start, start.Add(time.Hour), server.Client())
	if err != nil || len(batch) != len(events) {
		t.Fatalf("documented filters/flexible event identity lost events: %d, %v", len(batch), err)
	}
}
