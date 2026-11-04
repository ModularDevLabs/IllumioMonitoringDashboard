package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func estateTestStats(at time.Time) DashboardStats {
	var stats DashboardStats
	stats.Timestamp = at
	stats.Workloads.Status.Success = true
	stats.VENStatus.Status.Success = true
	stats.VENStatus.StoppedStatus.Success = true
	stats.Tampering.Status.Success = true
	return stats
}

func estateTestWorkload(id, version string) map[string]interface{} {
	agent := map[string]interface{}{"href": "/vens/" + id}
	if version != "" {
		agent["status"] = map[string]interface{}{"agent_version": version}
	}
	return map[string]interface{}{"href": "/workloads/" + id, "hostname": "host-" + id, "agent": agent, "enforcement_mode": "full",
		"labels": []interface{}{map[string]interface{}{"key": "env", "value": "prod"}, map[string]interface{}{"key": "BU", "value": "Finance"}}}
}

func TestEstateVersionsAndInventory(t *testing.T) {
	at := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	loc := time.FixedZone("test", -6*60*60)
	w1 := estateTestWorkload("1", "25.4.1")
	rows := []map[string]interface{}{w1, w1, estateTestWorkload("2", "25.4.1"), estateTestWorkload("3", ""),
		{"href": "/workloads/4", "name": "unmanaged", "unmanaged": true, "agent_version": "ignore"}}
	snapshot := estateBuildSnapshot(at, loc, rows, nil, nil, estateTestStats(at), EstateOptions{WeeklyReportEnabled: true})
	if snapshot.Total != 4 || snapshot.Managed != 3 || snapshot.Unmanaged != 1 || !snapshot.InventoryComplete {
		t.Fatalf("incorrect managed inventory: %+v", snapshot)
	}
	if snapshot.Day != "2026-11-01" || snapshot.Modes["full"] != 3 {
		t.Fatalf("timezone/enforcement: %+v", snapshot)
	}
	want := []EstateVersionCount{{Version: "25.4.1", Count: 2}, {Version: "Unknown", Count: 1}}
	if !reflect.DeepEqual(snapshot.Versions, want) {
		t.Fatalf("weekly versions (without live versions enabled): got %+v want %+v", snapshot.Versions, want)
	}
	if snapshot.CompleteSamples != 1 {
		t.Fatal("complete inventory sample was not counted")
	}
	if snapshot.VENCount != 3 || !snapshot.VersionsCollected {
		t.Fatalf("VEN coverage metadata incorrect: %+v", snapshot)
	}
}

func TestEstateVersionsCountSharedVENOnce(t *testing.T) {
	at := time.Now().UTC()
	one, two := estateTestWorkload("1", ""), estateTestWorkload("2", "25.4.1")
	two["agent"].(map[string]interface{})["href"] = "/vens/1"
	snapshot := estateBuildSnapshot(at, time.UTC, []map[string]interface{}{one, two}, nil, nil, estateTestStats(at), EstateOptions{WeeklyReportEnabled: true})
	if snapshot.Managed != 2 || snapshot.VENCount != 1 || len(snapshot.Versions) != 1 || snapshot.Versions[0].Count != 1 || snapshot.Versions[0].Version != "25.4.1" {
		t.Fatalf("one VEN was counted per workload: %+v", snapshot)
	}
	withoutVersions := estateBuildSnapshot(at, time.UTC, []map[string]interface{}{one, two}, nil, nil, estateTestStats(at), EstateOptions{InventoryEnabled: true})
	if withoutVersions.VENCount != 1 || withoutVersions.VersionsCollected || len(withoutVersions.Versions) != 0 {
		t.Fatalf("inventory-only version collection was not opt-in: %+v", withoutVersions)
	}
}

func TestEstateLabelHygieneCustomTypesAndUnknown(t *testing.T) {
	at := time.Now().UTC()
	known := estateTestWorkload("1", "25.4")
	unknown := estateTestWorkload("2", "25.4")
	unknown["labels"] = []interface{}{map[string]interface{}{"href": "/labels/1"}}
	snapshot := estateBuildSnapshot(at, time.UTC, []map[string]interface{}{known, unknown}, nil, nil, estateTestStats(at), EstateOptions{LabelHygieneEnabled: true, RequiredLabelTypes: []string{"env", "bu", "owner"}})
	if len(snapshot.LabelGaps) != 1 || snapshot.LabelGaps[0].ID != "/workloads/1" || !reflect.DeepEqual(snapshot.LabelGaps[0].Missing, []string{"owner"}) {
		t.Fatalf("custom types or unknown label schema misclassified: %+v", snapshot.LabelGaps)
	}
	if len(snapshot.Warnings) == 0 {
		t.Fatal("unresolved label types must be visible")
	}
}

