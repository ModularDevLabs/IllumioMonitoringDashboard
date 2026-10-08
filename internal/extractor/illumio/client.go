package illumio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxResponseBodySize                  = 256 << 20
	maxCreateAttempts                    = 5
	asyncQueryHeartbeatPeriod            = time.Minute
	trafficDownloadHeartbeatBytes  int64 = 64 << 20
	trafficDownloadHeartbeatPeriod       = 10 * time.Second
)

var (
	// ErrResponseTooLarge identifies a bounded PCE control or metadata response
	// that exceeded the client's per-response safety limit. Traffic result bodies
	// are streamed without this limit, so this error does not indicate that a
	// smaller traffic query window should be attempted.
	ErrResponseTooLarge = errors.New("PCE response too large")
	// ErrQueryResultTruncated identifies a completed async query whose result was
	// capped by the PCE's maximum-row limit.
	ErrQueryResultTruncated = errors.New("PCE query result truncated")
)

func responseTooLargeError() error {
	return fmt.Errorf("PCE response exceeded %d MiB limit: %w", maxResponseBodySize>>20, ErrResponseTooLarge)
}

func responseSnippet(data []byte) string {
	const maxSnippet = 4096
	if len(data) <= maxSnippet {
		return string(data)
	}
	return string(data[:maxSnippet]) + "...[truncated]"
}

type Client struct {
	PCEURL        string
	OrgID         string
	APIKey        string
	APISecret     string
	HTTP          *http.Client
	Mu            sync.Mutex
	CooldownUntil time.Time
	RateLimit     chan bool
}

func NewClient(pceUrl, orgId, apiKey, apiSecret string) *Client {
	baseURL := strings.TrimSuffix(strings.TrimSpace(pceUrl), "/")
	transport := http.DefaultTransport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		isolatedTransport := defaultTransport.Clone()
		isolatedTransport.MaxIdleConns = 32
		isolatedTransport.MaxIdleConnsPerHost = 8
		transport = isolatedTransport
	}
	return &Client{
		PCEURL:    baseURL,
		OrgID:     orgId,
		APIKey:    apiKey,
		APISecret: apiSecret,
		HTTP: &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				base, err := url.Parse(baseURL)
				if err != nil || !sameOriginURL(base, req.URL) {
					return fmt.Errorf("cross-origin redirect rejected")
				}
				return nil
			},
		},
		RateLimit: make(chan bool, 1),
	}
}

func sameOriginURL(left, right *url.URL) bool {
	return left != nil && right != nil &&
		strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Host, right.Host)
}

func (c *Client) validateRequestURL(raw string) (*url.URL, error) {
	base, err := url.Parse(c.PCEURL)
	if err != nil {
		return nil, fmt.Errorf("invalid PCE URL: %w", err)
	}
	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid PCE request URL: %w", err)
	}
	if !sameOriginURL(base, target) {
		return nil, fmt.Errorf("cross-origin PCE request rejected")
	}
	if target.User != nil || target.Fragment != "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, fmt.Errorf("invalid PCE request URL components")
	}
	return target, nil
}

func (c *Client) buildURL(path string) (string, error) {
	var raw string
	switch {
	case strings.HasPrefix(path, "http://"), strings.HasPrefix(path, "https://"):
		raw = path
	case strings.HasPrefix(path, "/api/"):
		raw = fmt.Sprintf("%s%s", c.PCEURL, path)
	case strings.HasPrefix(path, "/orgs/"):
		raw = fmt.Sprintf("%s/api/v2%s", c.PCEURL, path)
	default:
		raw = fmt.Sprintf("%s/api/v2/orgs/%s/%s", c.PCEURL, c.OrgID, path)
	}
	target, err := c.validateRequestURL(raw)
	if err != nil {
		return "", err
	}
	return target.String(), nil
}

