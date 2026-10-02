package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const configAtomicityHelperEnv = "ILLUMIO_TEST_CONFIG_ATOMICITY_HELPER"

func runConfigAtomicityHelper(t *testing.T, scenario string, files map[string]string) string {
	t.Helper()
	tempDir := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(tempDir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write helper file %s: %v", name, err)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigAtomicityHelper$")
	cmd.Dir = tempDir
	cmd.Env = append(os.Environ(), configAtomicityHelperEnv+"="+scenario)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config helper scenario %q failed: %v\n%s", scenario, err, output)
	}
	return string(output)
}

func TestDirectConfigLoadRejectsCaseInsensitiveDuplicateTrafficTargetNames(t *testing.T) {
	runConfigAtomicityHelper(t, "duplicate-target-load", map[string]string{
		configFileName: `{
  "pce_url": "https://pce.example",
  "org_id": "1",
  "api_key": "api_key",
  "api_secret": "api_secret",
  "traffic_targets": [
    {"name": "Payments", "kind": "label"},
    {"name": " payments ", "kind": "label_group"}
  ]
}`,
	})
}

func TestCredentialUpdateIsAtomicAndInvalidatesServiceCatalogs(t *testing.T) {
	runConfigAtomicityHelper(t, "credential-update", nil)
}

func TestConfigAtomicityHelper(t *testing.T) {
	switch os.Getenv(configAtomicityHelperEnv) {
	case "":
		t.Skip("subprocess helper")
	case "duplicate-target-load":
		if _, _, ok := loadConfigFile(); ok {
			t.Fatal("loadConfigFile accepted case-insensitive duplicate traffic target names")
		}
	case "credential-update":
		runCredentialUpdateAtomicityHelper(t)
	default:
		t.Fatalf("unknown helper scenario %q", os.Getenv(configAtomicityHelperEnv))
	}
}

func runCredentialUpdateAtomicityHelper(t *testing.T) {
	const (
		oldPCE  = "https://old-pce.example"
		newPCE  = "https://new-pce.example"
		oldBase = oldPCE + "/api/v2/orgs/1"
		newBase = newPCE + "/api/v2/orgs/2"
	)

	configMutex.Lock()
	config = Config{
		PCEURL:            oldPCE,
		PCEAllowedOrigins: []string{newPCE},
		OrgID:             "1",
		APIKey:            "old-key",
		APISecret:         "old-secret",
	}
	configMutex.Unlock()

	trafficServiceCatalogMu.Lock()
	trafficServiceCatalog = map[string]trafficServiceCatalogCacheEntry{
		oldBase: {ExpiresAt: time.Now().Add(time.Hour)},
		newBase: {ExpiresAt: time.Now().Add(time.Hour)},
	}
	trafficServiceCatalogFlights = map[string]*trafficServiceCatalogFlight{}
	trafficServiceCatalogMu.Unlock()

	configUpdateMu.Lock()
	started := make(chan struct{})
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		close(started)
		body := fmt.Sprintf(`{"pce_url":%q,"org_id":"2","api_key":"new-key","api_secret":"new-secret"}`, newPCE)
		recorder := httptest.NewRecorder()
		handleConfigCredentials(recorder, httptest.NewRequest(http.MethodPut, "/api/config/credentials", bytes.NewBufferString(body)))
		finished <- recorder
	}()
	<-started

	select {
	case recorder := <-finished:
		configUpdateMu.Unlock()
		t.Fatalf("credential update bypassed configUpdateMu (status=%d body=%s)", recorder.Code, recorder.Body.String())
	case <-time.After(100 * time.Millisecond):
	}

	configMutex.RLock()
	pceURLWhileLocked := config.PCEURL
	configMutex.RUnlock()
	if pceURLWhileLocked != oldPCE {
		configUpdateMu.Unlock()
		t.Fatalf("credentials changed while configUpdateMu was held: got %q want %q", pceURLWhileLocked, oldPCE)
	}

	configUpdateMu.Unlock()
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("credential update did not complete after configUpdateMu was released")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("credential update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"saved":true`) {
		t.Fatalf("credential update response did not report success: %s", recorder.Body.String())
	}

	configMutex.RLock()
	gotPCE, gotOrg, gotKey, gotSecret := config.PCEURL, config.OrgID, config.APIKey, config.APISecret
	configMutex.RUnlock()
	if gotPCE != newPCE || gotOrg != "2" || gotKey != "new-key" || gotSecret != "new-secret" {
		t.Fatalf("credential update was not committed atomically: pce=%q org=%q key=%q secret=%q", gotPCE, gotOrg, gotKey, gotSecret)
	}

	trafficServiceCatalogMu.Lock()
	_, oldCached := trafficServiceCatalog[oldBase]
	_, newCached := trafficServiceCatalog[newBase]
	trafficServiceCatalogMu.Unlock()
	if oldCached || newCached {
		t.Fatalf("credential update left stale service catalogs cached: old=%v new=%v", oldCached, newCached)
	}

	trafficServiceCatalogMu.Lock()
	trafficServiceCatalog[newBase] = trafficServiceCatalogCacheEntry{ExpiresAt: time.Now().Add(time.Hour)}
	trafficServiceCatalogMu.Unlock()
	rotateRecorder := httptest.NewRecorder()
	handleConfigCredentials(
		rotateRecorder,
		httptest.NewRequest(http.MethodPut, "/api/config/credentials", bytes.NewBufferString(`{"api_key":"rotated-key"}`)),
	)
	if rotateRecorder.Code != http.StatusOK {
		t.Fatalf("same-PCE credential rotation status=%d body=%s", rotateRecorder.Code, rotateRecorder.Body.String())
	}
	trafficServiceCatalogMu.Lock()
	_, rotatedCredentialCachePresent := trafficServiceCatalog[newBase]
	trafficServiceCatalogMu.Unlock()
	if rotatedCredentialCachePresent {
		t.Fatal("same-PCE credential rotation left the service catalog cached")
	}
	configMutex.RLock()
	rotatedKey := config.APIKey
	configMutex.RUnlock()
	if rotatedKey != "rotated-key" {
		t.Fatalf("same-PCE credential rotation saved api_key=%q", rotatedKey)
	}
}
