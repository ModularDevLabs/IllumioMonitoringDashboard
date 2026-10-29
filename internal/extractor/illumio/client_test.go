package illumio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type repeatedChunkReader struct {
	chunk     []byte
	remaining int64
}

func (r *repeatedChunkReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	limit := len(buffer)
	if int64(limit) > r.remaining {
		limit = int(r.remaining)
	}
	written := 0
	for written < limit {
		written += copy(buffer[written:limit], r.chunk)
	}
	r.remaining -= int64(written)
	return written, nil
}

func TestNewClientUsesIndependentConnectionPools(t *testing.T) {
	t.Parallel()
	first := NewClient("https://pce.example.com", "1", "key-one", "secret-one")
	second := NewClient("https://pce.example.com", "1", "key-two", "secret-two")
	firstTransport, firstOK := first.HTTP.Transport.(*http.Transport)
	secondTransport, secondOK := second.HTTP.Transport.(*http.Transport)
	if !firstOK || !secondOK {
		t.Fatalf("extractor transports = %T and %T", first.HTTP.Transport, second.HTTP.Transport)
	}
	if firstTransport == secondTransport || firstTransport == http.DefaultTransport || secondTransport == http.DefaultTransport {
		t.Fatal("extractor clients should not share an HTTP connection pool")
	}
	if firstTransport.MaxIdleConnsPerHost < 3 || secondTransport.MaxIdleConnsPerHost < 3 {
		t.Fatal("extractor connection pools do not support all three chunk workers")
	}
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
			var query AsyncQueryRequest
			if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
				t.Fatalf("decode query: %v", err)
			}
			if len(query.PolicyDecisions) != 1 || query.PolicyDecisions[0] != "blocked" {
				t.Fatalf("policy decisions = %#v, want blocked-only default", query.PolicyDecisions)
			}
			status = http.StatusCreated
			body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-123"}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-123/download"):
			body = `[{
				"src":{"ip":"10.0.0.1","workload":{"href":"/workloads/1","labels":[{"key":"env","value":"Prod"}]}},
				"dst":{"ip":"10.0.0.2","workload":{"href":"/workloads/2","labels":[{"key":"app","value":"API"}]}},
				"service":{"port":443,"proto":6},
				"num_connections":7,
				"policy_decision":"blocked",
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
	queryLogs := []string{}
	flows, err := client.FetchDayOfTraffic(context.Background(), request, func(message string) {
		queryLogs = append(queryLogs, message)
	})
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
	if flows[0].PolicyDecision != "blocked" {
		t.Fatalf("PolicyDecision = %q, want blocked", flows[0].PolicyDecision)
	}
	if flows[0].SrcWorkloadHref != "/workloads/1" || len(flows[0].SrcLabels) != 1 || flows[0].SrcLabels[0].Key != "env" || flows[0].SrcLabels[0].Value != "Prod" {
		t.Fatalf("source workload/labels were not preserved: %#v", flows[0])
	}
	if flows[0].DstWorkloadHref != "/workloads/2" || len(flows[0].DstLabels) != 1 || flows[0].DstLabels[0].Key != "app" || flows[0].DstLabels[0].Value != "API" {
		t.Fatalf("destination workload/labels were not preserved: %#v", flows[0])
	}
	if !deleted {
		t.Fatal("FetchDayOfTraffic did not delete the asynchronous query")
	}
	joinedLogs := strings.Join(queryLogs, "\n")
	for _, expected := range []string{"PCE async query accepted", "PCE async query status completed"} {
		if !strings.Contains(joinedLogs, expected) {
			t.Fatalf("query logs are missing %q: %s", expected, joinedLogs)
		}
	}
}

func TestFetchDayOfTrafficAllScopeSendsEmptyDecisionFilterAndPreservesDecision(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{}`
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/traffic_flows/async_queries"):
			var query AsyncQueryRequest
			if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
				t.Fatalf("decode query: %v", err)
			}
			if query.PolicyDecisions == nil || len(query.PolicyDecisions) != 0 {
				t.Fatalf("policy decisions = %#v, want a non-nil empty all-traffic filter", query.PolicyDecisions)
			}
			status = http.StatusCreated
			body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-all"}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-all"):
			body = `{"status":"completed","matches_count":1,"flows_count":1}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-all/download"):
			body = `[{"src":{"ip":"10.0.0.1"},"dst":{"ip":"10.0.0.2"},"service":{"port":443,"proto":6},"num_connections":3,"policy_decision":"allowed","draft_policy_decision":"potentially_blocked","timestamp_range":{"first_detected":"2026-03-01T01:02:03Z","last_detected":"2026-03-01T01:03:03Z"}}]`
		case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/query-all"):
			status = http.StatusNoContent
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	flows, err := client.FetchDayOfTraffic(context.Background(), AsyncQueryRequest{
		StartDate: "2026-03-01T00:00:00Z", EndDate: "2026-03-02T00:00:00Z", PolicyDecisions: []string{},
	}, nil)
	if err != nil {
		t.Fatalf("FetchDayOfTraffic: %v", err)
	}
	if len(flows) != 1 || flows[0].PolicyDecision != "allowed" || flows[0].DraftDecision != "potentially_blocked" {
		t.Fatalf("flows = %#v, want preserved allowed and potentially_blocked decisions", flows)
	}
}

func TestFetchDayOfTrafficRejectsTruncatedResult(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{}`
		switch {
		case req.Method == http.MethodPost:
			status = http.StatusCreated
			body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-large"}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-large"):
			body = `{"status":"completed","matches_count":250001,"flows_count":200000}`
		case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/query-large"):
			status = http.StatusNoContent
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	_, err := client.FetchDayOfTraffic(context.Background(), AsyncQueryRequest{StartDate: "2026-03-01T00:00:00Z", PolicyDecisions: []string{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "200000-row maximum") {
		t.Fatalf("error = %v, want explicit truncation error", err)
	}
	if !errors.Is(err, ErrQueryResultTruncated) {
		t.Fatalf("error = %v, want errors.Is(..., ErrQueryResultTruncated)", err)
	}
}

func TestFetchDayOfTrafficDetectsTruncationWhenFlowsCountIsMissingOrUnderreported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		matchesCount int
		flowsCount   int
	}{
		{matchesCount: 200001, flowsCount: 0},
		{matchesCount: 200001, flowsCount: 199999},
	}
	for _, test := range tests {
		test := test
		t.Run(strconv.Itoa(test.matchesCount)+"-"+strconv.Itoa(test.flowsCount), func(t *testing.T) {
			client := NewClient("https://pce.example.com", "1", "key", "secret")
			client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := `{}`
				switch {
				case req.Method == http.MethodPost:
					status = http.StatusCreated
					body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-truncated"}`
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-truncated"):
					body = fmt.Sprintf(`{"status":"completed","matches_count":%d,"flows_count":%d}`, test.matchesCount, test.flowsCount)
				case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/query-truncated"):
					status = http.StatusNoContent
				default:
					t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})

			_, err := client.FetchDayOfTraffic(context.Background(), AsyncQueryRequest{StartDate: "2026-03-01T00:00:00Z"}, nil)
			if !errors.Is(err, ErrQueryResultTruncated) {
				t.Fatalf("matches_count=%d flows_count=%d error=%v, want ErrQueryResultTruncated", test.matchesCount, test.flowsCount, err)
			}
		})
	}
}

