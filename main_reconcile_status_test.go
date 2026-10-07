package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

const reconcileStatusHelperScenarioEnv = "ILLUMIO_RECONCILE_STATUS_HELPER_SCENARIO"

func TestReconcileStatusEmptyOmitsTimestamps(t *testing.T) {
	runReconcileStatusHelper(t, "empty")
}

func TestReconcileStatusMarkerOnlyOmitsRunTimestamps(t *testing.T) {
	runReconcileStatusHelper(t, "marker-only")
}

func TestReconcileStatusLifecycleResetsAndPublishesAtomically(t *testing.T) {
	runReconcileStatusHelper(t, "lifecycle")
}

func TestConfigTargetsSourceExclusionSaveDoesNotMutateTamperingReconcileStatus(t *testing.T) {
	runReconcileStatusHelper(t, "source-save")
}

func runReconcileStatusHelper(t *testing.T, scenario string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReconcileStatusHelperProcess$", "-test.v")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), reconcileStatusHelperScenarioEnv+"="+scenario)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reconcile status helper %q failed: %v\n%s", scenario, err, output)
	}
}

func TestReconcileStatusHelperProcess(t *testing.T) {
	scenario := os.Getenv(reconcileStatusHelperScenarioEnv)
	if scenario == "" {
		return
	}
	switch scenario {
	case "empty":
		testEmptyReconcileStatus(t)
	case "marker-only":
		testMarkerOnlyReconcileStatus(t)
	case "lifecycle":
		testReconcileStatusLifecycle(t)
	case "source-save":
		testSourceExclusionSaveDoesNotMutateTamperingStatus(t)
	default:
		t.Fatalf("unknown helper scenario %q", scenario)
	}
}

func testEmptyReconcileStatus(t *testing.T) {
	reconcileStatusMu.Lock()
	reconcileStatus = blockedHistoryReconcileStatus{}
	tamperingReconcileState = tamperingHistoryReconcileStatus{}
	fullReconcileInProgress.Store(false)
	tamperingReconcileBusy.Store(false)
	reconcileStatusMu.Unlock()

	blocked := getReconcileStatusResponse(t, handleReconcileBlockedHistoryStatus, "/api/reconcile/blocked-history/status")
	tampering := getReconcileStatusResponse(t, handleReconcileTamperingHistoryStatus, "/api/reconcile/tampering-history/status")
	assertNoReconcileTimestamps(t, blocked)
	assertNoReconcileTimestamps(t, tampering)
	if running, ok := blocked["running"].(bool); !ok || running {
		t.Fatalf("empty blocked running = %#v, want false", blocked["running"])
	}
	if running, ok := tampering["running"].(bool); !ok || running {
		t.Fatalf("empty tampering running = %#v, want false", tampering["running"])
	}
}

func testMarkerOnlyReconcileStatus(t *testing.T) {
	completed := time.Date(2026, time.September, 14, 15, 16, 17, 0, time.UTC)
	reconcileStatusMu.Lock()
	reconcileStatus = blockedHistoryReconcileStatus{
		StartupSkipped:    true,
		StartupSkipReason: "marker exists for current target set",
		LastCompletedAt:   completed,
		LastMessage:       "startup reconcile skipped (already completed)",
	}
	tamperingReconcileState = tamperingHistoryReconcileStatus{
		StartupSkipped:    true,
		StartupSkipReason: "marker exists for stored day set",
		LastCompletedAt:   completed,
		LastMessage:       "startup reconcile skipped (already completed)",
	}
	fullReconcileInProgress.Store(false)
	tamperingReconcileBusy.Store(false)
	reconcileStatusMu.Unlock()

	for name, response := range map[string]map[string]any{
		"blocked":   getReconcileStatusResponse(t, handleReconcileBlockedHistoryStatus, "/api/reconcile/blocked-history/status"),
		"tampering": getReconcileStatusResponse(t, handleReconcileTamperingHistoryStatus, "/api/reconcile/tampering-history/status"),
	} {
		if _, ok := response["last_started_at"]; ok {
			t.Fatalf("%s marker-only response unexpectedly contains last_started_at: %#v", name, response)
		}
		if _, ok := response["last_finished_at"]; ok {
			t.Fatalf("%s marker-only response unexpectedly contains last_finished_at: %#v", name, response)
		}
		if got := response["last_completed_at"]; got != completed.Format(time.RFC3339) {
			t.Fatalf("%s last_completed_at = %#v, want %q", name, got, completed.Format(time.RFC3339))
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "0001-01-01") {
			t.Fatalf("%s marker-only response contains a zero timestamp: %s", name, encoded)
		}
	}
}

