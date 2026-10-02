package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func serviceFilterTestMap(t *testing.T, filter trafficServiceFilter) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(filter)
	if err != nil {
		t.Fatalf("marshal service filter: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode service filter: %v", err)
	}
	return out
}

func assertServiceFilterTestFields(t *testing.T, filter trafficServiceFilter, proto, port, toPort int) {
	t.Helper()
	got := serviceFilterTestMap(t, filter)
	if got["proto"] != float64(proto) {
		t.Fatalf("unexpected protocol: got %#v want %d (filter=%#v)", got["proto"], proto, got)
	}
	if port == 0 {
		if _, ok := got["port"]; ok {
			t.Fatalf("unexpected port in filter: %#v", got)
		}
	} else if got["port"] != float64(port) {
		t.Fatalf("unexpected port: got %#v want %d (filter=%#v)", got["port"], port, got)
	}
	if toPort == 0 {
		if _, ok := got["to_port"]; ok {
			t.Fatalf("unexpected to_port in filter: %#v", got)
		}
	} else if got["to_port"] != float64(toPort) {
		t.Fatalf("unexpected to_port: got %#v want %d (filter=%#v)", got["to_port"], toPort, got)
	}
}

func TestParseDirectServiceExclusion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		proto   int
		port    int
		toPort  int
		direct  bool
		wantErr bool
	}{
		{name: "canonical TCP", input: "TCP:9300", proto: 6, port: 9300, direct: true},
		{name: "case and whitespace", input: " udp : 53 ", proto: 17, port: 53, direct: true},
		{name: "range", input: "TCP:8000-8010", proto: 6, port: 8000, toPort: 8010, direct: true},
		{name: "port slash protocol", input: "9300/tcp", proto: 6, port: 9300, direct: true},
		{name: "port space protocol", input: "9300 tcp", proto: 6, port: 9300, direct: true},
		{name: "numeric protocol", input: "6:443", proto: 6, port: 443, direct: true},
		{name: "zero protocol", input: "0:443", direct: true, wantErr: true},
		{name: "PCE service name", input: "Approved Backup Service", direct: false},
		{name: "PCE service name with colon", input: "Application: Backend", direct: false},
		{name: "zero port", input: "TCP:0", direct: true, wantErr: true},
		{name: "reversed range", input: "TCP:9000-8000", direct: true, wantErr: true},
		{name: "invalid protocol", input: "NOT-A-PROTO:9300", direct: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, direct, err := parseDirectServiceExclusion(tt.input)
			if direct != tt.direct {
				t.Fatalf("direct=%v, want %v", direct, tt.direct)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tt.wantErr)
			}
			if err == nil && direct {
				assertServiceFilterTestFields(t, filter, tt.proto, tt.port, tt.toPort)
			}
		})
	}
}

func TestSanitizeServiceExclusionsAndTargets(t *testing.T) {
	input := []string{
		" tcp:9300 ",
		"TCP:9300",
		" Approved Backup Service ",
		"approved backup service",
		"UDP:53",
		"",
	}
	want := []string{"TCP:9300", "Approved Backup Service", "UDP:53"}
	if got := sanitizeServiceExclusions(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeServiceExclusions() = %#v, want %#v", got, want)
	}

	targets := sanitizeTargets([]TrafficTarget{{
		Name:              " Payments ",
		Kind:              "LABEL",
		ServiceExclusions: input,
	}})
	if len(targets) != 1 {
		t.Fatalf("sanitizeTargets() returned %d targets, want 1", len(targets))
	}
	if !reflect.DeepEqual(targets[0].ServiceExclusions, want) {
		t.Fatalf("target service exclusions = %#v, want %#v", targets[0].ServiceExclusions, want)
	}

	if err := validateServiceExclusions([]string{"TCP:9300", "Approved Backup Service"}); err != nil {
		t.Fatalf("valid service exclusions rejected: %v", err)
	}
	if err := validateServiceExclusions([]string{"TCP:70000"}); err == nil {
		t.Fatal("out-of-range direct service exclusion unexpectedly validated")
	}
}

