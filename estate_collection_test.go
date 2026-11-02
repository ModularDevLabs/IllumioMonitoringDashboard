package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func estateCollectionTestReader(t *testing.T, server *httptest.Server, ctx context.Context) *estatePCEReader {
	t.Helper()
	previous := apiRateLimiter
	apiRateLimiter = newAPIRateController(600, 60, 10000)
	t.Cleanup(func() { apiRateLimiter = previous })
	reader, err := newEstatePCEReader(ctx, server.URL, "dashboard-key", "dashboard-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func estateCollectionFixture(count int) []map[string]interface{} {
	rows := make([]map[string]interface{}, count)
	for i := range rows {
		rows[i] = map[string]interface{}{"href": fmt.Sprintf("/orgs/1/items/%d", i), "rules_count": 2}
	}
	return rows
}

func assertEstateCollectionRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if key, secret, ok := r.BasicAuth(); !ok || key != "dashboard-key" || secret != "dashboard-secret" {
		t.Error("estate request did not retain dashboard credentials")
	}
	if r.Method != http.MethodGet {
		t.Error("estate inventory must not mutate PCE policy")
	}
	for _, key := range []string{"skip", "offset", "max_results"} {
		if r.URL.Query().Has(key) {
			t.Errorf("collection request retained unsupported/capping parameter %q", key)
		}
	}
}

func TestEstateCollectionAsyncRetrievesMoreThanOneSyncPage(t *testing.T) {
	for _, resource := range []string{"sec_policy/active/rule_sets", "sec_policy/draft/rule_sets", "labels", "sec_policy"} {
		t.Run(resource, func(t *testing.T) {
			rows := estateCollectionFixture(1203)
			rows = append(rows, rows[0]) // Identity dedupe, not a lower completeness ceiling.
			var polls atomic.Int64
			var syncRequests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertEstateCollectionRequest(t, r)
				w.Header().Set("Retry-After", "0")
				switch r.URL.Path {
				case "/api/v2/orgs/1/" + resource:
					if r.Header.Get("Prefer") != "respond-async" {
						// Model a PCE that ignores offset and always returns page one.
						syncRequests.Add(1)
						_ = json.NewEncoder(w).Encode(rows[:500])
						return
					}
					w.Header().Set("Location", "/orgs/1/jobs/collection")
					w.WriteHeader(http.StatusAccepted)
				case "/api/v2/orgs/1/jobs/collection":
					if polls.Add(1) == 1 {
						_, _ = w.Write([]byte(`{"status":"running"}`))
					} else {
						_, _ = w.Write([]byte(`{"status":"done","result":{"href":"/orgs/1/datafiles/collection"}}`))
					}
				case "/api/v2/orgs/1/datafiles/collection":
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": rows})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			reader := estateCollectionTestReader(t, server, context.Background())
			got, err := reader.collection(server.URL + "/api/v2/orgs/1/" + resource + "?skip=500&offset=500&max_results=500")
			if err != nil || len(got) != 1203 || polls.Load() != 2 || syncRequests.Load() != 0 {
				t.Fatalf("complete async inventory: rows=%d polls=%d sync=%d err=%v", len(got), polls.Load(), syncRequests.Load(), err)
			}
		})
	}
}

func TestEstateCollectionSyncCompletenessAndUnsupportedAsync(t *testing.T) {
	for _, scenario := range []string{"small", "full", "full-with-duplicates", "unsupported-small", "unsupported-full"} {
		t.Run(scenario, func(t *testing.T) {
			rows := estateCollectionFixture(499)
			if strings.Contains(scenario, "full") {
				rows = estateCollectionFixture(500)
			}
			if scenario == "full-with-duplicates" {
				rows[499] = rows[0]
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertEstateCollectionRequest(t, r)
				requests.Add(1)
				if strings.HasPrefix(scenario, "unsupported") && r.Header.Get("Prefer") != "" {
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				_ = json.NewEncoder(w).Encode(rows)
			}))
			defer server.Close()
			reader := estateCollectionTestReader(t, server, context.Background())
			got, err := reader.collection(server.URL + "/api/v2/orgs/1/labels")
			if strings.Contains(scenario, "full") != (err != nil) {
				t.Fatalf("wrong completeness: rows=%d err=%v", len(got), err)
			}
			wantRequests := int64(1)
			if strings.HasPrefix(scenario, "unsupported") {
				wantRequests = 2
			}
			if requests.Load() != wantRequests {
				t.Fatal("unexpected retry/unsupported offset pagination")
			}
		})
	}
}

