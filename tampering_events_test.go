package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func isolateTamperingRetrieval(t *testing.T) {
	t.Helper()
	isolateTamperingState(t)
	previous := apiRateLimiter
	apiRateLimiter = newAPIRateController(600, 60, 10000)
	t.Cleanup(func() { apiRateLimiter = previous })
}

func tamperingRetrievalFixture(count int, start time.Time, sameTime bool) []map[string]interface{} {
	result := make([]map[string]interface{}, count)
	for i := range result {
		at := start
		if !sameTime {
			at = at.Add(time.Duration(i) * time.Second)
		}
		result[i] = tamperEvent(fmt.Sprintf("ven-%d", i%151), "event_timestamp", at.Format(time.RFC3339Nano))
		result[i]["href"] = fmt.Sprintf("/orgs/1/events/event-%d", i)
	}
	return result
}

func assertTamperingRequest(t *testing.T, req *http.Request) {
	t.Helper()
	key, secret, ok := req.BasicAuth()
	if !ok || key != "test-key" || secret != "test-secret" {
		t.Error("request did not use dashboard credentials")
	}
	if !strings.HasSuffix(req.URL.Path, "/events") {
		return
	}
	q := req.URL.Query()
	for _, invalid := range []string{"skip", "offset", "start_date", "end_date", "start_time", "end_time"} {
		if q.Has(invalid) {
			t.Errorf("unsupported events query parameter %s", invalid)
		}
	}
	if q.Get("event_type") != "agent.tampering" {
		t.Error("missing event type filter")
	}
	for _, field := range []string{"timestamp[gte]", "timestamp[lte]"} {
		if _, err := time.Parse(time.RFC3339Nano, q.Get(field)); err != nil {
			t.Errorf("invalid %s: %v", field, err)
		}
	}
	if req.Header.Get("Prefer") == "respond-async" && q.Has("max_results") {
		t.Error("async export must not be capped by max_results")
	}
	if req.Header.Get("Prefer") == "" && q.Get("max_results") != "10000" {
		t.Error("sync request must use documented 10000 event limit")
	}
}

func serveTamperingSyncFixture(t *testing.T, w http.ResponseWriter, req *http.Request, events []map[string]interface{}) {
	t.Helper()
	assertTamperingRequest(t, req)
	start, _ := time.Parse(time.RFC3339Nano, req.URL.Query().Get("timestamp[gte]"))
	end, _ := time.Parse(time.RFC3339Nano, req.URL.Query().Get("timestamp[lte]"))
	limit := 100
	if req.URL.Query().Has("max_results") {
		limit, _ = strconv.Atoi(req.URL.Query().Get("max_results"))
	}
	batch := make([]map[string]interface{}, 0)
	for _, event := range events {
		at, _ := eventTimestampUTC(event)
		if at.Before(start) || at.After(end) {
			continue
		}
		batch = append(batch, event)
		if len(batch) == limit {
			break
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"events": batch, "metadata": map[string]interface{}{"ignored": []interface{}{1, map[string]interface{}{"nested": true}}}})
}

func TestTamperingRetrievalAsyncUnboundedAndDeduplicated(t *testing.T) {
	for _, sameTimestamp := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_timestamp_%t", sameTimestamp), func(t *testing.T) {
			isolateTamperingRetrieval(t)
			start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			events := tamperingRetrievalFixture(12051, start, sameTimestamp)
			events = append(events, events[0])
			polls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assertTamperingRequest(t, req)
				w.Header().Set("Retry-After", "0")
				switch req.URL.Path {
				case "/api/v2/orgs/1/events":
					if req.Header.Get("Prefer") != "respond-async" {
						t.Error("async completion unexpectedly fell back")
					}
					w.Header().Set("Location", "/orgs/1/jobs/test")
					w.WriteHeader(http.StatusAccepted)
				case "/api/v2/orgs/1/jobs/test":
					polls++
					status := "pending"
					if polls == 2 {
						status = "running"
					}
					if polls >= 3 {
						status = "done"
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "result": map[string]string{"href": "/orgs/1/datafiles/test"}})
				case "/api/v2/orgs/1/datafiles/test":
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": events})
				default:
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			useTestPCEOrigin(t, server.URL)
			batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start, start.Add(24*time.Hour), server.Client())
			if err != nil || len(batch) != 12051 {
				t.Fatalf("unbounded async collection = %d events, %v", len(batch), err)
			}
			summary := summarizeTamperingEvents(batch, start, start.Add(24*time.Hour))
			if summary.EventCount != 12051 || len(summary.Names) != 151 || polls != 3 {
				t.Fatalf("incomplete async result: %d events, %d workloads, %d polls", summary.EventCount, len(summary.Names), polls)
			}
		})
	}
}