func testReconcileStatusLifecycle(t *testing.T) {
	completed := time.Date(2026, time.August, 1, 2, 3, 4, 0, time.UTC)
	staleFinished := completed.Add(-time.Hour)
	reconcileStatusMu.Lock()
	reconcileStatus = blockedHistoryReconcileStatus{
		LastTriggerReason:   "stale-blocked",
		LastStartedAt:       staleFinished.Add(-time.Minute),
		LastFinishedAt:      staleFinished,
		LastDays:            99,
		LastUpdated:         98,
		LastFailed:          97,
		LastCompletedAt:     completed,
		LastTargetSignature: "stale-targets",
		StartupSkipped:      true,
		StartupSkipReason:   "stale skip",
	}
	tamperingReconcileState = tamperingHistoryReconcileStatus{
		LastTriggerReason: "stale-tampering",
		LastStartedAt:     staleFinished.Add(-time.Minute),
		LastFinishedAt:    staleFinished,
		LastDays:          88,
		LastUpdated:       87,
		LastFailed:        86,
		LastCompletedAt:   completed,
		LastDaySignature:  "stale-days",
		StartupSkipped:    true,
		StartupSkipReason: "stale skip",
	}
	fullReconcileInProgress.Store(false)
	tamperingReconcileBusy.Store(false)
	reconcileStatusMu.Unlock()

	if !beginBlockedHistoryReconcileStatus("unit-blocked") {
		t.Fatal("blocked lifecycle did not start")
	}
	blockedStarted := snapshotBlockedReconcileStatus()
	assertBlockedStartedState(t, blockedStarted, completed)
	blockedResponse := getReconcileStatusResponse(t, handleReconcileBlockedHistoryStatus, "/api/reconcile/blocked-history/status")
	if blockedResponse["running"] != true || blockedResponse["last_trigger_reason"] != "unit-blocked" {
		t.Fatalf("blocked running endpoint response = %#v", blockedResponse)
	}
	if beginBlockedHistoryReconcileStatus("must-not-replace") {
		t.Fatal("second blocked lifecycle start unexpectedly succeeded")
	}
	if got := snapshotBlockedReconcileStatus(); !reflect.DeepEqual(got, blockedStarted) {
		t.Fatalf("rejected blocked start mutated status:\n got  %#v\n want %#v", got, blockedStarted)
	}
	finishBlockedHistoryReconcileStatus(4, 3, 1, "blocked-signature")
	blockedFinished := snapshotBlockedReconcileStatus()
	if blockedFinished.Running || fullReconcileInProgress.Load() {
		t.Fatalf("blocked finish still running: status=%#v busy=%v", blockedFinished, fullReconcileInProgress.Load())
	}
	if blockedFinished.LastFinishedAt.IsZero() || blockedFinished.LastFinishedAt.Before(blockedFinished.LastStartedAt) {
		t.Fatalf("blocked finish timestamp invalid: %#v", blockedFinished)
	}
	if blockedFinished.LastTriggerReason != "unit-blocked" || blockedFinished.LastDays != 4 || blockedFinished.LastUpdated != 3 || blockedFinished.LastFailed != 1 || blockedFinished.LastTargetSignature != "blocked-signature" {
		t.Fatalf("blocked finish counters/reason invalid: %#v", blockedFinished)
	}
	if !blockedFinished.LastCompletedAt.Equal(completed) {
		t.Fatalf("blocked completion marker changed: %s", blockedFinished.LastCompletedAt)
	}
	assertRealRunTimestamps(t, getReconcileStatusResponse(t, handleReconcileBlockedHistoryStatus, "/api/reconcile/blocked-history/status"), "unit-blocked")

	if !beginTamperingHistoryReconcileStatus("unit-tampering") {
		t.Fatal("tampering lifecycle did not start")
	}
	tamperingStarted := snapshotTamperingReconcileStatus()
	assertTamperingStartedState(t, tamperingStarted, completed)
	tamperingResponse := getReconcileStatusResponse(t, handleReconcileTamperingHistoryStatus, "/api/reconcile/tampering-history/status")
	if tamperingResponse["running"] != true || tamperingResponse["last_trigger_reason"] != "unit-tampering" {
		t.Fatalf("tampering running endpoint response = %#v", tamperingResponse)
	}
	if beginTamperingHistoryReconcileStatus("must-not-replace") {
		t.Fatal("second tampering lifecycle start unexpectedly succeeded")
	}
	if got := snapshotTamperingReconcileStatus(); !reflect.DeepEqual(got, tamperingStarted) {
		t.Fatalf("rejected tampering start mutated status:\n got  %#v\n want %#v", got, tamperingStarted)
	}
	finishTamperingHistoryReconcileStatus(7, 6, 2, "day-signature")
	tamperingFinished := snapshotTamperingReconcileStatus()
	if tamperingFinished.Running || tamperingReconcileBusy.Load() {
		t.Fatalf("tampering finish still running: status=%#v busy=%v", tamperingFinished, tamperingReconcileBusy.Load())
	}
	if tamperingFinished.LastFinishedAt.IsZero() || tamperingFinished.LastFinishedAt.Before(tamperingFinished.LastStartedAt) {
		t.Fatalf("tampering finish timestamp invalid: %#v", tamperingFinished)
	}
	if tamperingFinished.LastTriggerReason != "unit-tampering" || tamperingFinished.LastDays != 7 || tamperingFinished.LastUpdated != 6 || tamperingFinished.LastFailed != 2 || tamperingFinished.LastDaySignature != "day-signature" {
		t.Fatalf("tampering finish counters/reason invalid: %#v", tamperingFinished)
	}
	if !tamperingFinished.LastCompletedAt.Equal(completed) {
		t.Fatalf("tampering completion marker changed: %s", tamperingFinished.LastCompletedAt)
	}
	assertRealRunTimestamps(t, getReconcileStatusResponse(t, handleReconcileTamperingHistoryStatus, "/api/reconcile/tampering-history/status"), "unit-tampering")
}