func (c *Client) openResponseWithClient(ctx context.Context, httpClient *http.Client, respectCooldown bool, method, path string, body interface{}, extraHeaders map[string]string) (*http.Response, error) {
	if respectCooldown {
		// Global Rate Limit Cool-down
		c.Mu.Lock()
		cooldownUntil := c.CooldownUntil
		c.Mu.Unlock()
		if !cooldownUntil.IsZero() {
			wait := time.Until(cooldownUntil)
			if wait > 0 {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
	}

	requestURL, err := c.buildURL(path)
	if err != nil {
		return nil, err
	}
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.APIKey, c.APISecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-public-api-version", "2")
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}

	// requestURL is derived from a server-side saved PCE profile and is checked
	// against that exact origin. Async Location URLs and redirects are rejected
	// unless they retain the same scheme and authority.
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) openResponseWithHeaders(ctx context.Context, method, path string, body interface{}, extraHeaders map[string]string) (*http.Response, error) {
	return c.openResponseWithClient(ctx, c.HTTP, true, method, path, body, extraHeaders)
}

func (c *Client) requestWithHeaders(ctx context.Context, method, path string, body interface{}, extraHeaders map[string]string) ([]byte, int, http.Header, error) {
	resp, err := c.openResponseWithHeaders(ctx, method, path, body, extraHeaders)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxResponseBodySize {
		return nil, resp.StatusCode, resp.Header.Clone(), responseTooLargeError()
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize+1))
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), fmt.Errorf("read PCE response: %w", err)
	}
	if len(data) > maxResponseBodySize {
		return nil, resp.StatusCode, resp.Header.Clone(), responseTooLargeError()
	}

	if resp.StatusCode == 429 {
		c.Mu.Lock()
		c.CooldownUntil = time.Now().Add(60 * time.Second)
		c.Mu.Unlock()
		return data, 429, resp.Header.Clone(), fmt.Errorf("rate limit hit")
	}

	return data, resp.StatusCode, resp.Header.Clone(), nil
}

