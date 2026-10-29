package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const outboundIntegrationHelperEnv = "ILLUMIO_TEST_OUTBOUND_INTEGRATION_HELPER"

func runOutboundIntegrationHelper(t *testing.T, scenario string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOutboundIntegrationHelper$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), outboundIntegrationHelperEnv+"="+scenario)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("outbound integration helper scenario %q failed: %v\n%s", scenario, err, output)
	}
}

func TestAPICallRawWithClientEnforcesPCEOriginAcrossRedirects(t *testing.T) {
	runOutboundIntegrationHelper(t, "pce-redirects")
}

func TestConfigCredentialsRejectsUnauthorizedPCEOriginWithoutMutation(t *testing.T) {
	runOutboundIntegrationHelper(t, "credential-origin")
}

func TestConfigAlertsRequiresExactPrivateWebhookOriginWithoutMutation(t *testing.T) {
	runOutboundIntegrationHelper(t, "private-webhook-origin")
}

func TestOutboundIntegrationHelper(t *testing.T) {
	switch os.Getenv(outboundIntegrationHelperEnv) {
	case "":
		t.Skip("subprocess helper")
	case "pce-redirects":
		testAPICallRawWithClientRedirects(t)
	case "credential-origin":
		testUnauthorizedCredentialOrigin(t)
	case "private-webhook-origin":
		testPrivateWebhookOriginSettings(t)
	default:
		t.Fatalf("unknown outbound integration helper scenario %q", os.Getenv(outboundIntegrationHelperEnv))
	}
}

func installOutboundIntegrationConfig(t *testing.T, next Config) {
	t.Helper()
	configUpdateMu.Lock()
	configMutex.Lock()
	previous := cloneOutboundIntegrationConfig(config)
	previousModTime := configModTime
	config = cloneOutboundIntegrationConfig(next)
	configModTime = time.Time{}
	configMutex.Unlock()
	configUpdateMu.Unlock()
	t.Cleanup(func() {
		configUpdateMu.Lock()
		configMutex.Lock()
		config = previous
		configModTime = previousModTime
		configMutex.Unlock()
		configUpdateMu.Unlock()
	})
}

func cloneOutboundIntegrationConfig(value Config) Config {
	cloned := value
	cloned.PCEAllowedOrigins = append([]string(nil), value.PCEAllowedOrigins...)
	cloned.WebhookPrivateAllowedOrigins = append([]string(nil), value.WebhookPrivateAllowedOrigins...)
	cloned.TrafficTargets = append([]TrafficTarget(nil), value.TrafficTargets...)
	cloned.SourceExclusions = append([]TrafficTarget(nil), value.SourceExclusions...)
	cloned.TrafficServiceExclusions = append([]string(nil), value.TrafficServiceExclusions...)
	return cloned
}

func outboundIntegrationConfigSnapshot() Config {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return cloneOutboundIntegrationConfig(config)
}