func TestResolveTrafficServiceExclusionsDirectSelectorDoesNotLoadCatalog(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "catalog should not be requested", http.StatusInternalServerError)
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	filters, err := resolveTrafficServiceExclusions(server.URL+"/api/v2/orgs/1", []string{"TCP:9300"})
	if err != nil {
		t.Fatalf("resolve direct exclusion: %v", err)
	}
	if requests != 0 {
		t.Fatalf("direct selector caused %d service-catalog requests", requests)
	}
	if len(filters) != 1 {
		t.Fatalf("resolved %d direct filters, want 1", len(filters))
	}
	assertServiceFilterTestFields(t, filters[0], 6, 9300, 0)
}

func TestResolveTrafficServiceExclusionsExpandsNamedServices(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"href":"/orgs/1/sec_policy/active/services/1","name":"Approved Backup Service","process_name":"backupd","service_ports":[{"port":9301,"proto":6},{"port":9302,"to_port":9310,"proto":6}],"windows_services":[{"service_name":"BackupSvc","process_name":"backup.exe","port":9303,"proto":6}]},
			{"href":"/orgs/1/sec_policy/active/services/2","name":"Approved Backup Service Extra","service_ports":[{"port":9999,"proto":6}]}
		]`))
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	filters, err := resolveTrafficServiceExclusions(
		server.URL+"/api/v2/orgs/1",
		[]string{"TCP:9300", "APPROVED BACKUP SERVICE"},
	)
	if err != nil {
		t.Fatalf("resolveTrafficServiceExclusions() error: %v", err)
	}
	if requestedPath != "/api/v2/orgs/1/sec_policy/active/services" {
		t.Fatalf("service catalog path = %q", requestedPath)
	}
	if len(filters) != 5 {
		t.Fatalf("resolved %d filters, want 5: %#v", len(filters), filters)
	}
	assertServiceFilterTestFields(t, filters[0], 6, 9300, 0)
	if filters[1].ProcessName != "backupd" || filters[1].Port != 0 {
		t.Fatalf("named service process entry was not expanded correctly: %#v", filters[1])
	}
	assertServiceFilterTestFields(t, filters[2], 6, 9301, 0)
	assertServiceFilterTestFields(t, filters[3], 6, 9302, 9310)
	assertServiceFilterTestFields(t, filters[4], 6, 9303, 0)
	if filters[4].WindowsServiceName != "BackupSvc" || filters[4].ProcessName != "backup.exe" {
		t.Fatalf("named Windows service entry was not expanded correctly: %#v", filters[4])
	}
}

func TestResolveTrafficServiceExclusionsRejectsAmbiguousNamedService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"href":"/orgs/1/sec_policy/active/services/1","name":"Shared Service","service_ports":[{"port":443,"proto":6}]},
			{"href":"/orgs/1/sec_policy/active/services/2","name":"shared service","service_ports":[{"port":8443,"proto":6}]}
		]`))
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	_, err := resolveTrafficServiceExclusions(server.URL+"/api/v2/orgs/1", []string{"Shared Service"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "must be unique") {
		t.Fatalf("ambiguous named service error = %v, want unique-name validation", err)
	}
}

