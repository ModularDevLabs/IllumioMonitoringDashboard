package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Events do not support skip/offset pagination. The documented synchronous
// maximum is 10,000; larger collections must use Prefer: respond-async.
const tamperingSyncLimit = 10000

type tamperingEventReader struct {
	ctx                          context.Context
	client                       *http.Client
	origin, baseURL, key, secret string
	events                       []map[string]interface{}
	seen                         map[string]int
	start, end                   time.Time
}

type tamperingUnsafeLinkError struct{}

func (e *tamperingUnsafeLinkError) Error() string {
	return "PCE tampering link is outside the configured origin"
}

func newTamperingEventReader(ctx context.Context, baseURL string, start, end time.Time, client *http.Client) (*tamperingEventReader, error) {
	if start.After(end) {
		return nil, errors.New("tampering query start is after its end")
	}
	reloadConfigIfFileChanged()
	configMutex.RLock()
	pceURL, key, secret := config.PCEURL, config.APIKey, config.APISecret
	configMutex.RUnlock()
	origin, err := normalizePCEOrigin(strings.TrimSpace(pceURL))
	if err != nil {
		return nil, err
	}
	if !isValidRedirectURL(baseURL, origin) {
		return nil, &tamperingUnsafeLinkError{}
	}
	if client == nil {
		client = httpClient
	}
	secureClient, err := cloneHTTPClientWithExactOrigin(client, origin)
	if err != nil {
		return nil, err
	}
	checkRedirect := secureClient.CheckRedirect
	secureClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req == nil || req.URL == nil || !isValidRedirectURL(req.URL.String(), origin) {
			return &tamperingUnsafeLinkError{}
		}
		return checkRedirect(req, via)
	}
	return &tamperingEventReader{ctx: ctx, client: secureClient, origin: origin,
		baseURL: strings.TrimRight(baseURL, "/"), key: key, secret: secret,
		start: start.UTC(), end: end.UTC(), seen: map[string]int{}}, nil
}

// Compatibility entry points share one retrieval strategy and one deadline.
func fetchTamperingEventsPaged(baseURL string, startUTC, endUTC time.Time, client *http.Client) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tamperingQueryMaxDuration)
	defer cancel()
	reader, err := newTamperingEventReader(ctx, baseURL, startUTC, endUTC, client)
	if err != nil {
		return nil, err
	}
	return reader.fetch()
}

func fetchTamperingSlice(baseURL string, startUTC, endUTC time.Time, client *http.Client) ([]map[string]interface{}, error) {
	return fetchTamperingEventsPaged(baseURL, startUTC, endUTC, client)
}

func fetchTamperingEventsBySlices(baseURL string, startUTC, endUTC time.Time, step time.Duration, client *http.Client) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tamperingQueryMaxDuration)
	defer cancel()
	reader, err := newTamperingEventReader(ctx, baseURL, startUTC, endUTC, client)
	if err != nil {
		return nil, err
	}
	if step <= 0 {
		step = time.Hour
	}
	var failures []error
	for start := startUTC; !start.After(endUTC); {
		end := start.Add(step)
		if end.After(endUTC) {
			end = endUTC
		}
		if err := reader.syncRange(start, end); err != nil {
			failures = append(failures, err)
		}
		if !end.Before(endUTC) || ctx.Err() != nil {
			break
		}
		start = end // Inclusive shared boundaries are deduplicated by event identity.
	}
	return reader.events, errors.Join(failures...)
}

func (r *tamperingEventReader) endpoint(start, end time.Time, synchronous bool) string {
	q := url.Values{"event_type": {"agent.tampering"},
		"timestamp[gte]": {start.UTC().Format(time.RFC3339Nano)},
		"timestamp[lte]": {end.UTC().Format(time.RFC3339Nano)}}
	if synchronous {
		q.Set("max_results", strconv.Itoa(tamperingSyncLimit))
	}
	return r.baseURL + "/events?" + q.Encode()
}

func (r *tamperingEventReader) fetch() ([]map[string]interface{}, error) {
	// Do not send max_results for the async request: the downloaded collection
	// must be unbounded, rather than a larger-looking truncated first page.
	resp, asyncErr := r.open(r.endpoint(r.start, r.end, false), true)
	if asyncErr == nil {
		switch resp.StatusCode {
		case http.StatusAccepted:
			href, delay := resp.Header.Get("Location"), tamperingRetryDelay(resp.Header)
			closeTamperingResponse(resp)
			asyncErr = r.readJob(href, delay)
			if asyncErr == nil {
				return r.events, nil
			}
		case http.StatusOK:
			// Some PCE versions ignore Prefer. Keep these observations, but
			// re-query with the documented sync limit to establish completeness.
			_, _, _, asyncErr = r.readEvents(resp.Body, r.start, r.end)
			resp.Body.Close()
		default:
			closeTamperingResponse(resp)
			asyncErr = fmt.Errorf("unexpected async collection HTTP %d", resp.StatusCode)
		}
	}
	var unsafe *tamperingUnsafeLinkError
	if errors.As(asyncErr, &unsafe) || r.ctx.Err() != nil {
		return r.events, fmt.Errorf("partial tampering result: %w", asyncErr)
	}
	// A rejected/failed async job is recoverable if all synchronous time slices
	// can be exhausted. Otherwise retain all evidence from both paths.
	err := r.syncRange(r.start, r.end)
	if err == nil {
		for _, event := range r.events {
			if strings.TrimSpace(extractEventWorkloadName(event)) == "" {
				err = errors.New("retained tampering event still has no workload identity after fallback")
				break
			}
		}
	}
	if err != nil {
		return r.events, fmt.Errorf("partial tampering result: %w", errors.Join(asyncErr, err))
	}
	return r.events, nil
}

