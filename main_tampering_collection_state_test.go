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
	"testing"
	"time"
)

const tamperingCollectionScenarioEnv = "ILLUMIO_TAMPERING_COLLECTION_STATE_TEST_SCENARIO"

// Collector state, persistence and alerts are process globals. Each scenario
// runs in its own temporary directory/process, never loading customer data.
func TestTamperingCollectionStateRegression(t *testing.T) {
	for _, scenario := range []string{"dedup-and-windows", "boundaries", "query-window", "partial-and-zero", "daily", "persistence-json", "persistence-sqlite", "migration", "api-and-alerts"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTamperingCollectionStateHelperProcess$", "-test.v")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), tamperingCollectionScenarioEnv+"="+scenario)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("tampering collection scenario %q: %v\n%s", scenario, err, output)
			}
		})
	}
}

func TestTamperingCollectionStateHelperProcess(t *testing.T) {
	scenario := os.Getenv(tamperingCollectionScenarioEnv)
	if scenario == "" {
		return
	}
	dataDir = t.TempDir()
	config = Config{Timezone: "UTC", BlockedPortStoreBackend: "sqlite", BlockedRollingDedupeBackend: "memory",
		TamperingMAWindow: 2, DailyMAWindow: 2, TamperingAnomalyPct: 50, TamperingAnomalyBaseline: "5m", TamperingAnomalyDays: 3, TamperingAnomalyMinPct: 100}
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
	case "dedup-and-windows":
		testTamperingCollectionDedupAndWindows(t)
	case "boundaries":
		testTamperingCollectionBoundaries(t)
	case "query-window":
		testTamperingCollectionQueryWindow(t)
	case "partial-and-zero":
		testTamperingCollectionPartialAndZero(t)
	case "daily":
		testTamperingCollectionDaily(t)
	case "persistence-json", "persistence-sqlite":
		testTamperingCollectionPersistence(t, scenario == "persistence-json")
	case "migration":
		testTamperingCollectionMigration(t)
	case "api-and-alerts":
		testTamperingCollectionAPIAndAlerts(t)
	default:
		t.Fatalf("unknown tampering scenario %q", scenario)
	}
}

func tamperingStateBool(value bool) *bool { return &value }
func tamperingStateInt(value int) *int    { return &value }

func tamperingStateResult(now time.Time, lastSeen map[string]time.Time, complete, full bool) tamperingWindowResult {
	result := tamperingWindowResult{LastSeen: lastSeen, Complete: complete, FullWindow: full, IntervalStart: now.Add(-24 * time.Hour)}
	for name := range lastSeen {
		result.Names = append(result.Names, name)
	}
	return result
}

func testTamperingCollectionDedupAndWindows(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var events []map[string]interface{}
	for i := 0; i < 95; i++ {
		at := now.Add(-time.Minute)
		if i >= 75 {
			at = now.Add(-2 * time.Hour)
		}
		name := fmt.Sprintf("workload-%03d", i)
		event := tamperEvent(name, "timestamp", at.Format(time.RFC3339))
		events = append(events, event, event, tamperEvent(name, "timestamp", at.Add(-time.Second).Format(time.RFC3339)))
	}
	result := summarizeTamperingEvents(events, now.Add(-24*time.Hour), now)
	result.Complete, result.FullWindow = true, true
	if len(result.Names) != 95 || result.EventCount != 190 {
		t.Fatalf("raw-event duplicates changed unique workloads: names=%d events=%d", len(result.Names), result.EventCount)
	}
	if names := updateTamperingFixture(now, true, result); len(names) != 95 {
		t.Fatalf("24h workload count = %d, want 95", len(names))
	}
	if bucket := rollingCache.Buckets[0]; bucket.TamperingCount != 75 || bucket.TamperingComplete == nil || !*bucket.TamperingComplete || len(bucket.TamperingWorkloads) != 75 {
		t.Fatalf("startup must count 75 unique current-5m workloads, not all 95 or zero: %#v", bucket)
	}
	next := now.Add(5 * time.Minute)
	lastSeen := map[string]time.Time{}
	for name, at := range result.LastSeen {
		lastSeen[name] = at.Add(-time.Second) // Older copies must not regress timestamps.
	}
	for i := 0; i < 30; i++ {
		lastSeen[fmt.Sprintf("workload-%03d", i)] = next.Add(-time.Minute)
	}
	for i := 95; i < 105; i++ {
		lastSeen[fmt.Sprintf("workload-%03d", i)] = next.Add(-time.Minute)
	}
	if names := updateTamperingFixture(next, false, tamperingStateResult(next, lastSeen, true, true)); len(names) != 105 {
		t.Fatalf("repeated workloads were counted twice across cycles: %d", len(names))
	}
	if got := rollingCache.Buckets[1].TamperingCount; got != 40 {
		t.Fatalf("backfill must count recurring 30 plus new 10, got %d", got)
	}
	if got := rollingTamperingLastSeen(next)["workload-074"]; !got.Equal(result.LastSeen["workload-074"]) {
		t.Fatalf("out-of-order duplicate regressed last seen: %v", got)
	}
}