func TestResolveTrafficServiceExclusionsRejectsUnknownNamedService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"Known Service","service_ports":[{"port":443,"proto":6}]}]`))
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	_, err := resolveTrafficServiceExclusions(server.URL+"/api/v2/orgs/1", []string{"Missing Service"})
	if err == nil {
		t.Fatal("unknown PCE service unexpectedly resolved")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "missing service") {
		t.Fatalf("unknown-service error should name the unresolved selector, got %q", err)
	}
}

func TestResolveTrafficServiceExclusionsLoadsAsyncCatalogBeyondSyncLimit(t *testing.T) {
	catalog := make([]map[string]interface{}, 0, 501)
	for i := 0; i < 500; i++ {
		catalog = append(catalog, map[string]interface{}{
			"href":          "/orgs/1/sec_policy/active/services/" + strconv.Itoa(i),
			"name":          "Service " + strconv.Itoa(i),
			"service_ports": []map[string]interface{}{{"port": 1000 + i, "proto": 6}},
		})
	}
	catalog = append(catalog, map[string]interface{}{
		"href":          "/orgs/1/sec_policy/active/services/500",
		"name":          "Target Paginated Service",
		"service_ports": []map[string]interface{}{{"port": 9300, "proto": 6}},
	})
	resultDownloaded := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sec_policy/active/services"):
			if r.Header.Get("Prefer") != "respond-async" {
				t.Fatalf("service catalog request did not ask for an async response")
			}
			w.Header().Set("Location", server.URL+"/jobs/1")
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/jobs/1" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"href":   "/jobs/1",
				"status": "done",
				"result": map[string]interface{}{"href": server.URL + "/datafile/1"},
			})
		case r.URL.Path == "/jobs/1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/datafile/1":
			resultDownloaded = true
			_ = json.NewEncoder(w).Encode(catalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	filters, err := resolveTrafficServiceExclusions(server.URL+"/api/v2/orgs/1", []string{"Target Paginated Service"})
	if err != nil {
		t.Fatalf("resolve paginated named service: %v", err)
	}
	if !resultDownloaded {
		t.Fatal("service catalog did not download the async result")
	}
	if len(filters) != 1 {
		t.Fatalf("resolved filters = %#v, want one", filters)
	}
	assertServiceFilterTestFields(t, filters[0], 6, 9300, 0)
}

func TestAsyncTrafficQueryPayloadsIncludeServiceExclusions(t *testing.T) {
	var mu sync.Mutex
	payloads := make([]map[string]interface{}, 0, 3)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/traffic_flows/async_queries") {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			payloads = append(payloads, payload)
			jobID := len(payloads)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"href": server.URL + "/jobs/" + string(rune('0'+jobID))})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/download") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/jobs/") {
			_, _ = w.Write([]byte(`{"status":"completed","result_count":1}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	start := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	exclusions := []trafficServiceFilter{{Port: 9300, Proto: 6}}
	if _, err := performAsyncTrafficQueryWindowWithInclude(server.URL, nil, nil, exclusions, "count", start, end, false); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if _, err := performAsyncTrafficQueryWindowPortCountsWithInclude(server.URL, nil, nil, exclusions, "ports", start, end, false); err != nil {
		t.Fatalf("port query: %v", err)
	}
	if _, _, _, _, err := performAsyncTrafficQueryWindowCountPortsHostsAndSamplesWithInclude(server.URL, nil, true, nil, exclusions, "detailed", start, end, false); err != nil {
		t.Fatalf("detailed query: %v", err)
	}

	mu.Lock()
	gotPayloads := append([]map[string]interface{}(nil), payloads...)
	mu.Unlock()
	if len(gotPayloads) != 3 {
		t.Fatalf("captured %d async query payloads, want 3", len(gotPayloads))
	}
	for i, payload := range gotPayloads {
		services, ok := payload["services"].(map[string]interface{})
		if !ok {
			t.Fatalf("payload %d missing services object: %#v", i, payload)
		}
		exclude, ok := services["exclude"].([]interface{})
		if !ok || len(exclude) != 1 {
			t.Fatalf("payload %d services.exclude = %#v, want one filter", i, services["exclude"])
		}
		filter, ok := exclude[0].(map[string]interface{})
		if !ok || filter["proto"] != float64(6) || filter["port"] != float64(9300) {
			t.Fatalf("payload %d unexpected service exclusion: %#v", i, exclude[0])
		}
	}
}