func TestTamperingRetrievalSyncAdaptiveSlices(t *testing.T) {
	isolateTamperingRetrieval(t)
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	events := tamperingRetrievalFixture(25001, start, false)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		serveTamperingSyncFixture(t, w, req, events)
	}))
	defer server.Close()
	useTestPCEOrigin(t, server.URL)
	batch, err := fetchTamperingSlice(server.URL+"/api/v2/orgs/1", start, start.Add(25000*time.Second), server.Client())
	if err != nil || len(batch) != len(events) {
		t.Fatalf("adaptive sync = %d of %d, %v", len(batch), len(events), err)
	}
	if requests.Load() < 4 || requests.Load() > 20 {
		t.Fatalf("unexpected adaptive request count %d", requests.Load())
	}
	if len(summarizeTamperingEvents(batch, start, start.Add(24*time.Hour)).Names) != 151 {
		t.Fatal("workloads still truncated at a page boundary")
	}
}

func TestTamperingRetrievalSameInstantSaturationIsPartial(t *testing.T) {
	isolateTamperingRetrieval(t)
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	events := tamperingRetrievalFixture(10001, start.Add(time.Hour), true)
	events = append(events, tamperEvent("before-hotspot", "timestamp", start.Add(time.Minute).Format(time.RFC3339)), tamperEvent("after-hotspot", "timestamp", start.Add(119*time.Minute).Format(time.RFC3339)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { serveTamperingSyncFixture(t, w, req, events) }))
	defer server.Close()
	useTestPCEOrigin(t, server.URL)
	batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start, start.Add(2*time.Hour), server.Client())
	if err == nil || !strings.Contains(err.Error(), "complete retrieval requires asynchronous") || len(batch) != 10002 {
		t.Fatalf("same-instant saturation = %d, %v", len(batch), err)
	}
	names := summarizeTamperingEvents(batch, start, start.Add(2*time.Hour)).LastSeen
	if names["before-hotspot"].IsZero() || names["after-hotspot"].IsZero() {
		t.Fatal("failed hotspot discarded other complete slices")
	}
}

func TestTamperingRetrievalAsyncFailureAndPartialDownloadFallback(t *testing.T) {
	for _, scenario := range []string{"job-failed-recovered", "job-failed-unavailable", "partial-download", "done-missing-result"} {
		t.Run(scenario, func(t *testing.T) {
			isolateTamperingRetrieval(t)
			start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			events := tamperingRetrievalFixture(2, start, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assertTamperingRequest(t, req)
				w.Header().Set("Retry-After", "0")
				switch req.URL.Path {
				case "/api/v2/orgs/1/events":
					if req.Header.Get("Prefer") != "" {
						w.Header().Set("Location", "/orgs/1/jobs/test")
						w.WriteHeader(http.StatusAccepted)
						return
					}
					if scenario == "job-failed-recovered" {
						serveTamperingSyncFixture(t, w, req, events)
						return
					}
					http.Error(w, "sync fixture unavailable", http.StatusBadRequest)
				case "/api/v2/orgs/1/jobs/test":
					status, href := "failed", "/orgs/1/datafiles/test"
					if scenario == "partial-download" {
						status = "done"
					}
					if scenario == "done-missing-result" {
						status = "done"
						href = ""
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "result": map[string]string{"href": href}})
				case "/api/v2/orgs/1/datafiles/test":
					encoded, _ := json.Marshal(events)
					_, _ = w.Write(append(encoded[:len(encoded)-1], []byte(",BROKEN")...))
				default:
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			useTestPCEOrigin(t, server.URL)
			batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start, start.Add(time.Hour), server.Client())
			if scenario == "job-failed-recovered" {
				if err != nil || len(batch) != 2 {
					t.Fatalf("recoverable failed async job: %d, %v", len(batch), err)
				}
			} else {
				if err == nil {
					t.Fatal("incomplete job/download marked complete")
				}
				if scenario == "partial-download" && len(batch) != 2 {
					t.Fatalf("lost partial streamed events: %d", len(batch))
				}
			}
		})
	}
}

