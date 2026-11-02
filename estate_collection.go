package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PCE resource collections do not support skip/offset. Their synchronous
// response is limited to 500 objects; larger inventories require the documented
// Prefer: respond-async job/datafile workflow. This is a completeness boundary,
// not an application-imposed data-size cap on the streamed async download.
const estateCollectionSyncLimit = 500
const estateCollectionTimeout = 4 * time.Minute

type estatePCEReader struct {
	ctx                 context.Context
	client              *http.Client
	origin, key, secret string
}

type estateCollectionHTTPError struct{ status int }

func (e *estateCollectionHTTPError) Error() string {
	return fmt.Sprintf("estate collection PCE HTTP %d", e.status)
}

func configuredEstatePCEReader(ctx context.Context) (*estatePCEReader, error) {
	configMutex.RLock()
	origin, key, secret := config.PCEURL, config.APIKey, config.APISecret
	configMutex.RUnlock()
	return newEstatePCEReader(ctx, origin, key, secret, httpClient)
}

func newEstatePCEReader(ctx context.Context, rawOrigin, key, secret string, client *http.Client) (*estatePCEReader, error) {
	origin, err := normalizePCEOrigin(strings.TrimSpace(rawOrigin))
	if err != nil {
		return nil, err
	}
	client, err = cloneHTTPClientWithExactOrigin(client, origin)
	if err != nil {
		return nil, err
	}
	// Only this private clone uses the whole bounded collection context. A
	// large streamed result must not inherit the shared client's shorter timeout.
	client.Timeout = 0
	return &estatePCEReader{ctx: ctx, client: client, origin: origin, key: key, secret: secret}, nil
}