func TestEstateIncompleteInventoryNeverCreatesChurn(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	options := EstateOptions{ChangeLogEnabled: true, HistoryDays: 30}
	history := EstateHistory{}
	apply := func(offset time.Duration, rows []map[string]interface{}, partial bool, opts EstateOptions) {
		now := at.Add(offset)
		stats := estateTestStats(now)
		var err error
		if partial {
			err = errors.New("partial page")
			stats.Workloads.Status.Error = err.Error()
		}
		snapshot := estateBuildSnapshot(now, time.UTC, rows, err, nil, stats, opts)
		estateApplySnapshot(&history, snapshot, nil, opts, time.UTC)
	}
	one, two := estateTestWorkload("1", "25.4"), estateTestWorkload("2", "25.4")
	apply(0, []map[string]interface{}{one, two}, false, options)
	if len(history.Changes) != 0 {
		t.Fatal("initial baseline recorded additions")
	}
	apply(5*time.Minute, []map[string]interface{}{one}, true, options)
	apply(10*time.Minute, []map[string]interface{}{one}, false, options)
	if len(history.Changes) != 0 {
		t.Fatalf("partial results created removals: %+v", history.Changes)
	}
	apply(15*time.Minute, []map[string]interface{}{one, two}, false, options)
	if len(history.Changes) != 1 || history.Changes[0].Kind != "added" {
		t.Fatalf("valid change not recorded: %+v", history.Changes)
	}
	apply(time.Hour, []map[string]interface{}{one}, false, options)
	if len(history.Changes) != 1 {
		t.Fatal("downtime created removals")
	}
	options.ChangeLogEnabled = false
	apply(65*time.Minute, []map[string]interface{}{}, false, options)
	options.ChangeLogEnabled = true
	apply(70*time.Minute, []map[string]interface{}{two}, false, options)
	if len(history.Changes) != 1 {
		t.Fatal("disable/re-enable created false changes")
	}
}

func TestEstateIssueAgingPartialResolutionAndRecurrence(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	options := EstateOptions{IssueAgingEnabled: true}
	history := EstateHistory{}
	ven := map[string]interface{}{"href": "/vens/1", "hostname": "db", "conditions": []interface{}{map[string]interface{}{"notification_type": "first reason"}}}
	apply := func(offset time.Duration, rows []map[string]interface{}, complete bool) {
		now := at.Add(offset)
		stats := estateTestStats(now)
		stats.VENStatus.Status.Success = complete
		health := map[string][]map[string]interface{}{"warning": rows}
		snapshot := estateBuildSnapshot(now, time.UTC, nil, nil, health, stats, options)
		estateApplySnapshot(&history, snapshot, health, options, time.UTC)
	}
	apply(0, []map[string]interface{}{ven, ven}, true)
	apply(5*time.Minute, nil, false)
	if len(history.Issues) != 1 || !history.Issues[0].Active || !history.Issues[0].ResolvedAt.IsZero() {
		t.Fatalf("partial query resolved issue: %+v", history.Issues)
	}
	ven["conditions"] = []interface{}{map[string]interface{}{"notification_type": "new reason"}}
	apply(10*time.Minute, []map[string]interface{}{ven}, true)
	if history.Issues[0].Occurrences != 1 || !history.Issues[0].FirstSeen.Equal(at) {
		t.Fatalf("reason change created recurrence: %+v", history.Issues[0])
	}
	apply(15*time.Minute, nil, true)
	if history.Issues[0].Active {
		t.Fatal("complete healthy query failed to resolve issue")
	}
	apply(20*time.Minute, []map[string]interface{}{ven}, true)
	if history.Issues[0].Occurrences != 2 || !history.Issues[0].FirstSeen.Equal(at.Add(20*time.Minute)) {
		t.Fatalf("new incident should have new age: %+v", history.Issues[0])
	}
	if len(history.Issues[0].ObservedDays) != 1 {
		t.Fatal("daily identity was not deduplicated")
	}
}

func TestEstateDailyPeaksDoNotMutateLiveSnapshot(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	options := EstateOptions{WeeklyReportEnabled: true}
	history := EstateHistory{}
	rows := []map[string]interface{}{{"href": "/vens/1"}, {"href": "/vens/2"}}
	for i := 0; i < 2; i++ {
		now := at.Add(time.Duration(i) * 5 * time.Minute)
		health := map[string][]map[string]interface{}{"warning": rows[:2-i]}
		snapshot := estateBuildSnapshot(now, time.UTC, nil, nil, health, estateTestStats(now), options)
		estateApplySnapshot(&history, snapshot, health, options, time.UTC)
	}
	if history.Latest.Health["warning"] != 1 || history.Days[0].Health["warning"] != 2 {
		t.Fatalf("daily/live map aliasing: latest=%v daily=%v", history.Latest.Health, history.Days[0].Health)
	}
	if history.Days[0].Samples != 2 || history.Days[0].HealthSamples["warning"] != 2 || !history.Days[0].FirstAt.Equal(at) {
		t.Fatalf("coverage aggregation: %+v", history.Days[0])
	}
	copyHistory := estateCloneHistory(history)
	copyHistory.Latest.Health["warning"] = 99
	if history.Latest.Health["warning"] != 1 {
		t.Fatal("public copy mutates collector")
	}
}