func TestConfigTargetsGETSerializesServiceExclusions(t *testing.T) {
	configMutex.Lock()
	originalConfig := config
	config = Config{
		TrafficServiceExclusions: []string{"TCP:9300", "Approved Backup Service"},
		TrafficTargets: []TrafficTarget{{
			Name:              "Payments",
			Kind:              "label",
			ServiceExclusions: []string{"UDP:53"},
		}},
	}
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
	}()

	recorder := httptest.NewRecorder()
	handleConfigTargets(recorder, httptest.NewRequest(http.MethodGet, "/api/config/targets", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET settings status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		TrafficServiceExclusions []string        `json:"traffic_service_exclusions"`
		TrafficTargets           []TrafficTarget `json:"traffic_targets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET settings response: %v", err)
	}
	if !reflect.DeepEqual(response.TrafficServiceExclusions, []string{"TCP:9300", "Approved Backup Service"}) {
		t.Fatalf("global exclusions = %#v", response.TrafficServiceExclusions)
	}
	if len(response.TrafficTargets) != 1 || !reflect.DeepEqual(response.TrafficTargets[0].ServiceExclusions, []string{"UDP:53"}) {
		t.Fatalf("target exclusions missing from settings response: %#v", response.TrafficTargets)
	}
}

func TestConfigTargetsPUTRejectsUnknownNamedServiceWithoutMutation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"href":"/orgs/1/sec_policy/active/services/1","name":"Known Service","service_ports":[{"port":443,"proto":6}]}]`))
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	configMutex.Lock()
	originalConfig := config
	config = Config{
		PCEURL: server.URL,
		OrgID:  "1",
		TrafficTargets: []TrafficTarget{{
			Name: "Payments",
			Kind: "label",
		}},
	}
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
	}()

	body := []byte(`{"traffic_targets":[{"name":"Payments","kind":"label","service_exclusions":["Missing Service"]}]}`)
	recorder := httptest.NewRecorder()
	handleConfigTargets(recorder, httptest.NewRequest(http.MethodPut, "/api/config/targets", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
	}
	configMutex.RLock()
	saved := append([]TrafficTarget(nil), config.TrafficTargets...)
	configMutex.RUnlock()
	if len(saved) != 1 || len(saved[0].ServiceExclusions) != 0 {
		t.Fatalf("rejected settings request mutated config: %#v", saved)
	}
}

func TestValidateUniqueTrafficTargetNamesIsCaseInsensitive(t *testing.T) {
	err := validateUniqueTrafficTargetNames([]TrafficTarget{
		{Name: "Payments", Kind: "label"},
		{Name: " payments ", Kind: "label_group"},
	})
	if err == nil {
		t.Fatal("duplicate target names unexpectedly validated")
	}
}

func TestBlockedHistoryReconcileFingerprintIncludesEffectiveServiceExclusions(t *testing.T) {
	target := TrafficTarget{Name: "Payments", Kind: "label"}

	configMutex.Lock()
	originalConfig := config
	config.TrafficServiceExclusions = []string{"TCP:9300"}
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
	}()

	first := blockedHistoryReconcileFingerprint([]TrafficTarget{target})
	configMutex.Lock()
	config.TrafficServiceExclusions = []string{"TCP:9301"}
	configMutex.Unlock()
	second := blockedHistoryReconcileFingerprint([]TrafficTarget{target})
	if first == second {
		t.Fatal("reconcile fingerprint did not change with global service exclusions")
	}

	configMutex.Lock()
	config.TrafficServiceExclusions = []string{"TCP:9300"}
	configMutex.Unlock()
	target.ServiceExclusions = []string{"UDP:53"}
	third := blockedHistoryReconcileFingerprint([]TrafficTarget{target})
	if first == third {
		t.Fatal("reconcile fingerprint did not change with per-target service exclusions")
	}

	target.ServiceExclusions = []string{"udp:53", "TCP:9300"}
	ordered := blockedHistoryReconcileFingerprint([]TrafficTarget{target})
	target.ServiceExclusions = []string{"tcp:9300", "UDP:53"}
	reordered := blockedHistoryReconcileFingerprint([]TrafficTarget{target})
	if ordered != reordered {
		t.Fatal("reconcile fingerprint should be insensitive to selector order and case")
	}
}

