package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeOrigin_DefaultPorts(t *testing.T) {
	v, err := normalizeOrigin("https://Example.COM")
	if err != nil {
		t.Fatalf("normalizeOrigin unexpected error: %v", err)
	}
	if v != "https://example.com:443" {
		t.Fatalf("unexpected normalized origin: %q", v)
	}

	v, err = normalizeOrigin("http://Example.COM")
	if err != nil {
		t.Fatalf("normalizeOrigin unexpected error: %v", err)
	}
	if v != "http://example.com:80" {
		t.Fatalf("unexpected normalized origin: %q", v)
	}
}

func TestNormalizeOrigin_RejectsNull(t *testing.T) {
	if _, err := normalizeOrigin("null"); err == nil {
		t.Fatalf("expected error for null origin")
	}
}

func TestIsTrustedOriginRequest_SameOriginAllowed(t *testing.T) {
	configMutex.Lock()
	orig := config.PublicBaseURL
	config.PublicBaseURL = "http://localhost:18443"
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config.PublicBaseURL = orig
		configMutex.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPut, "http://localhost:18443/api/config/targets", nil)
	req.Host = "localhost:18443"
	req.Header.Set("Origin", "http://localhost:18443")

	ok, reason := isTrustedOriginRequest(req)
	if !ok {
		t.Fatalf("expected same-origin request to be allowed, reason=%s", reason)
	}
}

func TestIsTrustedOriginRequest_CrossOriginBlocked(t *testing.T) {
	configMutex.Lock()
	orig := config.PublicBaseURL
	config.PublicBaseURL = "http://localhost:18443"
	configMutex.Unlock()
	defer func() {
		configMutex.Lock()
		config.PublicBaseURL = orig
		configMutex.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPut, "http://localhost:18443/api/config/targets", nil)
	req.Host = "localhost:18443"
	req.Header.Set("Origin", "https://evil.example")

	ok, reason := isTrustedOriginRequest(req)
	if ok {
		t.Fatalf("expected cross-origin request to be blocked")
	}
	if reason == "" {
		t.Fatalf("expected block reason")
	}
}

