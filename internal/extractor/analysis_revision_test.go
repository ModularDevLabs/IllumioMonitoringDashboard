package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"illumio-dash/internal/extractor/illumio"
)

const analysisRevisionScenarioEnv = "ILLUMIO_ANALYSIS_REVISION_TEST_SCENARIO"

// Run lifecycle tests in an isolated process because the extractor holds global
// app/automation state. The child also gets a temporary OS config directory.
func TestAnalysisRevisionLifecycle(t *testing.T) {
	for _, scenario := range []string{"import", "dataset", "extraction", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAnalysisRevisionHelperProcess$", "-test.v")
			cmd.Dir = root
			for _, entry := range os.Environ() {
				if strings.HasPrefix(entry, analysisRevisionScenarioEnv+"=") ||
					strings.HasPrefix(entry, "XDG_CONFIG_HOME=") || strings.HasPrefix(entry, "APPDATA=") ||
					strings.HasPrefix(entry, "HOME=") {
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			// HOME is only changed in this isolated child, so macOS UserConfigDir
			// cannot write test datasets to the developer's real home directory.
			cmd.Env = append(cmd.Env, analysisRevisionScenarioEnv+"="+scenario,
				"XDG_CONFIG_HOME="+root, "APPDATA="+root, "HOME="+root)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("analysis revision scenario %q failed: %v\n%s", scenario, err, output)
			}
		})
	}
}

func TestAnalysisRevisionHelperProcess(t *testing.T) {
	switch os.Getenv(analysisRevisionScenarioEnv) {
	case "":
		return
	case "import":
		testAnalysisImportRevision(t)
	case "dataset":
		testAnalysisDatasetRevision(t)
	case "extraction":
		testAnalysisExtractionRevision(t)
	case "partial":
		runPartialExtractionScenario(t, "one-failed-hour")
		revision := currentAnalysisRevision(t)
		if revision == "" {
			t.Fatal("partial extraction published analytics without a revision")
		}
		if got := currentAnalysisRevision(t); got != revision {
			t.Fatalf("partial summary refresh changed revision: %q => %q", revision, got)
		}
	default:
		t.Fatal("unknown analysis revision test scenario")
	}
}

