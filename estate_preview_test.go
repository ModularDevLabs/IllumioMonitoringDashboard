package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"illumio-dash/internal/extractor"
)

// Opt-in, entirely synthetic UI fixture. It never loads the user's config,
// calls a real PCE, or sends webhooks. Used for manual browser regression tests.
func TestEstatePreviewServer(t *testing.T) {
	if os.Getenv("ILLUMIO_ESTATE_PREVIEW") != "1" {
		t.Skip("synthetic preview server only")
	}
	t.Chdir(t.TempDir())
	dataDir = t.TempDir()
	provisionedAt := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	rule := func(version, id string, port int) map[string]any {
		return map[string]any{"href": "/orgs/1/sec_policy/" + version + "/rule_sets/1/sec_rules/" + id, "enabled": true, "providers": []any{}, "consumers": []any{}, "ingress_services": []any{map[string]any{"proto": 6, "port": port}}, "resolve_labels_as": map[string]any{"providers": []any{"workloads"}, "consumers": []any{"workloads"}}}
	}
	pce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/orgs/1/sec_policy/active/rule_sets":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"href": "/orgs/1/sec_policy/active/rule_sets/1", "name": "Payments", "rules": []any{rule("active", "1", 443)}}})
		case "/api/v2/orgs/1/sec_policy/draft/rule_sets":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"href": "/orgs/1/sec_policy/draft/rule_sets/1", "name": "Payments", "rules": []any{rule("draft", "1", 8443), rule("draft", "2", 5432)}}})
		case "/api/v2/orgs/1/sec_policy/pending":
			_ = json.NewEncoder(w).Encode(map[string]any{"rule_sets": []any{map[string]any{"href": "/orgs/1/sec_policy/draft/rule_sets/1", "name": "Payments", "update_type": "update", "updated_at": time.Now().UTC().Format(time.RFC3339)}}})
		case "/api/v2/orgs/1/sec_policy/active":
			_ = json.NewEncoder(w).Encode(map[string]any{"href": "/orgs/1/sec_policy/12", "version": 12, "created_at": provisionedAt})
		case "/api/v2/orgs/1/sec_policy":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"href": "/orgs/1/sec_policy/12", "version": 12, "created_at": provisionedAt}})
		default:
			_ = json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer pce.Close()
	all, _ := normalizeEstateOptions(EstateOptions{InventoryEnabled: true, PolicyEnabled: true, IssueAgingEnabled: true, LabelHygieneEnabled: true, VENVersionsEnabled: true, ChangeLogEnabled: true, WeeklyReportEnabled: true, RequiredLabelTypes: []string{"app", "env"}})
	config = Config{PCEURL: pce.URL, OrgID: "1", APIKey: "fixture-only", APISecret: "fixture-only", Timezone: "America/Chicago", Estate: all}
	buildVersion = "v1.3.1-dev-estate"
	loc := configuredDayLocation()
	workloads := []map[string]interface{}{
		{"href": "/workloads/1", "name": "payments-api-01", "managed": true, "enforcement_mode": "full", "agent": map[string]any{"href": "/vens/1", "status": map[string]any{"agent_version": "25.2.10"}}, "labels": []any{map[string]any{"key": "app", "value": "Payments"}, map[string]any{"key": "env", "value": "Production"}}},
		{"href": "/workloads/2", "name": "payments-db-01", "managed": true, "enforcement_mode": "selective", "agent": map[string]any{"href": "/vens/2", "status": map[string]any{"agent_version": "24.5.20"}}, "labels": []any{map[string]any{"key": "app", "value": "Payments"}}},
		{"href": "/workloads/3", "name": "legacy-web-01", "managed": true, "enforcement_mode": "visibility_only", "agent": map[string]any{"href": "/vens/3", "status": map[string]any{}}, "labels": []any{}},
		{"href": "/workloads/4", "name": "unmanaged-test-host", "managed": false, "labels": []any{}},
	}
	health := map[string][]map[string]interface{}{"warning": {{"href": "/vens/2", "hostname": "payments-db-01"}}, "stopped": {{"href": "/vens/3", "hostname": "legacy-web-01"}}}
	snapshot := DashboardStats{}
	snapshot.Workloads.Total = 4
	snapshot.Workloads.Unmanaged = 1
	snapshot.Workloads.EnforcementModes = map[string]int{"full": 1, "selective": 1, "visibility_only": 1}
	snapshot.Workloads.ModeMembers = map[string][]string{"full": {"payments-api-01"}, "selective": {"payments-db-01"}, "visibility_only": {"legacy-web-01"}, "unmanaged": {"unmanaged-test-host"}}
	snapshot.Workloads.Status.Success = true
	snapshot.VENStatus.Status.Success = true
	snapshot.VENStatus.StoppedStatus.Success = true
	snapshot.VENStatus.Warning = []string{"payments-db-01"}
	snapshot.VENStatus.Stopped = []string{"legacy-web-01"}
	snapshot.Tampering.Status.Success = true
	snapshot.Blocked.Status.Success = true
	snapshot.Blocked.PRODStatus.Success = true
	snapshot.Blocked.NONPRODStatus.Success = true
	week := estateCompletedWeek(time.Now(), loc)
	for day := -7; day < 7; day++ {
		snapshot.Timestamp = week.Start.AddDate(0, 0, day).Add(12 * time.Hour).UTC()
		snapshot.Tampering.LastSeen = map[string]time.Time{"payments-db-01": snapshot.Timestamp.Add(-time.Hour)}
		updateEstateMonitor(pce.URL+"/api/v2/orgs/1", workloads, nil, health, snapshot, all)
	}
	refresh := func() {
		snapshot.Timestamp = time.Now().UTC()
		snapshot.Tampering.LastSeen = map[string]time.Time{}
		updateEstateMonitor(pce.URL+"/api/v2/orgs/1", workloads, nil, health, snapshot, configuredEstateOptions())
		statsMutex.Lock()
		currentStats = snapshot
		statsMutex.Unlock()
	}
	refresh()
	config.Estate, _ = normalizeEstateOptions(EstateOptions{})
	if err := writeJSONFileAtomic(configFileName, config); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", serveDashboard)
	mux.HandleFunc("/settings", serveSettings)
	mux.HandleFunc("/estate", serveEstate)
	mux.HandleFunc("/executive", serveExecutive)
	mux.HandleFunc("/api/stats", handleStats)
	mux.HandleFunc("/api/version", handleDashboardVersion)
	mux.HandleFunc("/api/config/estate", withTrustedOrigin("estate.fixture", handleConfigEstate))
	mux.HandleFunc("/api/config/targets", handleConfigTargets)
	mux.HandleFunc("/api/config/credentials", handleConfigCredentials)
	mux.HandleFunc("/api/estate", handleEstate)
	mux.HandleFunc("/api/estate/report", handleEstateReport)
	mux.HandleFunc("/api/estate/report/snapshot", withTrustedOrigin("api.estate.report.snapshot", handleEstateReportSnapshot))
	mux.HandleFunc("/api/estate/report/now", withTrustedOrigin("api.estate.report.now", handleEstateReportNow))
	mux.HandleFunc("/api/estate/report/runs", handleEstateReportRuns)
	mux.HandleFunc("/api/refresh", func(w http.ResponseWriter, r *http.Request) {
		refresh()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/static/product-shell.css", extractor.ServeProductShellCSS)
	mux.HandleFunc("/static/product-shell.js", extractor.ServeProductShellJS)
	staticFS, _ := fs.Sub(templateFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	server := httptest.NewServer(mux)
	defer server.Close()
	fmt.Printf("ESTATE_PREVIEW_URL=%s\n", server.URL)
	timer := time.NewTimer(45 * time.Minute)
	defer timer.Stop()
	<-timer.C
}
