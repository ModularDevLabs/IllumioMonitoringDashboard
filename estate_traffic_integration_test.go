package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Daily reconciliation touches legacy process-wide caches. Exercise the real
// hooks in a subprocess and a synthetic PCE, never the user's configuration.
func TestEstateTrafficDailyHooksIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEstateTrafficDailyHooksHelper$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "ILLUMIO_ESTATE_TRAFFIC_HOOKS_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("verified daily traffic hooks: %v\n%s", err, output)
	}
}

func TestEstateTrafficDailyHooksHelper(t *testing.T) {
	if os.Getenv("ILLUMIO_ESTATE_TRAFFIC_HOOKS_HELPER") != "1" {
		return
	}
	for _, scenario := range []struct {
		name, method                  string
		ports, stale, missing, capped bool
		wantVerified                  bool
	}{
		{name: "count", method: "daily", wantVerified: true},
		{name: "combined", method: "daily", ports: true, wantVerified: true},
		{name: "previous-day", method: "previous", wantVerified: true},
		{name: "full-reconcile", method: "full", wantVerified: true},
		{name: "stale-source-after-dispatch", method: "daily", stale: true},
		{name: "missing-origin-proof", method: "daily", missing: true},
		{name: "truncated-query", method: "daily", capped: true},
		{name: "partial-day-after-midnight", method: "current"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dataDir = t.TempDir()
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/traffic_flows/async_queries"):
					if scenario.stale {
						configUpdateMu.Lock()
						configMutex.Lock()
						config.SourceExclusions = []TrafficTarget{{Kind: "label", Name: "new-source-exclusion"}}
						configMutex.Unlock()
						configUpdateMu.Unlock()
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"href": server.URL + "/jobs/1"})
				case r.URL.Path == "/jobs/1/download":
					_, _ = w.Write([]byte(`[]`))
				case r.URL.Path == "/jobs/1":
					count := 7
					if scenario.capped {
						count = trafficQueryMaxResults
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "result_count": count})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			target := TrafficTarget{Name: "All traffic", Kind: "all"}
			config = Config{PCEURL: server.URL, OrgID: "1", APIKey: "fixture", APISecret: "fixture", Timezone: "UTC", TrafficTargets: []TrafficTarget{target}, BlockedPortDailyEnabled: &scenario.ports, Estate: EstateOptions{WeeklyReportEnabled: true}}
			httpClient = server.Client()
			apiRateLimiter = newAPIRateController(defaultAPIMaxRPM, minimumAPIRPMOnThrottle, defaultAPIBurst)
			blockedDaily = map[string]map[string]int{}
			blockedPortsDaily = map[string]map[string]map[string]int{}
			reconcileDayKey = ""
			reconcileDoneByT = map[string]bool{}
			reconcileSummarySentByD = map[string]bool{}
			provenance := map[string]string{trafficTargetIdentity(target): estateVerifiedTrafficFingerprint(target)}
			if scenario.missing {
				provenance = nil
			}
			baseURL := server.URL + "/api/v2/orgs/1"
			today := localDayStart(time.Now(), time.UTC)
			day := today.AddDate(0, 0, -1).Format("2006-01-02")
			switch scenario.method {
			case "daily":
				collectDailyBlockedHistory(baseURL, []TrafficTarget{target}, today.Add(time.Hour), nil, provenance)
			case "previous":
				reconcilePreviousDayBlockedHistory(baseURL, []TrafficTarget{target}, today.Add(time.Hour), nil, provenance)
			case "full":
				blockedDaily[day] = map[string]int{target.Name: 1000}
				reconcileAllStoredBlockedHistory(baseURL, []TrafficTarget{target}, today.Add(time.Hour), nil, provenance)
			case "current":
				// Simulate a midday query whose result is consumed after midnight.
				// The day is now completed, but the queried window never was.
				reconcileCurrentDayBlockedHistory(baseURL, []TrafficTarget{target}, today.Add(-12*time.Hour), nil, provenance)
			}
			store := estateReadVerifiedTraffic(estateScopeForConfig(), "UTC")
			record, exists := store.Records[estateVerifiedTrafficKey(day, target.Name)]
			if exists != scenario.wantVerified {
				t.Fatalf("verified=%v want=%v; records=%+v", exists, scenario.wantVerified, store.Records)
			}
			if exists && record.Count != 7 {
				t.Fatalf("count=%d want=7", record.Count)
			}
			if _, exists := blockedDaily[day][target.Name]; !exists {
				t.Fatal("weekly provenance changed legacy daily query behavior")
			}
		})
	}
}
