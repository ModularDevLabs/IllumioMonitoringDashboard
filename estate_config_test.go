package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEstateOptionsAreOptIn(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"pce_url":"https://pce.example","org_id":"1"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	o, err := normalizeEstateOptions(cfg.Estate)
	if err != nil {
		t.Fatal(err)
	}
	if o.Enabled() || o.WeeklyScheduleEnabled || o.WeeklyWebhookEnabled {
		t.Fatalf("legacy config enabled estate features: %+v", o)
	}
	if o.HistoryDays != 90 || o.WeeklyDay != "monday" || o.WeeklyTime != "09:00" {
		t.Fatalf("wrong defaults: %+v", o)
	}
}

func TestEstateOptionsValidation(t *testing.T) {
	for name, o := range map[string]EstateOptions{
		"schedule without report":  {WeeklyScheduleEnabled: true},
		"delivery without consent": {WeeklyWebhookEnabled: true, WeeklyWebhookURL: "https://example.com/hook"},
		"weekday":                  {WeeklyDay: "not-a-day"},
		"time":                     {WeeklyTime: "25:90"},
		"section":                  {WeeklySections: []string{"secrets"}},
		"negative retention":       {HistoryDays: -1},
		"embedded credentials":     {WeeklyWebhookURL: "https://user:password@example.com/hook"},
		"fragment":                 {WeeklyWebhookURL: "https://example.com/hook#secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeEstateOptions(o); err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
	o, err := normalizeEstateOptions(EstateOptions{RequiredLabelTypes: []string{" App ", "BU", "app", ""}, WeeklySections: []string{"estate", "estate", "health"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.RequiredLabelTypes, []string{"app", "bu"}) || !reflect.DeepEqual(o.WeeklySections, []string{"estate", "health"}) {
		t.Fatalf("normalization: %+v", o)
	}
}

func TestEstateConfigAPIIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEstateConfigHelper$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "ILLUMIO_ESTATE_CONFIG_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config regression: %v\n%s", err, output)
	}
}

func TestEstateConfigHelper(t *testing.T) {
	if os.Getenv("ILLUMIO_ESTATE_CONFIG_HELPER") != "1" {
		return
	}
	dataDir = t.TempDir()
	config = Config{PCEURL: "https://example.com", OrgID: "1", APIKey: "dashboard-only", APISecret: "do-not-export", Timezone: "UTC", Estate: EstateOptions{WeeklyWebhookURL: "https://example.com/secret-hook"}}
	get := httptest.NewRecorder()
	handleConfigEstate(get, httptest.NewRequest(http.MethodGet, "/api/config/estate", nil))
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), "secret-hook") || !strings.Contains(get.Body.String(), `"weekly_webhook_configured":true`) {
		t.Fatalf("config secret leak/status: %s", get.Body.String())
	}
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleConfigEstate(w, httptest.NewRequest(http.MethodPut, "/api/config/estate", bytes.NewBufferString(body)))
		return w
	}
	if w := put(`{"inventory_enabled":true,"ven_versions_enabled":true}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if config.APIKey != "dashboard-only" || config.APISecret != "do-not-export" || config.Estate.WeeklyWebhookURL != "https://example.com/secret-hook" {
		t.Fatal("estate settings changed credentials or cleared saved webhook")
	}
	estateMonitor.Lock()
	estateMonitor.history.Baseline = []EstateWorkload{{ID: "stable-baseline"}}
	estateMonitor.history.BaselineAt = time.Now()
	estateMonitor.Unlock()
	if w := put(`{"inventory_enabled":true,"ven_versions_enabled":true,"weekly_time":"10:30"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	estateMonitor.RLock()
	baselineKept := len(estateMonitor.history.Baseline) == 1 && !estateMonitor.history.BaselineAt.IsZero()
	estateMonitor.RUnlock()
	if !baselineKept {
		t.Fatal("delivery-only settings change discarded continuous inventory baseline")
	}
	before := config
	if w := put(`{"weekly_schedule_enabled":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid schedule accepted: %s", w.Body.String())
	}
	if !reflect.DeepEqual(config, before) {
		t.Fatal("rejected settings changed memory")
	}
	if w := put(`{"inventory_enabled":true} {}`); w.Code != http.StatusBadRequest {
		t.Fatal("multiple JSON bodies accepted")
	}
	if w := put(`{"unknown_option":true}`); w.Code != http.StatusBadRequest {
		t.Fatal("unknown options accepted")
	}
	if w := put(`{"weekly_webhook_url":""}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if config.Estate.Enabled() || config.Estate.WeeklyWebhookURL != "" {
		t.Fatal("explicit disable/clear did not take effect")
	}
	file, err := os.ReadFile(configFileName)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(file, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Estate.Enabled() || persisted.APISecret != "do-not-export" {
		t.Fatal("saved config did not preserve existing settings")
	}
	info, _ := os.Stat(configFileName)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config permissions expose secrets")
	}
	if err := os.Rename(configFileName, "saved-config.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(configFileName, 0700); err != nil {
		t.Fatal(err)
	}
	before = config
	if w := put(`{"inventory_enabled":true}`); w.Code != http.StatusInternalServerError {
		t.Fatal("failed persistence was reported as saved")
	}
	if !reflect.DeepEqual(config, before) {
		t.Fatal("failed persistence changed active settings")
	}
}

func TestEstateWorkloadRepeatedPageIsPartial(t *testing.T) {
	configMutex.RLock()
	oldConfig := config
	configMutex.RUnlock()
	t.Cleanup(func() { configMutex.Lock(); config = oldConfig; configMutex.Unlock() })
	rows := make([]map[string]interface{}, 500)
	for i := range rows {
		rows[i] = map[string]interface{}{"href": fmt.Sprintf("/workloads/%d", i)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("max_results") == "200000" {
			_ = json.NewEncoder(w).Encode([]interface{}{})
			return
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	configMutex.Lock()
	config = Config{PCEURL: server.URL, OrgID: "1"}
	configMutex.Unlock()
	got, err := getAllWorkloads(server.URL)
	if len(got) != 500 || err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Fatalf("repeated page claimed complete: count=%d err=%v", len(got), err)
	}
}
