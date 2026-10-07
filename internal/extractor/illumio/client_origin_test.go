package illumio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOutboundHelpersRejectUnsafeURLsBeforeTransport(t *testing.T) {
	t.Parallel()

	unsafeURLs := []struct {
		name string
		raw  string
	}{
		{name: "cross origin", raw: "https://other.example.test/api/v2/orgs/1/labels"},
		{name: "userinfo", raw: "https://user:secret@pce.example.test/api/v2/orgs/1/labels"},
		{name: "fragment", raw: "https://pce.example.test/api/v2/orgs/1/labels#unexpected"},
		{name: "malformed absolute URL", raw: "https://%zz/api/v2/orgs/1/labels"},
	}

	operations := []struct {
		name         string
		reportsError bool
		run          func(*Client, string) error
	}{
		{
			name:         "metadata request",
			reportsError: true,
			run: func(client *Client, raw string) error {
				_, _, _, err := client.requestWithHeaders(context.Background(), http.MethodGet, raw, nil, nil)
				return err
			},
		},
		{
			name:         "streamed download",
			reportsError: true,
			run: func(client *Client, raw string) error {
				_, _, err := client.downloadTrafficFlows(context.Background(), raw, false, nil)
				return err
			},
		},
		{
			name: "delete cleanup",
			run: func(client *Client, raw string) error {
				// Cleanup is best-effort and intentionally does not return its error.
				// The transport call count below proves validation still stopped it.
				client.deleteAsyncResource(raw)
				return nil
			},
		},
	}

	for _, operation := range operations {
		operation := operation
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			for _, unsafeURL := range unsafeURLs {
				unsafeURL := unsafeURL
				t.Run(unsafeURL.name, func(t *testing.T) {
					t.Parallel()

					client := NewClient("https://pce.example.test", "1", "key", "secret")
					var transportCalls atomic.Int32
					client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
						transportCalls.Add(1)
						return nil, errors.New("unexpected transport call")
					})

					err := operation.run(client, unsafeURL.raw)
					if operation.reportsError && err == nil {
						t.Fatal("unsafe URL was accepted")
					}
					if calls := transportCalls.Load(); calls != 0 {
						t.Fatalf("transport calls = %d, want 0", calls)
					}
				})
			}
		})
	}
}

func TestStreamedDownloadRedirectOriginPolicy(t *testing.T) {
	t.Run("cross-origin redirect is rejected before target request", func(t *testing.T) {
		t.Parallel()

		var targetHits atomic.Int32
		var targetReceivedAuthorization atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits.Add(1)
			if r.Header.Get("Authorization") != "" {
				targetReceivedAuthorization.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}))
		t.Cleanup(target.Close)

		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/result", http.StatusFound)
		}))
		t.Cleanup(origin.Close)

		client := NewClient(origin.URL, "1", "key", "secret")
		flows, code, err := client.downloadTrafficFlows(context.Background(), origin.URL+"/download", false, nil)
		if err == nil || !strings.Contains(err.Error(), "cross-origin redirect rejected") {
			t.Fatalf("download error = %v, want cross-origin redirect rejection", err)
		}
		if code != 0 {
			t.Fatalf("status code = %d, want 0", code)
		}
		if flows != nil {
			t.Fatalf("flows = %#v, want nil", flows)
		}
		if hits := targetHits.Load(); hits != 0 {
			t.Fatalf("redirect target hits = %d, want 0", hits)
		}
		if targetReceivedAuthorization.Load() {
			t.Fatal("redirect target received Authorization")
		}
	})

	t.Run("same-origin redirect succeeds", func(t *testing.T) {
		t.Parallel()

		var resultHits atomic.Int32
		var missingAuthorization atomic.Bool
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)

		mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/result", http.StatusFound)
		})
		mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
			resultHits.Add(1)
			user, password, ok := r.BasicAuth()
			if !ok || user != "key" || password != "secret" {
				missingAuthorization.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"src":{"ip":"10.0.0.1"},"dst":{"ip":"10.0.0.2"},"service":{"port":443,"proto":6},"num_connections":1,"policy_decision":"blocked","timestamp_range":{"first_detected":"2026-10-01T00:00:00Z","last_detected":"2026-10-01T00:01:00Z"}}]`))
		})

		client := NewClient(server.URL, "1", "key", "secret")
		flows, code, err := client.downloadTrafficFlows(context.Background(), server.URL+"/download", false, nil)
		if err != nil {
			t.Fatalf("downloadTrafficFlows() error = %v", err)
		}
		if code != http.StatusOK {
			t.Fatalf("status code = %d, want %d", code, http.StatusOK)
		}
		if len(flows) != 1 || flows[0].SrcIP != "10.0.0.1" || flows[0].DstPort != 443 {
			t.Fatalf("flows = %#v, want one decoded result", flows)
		}
		if hits := resultHits.Load(); hits != 1 {
			t.Fatalf("same-origin result hits = %d, want 1", hits)
		}
		if missingAuthorization.Load() {
			t.Fatal("same-origin redirect did not retain expected Authorization")
		}
	})
}