func testAPICallRawWithClientRedirects(t *testing.T) {
	var foreignHits atomic.Int32
	var foreignAuthorization atomic.Value
	foreignAuthorization.Store("")
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		foreignAuthorization.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer foreign.Close()

	var sameOriginFinalHits atomic.Int32
	var sameOriginAuthorized atomic.Bool
	var pce *httptest.Server
	pce = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same-start":
			http.Redirect(w, r, "/same-finish", http.StatusTemporaryRedirect)
		case "/same-finish":
			sameOriginFinalHits.Add(1)
			username, password, ok := r.BasicAuth()
			sameOriginAuthorized.Store(ok && username == "dashboard-key" && password == "dashboard-secret")
			_, _ = w.Write([]byte("same-origin-ok"))
		case "/cross-start":
			http.Redirect(w, r, foreign.URL+"/capture", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	defer pce.Close()

	installOutboundIntegrationConfig(t, Config{
		PCEURL:    pce.URL,
		OrgID:     "1",
		APIKey:    "dashboard-key",
		APISecret: "dashboard-secret",
	})
	originalLimiter := apiRateLimiter
	apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
	t.Cleanup(func() { apiRateLimiter = originalLimiter })

	body, err := apiCallRawWithClient(pce.Client(), pce.URL+"/same-start", http.MethodGet, nil)
	if err != nil {
		t.Fatalf("same-origin redirect failed: %v", err)
	}
	if string(body) != "same-origin-ok" {
		t.Fatalf("same-origin response = %q", body)
	}
	if sameOriginFinalHits.Load() != 1 || !sameOriginAuthorized.Load() {
		t.Fatalf("same-origin redirect hits=%d authorized=%v", sameOriginFinalHits.Load(), sameOriginAuthorized.Load())
	}

	if _, err := apiCallRawWithClient(pce.Client(), pce.URL+"/cross-start", http.MethodGet, nil); err == nil {
		t.Fatal("cross-origin redirect unexpectedly succeeded")
	}
	if foreignHits.Load() != 0 {
		t.Fatalf("cross-origin redirect reached the foreign server %d time(s)", foreignHits.Load())
	}
	if got, _ := foreignAuthorization.Load().(string); got != "" {
		t.Fatalf("cross-origin redirect leaked Authorization %q", got)
	}
}

func testUnauthorizedCredentialOrigin(t *testing.T) {
	initial := Config{
		PCEURL:            "https://current-pce.internal:8443",
		PCEAllowedOrigins: []string{"https://approved-pce.internal:8443"},
		OrgID:             "7",
		APIKey:            "old-key",
		APISecret:         "old-secret",
	}
	installOutboundIntegrationConfig(t, initial)
	before := outboundIntegrationConfigSnapshot()
	body := `{"pce_url":"https://unauthorized-pce.internal:8443","org_id":"9","api_key":"new-key","api_secret":"new-secret"}`
	recorder := httptest.NewRecorder()
	handleConfigCredentials(recorder, httptest.NewRequest(http.MethodPut, "/api/config/credentials", bytes.NewBufferString(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("credential update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(strings.ToLower(recorder.Body.String()), "not authorized") {
		t.Fatalf("credential update error did not explain the origin rejection: %s", recorder.Body.String())
	}
	if after := outboundIntegrationConfigSnapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected credential update mutated config:\n before=%#v\n after=%#v", before, after)
	}
	if _, err := os.Stat(configFileName); !os.IsNotExist(err) {
		t.Fatalf("rejected credential update wrote %s (stat error=%v)", configFileName, err)
	}
}

func testPrivateWebhookOriginSettings(t *testing.T) {
	privateWebhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer privateWebhook.Close()
	webhookURL := privateWebhook.URL + "/hook"

	initial := Config{
		PCEURL:                       "https://current-pce.internal:8443",
		OrgID:                        "1",
		APIKey:                       "key",
		APISecret:                    "secret",
		WebhookURL:                   "https://previous.example/hook",
		WebhookEnabled:               true,
		WebhookProvider:              "slack",
		WebhookSlackChannel:          "old-channel",
		WebhookPrivateAllowedOrigins: nil,
	}
	installOutboundIntegrationConfig(t, initial)
	before := outboundIntegrationConfigSnapshot()
	payload := `{"webhook_enabled":true,"webhook_url":"` + webhookURL + `","webhook_provider":"teams","webhook_teams_title_prefix":"new-prefix","daily_summary_webhook_enabled":false,"daily_summary_webhook_url":""}`

	rejected := httptest.NewRecorder()
	handleConfigAlerts(rejected, httptest.NewRequest(http.MethodPut, "/api/config/alerts", bytes.NewBufferString(payload)))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("private webhook rejection status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if !strings.Contains(strings.ToLower(rejected.Body.String()), "not authorized") {
		t.Fatalf("private webhook rejection was not explicit: %s", rejected.Body.String())
	}
	if after := outboundIntegrationConfigSnapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected private webhook update mutated config:\n before=%#v\n after=%#v", before, after)
	}
	if _, err := os.Stat(configFileName); !os.IsNotExist(err) {
		t.Fatalf("rejected private webhook update wrote %s (stat error=%v)", configFileName, err)
	}

	configMutex.Lock()
	config.WebhookPrivateAllowedOrigins = []string{privateWebhook.URL}
	configMutex.Unlock()
	accepted := httptest.NewRecorder()
	handleConfigAlerts(accepted, httptest.NewRequest(http.MethodPut, "/api/config/alerts", bytes.NewBufferString(payload)))
	if accepted.Code != http.StatusOK {
		t.Fatalf("allowlisted private webhook status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	after := outboundIntegrationConfigSnapshot()
	if after.WebhookURL != webhookURL || !after.WebhookEnabled {
		t.Fatalf("allowlisted private webhook was not saved: url=%q enabled=%v", after.WebhookURL, after.WebhookEnabled)
	}
	if after.WebhookProvider != "teams" || after.WebhookTeamsTitlePrefix != "new-prefix" {
		t.Fatalf("allowlisted webhook settings were not saved: provider=%q prefix=%q", after.WebhookProvider, after.WebhookTeamsTitlePrefix)
	}
}