func TestTamperingRetrievalFallbackUpgradesMissingWorkloadIdentity(t *testing.T) {
	for _, recovered := range []bool{true, false} {
		t.Run(fmt.Sprintf("recovered_%t", recovered), func(t *testing.T) {
			isolateTamperingRetrieval(t)
			start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			nameless := tamperingRetrievalFixture(1, start, false)[0]
			delete(nameless, "workload")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assertTamperingRequest(t, req)
				w.Header().Set("Retry-After", "0")
				switch req.URL.Path {
				case "/api/v2/orgs/1/events":
					if req.Header.Get("Prefer") != "" {
						w.Header().Set("Location", "/orgs/1/jobs/test")
						w.WriteHeader(http.StatusAccepted)
						return
					}
					batch := []map[string]interface{}{}
					if recovered {
						batch = tamperingRetrievalFixture(1, start, false)
					}
					_ = json.NewEncoder(w).Encode(batch)
				case "/api/v2/orgs/1/jobs/test":
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "done", "result": map[string]string{"href": "/orgs/1/datafiles/test"}})
				case "/api/v2/orgs/1/datafiles/test":
					_ = json.NewEncoder(w).Encode([]map[string]interface{}{nameless})
				default:
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			useTestPCEOrigin(t, server.URL)
			batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start, start.Add(time.Hour), server.Client())
			if len(batch) != 1 {
				t.Fatalf("event evidence not deduplicated/preserved: %d", len(batch))
			}
			if recovered {
				result := summarizeTamperingEvents(batch, start, start.Add(time.Hour))
				if err != nil || result.EventCount != 1 || len(result.Names) != 1 || result.Names[0] != "ven-0" || !result.LastSeen["ven-0"].Equal(start) {
					t.Fatalf("richer fallback event was discarded: result=%+v error=%v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "still has no workload identity") {
				t.Fatalf("unresolved retained evidence was marked complete: %v", err)
			}
		})
	}
}

func TestTamperingRetrievalEmptyAndIgnoredFilters(t *testing.T) {
	for _, scenario := range []string{"empty", "ignored-time-filter", "wrong-event-type", "missing-workload", "invalid-collection"} {
		t.Run(scenario, func(t *testing.T) {
			isolateTamperingRetrieval(t)
			start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				assertTamperingRequest(t, req)
				if scenario == "invalid-collection" {
					_, _ = w.Write([]byte(`{"metadata":{}}`))
					return
				}
				batch := []map[string]interface{}{}
				if scenario != "empty" {
					event := tamperEvent("wrong", "timestamp", start.Add(-time.Hour).Format(time.RFC3339))
					if scenario == "wrong-event-type" {
						event["timestamp"] = start.Format(time.RFC3339)
						event["event_type"] = "agent.deactivated"
					}
					if scenario == "missing-workload" {
						event["timestamp"] = start.Format(time.RFC3339)
						delete(event, "workload")
					}
					batch = append(batch, event)
				}
				_ = json.NewEncoder(w).Encode(batch)
			}))
			defer server.Close()
			useTestPCEOrigin(t, server.URL)
			batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start, start.Add(time.Hour), server.Client())
			if scenario == "missing-workload" && len(batch) != 1 {
				t.Fatal("nameless event evidence was discarded")
			}
			if scenario != "missing-workload" && len(batch) != 0 {
				t.Fatal("nonmatching event entered tampering history")
			}
			if scenario == "empty" {
				if err != nil || calls != 2 {
					t.Fatalf("empty result is not a genuine success: %v (%d calls)", err, calls)
				}
			} else if err == nil || calls > 2 {
				t.Fatalf("ignored/invalid response incorrectly complete or retried endlessly: %v (%d calls)", err, calls)
			}
		})
	}
}

