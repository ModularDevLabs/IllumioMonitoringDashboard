package illumio

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestFetchDayOfTrafficParsesTimestampRangeAndCleansUp(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	deleted := false
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if user, pass, ok := req.BasicAuth(); !ok || user != "key" || pass != "secret" {
			t.Fatalf("missing or incorrect basic auth")
		}
		status := http.StatusOK
		body := `{}`
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/traffic_flows/async_queries"):
			status = http.StatusCreated
			body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-123"}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-123/download"):
			body = `[{
				"src":{"ip":"10.0.0.1","workload":{"href":"/workloads/1","labels":[{"key":"env","value":"Prod"}]}},
				"dst":{"ip":"10.0.0.2","workload":{"href":"/workloads/2","labels":[{"key":"app","value":"API"}]}},
				"service":{"port":443,"proto":6},
				"num_connections":7,
				"timestamp_range":{"first_detected":"2026-03-01T01:02:03Z","last_detected":"2026-03-01T04:05:06Z"}
			}]`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-123"):
			body = `{"status":"completed"}`
		case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/query-123"):
			status = http.StatusNoContent
			deleted = true
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	request := AsyncQueryRequest{StartDate: "2026-03-01T00:00:00Z", EndDate: "2026-03-02T00:00:00Z"}
	flows, err := client.FetchDayOfTraffic(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("FetchDayOfTraffic returned error: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("FetchDayOfTraffic returned %d flows, want 1", len(flows))
	}
	if got, want := flows[0].FirstDetected, time.Date(2026, 3, 1, 1, 2, 3, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("FirstDetected = %v, want %v", got, want)
	}
	if got, want := flows[0].LastDetected, time.Date(2026, 3, 1, 4, 5, 6, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("LastDetected = %v, want %v", got, want)
	}
	if !deleted {
		t.Fatal("FetchDayOfTraffic did not delete the asynchronous query")
	}
}

func TestRequestRejectsCrossOriginAbsoluteURL(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	if _, _, _, err := client.requestWithHeaders(context.Background(), http.MethodGet, "https://evil.example/result", nil, nil); err == nil {
		t.Fatal("requestWithHeaders should reject a cross-origin absolute URL")
	}
}

func TestRetryAfterDelay(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("Retry-After", "12")
	if got := retryAfterDelay(headers); got != 12*time.Second {
		t.Fatalf("retryAfterDelay = %v, want 12s", got)
	}
	headers.Set("Retry-After", "invalid")
	if got := retryAfterDelay(headers); got != 5*time.Second {
		t.Fatalf("retryAfterDelay invalid fallback = %v, want 5s", got)
	}
}

func TestGetTrafficFlowsDatabaseMetrics(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://pce.example.com/api/v2/orgs/1/traffic_flows/database_metrics" {
			t.Fatalf("unexpected URL %q", req.URL.String())
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"flows_days": 35,
				"flows_oldest_day": "2026-02-17",
				"server": {
					"num_flows_days": 35,
					"num_flows_days_limit": 90,
					"flows_oldest_day": "2026-02-17",
					"num_daily_tables": 35,
					"num_weekly_tables": 5
				},
				"updated_at": "2026-03-23T16:20:00Z"
			}`)),
		}, nil
	})

	metrics, err := client.GetTrafficFlowsDatabaseMetrics(context.Background())
	if err != nil {
		t.Fatalf("GetTrafficFlowsDatabaseMetrics() error = %v", err)
	}

	if metrics.Server.NumFlowsDays != 35 {
		t.Fatalf("server.num_flows_days = %d, want 35", metrics.Server.NumFlowsDays)
	}
	if metrics.Server.FlowsOldestDay != "2026-02-17" {
		t.Fatalf("server.flows_oldest_day = %q, want 2026-02-17", metrics.Server.FlowsOldestDay)
	}
	if metrics.UpdatedAt != "2026-03-23T16:20:00Z" {
		t.Fatalf("updated_at = %q, want 2026-03-23T16:20:00Z", metrics.UpdatedAt)
	}
}