func testTamperingCollectionBoundaries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	lastSeen := map[string]time.Time{
		"at-5m": now.Add(-5 * time.Minute), "after-5m": now.Add(-5*time.Minute + time.Nanosecond),
		"at-now": now, "future": now.Add(time.Nanosecond),
		"at-24h": now.Add(-24 * time.Hour), "after-24h": now.Add(-24*time.Hour + time.Nanosecond),
		"old-but-active": now.Add(-6 * time.Minute),
	}
	names := updateTamperingFixture(now, true, tamperingStateResult(now, lastSeen, true, true))
	want := []string{"after-24h", "after-5m", "at-5m", "at-now", "old-but-active"}
	if !reflect.DeepEqual(names, want) || rollingCache.Buckets[0].TamperingCount != 2 {
		t.Fatalf("wrong (start,end] boundaries: names=%v bucket=%#v", names, rollingCache.Buckets[0])
	}
	next := now.Add(time.Nanosecond)
	names = updateTamperingFixture(next, false, tamperingStateResult(next, nil, true, false))
	if len(names) != 4 || rollingCache.Buckets[1].TamperingCount != 0 {
		t.Fatalf("actual-event expiry or complete zero failed: %v", names)
	}
	if _, present := rollingTamperingLastSeen(next)["after-24h"]; present {
		t.Fatal("workload survived its actual 24h expiry")
	}
}

func testTamperingCollectionQueryWindow(t *testing.T) {
	now := time.Now().UTC()
	rollingCache = rollingState{Initialized: true, LastCycle: now.Add(-time.Minute), TamperingTimestampsReady: true, TamperingCollectionVersion: tamperingCollectionMethodVersion}
	start, end, baseline := collectionWindow(now)
	gotStart, gotEnd, full := tamperingCollectionWindow(now, start, end, baseline)
	if baseline || full || !gotStart.Equal(now.Add(-5*time.Minute)) || !gotEnd.Equal(now) {
		t.Fatalf("manual early refresh failed to cover five minutes: %v %v %v", gotStart, gotEnd, full)
	}
	for _, reason := range []string{"legacy-version", "pending", "gap", "legacy-timestamps"} {
		rollingCache.TamperingCollectionVersion = tamperingCollectionMethodVersion
		rollingCache.TamperingTimestampsReady = true
		rollingCache.TamperingBackfillPending = false
		rollingCache.LastCycle = now.Add(-time.Minute)
		switch reason {
		case "legacy-version":
			rollingCache.TamperingCollectionVersion = ""
		case "pending":
			rollingCache.TamperingBackfillPending = true
		case "gap":
			rollingCache.LastCycle = now.Add(-31 * time.Minute)
		case "legacy-timestamps":
			rollingCache.TamperingTimestampsReady = false
		}
		start, end, baseline = collectionWindow(now)
		gotStart, gotEnd, full = tamperingCollectionWindow(now, start, end, baseline)
		if baseline || !full || !gotStart.Equal(now.Add(-24*time.Hour)) || !gotEnd.Equal(now) {
			t.Fatalf("%s did not request tampering-only 24h backfill: %v %v %v baseline=%v", reason, gotStart, gotEnd, full, baseline)
		}
	}
}