func (c *Client) deleteAsyncResource(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Cleanup must not wait behind a query's rate-limit cooldown; doing so can
	// consume the entire cleanup deadline without ever sending DELETE.
	resp, err := c.openResponseWithClient(ctx, c.HTTP, false, http.MethodDelete, path, nil, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
}

func (c *Client) request(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	data, code, _, err := c.requestWithHeaders(ctx, method, path, body, nil)
	return data, code, err
}

func retryAfterDelay(headers http.Header) time.Duration {
	if headers == nil {
		return 5 * time.Second
	}
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return 5 * time.Second
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 {
		return 5 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) getCollection(ctx context.Context, path string, target interface{}) error {
	data, code, headers, err := c.requestWithHeaders(ctx, "GET", path, nil, map[string]string{"Prefer": "respond-async"})
	if err != nil && code != http.StatusAccepted {
		return err
	}

	switch code {
	case http.StatusOK:
		return json.Unmarshal(data, target)
	case http.StatusAccepted:
		jobPath := headers.Get("Location")
		if jobPath == "" {
			return fmt.Errorf("async collection request missing job location")
		}

		delay := retryAfterDelay(headers)
		for {
			jobData, jobCode, jobHeaders, jobErr := c.requestWithHeaders(ctx, "GET", jobPath, nil, nil)
			if jobErr == nil && jobCode == http.StatusOK {
				var job AsyncJobStatus
				if err := json.Unmarshal(jobData, &job); err != nil {
					return err
				}

				switch strings.ToLower(job.Status) {
				case "done":
					if job.Result.Href == "" {
						return fmt.Errorf("async collection completed without result href")
					}
					resultData, resultCode, _, resultErr := c.requestWithHeaders(ctx, "GET", job.Result.Href, nil, nil)
					if resultErr != nil {
						return resultErr
					}
					if resultCode != http.StatusOK {
						return fmt.Errorf("async collection download failed (HTTP %d)", resultCode)
					}
					c.deleteAsyncResource(jobPath)
					return json.Unmarshal(resultData, target)
				case "failed":
					return fmt.Errorf("async collection job failed")
				}
			}

			delay = retryAfterDelay(jobHeaders)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	default:
		return fmt.Errorf("PCE returned %d", code)
	}
}

func parseFlowLabels(raw interface{}) []FlowLabel {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}

	labels := make([]FlowLabel, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		key, _ := m["key"].(string)
		value, _ := m["value"].(string)
		if key == "" && value == "" {
			continue
		}
		labels = append(labels, FlowLabel{Key: key, Value: value})
	}

	return labels
}

func parseTrafficFlowRow(row map[string]interface{}, rowIndex int, blockedOnly bool) (TrafficFlow, error) {
	flow := TrafficFlow{}
	if src, ok := row["src"].(map[string]interface{}); ok {
		if value, ok := src["ip"].(string); ok {
			flow.SrcIP = value
		}
		if workload, ok := src["workload"].(map[string]interface{}); ok {
			if value, ok := workload["href"].(string); ok {
				flow.SrcWorkloadHref = value
			}
			flow.SrcLabels = append(flow.SrcLabels, parseFlowLabels(workload["labels"])...)
		}
	}
	if dst, ok := row["dst"].(map[string]interface{}); ok {
		if value, ok := dst["ip"].(string); ok {
			flow.DstIP = value
		}
		if value, ok := dst["fqdn"].(string); ok {
			flow.DstFQDN = value
		}
		if workload, ok := dst["workload"].(map[string]interface{}); ok {
			if value, ok := workload["href"].(string); ok {
				flow.DstWorkloadHref = value
			}
			flow.DstLabels = append(flow.DstLabels, parseFlowLabels(workload["labels"])...)
		}
	}
	if service, ok := row["service"].(map[string]interface{}); ok {
		if value, ok := service["port"].(float64); ok {
			flow.DstPort = int(value)
		}
		if value, ok := service["proto"].(float64); ok {
			flow.Proto = int(value)
		}
		if value, ok := service["process_name"].(string); ok {
			flow.ProcessName = value
		}
	}
	if value, ok := row["num_connections"].(float64); ok {
		flow.NumConnections = int(value)
	}
	if value, ok := row["policy_decision"].(string); ok {
		flow.PolicyDecision = strings.ToLower(strings.TrimSpace(value))
	}
	if value, ok := row["draft_policy_decision"].(string); ok {
		flow.DraftDecision = strings.ToLower(strings.TrimSpace(value))
	}
	if timestampRange, ok := row["timestamp_range"].(map[string]interface{}); ok {
		if value, ok := timestampRange["first_detected"].(string); ok {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return TrafficFlow{}, fmt.Errorf("decode PCE result row %d first_detected: %w", rowIndex, err)
			}
			flow.FirstDetected = parsed
		}
		if value, ok := timestampRange["last_detected"].(string); ok {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return TrafficFlow{}, fmt.Errorf("decode PCE result row %d last_detected: %w", rowIndex, err)
			}
			flow.LastDetected = parsed
		}
	}
	if flow.FirstDetected.IsZero() {
		return TrafficFlow{}, fmt.Errorf("PCE result row %d is missing first_detected", rowIndex)
	}
	if flow.LastDetected.IsZero() {
		flow.LastDetected = flow.FirstDetected
	}
	if flow.PolicyDecision == "" {
		flow.PolicyDecision = "unknown"
		if blockedOnly {
			flow.PolicyDecision = "blocked"
		}
	}
	return flow, nil
}

type trafficDownloadTracker struct {
	reader          io.Reader
	logFn           func(string)
	bytesRead       int64
	rowsDecoded     int
	lastLoggedBytes int64
	lastLoggedAt    time.Time
}

func newTrafficDownloadTracker(reader io.Reader, logFn func(string)) *trafficDownloadTracker {
	tracker := &trafficDownloadTracker{reader: reader, logFn: logFn, lastLoggedAt: time.Now()}
	if logFn != nil {
		logFn("PCE query result download started; byte counts reflect response-body bytes after HTTP decompression, when applicable.")
	}
	return tracker
}

func formatTrafficDownloadCounts(bytesRead int64, rowsDecoded int) string {
	return fmt.Sprintf("bytes=%d (%.1f MiB) rows=%d", bytesRead, float64(bytesRead)/(1<<20), rowsDecoded)
}