func TestTamperingRetrievalRejectsCrossOriginLinksAndRedirects(t *testing.T) {
	for _, scenario := range []string{"location", "result", "initial-redirect", "job-redirect", "result-redirect", "protocol-relative-location"} {
		t.Run(scenario, func(t *testing.T) {
			isolateTamperingRetrieval(t)
			var leaked atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { leaked.Add(1); _, _ = w.Write([]byte(`[]`)) }))
			defer outside.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assertTamperingRequest(t, req)
				w.Header().Set("Retry-After", "0")
				switch req.URL.Path {
				case "/api/v2/orgs/1/events":
					if scenario == "initial-redirect" {
						http.Redirect(w, req, outside.URL+"/leak", http.StatusFound)
						return
					}
					location := "/orgs/1/jobs/test"
					if scenario == "location" {
						location = outside.URL + "/job"
					}
					if scenario == "protocol-relative-location" {
						location = strings.TrimPrefix(outside.URL, "http:") + "/job"
					}
					w.Header().Set("Location", location)
					w.WriteHeader(http.StatusAccepted)
				case "/api/v2/orgs/1/jobs/test":
					if scenario == "job-redirect" {
						http.Redirect(w, req, outside.URL+"/leak", http.StatusFound)
						return
					}
					href := "/orgs/1/datafiles/test"
					if scenario == "result" {
						href = outside.URL + "/data"
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "done", "result": map[string]string{"href": href}})
				case "/api/v2/orgs/1/datafiles/test":
					http.Redirect(w, req, outside.URL+"/leak", http.StatusFound)
				default:
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			useTestPCEOrigin(t, server.URL)
			start := time.Now().UTC()
			batch, err := fetchTamperingEventsPaged(server.URL+"/api/v2/orgs/1", start.Add(-time.Hour), start, server.Client())
			var unsafe *tamperingUnsafeLinkError
			if !errors.As(err, &unsafe) || len(batch) != 0 || leaked.Load() != 0 {
				t.Fatalf("unsafe link handling: events=%d requests=%d error=%v", len(batch), leaked.Load(), err)
			}
		})
	}
}

func TestTamperingRetrievalDeadlineAndBoundedRetries(t *testing.T) {
	for _, scenario := range []string{"long-job-delay", "rate-limit", "transient-server-error", "persistent-server-error", "limiter-wait"} {
		t.Run(scenario, func(t *testing.T) {
			isolateTamperingRetrieval(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				count := calls.Add(1)
				assertTamperingRequest(t, req)
				if scenario == "long-job-delay" {
					w.Header().Set("Location", "/orgs/1/jobs/test")
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if scenario == "rate-limit" {
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if scenario == "persistent-server-error" || (scenario == "transient-server-error" && count == 1) {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer server.Close()
			useTestPCEOrigin(t, server.URL)
			if scenario == "limiter-wait" {
				apiRateLimiter.backoffUntil = time.Now().Add(time.Hour)
			}
			timeout := 50 * time.Millisecond
			if strings.Contains(scenario, "server-error") {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			end := time.Now().UTC()
			reader, err := newTamperingEventReader(ctx, server.URL+"/api/v2/orgs/1", end.Add(-time.Hour), end, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = reader.fetch()
			if scenario == "transient-server-error" {
				if err != nil || calls.Load() != 3 {
					t.Fatalf("transient retry not recovered: %v, %d calls", err, calls.Load())
				}
			} else if scenario == "persistent-server-error" {
				if err == nil || errors.Is(err, context.DeadlineExceeded) || calls.Load() != 10 {
					t.Fatalf("retries not bounded per request: %v, %d calls", err, calls.Load())
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || calls.Load() > 1 {
				t.Fatalf("deadline ignored: %v, %d calls", err, calls.Load())
			}
		})
	}
}