func TestRollingServiceFilterFingerprintMigrationPreservesUnfilteredHistory(t *testing.T) {
	target := TrafficTarget{Name: "Payments", Kind: "label"}

	configMutex.Lock()
	originalConfig := config
	config.TrafficServiceExclusions = nil
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
	}()

	rollingMu.Lock()
	originalRolling := rollingCache
	rollingCache = rollingState{
		Initialized:               true,
		BaselineBlocked:           map[string]targetBaseline{"Payments": {Count: 42, CapturedUTC: time.Now().UTC()}},
		ServiceFilterFingerprints: nil,
		Buckets: []rollingBucket{{
			EndUTC:          time.Now().UTC(),
			BlockedByTarget: map[string]int{"Payments": 3},
		}},
		BlockedFlowLastSeen: map[string]map[string]blockedFlowSeenState{},
	}
	rollingMu.Unlock()
	defer func() {
		rollingMu.Lock()
		rollingCache = originalRolling
		rollingMu.Unlock()
	}()

	if changed := trafficTargetsWithRollingServiceFilterChanges([]TrafficTarget{target}); len(changed) != 0 {
		t.Fatalf("empty-exclusion fingerprint migration marked target changed: %#v", changed)
	}
	rollingMu.Lock()
	_, baselinePreserved := rollingCache.BaselineBlocked["Payments"]
	_, fingerprintMigrated := rollingCache.ServiceFilterFingerprints["Payments"]
	rollingMu.Unlock()
	if !baselinePreserved || !fingerprintMigrated {
		t.Fatalf("migration did not preserve baseline and add fingerprint: baseline=%v fingerprint=%v", baselinePreserved, fingerprintMigrated)
	}

	configMutex.Lock()
	config.TrafficServiceExclusions = []string{"TCP:9300"}
	configMutex.Unlock()
	rollingMu.Lock()
	rollingCache.ServiceFilterFingerprints = nil
	rollingMu.Unlock()
	if changed := trafficTargetsWithRollingServiceFilterChanges([]TrafficTarget{target}); len(changed) != 1 {
		t.Fatalf("new nonempty exclusion should invalidate unversioned rolling data, got %#v", changed)
	}
}

func TestResolvedServiceFingerprintChangesWhenNamedPolicyObjectChanges(t *testing.T) {
	port := 9300
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{{
			"href": "/orgs/1/sec_policy/active/services/1",
			"name": "Customer Suppression",
			"service_ports": []map[string]interface{}{{
				"port":  port,
				"proto": 6,
			}},
		}})
	}))
	defer server.Close()

	originalClient := httpClient
	originalLimiter := apiRateLimiter
	httpClient = server.Client()
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	defer func() {
		httpClient = originalClient
		apiRateLimiter = originalLimiter
	}()

	baseURL := server.URL + "/api/v2/orgs/1"
	target := TrafficTarget{Name: "Payments", Kind: "label", effectiveServiceExclusions: []string{"Customer Suppression"}}
	first := prepareTrafficTargetServiceExclusions(baseURL, target)
	if first.serviceResolutionError != "" {
		t.Fatalf("first resolution: %s", first.serviceResolutionError)
	}
	port = 9301
	invalidateTrafficServiceCatalog(baseURL)
	second := prepareTrafficTargetServiceExclusions(baseURL, target)
	if second.serviceResolutionError != "" {
		t.Fatalf("second resolution: %s", second.serviceResolutionError)
	}
	if targetServiceExclusionFingerprint(first) == targetServiceExclusionFingerprint(second) {
		t.Fatal("resolved fingerprint did not change after the PCE service definition changed")
	}
}

