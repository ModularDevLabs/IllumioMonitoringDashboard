package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type staticOutboundResolver struct {
	addresses []net.IPAddr
	err       error
	calls     atomic.Int32
}

func (r *staticOutboundResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	r.calls.Add(1)
	return r.addresses, r.err
}

func TestNormalizePCEOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "canonical HTTPS", raw: "HTTPS://PCE.EXAMPLE.COM:443/", want: "https://pce.example.com", ok: true},
		{name: "nondefault port", raw: "https://pce.example.com:8443", want: "https://pce.example.com:8443", ok: true},
		{name: "IPv6", raw: "https://[2001:4860:4860::8888]:8443/", want: "https://[2001:4860:4860::8888]:8443", ok: true},
		{name: "localhost HTTP", raw: "http://localhost:8080/", want: "http://localhost:8080", ok: true},
		{name: "IPv4 loopback HTTP", raw: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080", ok: true},
		{name: "IPv6 loopback HTTP", raw: "http://[::1]:8080", want: "http://[::1]:8080", ok: true},
		{name: "non-loopback HTTP", raw: "http://pce.example.com:8080", ok: false},
		{name: "private name HTTP", raw: "http://pce.internal:8080", ok: false},
		{name: "private IP HTTP", raw: "http://10.1.2.3:8080", ok: false},
		{name: "path", raw: "https://pce.example.com/api", ok: false},
		{name: "query", raw: "https://pce.example.com/?x=1", ok: false},
		{name: "userinfo", raw: "https://user@pce.example.com", ok: false},
		{name: "wrong scheme", raw: "file:///etc/passwd", ok: false},
		{name: "whitespace", raw: "https://pce.example.com/a b", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizePCEOrigin(test.raw)
			if test.ok && err != nil {
				t.Fatalf("normalizePCEOrigin() error = %v", err)
			}
			if !test.ok && err == nil {
				t.Fatalf("normalizePCEOrigin() = %q, want error", got)
			}
			if got != test.want {
				t.Fatalf("normalizePCEOrigin() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateAuthorizedPCEOrigin(t *testing.T) {
	t.Parallel()
	if got, err := validateAuthorizedPCEOrigin("https://current.example:8443/", "https://CURRENT.example:8443", nil); err != nil || got != "https://current.example:8443" {
		t.Fatalf("current origin: got %q, err %v", got, err)
	}
	if got, err := validateAuthorizedPCEOrigin("https://next.example", "https://current.example", []string{"https://NEXT.example:443/"}); err != nil || got != "https://next.example" {
		t.Fatalf("allowlisted origin: got %q, err %v", got, err)
	}
	if _, err := validateAuthorizedPCEOrigin("https://other.example", "https://current.example", []string{"https://next.example"}); err == nil {
		t.Fatal("unauthorized origin was accepted")
	}
}

func TestExactOriginPCERequestAndRedirectGuard(t *testing.T) {
	t.Parallel()
	trusted := "https://pce.example.com:8443"
	if _, err := validateExactOriginPCERequestURL("https://PCE.example.com:8443/api/v2/orgs/1?x=1", trusted); err != nil {
		t.Fatalf("same-origin request rejected: %v", err)
	}
	for _, raw := range []string{
		"https://evil.example.com/api",
		"https://pce.example.com/api",
		"https://user@pce.example.com:8443/api",
		"javascript:alert(1)",
		"https://pce.example.com:8443/api#fragment",
	} {
		if isValidRedirectURL(raw, trusted) {
			t.Fatalf("isValidRedirectURL(%q) = true", raw)
		}
	}
	if !isValidRedirectURL("https://pce.example.com:8443/api", trusted) {
		t.Fatal("same-origin redirect was rejected")
	}

	client, err := cloneHTTPClientWithExactOrigin(&http.Client{Timeout: time.Second}, trusted)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := http.NewRequest(http.MethodGet, "https://pce.example.com:8443/next", nil)
	if err := client.CheckRedirect(good, []*http.Request{{}}); err != nil {
		t.Fatalf("same-origin redirect rejected: %v", err)
	}
	bad, _ := http.NewRequest(http.MethodGet, "https://evil.example/next", nil)
	if err := client.CheckRedirect(bad, []*http.Request{{}}); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
}

func TestExactOriginClientPreservesStricterRedirectPolicy(t *testing.T) {
	t.Parallel()
	want := errors.New("base policy rejected redirect")
	base := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return want }}
	client, err := cloneHTTPClientWithExactOrigin(base, "https://pce.example.com")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://pce.example.com/next", nil)
	if got := client.CheckRedirect(request, nil); !errors.Is(got, want) {
		t.Fatalf("CheckRedirect() error = %v, want %v", got, want)
	}
}

func TestValidateWebhookURLPolicy(t *testing.T) {
	t.Parallel()
	public := net.IPAddr{IP: net.ParseIP("93.184.216.34")}
	private := net.IPAddr{IP: net.ParseIP("10.1.2.3")}
	tests := []struct {
		name      string
		raw       string
		addresses []net.IPAddr
		allow     []string
		wantErr   string
	}{
		{name: "public HTTPS", raw: "https://hooks.example.test/x", addresses: []net.IPAddr{public}},
		{name: "public HTTP", raw: "http://hooks.example.test/x", addresses: []net.IPAddr{public}, wantErr: "HTTPS"},
		{name: "public HTTP even when allowlisted", raw: "http://hooks.example.test/x", addresses: []net.IPAddr{public}, allow: []string{"http://hooks.example.test"}, wantErr: "HTTPS"},
		{name: "private authorized HTTP", raw: "http://hooks.internal.test:8080/x", addresses: []net.IPAddr{private}, allow: []string{"http://HOOKS.internal.test:8080/"}},
		{name: "private unauthorized", raw: "https://hooks.internal.test/x", addresses: []net.IPAddr{private}, wantErr: "not authorized"},
		{name: "wrong private port", raw: "https://hooks.internal.test:8443/x", addresses: []net.IPAddr{private}, allow: []string{"https://hooks.internal.test"}, wantErr: "not authorized"},
		{name: "mixed answers unauthorized", raw: "https://hooks.example.test/x", addresses: []net.IPAddr{public, private}, wantErr: "not authorized"},
		{name: "link local always forbidden", raw: "http://metadata.test/x", addresses: []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, allow: []string{"http://metadata.test"}, wantErr: "forbidden"},
		{name: "metadata always forbidden", raw: "https://metadata.test/x", addresses: []net.IPAddr{{IP: net.ParseIP("100.100.100.200")}}, allow: []string{"https://metadata.test"}, wantErr: "forbidden"},
		{name: "shared address space forbidden", raw: "https://shared.test/x", addresses: []net.IPAddr{{IP: net.ParseIP("100.64.0.1")}}, allow: []string{"https://shared.test"}, wantErr: "forbidden"},
		{name: "multicast always forbidden", raw: "https://multicast.test/x", addresses: []net.IPAddr{{IP: net.ParseIP("224.0.0.1")}}, allow: []string{"https://multicast.test"}, wantErr: "forbidden"},
		{name: "userinfo", raw: "https://user@hooks.example.test/x", addresses: []net.IPAddr{public}, wantErr: "credentials"},
		{name: "fragment", raw: "https://hooks.example.test/x#frag", addresses: []net.IPAddr{public}, wantErr: "fragments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &staticOutboundResolver{addresses: test.addresses}
			_, _, err := validateWebhookURLWithResolver(context.Background(), test.raw, test.allow, resolver)
			if test.wantErr == "" && err != nil {
				t.Fatalf("validateWebhookURLWithResolver() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validateWebhookURLWithResolver() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestNormalizeWebhookPrivateAllowedOrigins(t *testing.T) {
	t.Parallel()
	got, err := normalizeWebhookPrivateAllowedOrigins([]string{
		" HTTP://Hooks.Internal.Test:80/ ",
		"http://hooks.internal.test",
		"https://hooks.internal.test:8443/",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://hooks.internal.test", "https://hooks.internal.test:8443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeWebhookPrivateAllowedOrigins() = %#v, want %#v", got, want)
	}
	for _, raw := range []string{
		"https://hooks.internal.test/path",
		"https://hooks.internal.test?query=1",
		"https://user@hooks.internal.test",
	} {
		if _, err := normalizeWebhookPrivateAllowedOrigins([]string{raw}); err == nil {
			t.Fatalf("normalizeWebhookPrivateAllowedOrigins(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestPinnedWebhookClientPinsDNSBypassesProxyAndRefusesRedirects(t *testing.T) {
	var redirected atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer redirectTarget.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" || !strings.HasPrefix(r.Host, "hooks.internal.test:") {
			t.Errorf("Host = %q", r.Host)
		}
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(targetURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	raw := "http://hooks.internal.test:" + port + "/hook"
	origin := "http://hooks.internal.test:" + port
	resolver := &staticOutboundResolver{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}

	var dialAddresses []string
	realDialer := &net.Dialer{Timeout: time.Second}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialAddresses = append(dialAddresses, address)
		return realDialer.DialContext(ctx, network, address)
	}
	client, parsed, err := newPinnedWebhookClientWithDependencies(context.Background(), raw, []string{origin}, 2*time.Second, resolver, dial)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("DNS lookup calls = %d, want 1", resolver.calls.Load())
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("pinned webhook transport must disable proxies")
	}

	request, err := http.NewRequest(http.MethodPost, parsed.String(), strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusTemporaryRedirect)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target received %d requests", redirected.Load())
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("DNS was resolved again during dial: calls = %d", resolver.calls.Load())
	}
	if len(dialAddresses) != 1 || !strings.HasPrefix(dialAddresses[0], "127.0.0.1:") {
		t.Fatalf("dial addresses = %v, want pinned loopback address", dialAddresses)
	}
}

func TestPinnedWebhookClientRefusesUseForAnotherOrigin(t *testing.T) {
	t.Parallel()
	resolver := &staticOutboundResolver{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("should not reach network")
	}
	client, _, err := newPinnedWebhookClientWithDependencies(context.Background(), "http://allowed.internal:8080/hook", []string{"http://allowed.internal:8080"}, time.Second, resolver, dial)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://other.internal:8080/hook", nil)
	_, err = client.Do(request)
	if err == nil || !strings.Contains(err.Error(), "refused a dial outside") {
		t.Fatalf("cross-origin client use error = %v", err)
	}
}

func TestPinnedWebhookClientTriesOnlyPinnedAddresses(t *testing.T) {
	t.Parallel()
	resolver := &staticOutboundResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("10.0.0.1")},
		{IP: net.ParseIP("10.0.0.2")},
	}}
	var got []string
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		got = append(got, address)
		return nil, fmt.Errorf("cannot dial %s", address)
	}
	client, parsed, err := newPinnedWebhookClientWithDependencies(context.Background(), "https://internal.example:8443/hook", []string{"https://internal.example:8443"}, time.Second, resolver, dial)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, parsed.String(), nil)
	_, _ = client.Do(request)
	if len(got) != 2 || got[0] != "10.0.0.1:8443" || got[1] != "10.0.0.2:8443" {
		t.Fatalf("dialed %v, want only the two pinned addresses", got)
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("DNS lookup calls = %d, want 1", resolver.calls.Load())
	}
}