func TestEstateCollectionAsyncFailuresAreIncomplete(t *testing.T) {
	for _, scenario := range []string{"failed", "unknown-status", "missing-location", "missing-result", "malformed-job", "truncated-download", "trailing-download", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertEstateCollectionRequest(t, r)
				w.Header().Set("Retry-After", "0")
				switch r.URL.Path {
				case "/api/v2/orgs/1/labels":
					if scenario != "missing-location" {
						w.Header().Set("Location", "/orgs/1/jobs/1")
					}
					if scenario == "deadline" {
						w.Header().Set("Retry-After", "3600")
					}
					w.WriteHeader(http.StatusAccepted)
				case "/api/v2/orgs/1/jobs/1":
					switch scenario {
					case "failed":
						_, _ = w.Write([]byte(`{"status":"failed"}`))
					case "unknown-status":
						_, _ = w.Write([]byte(`{"status":"surprise"}`))
					case "missing-result":
						_, _ = w.Write([]byte(`{"status":"done"}`))
					case "malformed-job":
						_, _ = w.Write([]byte(`{"status":"done"`))
					default:
						_, _ = w.Write([]byte(`{"status":"done","result":{"href":"/orgs/1/datafiles/1"}}`))
					}
				case "/api/v2/orgs/1/datafiles/1":
					if scenario == "truncated-download" {
						_, _ = w.Write([]byte(`[{"href":"/labels/1"},{"href":`))
					} else {
						_, _ = w.Write([]byte(`[{"href":"/labels/1"}] {"unexpected":true}`))
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			reader := estateCollectionTestReader(t, server, ctx)
			got, err := reader.collection(server.URL + "/api/v2/orgs/1/labels")
			if err == nil {
				t.Fatal("incomplete response accepted")
			}
			if strings.Contains(scenario, "download") && len(got) != 1 {
				t.Fatalf("fully decoded evidence discarded: rows=%d err=%v", len(got), err)
			}
			if scenario == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline not honored: %v", err)
			}
		})
	}
}

func TestEstateCollectionRejectsOffOriginJobsResultsAndRedirects(t *testing.T) {
	for _, scenario := range []string{"job-link", "result-link", "initial-redirect", "job-redirect", "result-redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var leaked atomic.Int64
			external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
			defer external.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertEstateCollectionRequest(t, r)
				w.Header().Set("Retry-After", "0")
				switch r.URL.Path {
				case "/api/v2/orgs/1/labels":
					if scenario == "initial-redirect" {
						http.Redirect(w, r, external.URL, http.StatusTemporaryRedirect)
						return
					}
					location := "/orgs/1/jobs/1"
					if scenario == "job-link" {
						location = external.URL
					}
					w.Header().Set("Location", location)
					w.WriteHeader(http.StatusAccepted)
				case "/api/v2/orgs/1/jobs/1":
					if scenario == "job-redirect" {
						http.Redirect(w, r, external.URL, http.StatusTemporaryRedirect)
						return
					}
					href := "/orgs/1/datafiles/1"
					if scenario == "result-link" {
						href = external.URL
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "done", "result": map[string]string{"href": href}})
				case "/api/v2/orgs/1/datafiles/1":
					http.Redirect(w, r, external.URL, http.StatusTemporaryRedirect)
				}
			}))
			defer server.Close()
			reader := estateCollectionTestReader(t, server, context.Background())
			if _, err := reader.collection(server.URL + "/api/v2/orgs/1/labels"); err == nil || leaked.Load() != 0 {
				t.Fatalf("unsafe collection link: err=%v leaked=%d", err, leaked.Load())
			}
		})
	}
}

