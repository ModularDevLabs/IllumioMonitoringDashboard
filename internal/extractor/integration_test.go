package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegratedHandlerUsesNamespacedUIAndIndependentCredentials(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	handler, err := NewHandler(ctx, "dashboard-dev-test")
	if err != nil {
		t.Fatal(err)
	}

	pageRecorder := httptest.NewRecorder()
	pageRequest := httptest.NewRequest(http.MethodGet, "http://localhost:18443/", nil)
	pageRequest.RemoteAddr = "127.0.0.1:54321"
	handler.ServeHTTP(pageRecorder, pageRequest)
	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("extractor page status = %d", pageRecorder.Code)
	}
	page := pageRecorder.Body.String()
	for _, expected := range []string{
		`href="/blocked-traffic/summary"`,
		`fetch('/blocked-traffic/api/profiles/get')`,
		"stored separately from the Monitoring Dashboard credentials",
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("integrated extractor page is missing %q", expected)
		}
	}

	versionRecorder := httptest.NewRecorder()
	versionRequest := httptest.NewRequest(http.MethodGet, "http://localhost:18443/api/version", nil)
	versionRequest.RemoteAddr = "[::1]:54321"
	handler.ServeHTTP(versionRecorder, versionRequest)
	if versionRecorder.Code != http.StatusOK {
		t.Fatalf("extractor version status = %d", versionRecorder.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(versionRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] != "dashboard-dev-test" {
		t.Fatalf("extractor version = %q", payload["version"])
	}

	profilePath, err := getConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	wantProfilePath := filepath.Join(configRoot, "illumio-monitoring-dashboard-extractor", "pce_profiles.json")
	if profilePath != wantProfilePath {
		t.Fatalf("integrated profile path = %q, want %q", profilePath, wantProfilePath)
	}
}