func (r *tamperingEventReader) resolve(href string) (string, error) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", errors.New("async collection is missing a job/result href")
	}
	base, _ := url.Parse(r.baseURL + "/events")
	if strings.HasPrefix(href, "/orgs/") {
		// PCE resource hrefs omit the API version, unlike Location URLs.
		href = "/api/v2" + href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", &tamperingUnsafeLinkError{}
	}
	resolved := base.ResolveReference(ref).String()
	if !isValidRedirectURL(resolved, r.origin) {
		return "", &tamperingUnsafeLinkError{}
	}
	return resolved, nil
}

func (r *tamperingEventReader) readJob(href string, delay time.Duration) error {
	jobURL, err := r.resolve(href)
	if err != nil {
		return err
	}
	for {
		if err := waitTamperingContext(r.ctx, delay); err != nil {
			return err
		}
		resp, err := r.open(jobURL, false)
		if err != nil {
			return err
		}
		delay = tamperingRetryDelay(resp.Header)
		var job struct {
			Status string `json:"status"`
			Result struct {
				Href string `json:"href"`
			} `json:"result"`
		}
		err = json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode tampering async job: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(job.Status)) {
		case "done":
			resultURL, err := r.resolve(job.Result.Href)
			if err != nil {
				return err
			}
			result, err := r.open(resultURL, false)
			if err != nil {
				return err
			}
			defer result.Body.Close()
			if result.StatusCode != http.StatusOK {
				return fmt.Errorf("tampering async result returned HTTP %d", result.StatusCode)
			}
			_, _, _, err = r.readEvents(result.Body, r.start, r.end)
			return err
		case "failed":
			return errors.New("tampering asynchronous collection job failed")
		case "pending", "running":
			// Respect the suggested delay, with the overall query deadline.
		default:
			return fmt.Errorf("unknown tampering async job status %q", job.Status)
		}
	}
}

func (r *tamperingEventReader) syncRange(start, end time.Time) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	resp, err := r.open(r.endpoint(start, end, true), false)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		closeTamperingResponse(resp)
		return fmt.Errorf("synchronous tampering collection returned HTTP %d", resp.StatusCode)
	}
	count, earliest, latest, err := r.readEvents(resp.Body, start, end)
	resp.Body.Close()
	if err != nil || count < tamperingSyncLimit {
		return err
	}
	if !start.Before(end) {
		return fmt.Errorf("synchronous event limit reached at %s; complete retrieval requires asynchronous collections", start.Format(time.RFC3339Nano))
	}
	// A full batch at one instant cannot safely be paged by timestamp. Isolate
	// that instant while still retrieving the remainder of the requested range.
	if earliest.Equal(latest) {
		var failures []error
		if start.Before(earliest) {
			failures = append(failures, r.syncRange(start, earliest.Add(-time.Nanosecond)))
		}
		failures = append(failures, r.syncRange(earliest, earliest))
		if latest.Before(end) && r.ctx.Err() == nil {
			failures = append(failures, r.syncRange(latest.Add(time.Nanosecond), end))
		}
		return errors.Join(failures...)
	}
	middle := start.Add(end.Sub(start) / 2)
	if !middle.After(start) {
		// Adjacent representable instants: test both independently.
		return errors.Join(r.syncRange(start, start), r.syncRange(end, end))
	}
	leftErr := r.syncRange(start, middle)
	rightErr := r.syncRange(middle, end)
	return errors.Join(leftErr, rightErr)
}