func TestEstatePolicyVersionDirectLookupAndFallback(t *testing.T) {
	for _, scenario := range []string{"active", "fallback", "fallback-full", "forbidden", "malformed-version"} {
		t.Run(scenario, func(t *testing.T) {
			var historyRequests atomic.Int64
			at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertEstateCollectionRequest(t, r)
				if strings.HasSuffix(r.URL.Path, "/active") {
					if scenario == "active" {
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"version": 1234, "created_at": at.Format(time.RFC3339)})
					} else if scenario == "forbidden" {
						w.WriteHeader(http.StatusForbidden)
					} else {
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				historyRequests.Add(1)
				rows := []map[string]interface{}{{"href": "/sec_policy/1234", "created_at": at.Format(time.RFC3339)}, {"href": "/sec_policy/1233", "created_at": at.Add(-time.Hour).Format(time.RFC3339)}}
				if scenario == "fallback-full" {
					rows = estateCollectionFixture(500)
				}
				if scenario == "malformed-version" {
					rows[1]["created_at"] = "invalid"
				}
				_ = json.NewEncoder(w).Encode(rows)
			}))
			defer server.Close()
			reader := estateCollectionTestReader(t, server, context.Background())
			got, err := estateLastProvisionedAt(server.URL+"/api/v2/orgs/1", reader)
			complete := scenario == "active" || scenario == "fallback"
			if complete != (err == nil) || (complete && !got.Equal(at)) {
				t.Fatalf("provisioning time=%v err=%v", got, err)
			}
			if (scenario == "active" || scenario == "forbidden") && historyRequests.Load() != 0 {
				t.Fatal("unnecessary or unauthorized version fallback")
			}
		})
	}
}

func TestEstatePolicyCacheRetriesFailuresOnNextSample(t *testing.T) {
	at := time.Now()
	policy := &EstatePolicy{Active: EstatePolicyCount{Complete: true}, Draft: EstatePolicyCount{Complete: true}, PendingComplete: true}
	if estatePolicyRefreshDue(policy, at, at.Add(5*time.Minute)) || !estatePolicyRefreshDue(policy, at, at.Add(15*time.Minute)) {
		t.Fatal("successful policy inventory should be cached for fifteen minutes")
	}
	policy.Warnings = []string{"Last provisioning lookup failed"}
	if estatePolicyRefreshDue(policy, at, at.Add(time.Minute)) || !estatePolicyRefreshDue(policy, at, at.Add(5*time.Minute)) {
		t.Fatal("failed inventory must retry on the next normal sample, without a tight retry loop")
	}
	if !estatePolicyRefreshDue(policy, time.Time{}, at) {
		t.Fatal("settings reset must immediately invalidate policy cache")
	}
}

func TestEstatePolicyLargeAsyncInventoriesRemainSeparate(t *testing.T) {
	active, draft := estateCollectionFixture(601), estateCollectionFixture(803)
	pending := estateCollectionFixture(507)
	for _, row := range pending {
		row["update_type"] = "update"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertEstateCollectionRequest(t, r)
		w.Header().Set("Retry-After", "0")
		switch {
		case r.URL.Path == "/api/v2/orgs/1/sec_policy/active":
			_, _ = w.Write([]byte(`{"version":1001,"created_at":"2026-11-02T12:00:00Z"}`))
		case strings.HasPrefix(r.URL.Path, "/api/v2/orgs/1/sec_policy/"):
			if r.Header.Get("Prefer") != "respond-async" {
				t.Error("large inventories require asynchronous collections")
			}
			kind := "pending"
			if strings.Contains(r.URL.Path, "/active/") {
				kind = "active"
			} else if strings.Contains(r.URL.Path, "/draft/") {
				kind = "draft"
			}
			w.Header().Set("Location", "/orgs/1/jobs/"+kind)
			w.WriteHeader(http.StatusAccepted)
		case strings.HasPrefix(r.URL.Path, "/api/v2/orgs/1/jobs/"):
			kind := strings.TrimPrefix(r.URL.Path, "/api/v2/orgs/1/jobs/")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "done", "result": map[string]string{"href": "/orgs/1/datafiles/" + kind}})
		case strings.HasSuffix(r.URL.Path, "/datafiles/active"):
			_ = json.NewEncoder(w).Encode(active)
		case strings.HasSuffix(r.URL.Path, "/datafiles/draft"):
			_ = json.NewEncoder(w).Encode(draft)
		case strings.HasSuffix(r.URL.Path, "/datafiles/pending"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"rule_sets": pending, "services": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reader := estateCollectionTestReader(t, server, context.Background())
	policy := estateCollectPolicyWithReader(server.URL+"/api/v2/orgs/1", time.Now(), reader)
	if !policy.Active.Complete || policy.Active.Rulesets != 601 || policy.Active.Rules != 1202 || !policy.Draft.Complete || policy.Draft.Rulesets != 803 || policy.Draft.Rules != 1606 {
		t.Fatalf("complete active/draft counts lost or mixed: %+v", policy)
	}
	if !policy.PendingComplete || len(policy.Pending) != 507 || policy.LastProvisionedAt.IsZero() || len(policy.Warnings) != 0 {
		t.Fatalf("pending or provisioning history incomplete: %+v", policy)
	}
}

func TestEstatePendingSyncFullBucketStaysIncomplete(t *testing.T) {
	rows := estateCollectionFixture(500)
	for _, row := range rows {
		row["update_type"] = "update"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"rule_sets": rows, "services": []interface{}{}})
	}))
	defer server.Close()
	reader := estateCollectionTestReader(t, server, context.Background())
	got, err := reader.pending(server.URL + "/api/v2/orgs/1/sec_policy/pending")
	if err == nil || len(estateObjectArray(got["rule_sets"])) != 500 {
		t.Fatal("full pending bucket must retain evidence with a truncation warning")
	}
}