func TestFetchDayOfTrafficAcceptsExactMaximumWithoutProofOfTruncation(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	deleted := false
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{}`
		switch {
		case req.Method == http.MethodPost:
			status = http.StatusCreated
			body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-exact-max"}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-exact-max/download"):
			body = `[{"src":{"ip":"10.0.0.1"},"dst":{"ip":"10.0.0.2"},"service":{"port":443,"proto":6},"num_connections":1,"policy_decision":"blocked","timestamp_range":{"first_detected":"2026-03-01T01:02:03Z"}}]`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-exact-max"):
			body = `{"status":"completed","matches_count":200000,"flows_count":200000}`
		case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/query-exact-max"):
			status = http.StatusNoContent
			deleted = true
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	flows, err := client.FetchDayOfTraffic(context.Background(), AsyncQueryRequest{StartDate: "2026-03-01T00:00:00Z"}, nil)
	if err != nil {
		t.Fatalf("exact-maximum result was rejected without proof of truncation: %v", err)
	}
	if len(flows) != 1 || flows[0].SrcIP != "10.0.0.1" {
		t.Fatalf("exact-maximum valid download was not decoded: %#v", flows)
	}
	if !deleted {
		t.Fatal("exact-maximum async query was not cleaned up")
	}
}

func TestRequestWithHeadersResponseTooLargeIsTyped(t *testing.T) {
	t.Parallel()

	body := &trackingReadCloser{Reader: strings.NewReader(`[]`)}
	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          body,
			ContentLength: maxResponseBodySize + 1,
		}, nil
	})

	_, _, _, err := client.requestWithHeaders(context.Background(), http.MethodGet, "labels", nil, nil)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error=%v, want errors.Is(..., ErrResponseTooLarge)", err)
	}
	if !body.closed {
		t.Fatal("oversized control response body was not closed")
	}
}

func TestDownloadTrafficFlowsStreamsPastControlResponseLimit(t *testing.T) {
	t.Parallel()

	const row = `{"src":{"ip":"10.0.0.1","workload":{"href":"/workloads/1","labels":[{"key":"env","value":"Prod"}]}},"dst":{"ip":"10.0.0.2"},"service":{"port":443,"proto":6},"num_connections":2,"policy_decision":"allowed","timestamp_range":{"first_detected":"2026-03-01T01:02:03Z"}}`
	prefix := "[" + row + ","
	suffix := row + "]"
	paddingBytes := int64(maxResponseBodySize) + 1
	payloadBytes := int64(len(prefix)+len(suffix)) + paddingBytes
	t.Logf("streaming synthetic traffic response: %d bytes (%.3f MiB)", payloadBytes, float64(payloadBytes)/(1<<20))
	padding := &repeatedChunkReader{chunk: bytes.Repeat([]byte{' '}, 32<<10), remaining: paddingBytes}
	body := &trackingReadCloser{Reader: io.MultiReader(strings.NewReader(prefix), padding, strings.NewReader(suffix))}

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          body,
			ContentLength: payloadBytes,
		}, nil
	})
	logs := make([]string, 0)
	flows, code, err := client.downloadTrafficFlows(context.Background(), "traffic_flows/async_queries/query-large/download", false, func(message string) {
		logs = append(logs, message)
	})
	if err != nil {
		t.Fatalf("downloadTrafficFlows error=%v", err)
	}
	if code != http.StatusOK || len(flows) != 2 {
		t.Fatalf("download result code=%d flows=%d, want 200 and 2", code, len(flows))
	}
	if flows[0].PolicyDecision != "allowed" || flows[0].LastDetected != flows[0].FirstDetected || len(flows[0].SrcLabels) != 1 {
		t.Fatalf("streamed flow fields/labels were not preserved: %#v", flows[0])
	}
	if !body.closed {
		t.Fatal("streamed traffic response body was not closed")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "download progress") || !strings.Contains(joined, "download complete") || !strings.Contains(joined, "rows=2") {
		t.Fatalf("download byte/row progress was not logged: %s", joined)
	}
}

func TestDecodeTrafficFlowResponseRejectsMalformedTailWithoutPartialRows(t *testing.T) {
	t.Parallel()

	const row = `{"src":{"ip":"10.0.0.1"},"dst":{"ip":"10.0.0.2"},"timestamp_range":{"first_detected":"2026-03-01T01:02:03Z"}}`
	tests := map[string]string{
		"trailing garbage":    "[" + row + "]garbage",
		"trailing JSON value": "[" + row + `]{"unexpected":true}`,
		"missing array close": "[" + row,
		"invalid second row":  "[" + row + `,{"timestamp_range":{"first_detected":"not-a-time"}}]`,
	}
	for name, payload := range tests {
		name, payload := name, payload
		t.Run(name, func(t *testing.T) {
			tracker := newTrafficDownloadTracker(strings.NewReader(payload), nil)
			flows, err := decodeTrafficFlowResponse(tracker, false)
			if err == nil {
				t.Fatal("malformed response unexpectedly decoded")
			}
			if flows != nil {
				t.Fatalf("malformed response returned partial flows: %#v", flows)
			}
			if tracker.rowsDecoded != 1 {
				t.Fatalf("rows decoded before failure=%d, want 1", tracker.rowsDecoded)
			}
		})
	}
}

func TestDownloadTrafficFlowsLogsByteAndRowCountsOnDecodeFailure(t *testing.T) {
	t.Parallel()

	const payload = `[{"timestamp_range":{"first_detected":"2026-03-01T01:02:03Z"}}]garbage`
	body := &trackingReadCloser{Reader: strings.NewReader(payload)}
	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	logs := make([]string, 0)
	flows, _, err := client.downloadTrafficFlows(context.Background(), "traffic_flows/async_queries/query-malformed/download", false, func(message string) {
		logs = append(logs, message)
	})
	if err == nil {
		t.Fatal("malformed download unexpectedly succeeded")
	}
	if flows != nil {
		t.Fatalf("malformed download returned partial flows: %#v", flows)
	}
	if !body.closed {
		t.Fatal("malformed download response body was not closed")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "download failed") || !strings.Contains(joined, fmt.Sprintf("bytes=%d", len(payload))) || !strings.Contains(joined, "rows=1") {
		t.Fatalf("failed download byte/row counts were not logged: %s", joined)
	}
}

func TestDownloadTrafficFlowsUsesCallerDeadlineInsteadOfSharedClientTimeout(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	client.HTTP.Timeout = time.Nanosecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	wantDeadline, _ := ctx.Deadline()
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotDeadline, ok := req.Context().Deadline()
		if !ok || gotDeadline.Sub(wantDeadline) > time.Millisecond || wantDeadline.Sub(gotDeadline) > time.Millisecond {
			t.Fatalf("download request deadline=%v present=%v, want caller deadline %v", gotDeadline, ok, wantDeadline)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`[]`)),
		}, nil
	})

	flows, _, err := client.downloadTrafficFlows(ctx, "traffic_flows/async_queries/query-timeout/download", false, nil)
	if err != nil || len(flows) != 0 {
		t.Fatalf("download error=%v flows=%#v", err, flows)
	}
	if client.HTTP.Timeout != time.Nanosecond {
		t.Fatalf("shared client timeout mutated to %v", client.HTTP.Timeout)
	}
}

func TestFetchDayOfTrafficCleansUpAfterDownloadRateLimit(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	deleted := false
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{}`
		switch {
		case req.Method == http.MethodPost:
			status = http.StatusCreated
			body = `{"href":"/api/v2/orgs/1/traffic_flows/async_queries/query-rate-limited"}`
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-rate-limited/download"):
			status = http.StatusTooManyRequests
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/query-rate-limited"):
			body = `{"status":"completed","matches_count":1,"flows_count":1}`
		case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/query-rate-limited"):
			status = http.StatusNoContent
			deleted = true
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.String())
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	_, err := client.FetchDayOfTraffic(context.Background(), AsyncQueryRequest{StartDate: "2026-03-01T00:00:00Z"}, nil)
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("error=%v, want download rate-limit error", err)
	}
	if !deleted {
		t.Fatal("async query DELETE was not sent after download rate limit")
	}
}

func TestRequestRejectsCrossOriginAbsoluteURL(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	if _, _, _, err := client.requestWithHeaders(context.Background(), http.MethodGet, "https://evil.example/result", nil, nil); err == nil {
		t.Fatal("requestWithHeaders should reject a cross-origin absolute URL")
	}
}

func TestBuildURLRejectsUnsafeComponents(t *testing.T) {
	t.Parallel()

	client := NewClient("https://pce.example.com", "1", "key", "secret")
	for _, raw := range []string{
		"https://user:pass@pce.example.com/api/v2/orgs/1/workloads",
		"https://pce.example.com/api/v2/orgs/1/workloads#fragment",
		"https://pce.example.com.evil.test/api/v2/orgs/1/workloads",
	} {
		if _, err := client.buildURL(raw); err == nil {
			t.Fatalf("buildURL(%q) should fail", raw)
		}
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
