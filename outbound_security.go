package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// safeAbsoluteHTTPURLPattern is deliberately kept as a simple, anchored guard.
// Besides rejecting whitespace and non-HTTP schemes before parsing, the boolean
// isValidRedirectURL wrapper below gives static analysis a recognizable guard at
// redirect-sensitive request sites.
var safeAbsoluteHTTPURLPattern = regexp.MustCompile(`(?i)^https?://[^[:space:]]+$`)

var alwaysForbiddenOutboundPrefixes = mustParseOutboundPrefixes(
	"0.0.0.0/8",       // current network, including the unspecified address
	"100.64.0.0/10",   // shared address space (carrier-grade NAT)
	"169.254.0.0/16",  // IPv4 link-local and common metadata endpoints
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"198.18.0.0/15",   // benchmark testing
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved and limited broadcast
	"::/128",          // unspecified
	"fe80::/10",       // IPv6 link-local
	"2001:db8::/32",   // documentation
	"ff00::/8",        // IPv6 multicast
)

var knownMetadataServiceIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS, Azure, and GCP metadata
	net.ParseIP("169.254.170.2"),   // AWS container credentials
	net.ParseIP("168.63.129.16"),   // Azure platform virtual IP
	net.ParseIP("100.100.100.200"), // Alibaba Cloud metadata
	net.ParseIP("fd00:ec2::254"),   // AWS IPv6 metadata
}

type outboundIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type outboundDialContextFunc func(context.Context, string, string) (net.Conn, error)

func mustParseOutboundPrefixes(values ...string) []*net.IPNet {
	prefixes := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, prefix, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// normalizePCEOrigin validates and canonicalizes an origin-only PCE address.
// A trailing slash is accepted, but paths, query strings, fragments, and
// embedded credentials are not part of an origin and are rejected.
func normalizePCEOrigin(raw string) (string, error) {
	parsed, err := parseSafeAbsoluteHTTPURL(raw)
	if err != nil {
		return "", fmt.Errorf("invalid PCE origin: %w", err)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("invalid PCE origin: paths are not allowed")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid PCE origin: query strings and fragments are not allowed")
	}
	if parsed.Scheme != "https" && !isLoopbackOriginHost(parsed.Hostname()) {
		return "", fmt.Errorf("invalid PCE origin: HTTPS is required except for localhost or a loopback IP")
	}
	return normalizedOrigin(parsed), nil
}

// validateAuthorizedPCEOrigin accepts the currently configured PCE origin or
// an exact origin explicitly authorized by the config-file-only allowlist.
func validateAuthorizedPCEOrigin(candidateRaw, currentRaw string, configFileAllowedOrigins []string) (string, error) {
	candidate, err := normalizePCEOrigin(candidateRaw)
	if err != nil {
		return "", err
	}
	currentRaw = strings.TrimSpace(currentRaw)
	if currentRaw != "" {
		current, err := normalizePCEOrigin(currentRaw)
		if err != nil {
			return "", fmt.Errorf("invalid current PCE origin: %w", err)
		}
		if candidate == current {
			return candidate, nil
		}
	}
	allowed, err := normalizedPCEOriginAllowlist(configFileAllowedOrigins)
	if err != nil {
		return "", fmt.Errorf("invalid allowed PCE origin: %w", err)
	}
	if _, ok := allowed[candidate]; ok {
		return candidate, nil
	}
	return "", fmt.Errorf("PCE origin %q is not authorized", candidate)
}

// validateExactOriginPCERequestURL permits request paths and queries only when
// their normalized origin exactly matches the trusted PCE origin.
func validateExactOriginPCERequestURL(rawURL, trustedOrigin string) (*url.URL, error) {
	trusted, err := normalizePCEOrigin(trustedOrigin)
	if err != nil {
		return nil, err
	}
	parsed, err := parseSafeAbsoluteHTTPURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid PCE request URL: %w", err)
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid PCE request URL: fragments are not allowed")
	}
	if normalizedOrigin(parsed) != trusted {
		return nil, fmt.Errorf("PCE request URL origin is not authorized")
	}
	return parsed, nil
}