func TestEstatePolicySeparateCountsAndPending(t *testing.T) {
	fetch := func(endpoint string) ([]map[string]interface{}, error) {
		if strings.Contains(endpoint, "/active/") {
			return []map[string]interface{}{{"href": "/active/rule_sets/1", "rules": []interface{}{map[string]interface{}{}, map[string]interface{}{}}}}, nil
		}
		return []map[string]interface{}{{"href": "/draft/rule_sets/1", "rules_count": float64(3)}}, nil
	}
	active, err := estatePolicyCounts("https://pce.invalid/sec_policy/active", fetch)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := estatePolicyCounts("https://pce.invalid/sec_policy/draft", fetch)
	if err != nil {
		t.Fatal(err)
	}
	if active.Rulesets != 1 || active.Rules != 2 || draft.Rulesets != 1 || draft.Rules != 3 || !active.Complete || !draft.Complete {
		t.Fatalf("policy versions mixed: %+v %+v", active, draft)
	}
	changes, complete := estateParsePendingPolicy(map[string]interface{}{"rule_sets": []interface{}{map[string]interface{}{"href": "/rule_sets/1", "name": "Payments", "update_type": "update", "updated_at": "2026-11-02T12:00:00Z"}}, "services": []interface{}{}})
	if !complete || len(changes) != 1 || changes[0].Operation != "update" || changes[0].UpdatedAt.IsZero() {
		t.Fatalf("pending changes: %+v %v", changes, complete)
	}
	_, complete = estateParsePendingPolicy(map[string]interface{}{"unexpected": map[string]interface{}{"nested": true}})
	if complete {
		t.Fatal("unsupported pending schema reported zero pending changes")
	}
}

func TestEstateScopeIdentityExcludesSecretsAndSeparatesOrgs(t *testing.T) {
	if estateIdentity("https://user:secret@PCE.invalid/api/v2/orgs/1/") != estateIdentity("https://pce.invalid/api/v2/orgs/1") {
		t.Fatal("credentials or casing changed scope")
	}
	if estateIdentity("https://pce.invalid/api/v2/orgs/1") == estateIdentity("https://pce.invalid/api/v2/orgs/2") {
		t.Fatal("organizations share history")
	}
}

// Isolate package globals and the current working directory from real profiles.
func TestEstateMonitorIsolation(t *testing.T) {
	for _, scenario := range []string{"disabled", "persistence", "policy", "labels"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEstateMonitorHelperProcess$", "-test.v")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "ILLUMIO_ESTATE_MONITOR_TEST="+scenario)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", scenario, err, output)
			}
		})
	}
}

