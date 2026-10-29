package extractor

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"illumio-dash/internal/extractor/illumio"
)

const partialExtractionHelperScenarioEnv = "ILLUMIO_PARTIAL_EXTRACTION_HELPER_SCENARIO"

func TestRunExtractionRetainsPartialResults(t *testing.T) {
	for _, scenario := range []string{"one-failed-hour", "cancel-after-success", "deadline-after-success", "zero-row-success", "partial-name-collision", "all-failed"} {
		scenario := scenario
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPartialExtractionHelperProcess$", "-test.v")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), partialExtractionHelperScenarioEnv+"="+scenario)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("partial extraction helper %q timed out\n%s", scenario, output)
			}
			if err != nil {
				t.Fatalf("partial extraction helper %q failed: %v\n%s", scenario, err, output)
			}
		})
	}
}

func TestPartialExtractionHelperProcess(t *testing.T) {
	scenario := os.Getenv(partialExtractionHelperScenarioEnv)
	if scenario == "" {
		return
	}
	runPartialExtractionScenario(t, scenario)
}

type partialExtractionPCE struct {
	t             *testing.T
	scenario      string
	failureStart  string
	server        *httptest.Server
	mu            sync.Mutex
	nextQuery     int
	grantedCancel bool
	queries       map[string]partialExtractionQuery
	postAttempts  map[string]int
}

type partialExtractionQuery struct {
	request illumio.AsyncQueryRequest
	ordinal int
}

func newPartialExtractionPCE(t *testing.T, scenario, failureStart string) *partialExtractionPCE {
	t.Helper()
	fixture := &partialExtractionPCE{
		t:            t,
		scenario:     scenario,
		failureStart: failureStart,
		queries:      make(map[string]partialExtractionQuery),
		postAttempts: make(map[string]int),
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *partialExtractionPCE) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if user, password, ok := r.BasicAuth(); !ok || user != "fixture-key" || password != "fixture-secret" {
		fixture.t.Errorf("fake PCE received incorrect Basic Auth")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	queryCollection := strings.HasSuffix(r.URL.Path, "/traffic_flows/async_queries")
	switch {
	case r.Method == http.MethodPost && queryCollection:
		fixture.createQuery(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/download"):
		fixture.downloadQuery(w, r)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/traffic_flows/async_queries/"):
		fixture.queryStatus(w, r)
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/traffic_flows/async_queries/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		fixture.t.Errorf("unexpected fake PCE request: %s %s", r.Method, r.URL.String())
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}
}

func (fixture *partialExtractionPCE) createQuery(w http.ResponseWriter, r *http.Request) {
	var request illumio.AsyncQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		fixture.t.Errorf("decode fake PCE query: %v", err)
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}

	fixture.mu.Lock()
	fixture.postAttempts[request.StartDate]++
	shouldFail := fixture.scenario == "all-failed" || request.StartDate == fixture.failureStart
	shouldBlock := false
	if fixture.scenario == "cancel-after-success" || fixture.scenario == "deadline-after-success" {
		if fixture.grantedCancel {
			shouldBlock = true
		} else {
			fixture.grantedCancel = true
		}
	}
	if !shouldFail && !shouldBlock {
		fixture.nextQuery++
		queryID := fmt.Sprintf("fixture-%d", fixture.nextQuery)
		fixture.queries[queryID] = partialExtractionQuery{request: request, ordinal: fixture.nextQuery}
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"href": "/api/v2/orgs/1/traffic_flows/async_queries/" + queryID})
		return
	}
	fixture.mu.Unlock()

	if shouldFail {
		http.Error(w, "fixture rejected this query window", http.StatusBadRequest)
		return
	}

	select {
	case <-r.Context().Done():
		return
	case <-time.After(30 * time.Second):
		http.Error(w, "fixture cancellation did not arrive", http.StatusGatewayTimeout)
	}
}

func (fixture *partialExtractionPCE) queryStatus(w http.ResponseWriter, r *http.Request) {
	queryID := filepath.Base(r.URL.Path)
	fixture.mu.Lock()
	_, ok := fixture.queries[queryID]
	fixture.mu.Unlock()
	if !ok {
		fixture.t.Errorf("status requested for unknown fake query %q", queryID)
		http.Error(w, "unknown query", http.StatusNotFound)
		return
	}
	count := 1
	if fixture.scenario == "zero-row-success" {
		count = 0
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "matches_count": count, "flows_count": count})
}