// isValidRedirectURL is intentionally a small boolean security guard so the
// exact-origin check is visible to CodeQL at callers that issue HTTP requests.
func isValidRedirectURL(rawURL, trustedOrigin string) bool {
	if !safeAbsoluteHTTPURLPattern.MatchString(rawURL) {
		return false
	}
	_, err := validateExactOriginPCERequestURL(rawURL, trustedOrigin)
	return err == nil
}

// cloneHTTPClientWithExactOrigin returns a shallow client clone whose
// redirects cannot leave trustedOrigin. The caller must still apply
// validateExactOriginPCERequestURL to the initial request URL.
func cloneHTTPClientWithExactOrigin(base *http.Client, trustedOrigin string) (*http.Client, error) {
	trusted, err := normalizePCEOrigin(trustedOrigin)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	previousCheckRedirect := base.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many PCE redirects")
		}
		if req == nil || req.URL == nil || !isValidRedirectURL(req.URL.String(), trusted) {
			return errors.New("PCE redirect left the authorized origin")
		}
		if previousCheckRedirect != nil {
			return previousCheckRedirect(req, via)
		}
		return nil
	}
	return &clone, nil
}

// validateWebhookURL validates a webhook destination and returns the one-time
// DNS result that must be pinned by the transport. Public destinations require
// HTTPS. Private or loopback destinations require an exact origin present in a
// config-file-only allowlist. Link-local, multicast, unspecified, reserved,
// documentation, benchmark, and known metadata addresses are always rejected.
func validateWebhookURL(ctx context.Context, rawURL string, configFilePrivateOrigins []string) (*url.URL, []net.IP, error) {
	return validateWebhookURLWithResolver(ctx, rawURL, configFilePrivateOrigins, net.DefaultResolver)
}

func validateWebhookURLWithResolver(ctx context.Context, rawURL string, configFilePrivateOrigins []string, resolver outboundIPResolver) (*url.URL, []net.IP, error) {
	parsed, err := parseSafeAbsoluteHTTPURL(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid webhook URL: %w", err)
	}
	if parsed.Fragment != "" {
		return nil, nil, fmt.Errorf("invalid webhook URL: fragments are not allowed")
	}
	if resolver == nil {
		return nil, nil, errors.New("webhook DNS resolver is unavailable")
	}

	privateOrigins, err := normalizedWebhookOriginAllowlist(configFilePrivateOrigins)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid private webhook origin allowlist: %w", err)
	}
	_, originIsPrivateAuthorized := privateOrigins[normalizedOrigin(parsed)]

	addresses, err := resolveOutboundHost(ctx, parsed.Hostname(), resolver)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve webhook host: %w", err)
	}
	var hasPublic, hasPrivate bool
	pinned := make([]net.IP, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		ip := normalizedIP(address.IP)
		if ip == nil {
			return nil, nil, errors.New("webhook host resolved to an invalid address")
		}
		if isAlwaysForbiddenOutboundIP(ip) {
			return nil, nil, fmt.Errorf("webhook host resolves to a forbidden special-purpose or metadata address")
		}
		if isPubliclyRoutableOutboundIP(ip) {
			hasPublic = true
		} else {
			hasPrivate = true
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		pinned = append(pinned, append(net.IP(nil), ip...))
	}
	if len(pinned) == 0 {
		return nil, nil, errors.New("webhook host resolved to no usable addresses")
	}
	if hasPrivate && !originIsPrivateAuthorized {
		return nil, nil, fmt.Errorf("webhook host resolves to a private address and its exact origin is not authorized by local configuration")
	}
	// A hostname with any public answer is a public destination even if it also
	// has a private answer; the public leg must never be sent over cleartext.
	if hasPublic && parsed.Scheme != "https" {
		return nil, nil, errors.New("public webhook destinations must use HTTPS")
	}
	if !hasPrivate && parsed.Scheme != "https" {
		return nil, nil, errors.New("public webhook destinations must use HTTPS")
	}
	return parsed, pinned, nil
}