func (r *estatePCEReader) open(endpoint string, asynchronous bool) (*http.Response, error) {
	if !isValidRedirectURL(endpoint, r.origin) {
		return nil, errors.New("estate collection link is outside the configured PCE origin")
	}
	for attempt := 0; attempt < 5; attempt++ {
		// Reuse the dashboard's cancellable adaptive rate limiter. Credentials
		// are frozen for the entire collection, not reloaded between job polls.
		if err := apiRateLimiter.waitTamperingTurn(r.ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(r.key, r.secret)
		req.Header.Set("Accept", "application/json")
		if asynchronous {
			req.Header.Set("Prefer", "respond-async")
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request estate collection: %w", err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		status, delay := resp.StatusCode, tamperingRetryDelay(resp.Header)
		closeTamperingResponse(resp)
		if status == http.StatusTooManyRequests {
			apiRateLimiter.noteRateLimit(delay)
		}
		if attempt == 4 || (status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable && status != http.StatusBadGateway && status != http.StatusGatewayTimeout) {
			return nil, &estateCollectionHTTPError{status: status}
		}
		if err := waitTamperingContext(r.ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("estate collection retries exhausted")
}

func (r *estatePCEReader) resolve(baseURL, href string) (string, error) {
	if strings.TrimSpace(href) == "" {
		return "", errors.New("estate asynchronous collection omitted its job/result href")
	}
	if strings.HasPrefix(href, "/orgs/") {
		href = "/api/v2" + href
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	endpoint := base.ResolveReference(ref).String()
	if !isValidRedirectURL(endpoint, r.origin) {
		return "", errors.New("estate collection link is outside the configured PCE origin")
	}
	return endpoint, nil
}

// completeResponse returns a body and whether it is an unbounded async result.
// A server that ignores Prefer is still usable below its documented sync
// ceiling. A full synchronous response must never be mistaken for all objects.
func (r *estatePCEReader) completeResponse(endpoint string) (*http.Response, bool, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, false, err
	}
	q := u.Query()
	for _, key := range []string{"max_results", "skip", "offset"} {
		q.Del(key)
	}
	u.RawQuery = q.Encode()
	endpoint = u.String()
	resp, err := r.open(endpoint, true)
	// Some releases reject Prefer on collections that only support sync GET.
	// Do not retry authorization, unsafe redirects, job failure or malformed
	// downloads through another path that could hide an incomplete inventory.
	var statusErr *estateCollectionHTTPError
	if errors.As(err, &statusErr) && (statusErr.status == 400 || statusErr.status == 405 || statusErr.status == 406 || statusErr.status == 501) {
		resp, err = r.open(endpoint, false)
	}
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, false, nil
	}
	if resp.StatusCode != http.StatusAccepted {
		closeTamperingResponse(resp)
		return nil, false, fmt.Errorf("estate collection returned HTTP %d", resp.StatusCode)
	}
	jobURL, err := r.resolve(endpoint, resp.Header.Get("Location"))
	delay := tamperingRetryDelay(resp.Header)
	closeTamperingResponse(resp)
	if err != nil {
		return nil, false, err
	}
	for {
		if err := waitTamperingContext(r.ctx, delay); err != nil {
			return nil, false, fmt.Errorf("waiting for estate collection: %w", err)
		}
		resp, err := r.open(jobURL, false)
		if err != nil {
			return nil, false, err
		}
		delay = tamperingRetryDelay(resp.Header)
		var job struct {
			Status string `json:"status"`
			Result struct {
				Href string `json:"href"`
			} `json:"result"`
		}
		err = estateDecodeJSON(resp.Body, &job)
		resp.Body.Close()
		if err != nil {
			return nil, false, fmt.Errorf("decode estate collection job: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(job.Status)) {
		case "pending", "running":
			continue
		case "done":
			resultURL, err := r.resolve(jobURL, job.Result.Href)
			if err != nil {
				return nil, false, err
			}
			result, err := r.open(resultURL, false)
			if err != nil {
				return nil, false, err
			}
			if result.StatusCode != http.StatusOK {
				closeTamperingResponse(result)
				return nil, false, fmt.Errorf("estate collection result returned HTTP %d", result.StatusCode)
			}
			return result, true, nil
		case "failed":
			return nil, false, errors.New("estate asynchronous collection job failed")
		default:
			return nil, false, fmt.Errorf("unknown estate collection job status %q", job.Status)
		}
	}
}

func estateDecodeJSON(body io.Reader, target interface{}) error {
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("estate response contains trailing or malformed JSON")
	}
	return nil
}

func (r *estatePCEReader) collection(endpoint string) ([]map[string]interface{}, error) {
	resp, asynchronous, err := r.completeResponse(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rows, rawCount, err := estateDecodeCollection(r.ctx, resp.Body)
	if err == nil && !asynchronous && rawCount >= estateCollectionSyncLimit {
		err = errors.New("PCE returned a full synchronous collection instead of an asynchronous export; inventory may be truncated")
	}
	return rows, err
}

func (r *estatePCEReader) object(endpoint string, target interface{}) error {
	resp, err := r.open(endpoint, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("estate object returned HTTP %d", resp.StatusCode)
	}
	return estateDecodeJSON(resp.Body, target)
}

func (r *estatePCEReader) pending(endpoint string) (map[string]interface{}, error) {
	resp, asynchronous, err := r.completeResponse(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload map[string]interface{}
	if err := estateDecodeJSON(resp.Body, &payload); err != nil {
		return payload, err
	}
	if !asynchronous {
		for _, raw := range payload {
			var rawCount int
			switch values := raw.(type) {
			case []interface{}:
				rawCount = len(values)
			case []map[string]interface{}:
				rawCount = len(values)
			}
			if rawCount >= estateCollectionSyncLimit {
				return payload, errors.New("PCE pending policy response reached the per-type synchronous limit; changes may be truncated")
			}
		}
	}
	return payload, nil
}

// Stream collection rows so a large datafile is not copied into a second byte
// buffer. Preserve fully decoded rows on a broken download, with an error that
// prevents partial counts or label assessments from being labelled complete.
func estateDecodeCollection(ctx context.Context, body io.Reader) ([]map[string]interface{}, int, error) {
	rows := []map[string]interface{}{}
	seen := map[string]bool{}
	rawCount := 0
	decoder := json.NewDecoder(body)
	readArray := func() error {
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var row map[string]interface{}
			if err := decoder.Decode(&row); err != nil {
				return err
			}
			rawCount++
			if row == nil {
				return errors.New("estate collection contains a null object")
			}
			key := estateWorkloadIdentity(row)
			if key == "" {
				encoded, _ := json.Marshal(row)
				key = string(encoded)
			}
			if !seen[key] {
				seen[key] = true
				rows = append(rows, row)
			}
		}
		_, err := decoder.Token()
		return err
	}
	token, err := decoder.Token()
	if err != nil {
		return rows, rawCount, err
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
			case "items", "results", "rule_sets", "rules", "sec_rules", "labels", "versions":
				if found {
					err = errors.New("estate response contains multiple collections")
					break
				}
				found = true
				var opening json.Token
				opening, err = decoder.Token()
				if err == nil && opening != json.Delim('[') {
					err = errors.New("estate collection is not an array")
				}
				if err == nil {
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
			err = errors.New("estate response contains no collection")
		}
	default:
		err = errors.New("unexpected estate collection format")
	}
	if err == nil {
		if _, trailingErr := decoder.Token(); trailingErr != io.EOF {
			err = errors.New("estate collection contains trailing or malformed JSON")
		}
	}
	return rows, rawCount, err
}

func estateFetchCollection(endpoint string) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), estateCollectionTimeout)
	defer cancel()
	reader, err := configuredEstatePCEReader(ctx)
	if err != nil {
		return nil, err
	}
	return reader.collection(endpoint)
}