func (fixture *partialExtractionPCE) downloadQuery(w http.ResponseWriter, r *http.Request) {
	queryID := filepath.Base(filepath.Dir(r.URL.Path))
	fixture.mu.Lock()
	query, ok := fixture.queries[queryID]
	fixture.mu.Unlock()
	if !ok {
		fixture.t.Errorf("download requested for unknown fake query %q", queryID)
		http.Error(w, "unknown query", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if fixture.scenario == "zero-row-success" {
		_, _ = io.WriteString(w, "[]")
		return
	}

	start, err := time.Parse(time.RFC3339, query.request.StartDate)
	if err != nil {
		fixture.t.Errorf("parse fake query start %q: %v", query.request.StartDate, err)
		http.Error(w, "bad fixture time", http.StatusInternalServerError)
		return
	}
	ordinal := 10 + query.ordinal
	row := []map[string]any{{
		"src": map[string]any{
			"ip":       fmt.Sprintf("10.0.0.%d", ordinal),
			"workload": map[string]any{"href": fmt.Sprintf("/workloads/src-%d", ordinal), "labels": []map[string]string{{"key": "env", "value": "Prod"}, {"key": "app", "value": "Source"}}},
		},
		"dst": map[string]any{
			"ip":       fmt.Sprintf("10.0.1.%d", ordinal),
			"workload": map[string]any{"href": fmt.Sprintf("/workloads/dst-%d", ordinal), "labels": []map[string]string{{"key": "env", "value": "Prod"}, {"key": "app", "value": "Destination"}}},
		},
		"service":               map[string]any{"port": 443, "proto": 6},
		"num_connections":       1,
		"policy_decision":       "blocked",
		"draft_policy_decision": "blocked",
		"timestamp_range": map[string]string{
			"first_detected": start.Add(time.Minute).Format(time.RFC3339),
			"last_detected":  start.Add(2 * time.Minute).Format(time.RFC3339),
		},
	}}
	_ = json.NewEncoder(w).Encode(row)
}

func runPartialExtractionScenario(t *testing.T, scenario string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	outputDir := filepath.Join(root, "output")
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		t.Fatal(err)
	}

	chunkInterval := "1h"
	failureStart := "2026-03-01T12:00:00Z"
	switch scenario {
	case "cancel-after-success", "deadline-after-success":
		chunkInterval = "6h"
		failureStart = ""
	case "zero-row-success", "partial-name-collision":
		chunkInterval = "12h"
	case "all-failed":
		chunkInterval = "24h"
		failureStart = ""
	case "one-failed-hour":
	default:
		t.Fatalf("unknown partial extraction scenario %q", scenario)
	}

	fixture := newPartialExtractionPCE(t, scenario, failureStart)
	profile := PCEProfile{
		Name: "fixture", PCEURL: fixture.server.URL, OrgID: "1", APIKey: "fixture-key", APISecret: "fixture-secret",
		AnalysisPrimary: "env", AnalysisSecondary: "app", TrafficScope: trafficScopeBlocked,
	}
	discovery := &DiscoveryData{Labels: []illumio.Label{{Key: "env", Value: "Prod"}, {Key: "app", Value: "Source"}}}
	state = &AppState{
		Logs:           []string{},
		Profiles:       map[string]PCEProfile{"fixture": profile},
		DiscoveryCache: discovery,
		DiscoveryKey: discoveryCacheKey(Config{
			PCEURL: profile.PCEURL, OrgID: profile.OrgID, APIKey: profile.APIKey, APISecret: profile.APISecret,
		}),
	}

	var (
		parent       context.Context
		stopParent   func()
		deadlineTest *controlledDeadlineContext
	)
	if scenario == "deadline-after-success" {
		deadlineTest = newControlledDeadlineContext()
		parent = deadlineTest
		stopParent = deadlineTest.expire
	} else {
		var cancel context.CancelFunc
		parent, cancel = context.WithCancel(context.Background())
		stopParent = cancel
	}
	defer stopParent()
	var collisionSentinel string
	if scenario == "partial-name-collision" {
		collisionSentinel = filepath.Join(outputDir, "traffic_PARTIAL.csv")
		if err := os.WriteFile(collisionSentinel, []byte("existing partial must not be overwritten\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	resolved, runContext, err := beginExtractionWithContext(parent, Config{
		ProfileName: "fixture", StartDate: "2026-03-01", EndDate: "2026-03-01", ChunkIntvl: chunkInterval,
		SavePath: outputDir, FileName: "traffic.csv", AnalysisPrimary: "env", AnalysisSecondary: "app", TrafficScope: trafficScopeBlocked,
	})
	if err != nil {
		t.Fatalf("begin extraction: %v", err)
	}

	if scenario == "cancel-after-success" || scenario == "deadline-after-success" {
		done := make(chan struct{})
		go func() {
			runExtraction(runContext, resolved)
			close(done)
		}()
		waitForCommittedChunk(t)
		stopParent()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("cancelled extraction did not finish")
		}
	} else {
		runExtraction(runContext, resolved)
	}

	status := partialExtractionStatus(t)
	switch scenario {
	case "one-failed-hour":
		assertPartialExtraction(t, status, outputDir, 24, 23, 1, 23, false)
		fixture.assertOriginalAttempts(t, 24, failureStart)
	case "cancel-after-success":
		assertPartialExtraction(t, status, outputDir, 4, 1, 3, 1, true)
	case "deadline-after-success":
		assertPartialExtraction(t, status, outputDir, 4, 1, 3, 1, false)
	case "zero-row-success":
		assertPartialExtraction(t, status, outputDir, 2, 1, 1, 0, false)
		fixture.assertOriginalAttempts(t, 2, failureStart)
	case "partial-name-collision":
		assertPartialExtraction(t, status, outputDir, 2, 1, 1, 1, false)
		fixture.assertOriginalAttempts(t, 2, failureStart)
		artifact, _ := status["fileName"].(string)
		if artifact == collisionSentinel {
			t.Fatalf("partial output overwrote the existing sentinel %q", collisionSentinel)
		}
		contents, err := os.ReadFile(collisionSentinel)
		if err != nil {
			t.Fatalf("read collision sentinel: %v", err)
		}
		if string(contents) != "existing partial must not be overwritten\n" {
			t.Fatalf("partial collision sentinel changed: %q", contents)
		}
	case "all-failed":
		assertAllFailedExtraction(t, status, outputDir)
		fixture.assertOriginalAttempts(t, 1, "2026-03-01T00:00:00Z")
	}
}

type controlledDeadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newControlledDeadlineContext() *controlledDeadlineContext {
	return &controlledDeadlineContext{Context: context.Background(), done: make(chan struct{})}
}

func (ctx *controlledDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *controlledDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *controlledDeadlineContext) expire() { ctx.once.Do(func() { close(ctx.done) }) }

func waitForCommittedChunk(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state.Mu.Lock()
		completed := state.CompletedChunks
		state.Mu.Unlock()
		if completed >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture never observed a committed successful chunk")
}

func partialExtractionStatus(t *testing.T) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	handleStatus(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status response = %d: %s", recorder.Code, recorder.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return status
}

func assertPartialExtraction(t *testing.T, status map[string]any, outputDir string, requested, completed, missing, dataRows int, cancelled bool) {
	t.Helper()
	if status["done"] != true || status["partial"] != true {
		t.Fatalf("partial status flags = %#v", status)
	}
	if status["cancelled"] != cancelled {
		t.Fatalf("cancelled = %#v, want %v", status["cancelled"], cancelled)
	}
	if got := int(status["requestedChunks"].(float64)); got != requested {
		t.Fatalf("requested chunks = %d, want %d", got, requested)
	}
	if got := int(status["completedChunks"].(float64)); got != completed {
		t.Fatalf("completed chunks = %d, want %d", got, completed)
	}
	if got := int(status["failedChunks"].(float64)); got != missing {
		t.Fatalf("failed chunks = %d, want %d", got, missing)
	}
	if message, _ := status["error"].(string); strings.TrimSpace(message) == "" {
		t.Fatalf("partial status has no warning/error: %#v", status)
	}

	fileName, _ := status["fileName"].(string)
	if base := filepath.Base(fileName); !filepath.IsAbs(fileName) || !strings.Contains(base, "_PARTIAL") || !strings.HasSuffix(base, ".csv") {
		t.Fatalf("partial artifact path = %q", fileName)
	}
	if filepath.Dir(fileName) != outputDir {
		t.Fatalf("partial artifact directory = %q, want %q", filepath.Dir(fileName), outputDir)
	}
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatalf("open partial CSV: %v", err)
	}
	records, err := csv.NewReader(file).ReadAll()
	_ = file.Close()
	if err != nil {
		t.Fatalf("read partial CSV: %v", err)
	}
	if got := len(records) - 1; got != dataRows {
		t.Fatalf("partial CSV data rows = %d, want %d", got, dataRows)
	}
	if len(records) == 0 || len(records[0]) == 0 || records[0][0] != "First Detected" {
		t.Fatalf("partial CSV is missing its header: %#v", records)
	}

	manifestBytes, err := os.ReadFile(extractionManifestPath(fileName))
	if err != nil {
		t.Fatalf("read partial extraction manifest: %v", err)
	}
	var manifest extractionManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode partial extraction manifest: %v", err)
	}
	if !manifest.Partial || manifest.CSVFile != filepath.Base(fileName) {
		t.Fatalf("manifest identity = %#v", manifest)
	}
	if got := manifest.RequestedStart.Format(time.RFC3339); got != "2026-03-01T00:00:00Z" {
		t.Fatalf("manifest requested_start = %q", got)
	}
	if got := manifest.RequestedEnd.Format(time.RFC3339); got != "2026-03-02T00:00:00Z" {
		t.Fatalf("manifest requested_end_exclusive = %q", got)
	}
	if manifest.RequestedChunks != requested || manifest.CompletedChunks != completed || len(manifest.CompletedWindows) != completed || len(manifest.MissingWindows) != missing {
		t.Fatalf("manifest coverage = %#v", manifest)
	}
	for _, window := range manifest.MissingWindows {
		if !window.EndExclusive.After(window.Start) || strings.TrimSpace(window.Reason) == "" {
			t.Fatalf("manifest has invalid missing window: %#v", window)
		}
	}

	state.Mu.Lock()
	coverage := state.DatasetCoverage
	isPartial := state.IsPartial
	failedChunks := state.FailedChunks
	state.Mu.Unlock()
	if !isPartial || failedChunks < 1 || !coverage.Partial || len(coverage.MissingWindows) != missing || len(coverage.Warnings) == 0 {
		t.Fatalf("state/coverage did not preserve the partial outcome: isPartial=%v failed=%d coverage=%#v", isPartial, failedChunks, coverage)
	}
}