func TestEstatePolicyRulesFallbackUsesOnlyItsRulesetVersion(t *testing.T) {
	base := "https://pce.invalid/api/v2/orgs/1/sec_policy/draft"
	for _, href := range []string{"/orgs/1/sec_policy/draft/rule_sets/2", "/orgs/1/sec_policy/active/rule_sets/2", "https://other.invalid/api/v2/orgs/1/sec_policy/draft/rule_sets/2"} {
		t.Run(href, func(t *testing.T) {
			calls := 0
			fetch := func(endpoint string) ([]map[string]interface{}, error) {
				calls++
				if endpoint == base+"/rule_sets" {
					return []map[string]interface{}{{"href": href}}, nil
				}
				if endpoint != base+"/rule_sets/2/sec_rules" {
					t.Errorf("wrong rules endpoint %s", endpoint)
				}
				return estateCollectionFixture(3), nil
			}
			count, err := estatePolicyCounts(base, fetch)
			if href == "/orgs/1/sec_policy/draft/rule_sets/2" {
				if err != nil || !count.Complete || count.Rules != 3 || calls != 2 {
					t.Fatalf("documented rules fallback: %+v %v", count, err)
				}
			} else if err == nil || count.Complete || calls != 1 {
				t.Fatal("rules fallback followed an off-scope ruleset")
			}
		})
	}
}

func TestEstatePolicyInvalidRuleCountsRequireFallback(t *testing.T) {
	base := "https://pce.invalid/api/v2/orgs/1/sec_policy/draft"
	for _, value := range []interface{}{nil, "unknown", "-1", "2.5", "999999999999999999999999", float64(-1), 1.5, math.Inf(1), math.NaN(), true, map[string]interface{}{}, []interface{}{}} {
		t.Run(fmt.Sprintf("%T:%v", value, value), func(t *testing.T) {
			calls := 0
			fetch := func(endpoint string) ([]map[string]interface{}, error) {
				calls++
				if endpoint == base+"/rule_sets" {
					return []map[string]interface{}{{"href": "/orgs/1/sec_policy/draft/rule_sets/1", "rules_count": value}}, nil
				}
				return estateCollectionFixture(3), nil
			}
			count, err := estatePolicyCounts(base, fetch)
			if err != nil || !count.Complete || count.Rules != 3 || calls != 2 {
				t.Fatalf("invalid count did not trigger retrieval: %+v calls=%d err=%v", count, calls, err)
			}
		})
	}
	for _, raw := range []interface{}{nil, []interface{}{nil}, []interface{}{"bad"}, []interface{}{map[string]interface{}{}, nil}, []map[string]interface{}{nil}, "not an array"} {
		fetch := func(endpoint string) ([]map[string]interface{}, error) {
			if endpoint == base+"/rule_sets" {
				return []map[string]interface{}{{"href": "/orgs/1/sec_policy/draft/rule_sets/1", "rules": raw}}, nil
			}
			return nil, errors.New("rules fallback unavailable")
		}
		count, err := estatePolicyCounts(base, fetch)
		if err == nil || count.Complete {
			t.Fatalf("invalid rules array implied complete zero: raw=%#v count=%+v", raw, count)
		}
	}
}