// newPinnedWebhookClient constructs a webhook-only client. It bypasses
// environment proxies, refuses redirects, performs no second DNS lookup, and
// permits dialing only the exact validated origin and one of its pinned IPs.
func newPinnedWebhookClient(ctx context.Context, rawURL string, configFilePrivateOrigins []string, timeout time.Duration) (*http.Client, *url.URL, error) {
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	return newPinnedWebhookClientWithDependencies(ctx, rawURL, configFilePrivateOrigins, timeout, net.DefaultResolver, dialer.DialContext)
}

func newPinnedWebhookClientWithDependencies(ctx context.Context, rawURL string, configFilePrivateOrigins []string, timeout time.Duration, resolver outboundIPResolver, dialContext outboundDialContextFunc) (*http.Client, *url.URL, error) {
	parsed, pinned, err := validateWebhookURLWithResolver(ctx, rawURL, configFilePrivateOrigins, resolver)
	if err != nil {
		return nil, nil, err
	}
	if dialContext == nil {
		return nil, nil, errors.New("webhook dialer is unavailable")
	}
	expectedOrigin := normalizedOrigin(parsed)
	expectedHost := canonicalHostname(parsed.Hostname())
	expectedPort := parsed.Port()
	if expectedPort == "" {
		expectedPort = defaultPortForScheme(parsed.Scheme)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid webhook dial address: %w", err)
		}
		if canonicalHostname(host) != expectedHost || port != expectedPort {
			return nil, fmt.Errorf("webhook client refused a dial outside %s", expectedOrigin)
		}
		var lastErr error
		for _, ip := range pinned {
			connection, dialErr := dialContext(dialCtx, network, net.JoinHostPort(ip.String(), expectedPort))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("webhook destination has no pinned addresses")
		}
		return nil, lastErr
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, parsed, nil
}

func parseSafeAbsoluteHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !safeAbsoluteHTTPURLPattern.MatchString(raw) {
		return nil, errors.New("URL must be an absolute HTTP or HTTPS URL without whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" {
		return nil, errors.New("URL must contain a valid scheme and host")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("URL scheme must be HTTP or HTTPS")
	}
	if parsed.User != nil {
		return nil, errors.New("embedded URL credentials are not allowed")
	}
	hostname := canonicalHostname(parsed.Hostname())
	if hostname == "" || strings.Contains(hostname, "%") {
		return nil, errors.New("URL host is invalid")
	}
	port := parsed.Port()
	if port == defaultPortForScheme(parsed.Scheme) {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}
	return parsed, nil
}

func normalizedOrigin(parsed *url.URL) string {
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

func normalizedPCEOriginAllowlist(values []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		origin, err := normalizePCEOrigin(value)
		if err != nil {
			return nil, err
		}
		result[origin] = struct{}{}
	}
	return result, nil
}

func normalizedWebhookOriginAllowlist(values []string) (map[string]struct{}, error) {
	normalized, err := normalizeWebhookPrivateAllowedOrigins(values)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(normalized))
	for _, origin := range normalized {
		result[origin] = struct{}{}
	}
	return result, nil
}

func normalizeWebhookPrivateAllowedOrigins(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		parsed, err := parseSafeAbsoluteHTTPURL(value)
		if err != nil {
			return nil, err
		}
		if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return nil, errors.New("private webhook allowlist entries must be exact origins without paths, queries, or fragments")
		}
		origin := normalizedOrigin(parsed)
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		result = append(result, origin)
	}
	return result, nil
}

func isLoopbackOriginHost(host string) bool {
	host = canonicalHostname(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalHostname(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func defaultPortForScheme(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}

func resolveOutboundHost(ctx context.Context, host string, resolver outboundIPResolver) ([]net.IPAddr, error) {
	if literal := net.ParseIP(host); literal != nil {
		return []net.IPAddr{{IP: literal}}, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("host resolved to no addresses")
	}
	return addresses, nil
}

func normalizedIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	if v6 := ip.To16(); v6 != nil {
		return v6
	}
	return nil
}

func isAlwaysForbiddenOutboundIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, metadataIP := range knownMetadataServiceIPs {
		if metadataIP != nil && ip.Equal(metadataIP) {
			return true
		}
	}
	for _, prefix := range alwaysForbiddenOutboundPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func isPubliclyRoutableOutboundIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !isAlwaysForbiddenOutboundIP(ip)
}