func testTamperingCollectionPartialAndZero(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	updateTamperingFixture(now.Add(-10*time.Minute), true, tamperingStateResult(now, map[string]time.Time{"known": now.Add(-11 * time.Minute)}, true, true))
	before := tamperingTrendSeries()
	names := updateTamperingFixture(now.Add(-5*time.Minute), false, tamperingStateResult(now, map[string]time.Time{"partial": now.Add(-6 * time.Minute)}, false, false))
	if !reflect.DeepEqual(names, []string{"known", "partial"}) || !rollingCache.TamperingBackfillPending {
		t.Fatalf("failed query discarded partial or previous evidence: %v", names)
	}
	failed := rollingCache.Buckets[1]
	if failed.TamperingComplete == nil || *failed.TamperingComplete || !reflect.DeepEqual(before, tamperingTrendSeries()) {
		t.Fatal("failed query became a successful zero chart point")
	}
	updateTamperingFixture(now, false, tamperingStateResult(now, nil, true, true))
	series := tamperingTrendSeries()
	if len(series) != 2 || series[1].Value != 0 || rollingCache.TamperingBackfillPending {
		t.Fatalf("complete zero/backfill recovery did not become a valid point: %v", series)
	}
	if names, _ := rollingTamperingViewLocked(now); !reflect.DeepEqual(names, []string{"known", "partial"}) {
		t.Fatalf("same-method recovery discarded known unexpired observations: %v", names)
	}
	rollingCache.Buckets = append(rollingCache.Buckets, rollingBucket{EndUTC: now.Add(-time.Minute), TamperingCount: 999})
	if !reflect.DeepEqual(series, tamperingTrendSeries()) {
		t.Fatal("legacy unknown-completeness bucket contaminated current tampering chart")
	}
}

func testTamperingCollectionDaily(t *testing.T) {
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	update := func(value *int) { updateVENDailyHistory(now, 4, 3, 2, nil, value, 1, 2, 3, 4, 5) }
	update(tamperingStateInt(7))
	update(nil)
	if snap := venDaily[day]; snap.TamperingMax != 7 || snap.TamperingComplete == nil || *snap.TamperingComplete || snap.WarningMax != 4 || snap.ModeFullMax != 4 {
		t.Fatalf("failed daily query erased evidence or unrelated values: %#v", snap)
	}
	if got := tamperingDailyTrendSeries(365); len(got) != 0 {
		t.Fatalf("incomplete daily observation was charted: %v", got)
	}
	loadVENHistory()
	if snap := venDaily[day]; snap.TamperingMax != 7 || snap.TamperingComplete == nil || *snap.TamperingComplete {
		t.Fatal("daily partial state was lost on restart")
	}
	update(tamperingStateInt(0))
	venDaily[now.AddDate(0, 0, -1).Format("2006-01-02")] = venDailySnapshot{TamperingMax: 500, WarningMax: 12}
	if got := tamperingDailyTrendSeries(365); len(got) != 1 || got[0].Value != 0 {
		t.Fatalf("genuine daily zero or legacy exclusion failed: %v", got)
	}
}