func TestEstateMonitorHelperProcess(t *testing.T) {
	scenario := os.Getenv("ILLUMIO_ESTATE_MONITOR_TEST")
	if scenario == "" {
		return
	}
	dataDir = t.TempDir()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/rule_sets"):
			state := "active"
			port := 80
			if strings.Contains(r.URL.Path, "/draft/") {
				state, port = "draft", 443
			}
			_ = json.NewEncoder(w).Encode([]interface{}{estatePendingTestRuleset(state, "1", estatePendingTestRule(state, "1", "1", port), estatePendingTestRule(state, "1", "2", 22))})
		case strings.HasSuffix(r.URL.Path, "/sec_policy/pending"):
			_ = json.NewEncoder(w).Encode(estatePendingTestPayload(map[string]string{"1": "update"}))
		case strings.HasSuffix(r.URL.Path, "/sec_policy/active"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"href": "/sec_policy/8", "version": 8, "created_at": "2026-11-01T12:00:00Z"})
		case strings.HasSuffix(r.URL.Path, "/sec_policy"):
			_ = json.NewEncoder(w).Encode([]interface{}{map[string]interface{}{"href": "/sec_policy/8", "version": 8, "created_at": "2026-11-01T12:00:00Z"}})
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]interface{}{map[string]interface{}{"href": "/labels/1", "key": "BU", "value": "Finance"}})
		default:
			_ = json.NewEncoder(w).Encode([]interface{}{})
		}
	}))
	defer server.Close()
	config = Config{PCEURL: server.URL, OrgID: "1", Timezone: "UTC"}
	base := server.URL + "/api/v2/orgs/1"
	options := EstateOptions{}
	at := time.Now().UTC()
	if scenario == "disabled" {
		updateEstateMonitor(base, nil, nil, nil, estateTestStats(at), options)
		if requests.Load() != 0 {
			t.Fatal("disabled monitoring contacted PCE")
		}
		files, err := os.ReadDir(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("disabled monitoring wrote files: %v", files)
		}
		return
	}
	if scenario == "policy" {
		config.Estate = EstateOptions{PolicyEnabled: true}
		updateEstateMonitor(base, nil, nil, nil, estateTestStats(at), configuredEstateOptions())
		first := estateHistoryCopy()
		if first.Latest == nil || first.Latest.Policy == nil {
			t.Fatal("policy snapshot missing")
		}
		policy := first.Latest.Policy
		if requests.Load() != 4 || !policy.Active.Complete || !policy.Draft.Complete || !policy.PendingComplete || policy.LastProvisionedAt.IsZero() || policy.PendingCounts == nil || !policy.PendingCounts.Rules.Complete || policy.PendingCounts.Rules.Modified != 1 {
			t.Fatalf("policy collection invalid: requests=%d policy=%+v", requests.Load(), policy)
		}
		updateEstateMonitor(base, nil, nil, nil, estateTestStats(at.Add(5*time.Minute)), configuredEstateOptions())
		if requests.Load() != 4 {
			t.Fatal("policy cache not honored")
		}
		updateEstateMonitor(base, nil, nil, nil, estateTestStats(at.Add(20*time.Minute)), configuredEstateOptions())
		if requests.Load() != 8 {
			t.Fatal("policy cache not refreshed")
		}
		return
	}
	if scenario == "labels" {
		row := estateTestWorkload("1", "25.4")
		row["labels"] = []interface{}{map[string]interface{}{"href": "/labels/1"}}
		config.Estate = EstateOptions{LabelHygieneEnabled: true, RequiredLabelTypes: []string{"BU"}}
		updateEstateMonitor(base, []map[string]interface{}{row}, nil, nil, estateTestStats(at), configuredEstateOptions())
		latest := estateHistoryCopy().Latest
		if latest == nil || len(latest.LabelGaps) != 0 || latest.Workloads[0].Labels["BU"] != "Finance" {
			t.Fatalf("label catalog resolution: %+v", latest)
		}
		if requests.Load() != 1 || mapString(estateObjectArray(row["labels"])[0], "key") != "" {
			t.Fatal("unexpected fetch or mutation of shared workload")
		}
		config.Estate.RequiredLabelTypes = []string{"app"}
		resetEstateMonitorContinuity()
		view := estateView()
		if view.Snapshot == nil || view.Snapshot.LabelsComplete || view.Snapshot.LabelsAssessed != 0 || len(view.Snapshot.LabelGaps) != 0 {
			t.Fatalf("changed label requirements reused old assessment: %+v", view.Snapshot)
		}
		if latest.LabelsAssessed != 1 || !latest.LabelsComplete {
			t.Fatal("view invalidation mutated history copy")
		}
		return
	}
	options = EstateOptions{InventoryEnabled: true, VENVersionsEnabled: true, ChangeLogEnabled: true, HistoryDays: 90}
	config.Estate = options
	updateEstateMonitor(base, []map[string]interface{}{estateTestWorkload("1", "25.4")}, nil, nil, estateTestStats(at), configuredEstateOptions())
	path := estateHistoryPath(estateIdentity(base))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history not persisted: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("inventory monitoring should reuse workload fetch")
	}
	estateMonitor.Lock()
	estateMonitor.loaded = false
	estateMonitor.Unlock()
	loaded := estateHistoryCopy()
	if loaded.Latest == nil || loaded.Latest.Managed != 1 || len(loaded.Days) != 1 {
		t.Fatalf("history roundtrip: %+v", loaded)
	}
	if len(loaded.Baseline) != 0 || !loaded.BaselineAt.IsZero() {
		t.Fatal("restart incorrectly trusts old baseline")
	}
	config.Timezone = "America/Chicago"
	if differentZone := estateHistoryCopy(); differentZone.Latest != nil || differentZone.Timezone != "America/Chicago" {
		t.Fatal("timezone change mixed calendar-day histories")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("timezone switch removed prior history")
	}
	config.Timezone = "UTC"
	if restored := estateHistoryCopy(); restored.Latest == nil {
		t.Fatal("return to original timezone lost preserved history")
	}
	config.OrgID = "2"
	if other := estateHistoryCopy(); other.Latest != nil {
		t.Fatal("another organization can see prior estate")
	}
	if paths, _ := filepath.Glob(filepath.Join(dataDir, "*.tmp")); len(paths) != 0 {
		t.Fatalf("atomic temp files left: %v", paths)
	}
}