func (t *trafficDownloadTracker) Read(buffer []byte) (int, error) {
	read, err := t.reader.Read(buffer)
	t.bytesRead += int64(read)
	if t.logFn != nil && (t.bytesRead-t.lastLoggedBytes >= trafficDownloadHeartbeatBytes || time.Since(t.lastLoggedAt) >= trafficDownloadHeartbeatPeriod) {
		t.logFn(fmt.Sprintf("PCE query result download progress: %s.", formatTrafficDownloadCounts(t.bytesRead, t.rowsDecoded)))
		t.lastLoggedBytes = t.bytesRead
		t.lastLoggedAt = time.Now()
	}
	return read, err
}

func (t *trafficDownloadTracker) rowDecoded() {
	t.rowsDecoded++
}

func (t *trafficDownloadTracker) finish(err error) {
	if t.logFn == nil {
		return
	}
	if err != nil {
		t.logFn(fmt.Sprintf("PCE query result download failed: %s error=%v.", formatTrafficDownloadCounts(t.bytesRead, t.rowsDecoded), err))
		return
	}
	t.logFn(fmt.Sprintf("PCE query result download complete: %s.", formatTrafficDownloadCounts(t.bytesRead, t.rowsDecoded)))
}

func decodeTrafficFlowResponse(body *trafficDownloadTracker, blockedOnly bool) ([]TrafficFlow, error) {
	decoder := json.NewDecoder(body)
	decodeError := func(err error) error {
		return fmt.Errorf("decode PCE query result: %w", err)
	}

	opening, err := decoder.Token()
	if err != nil {
		return nil, decodeError(err)
	}
	openingDelimiter, ok := opening.(json.Delim)
	if !ok || openingDelimiter != '[' {
		return nil, fmt.Errorf("decode PCE query result: expected a JSON array")
	}

	flows := make([]TrafficFlow, 0)
	for rowIndex := 1; decoder.More(); rowIndex++ {
		var row map[string]interface{}
		if err := decoder.Decode(&row); err != nil {
			return nil, decodeError(err)
		}
		flow, err := parseTrafficFlowRow(row, rowIndex, blockedOnly)
		if err != nil {
			return nil, err
		}
		flows = append(flows, flow)
		body.rowDecoded()
	}

	closing, err := decoder.Token()
	if err != nil {
		return nil, decodeError(err)
	}
	closingDelimiter, ok := closing.(json.Delim)
	if !ok || closingDelimiter != ']' {
		return nil, fmt.Errorf("decode PCE query result: expected the JSON array to end")
	}

	var trailing interface{}
	err = decoder.Decode(&trailing)
	if err == nil {
		return nil, fmt.Errorf("decode PCE query result: unexpected trailing JSON value")
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode PCE query result trailing data: %w", err)
	}
	return flows, nil
}