func testTamperingCollectionPersistence(t *testing.T, fromJSON bool) {
	now := time.Now().UTC().Truncate(time.Second)
	rollingCache = rollingState{Initialized: true, LastCycle: now, TamperingTimestampsReady: true,
		TamperingBackfillPending: true, TamperingCollectionVersion: tamperingCollectionMethodVersion,
		TamperingLastSeen: map[string]time.Time{"known": now.Add(-time.Minute)},
		BaselineBlocked:   map[string]targetBaseline{"Payments": {Count: 44, CapturedUTC: now.Add(-time.Hour)}},
		Buckets: []rollingBucket{
			{EndUTC: now.Add(-10 * time.Minute), TamperingCount: 9, VENWarningCount: 3},
			{EndUTC: now.Add(-5 * time.Minute), TamperingCount: 2, TamperingComplete: tamperingStateBool(false)},
			{EndUTC: now, TamperingCount: 0, TamperingComplete: tamperingStateBool(true)},
		}}
	for i, complete := range []*bool{nil, tamperingStateBool(false), tamperingStateBool(true)} {
		venDaily[now.AddDate(0, 0, i-2).Format("2006-01-02")] = venDailySnapshot{TamperingMax: i, TamperingComplete: complete, WarningMax: 8}
	}
	beforeSeries, beforeDaily := tamperingTrendSeries(), tamperingDailyTrendSeries(365)
	saveRollingState()
	saveVENHistory()
	if fromJSON {
		// JSON is now a legacy import format, not a selectable storage backend.
		// Exercise the real JSON-to-SQLite migration, including absent flags.
		var persisted persistedRollingState
		var daily []venDailyRecord
		if ok, err := sqliteKVGetJSON(stateKeyRolling, &persisted); err != nil || !ok {
			t.Fatalf("read saved rolling fixture: %v %v", ok, err)
		}
		if ok, err := sqliteKVGetJSON(stateKeyVENDaily, &daily); err != nil || !ok {
			t.Fatalf("read saved daily fixture: %v %v", ok, err)
		}
		if err := writeJSONFileAtomic(dataFilePath(rollingStateFileName), persisted); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONFileAtomic(dataFilePath(venHistoryFileName), daily); err != nil {
			t.Fatal(err)
		}
		if _, err := metricsDB.Exec("DELETE FROM kv_state WHERE key IN (?, ?)", stateKeyRolling, stateKeyVENDaily); err != nil {
			t.Fatal(err)
		}
		migrateLegacyJSONStateToSQLite()
	}
	rollingCache = rollingState{}
	venDaily = map[string]venDailySnapshot{}
	loadRollingState()
	loadVENHistory()
	if !reflect.DeepEqual(beforeSeries, tamperingTrendSeries()) || !reflect.DeepEqual(beforeDaily, tamperingDailyTrendSeries(365)) {
		t.Fatal("restart changed complete/partial/unknown history semantics")
	}
	if len(rollingCache.Buckets) != 3 || rollingCache.Buckets[0].TamperingComplete != nil || rollingCache.Buckets[1].TamperingComplete == nil || *rollingCache.Buckets[1].TamperingComplete || rollingCache.Buckets[2].TamperingComplete == nil || !*rollingCache.Buckets[2].TamperingComplete {
		t.Fatal("rolling persistence lost tri-state completeness")
	}
	if rollingCache.TamperingCollectionVersion != tamperingCollectionMethodVersion || !rollingCache.TamperingBackfillPending || !rollingCache.TamperingTimestampsReady || rollingCache.BaselineBlocked["Payments"].Count != 44 || rollingCache.Buckets[0].VENWarningCount != 3 {
		t.Fatal("restart lost method version, recovery marker, or unrelated traffic/VEN state")
	}
	if !rollingCache.TamperingLastSeen["known"].Equal(now.Add(-time.Minute)) {
		t.Fatal("restart changed the last actual event timestamp")
	}
}

func testTamperingCollectionMigration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rollingCache = rollingState{Initialized: true, LastCycle: now.Add(-5 * time.Minute), TamperingTimestampsReady: true,
		TamperingLastSeen: map[string]time.Time{"old": now.Add(-2 * time.Hour), "legacy-wrong-identity": now.Add(-time.Minute)},
		BaselineBlocked:   map[string]targetBaseline{"Payments": {Count: 44, CapturedUTC: now.Add(-time.Hour)}},
		Buckets:           []rollingBucket{{EndUTC: now.Add(-5 * time.Minute), TamperingCount: 900, VENWarningCount: 3, BlockedByTarget: map[string]int{"Payments": 8}}}}
	saveRollingState()
	loadRollingState()
	start, end, baseline := collectionWindow(now)
	start, end, full := tamperingCollectionWindow(now, start, end, baseline)
	if baseline || !full || !start.Equal(now.Add(-24*time.Hour)) || !end.Equal(now) {
		t.Fatal("legacy collection version did not schedule a tampering-only migration")
	}
	if len(tamperingTrendSeries()) != 0 {
		t.Fatal("legacy incorrectly filtered counts were used as an anomaly baseline")
	}
	lastSeen := map[string]time.Time{"old": now.Add(-2 * time.Hour), "new": now.Add(-time.Minute)}
	if names := updateTamperingFixture(now, baseline, tamperingStateResult(now, lastSeen, true, full)); !reflect.DeepEqual(names, []string{"new", "old"}) {
		t.Fatalf("corrected authoritative backfill promoted a legacy misattribution: %v", names)
	}
	if rollingCache.TamperingCollectionVersion != tamperingCollectionMethodVersion || rollingCache.TamperingBackfillPending || len(rollingCache.Buckets) != 2 || rollingCache.Buckets[1].TamperingCount != 1 {
		t.Fatal("complete migration did not mark the new method or isolated latest-5m data")
	}
	if rollingCache.BaselineBlocked["Payments"].Count != 44 || rollingCache.Buckets[0].VENWarningCount != 3 || rollingCache.Buckets[0].BlockedByTarget["Payments"] != 8 {
		t.Fatal("tampering migration reset unrelated traffic/VEN history")
	}
	saveRollingState()
	loadRollingState()
	start, end, baseline = collectionWindow(now.Add(time.Minute))
	_, _, full = tamperingCollectionWindow(now.Add(time.Minute), start, end, baseline)
	if full {
		t.Fatal("successful method migration requested another full backfill after restart")
	}
}