// Decode one event at a time: do not buffer a second copy of a potentially
// large async download. The compatibility API still retains distinct events.
func (r *tamperingEventReader) readEvents(body io.Reader, start, end time.Time) (int, time.Time, time.Time, error) {
	count := 0
	var earliest, latest time.Time
	invalid := false
	decoder := json.NewDecoder(body)
	readArray := func() error {
		for decoder.More() {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			var event map[string]interface{}
			if err := decoder.Decode(&event); err != nil {
				return err
			}
			count++
			at, ok := eventTimestampUTC(event)
			if !ok || mapString(event, "event_type") != "agent.tampering" || at.Before(start) || at.After(end) {
				invalid = true
				continue
			}
			if strings.TrimSpace(extractEventWorkloadName(event)) == "" {
				// Keep the event evidence, but do not imply the affected-workload
				// count is complete when an event cannot be attributed.
				invalid = true
			}
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
			if latest.IsZero() || at.After(latest) {
				latest = at
			}
			identity := strings.TrimSpace(mapString(event, "href"))
			if identity == "" {
				identity = tamperingEventSignature(event)
			}
			if index, exists := r.seen[identity]; exists {
				// A partial async response may omit attribution that a later
				// synchronous response supplies for the same immutable event.
				// Upgrade that evidence instead of allowing first-wins dedup to
				// turn a recovered response into a complete-but-low VEN count.
				if strings.TrimSpace(extractEventWorkloadName(r.events[index])) == "" && strings.TrimSpace(extractEventWorkloadName(event)) != "" {
					r.events[index] = event
				}
			} else {
				r.seen[identity] = len(r.events)
				r.events = append(r.events, event)
			}
		}
		_, err := decoder.Token() // Closing array.
		return err
	}
	token, err := decoder.Token()
	if err != nil {
		return count, earliest, latest, err
	}
	switch token {
	case json.Delim('['):
		err = readArray()
	case json.Delim('{'):
		found := false
		for decoder.More() && err == nil {
			var key json.Token
			key, err = decoder.Token()
			if err != nil {
				break
			}
			switch key {
			case "items", "results", "events":
				var opening json.Token
				opening, err = decoder.Token()
				if err == nil && opening != json.Delim('[') {
					err = errors.New("event collection is not an array")
				}
				if err == nil {
					found = true
					err = readArray()
				}
			default:
				err = skipTamperingJSONValue(decoder)
			}
		}
		if err == nil {
			_, err = decoder.Token()
		}
		if err == nil && !found {
			err = errors.New("event response contains no collection")
		}
	default:
		err = errors.New("unexpected event collection format")
	}
	if err == nil {
		if _, trailing := decoder.Token(); trailing != io.EOF {
			err = errors.New("unexpected trailing event collection data")
		}
	}
	if err == nil && invalid {
		err = errors.New("PCE returned events outside the requested type/time window or without valid timestamps/workload identity")
	}
	return count, earliest, latest, err
}

func skipTamperingJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '[' || delim == '{' {
		for decoder.More() {
			if err := skipTamperingJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
	}
	return err
}

func closeTamperingResponse(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func tamperingRetryDelay(headers http.Header) time.Duration {
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		const maximumDelay = time.Duration(1<<63 - 1)
		if int64(seconds) > int64(maximumDelay/time.Second) {
			return maximumDelay // The query context still bounds this wait.
		}
		return time.Duration(seconds) * time.Second
	}
	if delay := parseRetryAfter(raw); delay > 0 {
		return delay
	}
	return time.Second
}

func waitTamperingContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Share the dashboard limiter, including adaptive 429 backoff, but allow the
// tampering deadline to cancel a wait instead of leaving a collector stranded.
func (c *apiRateController) waitTamperingTurn(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		c.mu.Lock()
		c.refillLocked(now)
		wait := c.backoffUntil.Sub(now)
		if wait <= 0 && c.tokens >= 1 {
			c.tokens--
			c.cycleUsed++
			c.mu.Unlock()
			return nil
		}
		if wait <= 0 {
			rps := float64(c.currentRPM) / 60
			if rps < .1 {
				rps = .1
			}
			wait = time.Duration((1 - c.tokens) / rps * float64(time.Second))
			if wait < 15*time.Millisecond {
				wait = 15 * time.Millisecond
			}
		}
		c.mu.Unlock()
		if err := waitTamperingContext(ctx, wait); err != nil {
			return err
		}
	}
}

func (r *tamperingEventReader) open(rawURL string, preferAsync bool) (*http.Response, error) {
	if !isValidRedirectURL(rawURL, r.origin) {
		return nil, &tamperingUnsafeLinkError{}
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err := apiRateLimiter.waitTamperingTurn(r.ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(r.key, r.secret)
		req.Header.Set("Accept", "application/json")
		if preferAsync {
			req.Header.Set("Prefer", "respond-async")
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request tampering events: %w", err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		status := resp.StatusCode
		delay := tamperingRetryDelay(resp.Header)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		err = fmt.Errorf("tampering PCE HTTP %d: %s", status, strings.TrimSpace(string(body)))
		if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable && status != http.StatusBadGateway && status != http.StatusGatewayTimeout {
			return nil, err
		}
		if status == http.StatusTooManyRequests {
			apiRateLimiter.noteRateLimit(delay)
		}
		if attempt == 4 {
			return nil, err
		}
		if err := waitTamperingContext(r.ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("tampering request retries exhausted")
}