func testSourceExclusionSaveDoesNotMutateTamperingStatus(t *testing.T) {
	target := TrafficTarget{Name: "All Traffic", Kind: "all"}
	configMutex.Lock()
	config = Config{
		PCEURL:         "https://pce.example.test",
		OrgID:          "1",
		TrafficTargets: []TrafficTarget{target},
		HistoryDays:    30,
	}
	configMutex.Unlock()

	completed := time.Date(2026, time.July, 1, 2, 3, 4, 0, time.UTC)
	before := tamperingHistoryReconcileStatus{
		LastTriggerReason: "api",
		LastStartedAt:     completed.Add(-time.Minute),
		LastFinishedAt:    completed,
		LastDays:          3,
		LastUpdated:       2,
		LastFailed:        1,
		LastMessage:       "distinctive completed status",
		LastCompletedAt:   completed,
		LastDaySignature:  "existing-days",
	}
	reconcileStatusMu.Lock()
	tamperingReconcileState = before
	tamperingReconcileBusy.Store(false)
	reconcileStatusMu.Unlock()

	body := []byte(`{"traffic_targets":[{"name":"All Traffic","kind":"all"}],"traffic_source_exclusions":[{"name":"Development","kind":"label"}],"history_days":30}`)
	recorder := httptest.NewRecorder()
	handleConfigTargets(recorder, httptest.NewRequest(http.MethodPut, "/api/config/targets", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("source exclusion save status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	after := snapshotTamperingReconcileStatus()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("source exclusion save mutated tampering reconcile status:\n got  %#v\n want %#v", after, before)
	}
	if tamperingReconcileBusy.Load() {
		t.Fatal("source exclusion save started tampering reconciliation")
	}
	configMutex.RLock()
	saved := append([]TrafficTarget(nil), config.SourceExclusions...)
	configMutex.RUnlock()
	if len(saved) != 1 || saved[0].Name != "Development" || saved[0].Kind != "label" {
		t.Fatalf("source exclusion was not saved: %#v", saved)
	}
}

func getReconcileStatusResponse(t *testing.T, handler http.HandlerFunc, path string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET %s response: %v", path, err)
	}
	return response
}

func assertNoReconcileTimestamps(t *testing.T, response map[string]any) {
	t.Helper()
	for _, key := range []string{"last_started_at", "last_finished_at", "last_completed_at"} {
		if value, ok := response[key]; ok {
			t.Fatalf("empty status unexpectedly contains %s=%#v: %#v", key, value, response)
		}
	}
}

func assertBlockedStartedState(t *testing.T, status blockedHistoryReconcileStatus, completed time.Time) {
	t.Helper()
	if !status.Running || !fullReconcileInProgress.Load() || status.LastTriggerReason != "unit-blocked" || status.LastStartedAt.IsZero() {
		t.Fatalf("blocked start state invalid: status=%#v busy=%v", status, fullReconcileInProgress.Load())
	}
	if !status.LastFinishedAt.IsZero() || status.LastDays != 0 || status.LastUpdated != 0 || status.LastFailed != 0 || status.LastTargetSignature != "" || status.StartupSkipped || status.StartupSkipReason != "" {
		t.Fatalf("blocked start did not clear stale run state: %#v", status)
	}
	if !status.LastCompletedAt.Equal(completed) {
		t.Fatalf("blocked start cleared completion marker: %s", status.LastCompletedAt)
	}
}

func assertTamperingStartedState(t *testing.T, status tamperingHistoryReconcileStatus, completed time.Time) {
	t.Helper()
	if !status.Running || !tamperingReconcileBusy.Load() || status.LastTriggerReason != "unit-tampering" || status.LastStartedAt.IsZero() {
		t.Fatalf("tampering start state invalid: status=%#v busy=%v", status, tamperingReconcileBusy.Load())
	}
	if !status.LastFinishedAt.IsZero() || status.LastDays != 0 || status.LastUpdated != 0 || status.LastFailed != 0 || status.LastDaySignature != "" || status.StartupSkipped || status.StartupSkipReason != "" {
		t.Fatalf("tampering start did not clear stale run state: %#v", status)
	}
	if !status.LastCompletedAt.Equal(completed) {
		t.Fatalf("tampering start cleared completion marker: %s", status.LastCompletedAt)
	}
}

func assertRealRunTimestamps(t *testing.T, response map[string]any, reason string) {
	t.Helper()
	if response["running"] != false || response["last_trigger_reason"] != reason {
		t.Fatalf("completed endpoint state invalid: %#v", response)
	}
	for _, key := range []string{"last_started_at", "last_finished_at"} {
		value, ok := response[key].(string)
		if !ok || value == "" {
			t.Fatalf("completed endpoint missing %s: %#v", key, response)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.Year() <= 1 {
			t.Fatalf("completed endpoint has invalid %s=%q: %v", key, value, err)
		}
	}
}

func snapshotBlockedReconcileStatus() blockedHistoryReconcileStatus {
	reconcileStatusMu.Lock()
	defer reconcileStatusMu.Unlock()
	return reconcileStatus
}

func snapshotTamperingReconcileStatus() tamperingHistoryReconcileStatus {
	reconcileStatusMu.Lock()
	defer reconcileStatusMu.Unlock()
	return tamperingReconcileState
}