func tamperingStateSeries(values ...int) {
	now := time.Now().UTC()
	rollingCache.Buckets = nil
	for i, value := range values {
		rollingCache.Buckets = append(rollingCache.Buckets, rollingBucket{EndUTC: now.Add(time.Duration(i-len(values)+1) * 5 * time.Minute), TamperingCount: value, TamperingComplete: tamperingStateBool(true)})
	}
}

func testTamperingCollectionAPIAndAlerts(t *testing.T) {
	stats := DashboardStats{}
	stats.Tampering.Status = FetchStatus{Success: true}
	tamperingStateSeries(1, 1, 5)
	processWebhookAlerts(stats)
	processWebhookAlerts(stats)
	if !alertState.Metrics["tampering"].Active || len(anomalyHistory) != 1 {
		t.Fatal("complete tampering spike did not trigger exactly once")
	}
	tamperingStateSeries(1, 1, 0)
	stats.Tampering.Status = FetchStatus{Success: false, Error: "PCE query incomplete"}
	stats.Tampering.Partial = true
	stats.Tampering.Count = 1
	stats.Tampering.Workloads = []string{"known"}
	stats.Tampering.LastSeen = map[string]time.Time{"known": time.Now().UTC().Add(-time.Minute)}
	processWebhookAlerts(stats)
	currentStats = stats
	recorder := httptest.NewRecorder()
	handleDrilldown(recorder, httptest.NewRequest(http.MethodGet, "/api/drilldown?metric=tampering", nil))
	var response DrilldownResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status == nil || response.Status.Success || !response.Partial || response.Count != 1 || !reflect.DeepEqual(response.Items, []string{"known"}) || response.Anomalous {
		t.Fatalf("partial API lost known evidence or pretended to be complete: %#v", response)
	}
	if !alertState.Metrics["tampering"].Active || len(anomalyHistory) != 1 {
		t.Fatal("query failure incorrectly resolved active tampering alert")
	}
	stats.Tampering.Status = FetchStatus{Success: true}
	processWebhookAlerts(stats) // Even inconsistent success+partial must remain unknown.
	stats.Tampering.Partial = false
	tamperingStateSeries(0)
	processWebhookAlerts(stats) // A restarted/migrated baseline is still warming up.
	if !alertState.Metrics["tampering"].Active || len(anomalyHistory) != 1 {
		t.Fatal("partial data or baseline warmup falsely resolved alert")
	}
	tamperingStateSeries(1, 1, 0)
	processWebhookAlerts(stats)
	if alertState.Metrics["tampering"].Active || len(anomalyHistory) != 2 || anomalyHistory[1].State != "resolved" {
		t.Fatal("genuine successful zero did not resolve tampering alert")
	}
	tamperingStateSeries(0, 0, 1)
	processWebhookAlerts(stats)
	if !alertState.Metrics["tampering"].Active || len(anomalyHistory) != 3 {
		t.Fatal("positive tampering count above an established zero baseline did not alert")
	}
}