func (c *Client) downloadTrafficFlows(ctx context.Context, path string, blockedOnly bool, logFn func(string)) ([]TrafficFlow, int, error) {
	// Traffic downloads intentionally do not use maxResponseBodySize. They can
	// legitimately exceed the control-response limit and are decoded one row at
	// a time so the raw response and a second full row collection are never held.
	// The normal client's short timeout is appropriate for control responses but
	// includes body reads. Use a value-copy with that aggregate timeout disabled
	// so the caller's chunk context remains the download deadline. The transport
	// and same-origin redirect policy are retained and the shared client is not
	// mutated while other chunk workers use it.
	downloadHTTPClient := *c.HTTP
	downloadHTTPClient.Timeout = 0
	resp, err := c.openResponseWithClient(ctx, &downloadHTTPClient, true, http.MethodGet, path, nil, nil)
	if err != nil {
		if logFn != nil {
			logFn(fmt.Sprintf("PCE query result download failed before response: %s error=%v.", formatTrafficDownloadCounts(0, 0), err))
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.Mu.Lock()
		c.CooldownUntil = time.Now().Add(60 * time.Second)
		c.Mu.Unlock()
		if logFn != nil {
			logFn(fmt.Sprintf("PCE query result download failed: %s error=rate limit hit.", formatTrafficDownloadCounts(0, 0)))
		}
		return nil, resp.StatusCode, fmt.Errorf("rate limit hit")
	}
	if resp.StatusCode != http.StatusOK {
		if logFn != nil {
			logFn(fmt.Sprintf("PCE query result download failed: %s error=HTTP %d.", formatTrafficDownloadCounts(0, 0), resp.StatusCode))
		}
		return nil, resp.StatusCode, nil
	}
	tracker := newTrafficDownloadTracker(resp.Body, logFn)
	flows, err := decodeTrafficFlowResponse(tracker, blockedOnly)
	tracker.finish(err)
	return flows, resp.StatusCode, err
}

func (c *Client) GetLabels(ctx context.Context) ([]Label, error) {
	var res []Label
	err := c.getCollection(ctx, "labels", &res)
	return res, err
}

func (c *Client) GetServices(ctx context.Context) ([]Service, error) {
	var res []Service
	err := c.getCollection(ctx, "sec_policy/active/services", &res)
	return res, err
}

func (c *Client) GetIPLists(ctx context.Context) ([]IPList, error) {
	var res []IPList
	err := c.getCollection(ctx, "sec_policy/active/ip_lists", &res)
	return res, err
}

func (c *Client) GetLabelGroups(ctx context.Context) ([]LabelGroup, error) {
	var res []LabelGroup
	err := c.getCollection(ctx, "sec_policy/active/label_groups", &res)
	return res, err
}

func (c *Client) GetUserGroups(ctx context.Context) ([]UserGroup, error) {
	var res []UserGroup
	err := c.getCollection(ctx, "security_principals", &res)
	return res, err
}

func (c *Client) GetVirtualServices(ctx context.Context) ([]VirtualService, error) {
	var res []VirtualService
	err := c.getCollection(ctx, "sec_policy/active/virtual_services", &res)
	return res, err
}

func (c *Client) GetVirtualServers(ctx context.Context) ([]VirtualServer, error) {
	var res []VirtualServer
	err := c.getCollection(ctx, "sec_policy/active/virtual_servers", &res)
	return res, err
}

func (c *Client) TestConnection(ctx context.Context) error {
	data, code, err := c.request(ctx, "GET", "labels?max_results=1", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("PCE returned %d", code)
	}

	var labels []Label
	if err := json.Unmarshal(data, &labels); err != nil {
		return err
	}

	return nil
}

func (c *Client) GetTrafficFlowsDatabaseMetrics(ctx context.Context) (TrafficFlowsDatabaseMetrics, error) {
	data, code, err := c.request(ctx, "GET", "traffic_flows/database_metrics", nil)
	if err != nil {
		return TrafficFlowsDatabaseMetrics{}, err
	}
	if code != http.StatusOK {
		return TrafficFlowsDatabaseMetrics{}, fmt.Errorf("PCE returned %d", code)
	}

	var metrics TrafficFlowsDatabaseMetrics
	if err := json.Unmarshal(data, &metrics); err != nil {
		return TrafficFlowsDatabaseMetrics{}, err
	}
	return metrics, nil
}

func (c *Client) FetchDayOfTraffic(ctx context.Context, req AsyncQueryRequest, logFn func(string)) ([]TrafficFlow, error) {
	if len(req.StartDate) < 10 {
		return nil, fmt.Errorf("invalid query start date")
	}
	req.MaxResults = 200000
	if req.PolicyDecisions == nil {
		// Preserve the historical safe default for callers that do not select a
		// scope. A non-nil empty slice intentionally requests all decisions.
		req.PolicyDecisions = []string{"blocked"}
	}
	queryPrefix := "AT"
	if len(req.PolicyDecisions) == 1 && strings.EqualFold(req.PolicyDecisions[0], "blocked") {
		queryPrefix = "BT"
	}
	req.QueryName = fmt.Sprintf("%s_%s_%d", queryPrefix, req.StartDate[:10], time.Now().UnixNano()%1000)

	// 1. Create
	var queryUUID string
	for attempt := 1; attempt <= maxCreateAttempts; attempt++ {
		data, code, err := c.request(ctx, "POST", "traffic_flows/async_queries", req)
		if err == nil && (code == 201 || code == 202) {
			var status AsyncQueryStatus
			if err := json.Unmarshal(data, &status); err != nil {
				return nil, fmt.Errorf("decode PCE query creation response: %w", err)
			}
			parts := strings.Split(status.Href, "/")
			if len(parts) > 0 && parts[len(parts)-1] != "" {
				queryUUID = parts[len(parts)-1]
				break
			}
			return nil, fmt.Errorf("PCE query creation response did not contain a query identifier")
		}
		if code == 406 || code == 400 || code == 401 || code == 403 {
			return nil, fmt.Errorf("PCE rejected request (HTTP %d): %s", code, responseSnippet(data))
		}
		if attempt == maxCreateAttempts {
			if err != nil {
				return nil, fmt.Errorf("create PCE query after %d attempts: %w", attempt, err)
			}
			return nil, fmt.Errorf("create PCE query after %d attempts: HTTP %d", attempt, code)
		}
		if logFn != nil {
			logFn(fmt.Sprintf("PCE query creation attempt %d/%d failed; retrying...", attempt, maxCreateAttempts))
		}
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer c.deleteAsyncResource(fmt.Sprintf("traffic_flows/async_queries/%s", queryUUID))
	if logFn != nil {
		logFn("PCE async query accepted; waiting for completion.")
	}

	// 2. Poll
	backoff := 5 * time.Second
	completedStatus := AsyncQueryStatus{}
	pollStarted := time.Now()
	lastHeartbeat := time.Time{}
	lastStatus := ""
	for {
		data, code, err := c.request(ctx, "GET", fmt.Sprintf("traffic_flows/async_queries/%s", queryUUID), nil)
		if err == nil && code == 200 {
			var status AsyncQueryStatus
			if err := json.Unmarshal(data, &status); err != nil {
				return nil, fmt.Errorf("decode PCE query status: %w", err)
			}
			normalizedStatus := strings.ToLower(strings.TrimSpace(status.Status))
			if normalizedStatus == "" {
				normalizedStatus = "unknown"
			}
			if logFn != nil && (normalizedStatus != lastStatus || lastHeartbeat.IsZero() || time.Since(lastHeartbeat) >= asyncQueryHeartbeatPeriod) {
				countDetails := ""
				if status.MatchesCount > 0 || status.FlowsCount > 0 {
					countDetails = fmt.Sprintf(" (matches=%d, available=%d)", status.MatchesCount, status.FlowsCount)
				}
				logFn(fmt.Sprintf("PCE async query status %s after %s%s.", normalizedStatus, time.Since(pollStarted).Round(time.Second), countDetails))
				lastHeartbeat = time.Now()
				lastStatus = normalizedStatus
			}
			if normalizedStatus == "completed" {
				completedStatus = status
				break
			}
			if normalizedStatus == "failed" {
				return nil, fmt.Errorf("PCE query failed")
			}
		} else if logFn != nil && (lastHeartbeat.IsZero() || time.Since(lastHeartbeat) >= asyncQueryHeartbeatPeriod) {
			if err != nil {
				logFn(fmt.Sprintf("PCE async query status check is retrying after %s: %v.", time.Since(pollStarted).Round(time.Second), err))
			} else {
				logFn(fmt.Sprintf("PCE async query status check is retrying after %s (HTTP %d).", time.Since(pollStarted).Round(time.Second), code))
			}
			lastHeartbeat = time.Now()
		}
		select {
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff = time.Duration(float64(backoff) * 1.5)
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if completedStatus.MatchesCount > req.MaxResults {
		return nil, fmt.Errorf("PCE query matched %d rows, exceeding the %d-row maximum (PCE reported %d available); use a smaller chunk interval to avoid an incomplete export: %w", completedStatus.MatchesCount, req.MaxResults, completedStatus.FlowsCount, ErrQueryResultTruncated)
	}

	// 3. Download
	blockedOnly := len(req.PolicyDecisions) == 1 && strings.EqualFold(req.PolicyDecisions[0], "blocked")
	flows, code, err := c.downloadTrafficFlows(ctx, fmt.Sprintf("traffic_flows/async_queries/%s/download", queryUUID), blockedOnly, logFn)
	if err != nil {
		return nil, fmt.Errorf("download PCE query result: %w", err)
	}
	if code != 200 {
		return nil, fmt.Errorf("download failed (HTTP %d)", code)
	}
	return flows, nil
}