func assertAllFailedExtraction(t *testing.T, status map[string]any, outputDir string) {
	t.Helper()
	if status["done"] != true || status["fileName"] != "" {
		t.Fatalf("all-failed status = %#v", status)
	}
	if got := int(status["completedChunks"].(float64)); got != 0 {
		t.Fatalf("all-failed completed chunks = %d", got)
	}
	if got := int(status["failedChunks"].(float64)); got != 1 {
		t.Fatalf("all-failed failed chunks = %d, want 1", got)
	}
	if message, _ := status["error"].(string); strings.TrimSpace(message) == "" {
		t.Fatalf("all-failed status has no clear error: %#v", status)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".csv") || strings.HasSuffix(entry.Name(), ".extraction.json") {
			t.Fatalf("all-failed run left an artifact: %s", entry.Name())
		}
	}
}

func (fixture *partialExtractionPCE) assertOriginalAttempts(t *testing.T, originalWindows int, failedStart string) {
	t.Helper()
	fixture.mu.Lock()
	attempts := make(map[string]int, len(fixture.postAttempts))
	for start, count := range fixture.postAttempts {
		attempts[start] = count
	}
	fixture.mu.Unlock()
	if len(attempts) != originalWindows {
		t.Fatalf("fake PCE saw %d original windows, want %d: %#v", len(attempts), originalWindows, attempts)
	}
	for start, count := range attempts {
		want := 1
		if start == failedStart || fixture.scenario == "all-failed" {
			want = maxChunkAttempts
		}
		if count != want {
			t.Fatalf("query attempts for %s = %d, want %d", start, count, want)
		}
	}
}