func TestEstatePolicyValidRuleCountsAndArrays(t *testing.T) {
	for _, test := range []struct {
		row  map[string]interface{}
		want int
	}{
		{map[string]interface{}{"rules_count": 0}, 0},
		{map[string]interface{}{"rules_count": float64(3)}, 3},
		{map[string]interface{}{"rules_count": "4"}, 4},
		{map[string]interface{}{"rules_count": json.Number("5")}, 5},
		{map[string]interface{}{"rules": []interface{}{}}, 0},
		{map[string]interface{}{"sec_rules": []interface{}{map[string]interface{}{}, map[string]interface{}{}}}, 2},
		{map[string]interface{}{"rules_count": nil, "rules": []map[string]interface{}{{}}}, 1},
	} {
		if got, ok := estateRulesetCount(test.row); !ok || got != test.want {
			t.Errorf("valid rule count: row=%v got=%d known=%v", test.row, got, ok)
		}
	}
	if _, err := estatePolicyCounts("https://%invalid", func(string) ([]map[string]interface{}, error) {
		t.Fatal("invalid URL must not be fetched")
		return nil, nil
	}); err == nil {
		t.Fatal("invalid fallback base URL accepted")
	}
}

func TestEstatePendingMalformedEntriesNeverMeanCompleteZero(t *testing.T) {
	valid := map[string]interface{}{"href": "/orgs/1/sec_policy/draft/rule_sets/1", "update_type": "update"}
	for _, raw := range []interface{}{nil, []interface{}{nil}, []interface{}{true}, []interface{}{map[string]interface{}{}}, []map[string]interface{}{nil}, "not an array"} {
		if rows, complete := estateParsePendingPolicy(map[string]interface{}{"rule_sets": raw, "services": []interface{}{}}); complete {
			t.Fatalf("malformed pending array accepted: raw=%#v rows=%v", raw, rows)
		}
	}
	rows, complete := estateParsePendingPolicy(map[string]interface{}{"rule_sets": []interface{}{nil, valid, "bad"}})
	if complete || len(rows) != 1 {
		t.Fatal("partial pending evidence must be retained but never marked complete")
	}
	if rows, complete := estateParsePendingPolicy(map[string]interface{}{"rule_sets": []interface{}{}}); !complete || len(rows) != 0 {
		t.Fatal("valid empty array should be a known zero")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"rule_sets": make([]interface{}, 500)})
	}))
	defer server.Close()
	reader := estateCollectionTestReader(t, server, context.Background())
	if _, err := reader.pending(server.URL + "/api/v2/orgs/1/sec_policy/pending"); err == nil || !strings.Contains(err.Error(), "synchronous limit") {
		t.Fatalf("sync limit used filtered objects instead of raw length: %v", err)
	}
}

func TestEstateCollectionPrivateClientUsesContextDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	previous := apiRateLimiter
	apiRateLimiter = newAPIRateController(600, 60, 10000)
	t.Cleanup(func() { apiRateLimiter = previous })
	sharedClient := server.Client()
	sharedClient.Timeout = time.Nanosecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader, err := newEstatePCEReader(ctx, server.URL, "dashboard-key", "dashboard-secret", sharedClient)
	if err != nil {
		t.Fatal(err)
	}
	if reader.client == sharedClient || reader.client.Timeout != 0 || sharedClient.Timeout != time.Nanosecond {
		t.Fatal("reader must remove only its private clone's per-request timeout")
	}
	if _, err := reader.collection(server.URL + "/api/v2/orgs/1/labels"); err != nil {
		t.Fatalf("collection inherited shared client's shorter timeout: %v", err)
	}
}
