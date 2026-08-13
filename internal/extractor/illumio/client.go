package illumio

import (
	"bytes"
	"context"
	"encoding/json"
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
	maxResponseBodySize = 256 << 20
	maxCreateAttempts   = 5
)

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
	return &Client{
		PCEURL:    baseURL,
		OrgID:     orgId,
		APIKey:    apiKey,
		APISecret: apiSecret,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
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

func (c *Client) validateRequestURL(raw string) error {
	base, err := url.Parse(c.PCEURL)
	if err != nil {
		return fmt.Errorf("invalid PCE URL: %w", err)
	}
	target, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid PCE request URL: %w", err)
	}
	if !sameOriginURL(base, target) {
		return fmt.Errorf("cross-origin PCE request rejected")
	}
	return nil
}

func (c *Client) buildURL(path string) string {
	switch {
	case strings.HasPrefix(path, "http://"), strings.HasPrefix(path, "https://"):
		return path
	case strings.HasPrefix(path, "/api/"):
		return fmt.Sprintf("%s%s", c.PCEURL, path)
	case strings.HasPrefix(path, "/orgs/"):
		return fmt.Sprintf("%s/api/v2%s", c.PCEURL, path)
	default:
		return fmt.Sprintf("%s/api/v2/orgs/%s/%s", c.PCEURL, c.OrgID, path)
	}
}

func (c *Client) requestWithHeaders(ctx context.Context, method, path string, body interface{}, extraHeaders map[string]string) ([]byte, int, http.Header, error) {
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
				return nil, 0, nil, ctx.Err()
			}
		}
	}

	url := c.buildURL(path)
	if err := c.validateRequestURL(url); err != nil {
		return nil, 0, nil, err
	}
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, 0, nil, err
		}
		bodyReader = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, nil, err
	}
	req.SetBasicAuth(c.APIKey, c.APISecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-public-api-version", "2")
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize+1))
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), fmt.Errorf("read PCE response: %w", err)
	}
	if len(data) > maxResponseBodySize {
		return nil, resp.StatusCode, resp.Header.Clone(), fmt.Errorf("PCE response exceeded %d MiB limit", maxResponseBodySize>>20)
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
	_, _, _, _ = c.requestWithHeaders(ctx, http.MethodDelete, path, nil, nil)
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
	req.PolicyDecisions = []string{"blocked"}
	req.QueryName = fmt.Sprintf("BT_%s_%d", req.StartDate[:10], time.Now().UnixNano()%1000)

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

	// 2. Poll
	backoff := 5 * time.Second
	for {
		data, code, err := c.request(ctx, "GET", fmt.Sprintf("traffic_flows/async_queries/%s", queryUUID), nil)
		if err == nil && code == 200 {
			var status AsyncQueryStatus
			if err := json.Unmarshal(data, &status); err != nil {
				return nil, fmt.Errorf("decode PCE query status: %w", err)
			}
			if strings.EqualFold(status.Status, "completed") {
				break
			}
			if strings.EqualFold(status.Status, "failed") {
				return nil, fmt.Errorf("PCE query failed")
			}
		}
		select {
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff = time.Duration(float64(backoff) * 1.5)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// 3. Download
	data, code, err := c.request(ctx, "GET", fmt.Sprintf("traffic_flows/async_queries/%s/download", queryUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("download PCE query result: %w", err)
	}
	if code != 200 {
		return nil, fmt.Errorf("download failed (HTTP %d)", code)
	}

	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode PCE query result: %w", err)
	}

	flows := make([]TrafficFlow, 0, len(raw))
	for rowIndex, r := range raw {
		f := TrafficFlow{}
		if src, ok := r["src"].(map[string]interface{}); ok {
			if v, ok := src["ip"].(string); ok {
				f.SrcIP = v
			}
			if wkld, ok := src["workload"].(map[string]interface{}); ok {
				if v, ok := wkld["href"].(string); ok {
					f.SrcWorkloadHref = v
				}
				f.SrcLabels = append(f.SrcLabels, parseFlowLabels(wkld["labels"])...)
			}
		}
		if dst, ok := r["dst"].(map[string]interface{}); ok {
			if v, ok := dst["ip"].(string); ok {
				f.DstIP = v
			}
			if v, ok := dst["fqdn"].(string); ok {
				f.DstFQDN = v
			}
			if wkld, ok := dst["workload"].(map[string]interface{}); ok {
				if v, ok := wkld["href"].(string); ok {
					f.DstWorkloadHref = v
				}
				f.DstLabels = append(f.DstLabels, parseFlowLabels(wkld["labels"])...)
			}
		}
		if svc, ok := r["service"].(map[string]interface{}); ok {
			if v, ok := svc["port"].(float64); ok {
				f.DstPort = int(v)
			}
			if v, ok := svc["proto"].(float64); ok {
				f.Proto = int(v)
			}
			if v, ok := svc["process_name"].(string); ok {
				f.ProcessName = v
			}
		}
		if v, ok := r["num_connections"].(float64); ok {
			f.NumConnections = int(v)
		}
		if ts, ok := r["timestamp_range"].(map[string]interface{}); ok {
			if v, ok := ts["first_detected"].(string); ok {
				f.FirstDetected, err = time.Parse(time.RFC3339, v)
				if err != nil {
					return nil, fmt.Errorf("decode PCE result row %d first_detected: %w", rowIndex+1, err)
				}
			}
			if v, ok := ts["last_detected"].(string); ok {
				f.LastDetected, err = time.Parse(time.RFC3339, v)
				if err != nil {
					return nil, fmt.Errorf("decode PCE result row %d last_detected: %w", rowIndex+1, err)
				}
			}
		}
		if f.FirstDetected.IsZero() {
			return nil, fmt.Errorf("PCE result row %d is missing first_detected", rowIndex+1)
		}
		if f.LastDetected.IsZero() {
			f.LastDetected = f.FirstDetected
		}
		flows = append(flows, f)
	}

	return flows, nil
}