func currentAnalysisRevision(t *testing.T) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handleSummary(recorder, httptest.NewRequest(http.MethodGet, "/api/results/summary", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("summary status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		AnalysisRevision string `json:"analysisRevision"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.AnalysisRevision
}

func analysisJSONRequest(t *testing.T, handler http.HandlerFunc, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://localhost:8000"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:8000")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != status {
		t.Fatalf("%s status = %d, want %d: %s", path, recorder.Code, status, recorder.Body.String())
	}
	return recorder
}

func testAnalysisImportRevision(t *testing.T) {
	const csv = "Source IP,Destination IP,Port,Protocol,Flows,Src Env,Dst Env,Src App,Dst App\n10.0.0.1,10.0.0.2,9300,TCP,3,Prod,Prod,API,DB\n"
	importCSV := func(data string, wantStatus int) {
		t.Helper()
		request := newCSVImportRequest(t, []struct{ name, data string }{{"same.csv", data}})
		recorder := httptest.NewRecorder()
		handleImportCSV(recorder, request)
		if recorder.Code != wantStatus {
			t.Fatalf("import status = %d, want %d: %s", recorder.Code, wantStatus, recorder.Body.String())
		}
	}
	if got := currentAnalysisRevision(t); got != "" {
		t.Fatalf("empty initial analysis revision = %q", got)
	}
	importCSV(csv, http.StatusOK)
	first := currentAnalysisRevision(t)
	if first == "" {
		t.Fatal("successful import did not create revision")
	}
	if got := currentAnalysisRevision(t); got != first {
		t.Fatalf("summary refresh changed revision: %q => %q", first, got)
	}
	importCSV(csv, http.StatusOK)
	second := currentAnalysisRevision(t)
	if second == "" || second == first {
		t.Fatalf("reimporting identical contents/filename reused revision: %q", second)
	}
	importCSV("invalid CSV header\n", http.StatusBadRequest)
	if got := currentAnalysisRevision(t); got != second {
		t.Fatalf("failed import discarded existing revision: %q => %q", second, got)
	}
	request := newCSVImportRequest(t, []struct{ name, data string }{{"same.csv", csv}})
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	recorder := httptest.NewRecorder()
	handleImportCSV(recorder, request.WithContext(ctx))
	if recorder.Code != http.StatusRequestTimeout || currentAnalysisRevision(t) != second {
		t.Fatalf("cancelled import changed revision or wrong status: %d", recorder.Code)
	}
}

func testAnalysisDatasetRevision(t *testing.T) {
	initial := newAnalysisRevision()
	state = &AppState{
		FileName: "saved.csv", AnalysisRevision: initial,
		LastSummary: []PortProtocolSummary{{Port: 9300, Protocol: "TCP", FlowCount: 3, UniqueConnections: 1}},
	}
	datasetManager = &DatasetManager{data: datasetStoreData{Version: datasetStoreVersion, Datasets: map[string]SavedDataset{}}}
	savedResponse := analysisJSONRequest(t, handleDatasetSave, "/api/datasets/save", `{"name":"Saved analysis"}`, http.StatusOK)
	if got := currentAnalysisRevision(t); got != initial {
		t.Fatalf("saving dataset changed revision: %q => %q", initial, got)
	}
	var saved struct {
		Dataset SavedDataset `json:"dataset"`
	}
	if err := json.Unmarshal(savedResponse.Body.Bytes(), &saved); err != nil || saved.Dataset.ID == "" {
		t.Fatalf("decode saved dataset: %v %s", err, savedResponse.Body.String())
	}
	body, err := json.Marshal(map[string]string{"id": saved.Dataset.ID})
	if err != nil {
		t.Fatal(err)
	}
	previous := initial
	for range 2 {
		analysisJSONRequest(t, handleDatasetLoad, "/api/datasets/load", string(body), http.StatusOK)
		revision := currentAnalysisRevision(t)
		if revision == "" || revision == previous {
			t.Fatalf("explicit saved dataset reload reused revision: %q", revision)
		}
		previous = revision
	}
	analysisJSONRequest(t, handleReportMetadataSave, "/api/results/report-metadata",
		`{"title":"Edited title","notes":"Report note"}`, http.StatusOK)
	if got := currentAnalysisRevision(t); got != previous {
		t.Fatalf("report edit changed analysis revision: %q => %q", previous, got)
	}
	analysisJSONRequest(t, handleDatasetLoad, "/api/datasets/load", `{"id":"missing"}`, http.StatusNotFound)
	if got := currentAnalysisRevision(t); got != previous {
		t.Fatalf("failed dataset load changed analysis revision: %q => %q", previous, got)
	}
}

func testAnalysisExtractionRevision(t *testing.T) {
	fixture := newPartialExtractionPCE(t, "success", "")
	profile := PCEProfile{
		Name: "fixture", PCEURL: fixture.server.URL, OrgID: "1", APIKey: "fixture-key", APISecret: "fixture-secret",
		AnalysisPrimary: "env", AnalysisSecondary: "app", TrafficScope: trafficScopeBlocked,
	}
	state = &AppState{
		FileName: "previous.csv", AnalysisRevision: "previous-analysis",
		LastSummary:    []PortProtocolSummary{{Port: 9300, Protocol: "TCP", FlowCount: 3, UniqueConnections: 1}},
		Profiles:       map[string]PCEProfile{"fixture": profile},
		DiscoveryCache: &DiscoveryData{Labels: []illumio.Label{{Key: "env", Value: "Prod"}, {Key: "app", Value: "Source"}}},
		DiscoveryKey: discoveryCacheKey(Config{
			PCEURL: profile.PCEURL, OrgID: profile.OrgID, APIKey: profile.APIKey, APISecret: profile.APISecret,
		}),
	}
	cfg := Config{
		ProfileName: "fixture", StartDate: "2026-03-01", EndDate: "2026-03-01", ChunkIntvl: "24h",
		SavePath: filepath.Join(t.TempDir(), "output"), FileName: "traffic.csv",
		AnalysisPrimary: "env", AnalysisSecondary: "app", TrafficScope: trafficScopeBlocked,
	}
	if err := os.MkdirAll(cfg.SavePath, 0700); err != nil {
		t.Fatal(err)
	}
	invalid := cfg
	invalid.ChunkIntvl = "invalid"
	if _, _, err := beginExtraction(invalid); err == nil {
		t.Fatal("invalid extraction configuration was accepted")
	}
	if got := currentAnalysisRevision(t); got != "previous-analysis" {
		t.Fatalf("failed extraction start changed revision: %q", got)
	}
	resolved, ctx, err := beginExtraction(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := currentAnalysisRevision(t); got != "" {
		t.Fatalf("new extraction retained old analysis revision: %q", got)
	}
	runExtraction(ctx, resolved)
	revision := currentAnalysisRevision(t)
	if revision == "" || revision == "previous-analysis" {
		t.Fatalf("completed extraction did not publish fresh analysis revision: %q", revision)
	}
	if got := currentAnalysisRevision(t); got != revision {
		t.Fatalf("completed extraction revision changed on refresh: %q => %q", revision, got)
	}
	state.Mu.Lock()
	defer state.Mu.Unlock()
	if state.FileName == "" || len(state.LastSummary) == 0 {
		t.Fatal("extraction test did not produce analytics")
	}
}