func TestPendingServiceHistoryIsHiddenUntilReconciled(t *testing.T) {
	target := prepareTrafficTargetServiceExclusions("", TrafficTarget{
		Name:                       "Payments",
		Kind:                       "label",
		effectiveServiceExclusions: []string{"TCP:9300"},
	})
	fingerprint := targetServiceExclusionFingerprint(target)
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	rollingMu.Lock()
	originalRolling := rollingCache
	rollingCache = rollingState{ServiceHistoryPending: map[string]string{"Payments": fingerprint}}
	rollingMu.Unlock()
	historyMu.Lock()
	originalDaily := blockedDaily
	originalPorts := blockedPortsDaily
	originalHosts := blockedHostsDaily
	blockedDaily = map[string]map[string]int{day: {"Payments": 42}}
	blockedPortsDaily = map[string]map[string]map[string]int{day: {"Payments": {"TCP:9300": 42}}}
	blockedHostsDaily = map[string]map[string]map[string]hostTrafficCount{day: {"Payments": {"host-a": {Inbound: 42}}}}
	historyMu.Unlock()
	defer func() {
		rollingMu.Lock()
		rollingCache = originalRolling
		rollingMu.Unlock()
		historyMu.Lock()
		blockedDaily = originalDaily
		blockedPortsDaily = originalPorts
		blockedHostsDaily = originalHosts
		historyMu.Unlock()
	}()

	if got := blockedDailyTrendSeries("Payments", 30); len(got) != 0 {
		t.Fatalf("pending daily history was exposed: %#v", got)
	}
	if got := blockedPortDailySeries("Payments", 30); len(got) != 0 {
		t.Fatalf("pending port history was exposed: %#v", got)
	}
	if got := blockedHostDailySeries("Payments", 30); len(got) != 0 {
		t.Fatalf("pending host history was exposed: %#v", got)
	}

	rollingMu.Lock()
	delete(rollingCache.ServiceHistoryPending, "Payments")
	rollingMu.Unlock()
	if got := blockedDailyTrendSeries("Payments", 30); len(got) == 0 {
		t.Fatal("reconciled daily history remained hidden")
	}
}

func TestServiceFilterInvalidationDropsDisabledPortHistory(t *testing.T) {
	portHistoryEnabled := false
	configMutex.Lock()
	originalConfig := config
	config.BlockedPortDailyEnabled = &portHistoryEnabled
	config.BlockedPortStoreBackend = "json"
	configMutex.Unlock()
	originalDataDir := dataDir
	dataDir = t.TempDir()

	target := prepareTrafficTargetServiceExclusions("", TrafficTarget{
		Name:                       "Payments",
		Kind:                       "label",
		effectiveServiceExclusions: []string{"TCP:9300"},
	})
	rollingMu.Lock()
	originalRolling := rollingCache
	rollingCache = rollingState{
		BaselineBlocked:           map[string]targetBaseline{"Payments": {Count: 10}},
		BlockedFlowLastSeen:       map[string]map[string]blockedFlowSeenState{},
		ServiceFilterFingerprints: map[string]string{},
		ServiceHistoryPending:     map[string]string{},
	}
	rollingMu.Unlock()
	historyMu.Lock()
	originalDaily5m := blockedDaily5mCaptured
	originalPorts := blockedPortsDaily
	originalHosts := blockedHostsDaily
	blockedDaily5mCaptured = map[string]map[string]int{"2026-09-30": {"Payments": 10}}
	blockedPortsDaily = map[string]map[string]map[string]int{"2026-09-30": {"Payments": {"TCP:9300": 10}}}
	blockedHostsDaily = map[string]map[string]map[string]hostTrafficCount{}
	historyMu.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
		dataDir = originalDataDir
		rollingMu.Lock()
		rollingCache = originalRolling
		rollingMu.Unlock()
		historyMu.Lock()
		blockedDaily5mCaptured = originalDaily5m
		blockedPortsDaily = originalPorts
		blockedHostsDaily = originalHosts
		historyMu.Unlock()
	}()

	invalidateTrafficServiceFilterTargets([]TrafficTarget{target})
	historyMu.Lock()
	_, stillPresent := blockedPortsDaily["2026-09-30"]["Payments"]
	historyMu.Unlock()
	if stillPresent {
		t.Fatal("disabled port history survived a service-filter invalidation")
	}
	if !trafficServiceHistoryPending("Payments") {
		t.Fatal("service history was not marked pending after invalidation")
	}
}

func TestSupersededCollectorResultsAreDiscardedBeforeCommitAndPublish(t *testing.T) {
	configMutex.Lock()
	originalConfig := config
	config.TrafficServiceExclusions = nil
	config.TrafficTargets = []TrafficTarget{{Name: "Payments", Kind: "label", ServiceExclusions: []string{"TCP:9301"}}}
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
	}()

	snapshot := freezeTrafficTargetServiceExclusionsFrom(nil, []TrafficTarget{{
		Name:              "Payments",
		Kind:              "label",
		ServiceExclusions: []string{"TCP:9300"},
	}})[0]
	results := []BlockedTargetResult{{
		Name:   "Payments",
		Kind:   "label",
		Count:  42,
		Status: FetchStatus{Success: true},
	}}
	current := map[string]int{"Payments": 42}
	ports := map[string]map[string]int{"Payments": {"TCP:443": 42}}
	hosts := map[string]map[string]hostTrafficCount{"Payments": {"host-a": {Inbound: 42}}}
	baseline := map[string]int{"Payments": 42}

	configUpdateMu.Lock()
	successCount, warnings := discardSupersededBlockedCycleResults(
		[]TrafficTarget{snapshot}, results, current, ports, hosts, baseline, 1, nil,
	)
	configUpdateMu.Unlock()

	if successCount != 0 || len(warnings) != 1 {
		t.Fatalf("unexpected discard summary: success=%d warnings=%#v", successCount, warnings)
	}
	if results[0].Status.Success || results[0].Count != 0 {
		t.Fatalf("superseded result remained successful: %#v", results[0])
	}
	if len(current) != 0 || len(ports) != 0 || len(hosts) != 0 || len(baseline) != 0 {
		t.Fatalf("superseded state survived: current=%#v ports=%#v hosts=%#v baseline=%#v", current, ports, hosts, baseline)
	}

	stats := DashboardStats{trafficServiceTargetSnapshots: []TrafficTarget{snapshot}}
	stats.Blocked.Targets = append(stats.Blocked.Targets, BlockedTargetResult{
		Name:      "Payments",
		Count:     42,
		Anomalous: true,
		Status:    FetchStatus{Success: true},
	})
	stats.Blocked.Status = FetchStatus{Success: true}
	if dashboardTrafficServiceFiltersCurrent(stats) {
		t.Fatal("superseded dashboard snapshot was accepted for publication")
	}
	markDashboardTrafficServiceFiltersStale(&stats)
	if stats.Blocked.Status.Success || stats.Blocked.Targets[0].Status.Success || stats.Blocked.Targets[0].Anomalous {
		t.Fatalf("stale dashboard data remained publishable or alertable: %#v", stats.Blocked)
	}
}

func TestProcessWebhookAlertsSkipsFailedTargets(t *testing.T) {
	configMutex.Lock()
	originalConfig := config
	config.WebhookEnabled = false
	config.WebhookURL = ""
	config.BlockedPortStoreBackend = "json"
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config = originalConfig
		configMutex.Unlock()
	}()

	alertMu.Lock()
	originalAlertState := alertState
	alertState = persistedAlertState{
		SchemaVersion: 1,
		Targets: map[string]alertTargetState{
			"payments": {Active: true, LastEventUTC: time.Now().Add(-time.Hour), LastEventType: "triggered"},
		},
		Metrics: map[string]alertTargetState{},
	}
	alertMu.Unlock()
	defer func() {
		alertMu.Lock()
		alertState = originalAlertState
		alertMu.Unlock()
	}()

	originalDataDir := dataDir
	dataDir = t.TempDir()
	defer func() { dataDir = originalDataDir }()

	stats := DashboardStats{}
	stats.Blocked.Targets = []BlockedTargetResult{{
		Name:      "Payments",
		Kind:      "label",
		Anomalous: false,
		Status:    FetchStatus{Success: false, Error: "PCE query failed"},
	}}
	processWebhookAlerts(stats)

	alertMu.Lock()
	state := alertState.Targets["payments"]
	alertMu.Unlock()
	if !state.Active || state.LastEventType != "triggered" {
		t.Fatalf("failed target incorrectly resolved active alert: %#v", state)
	}
}
