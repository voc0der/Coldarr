package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTrustedReverseProxyCIDRs(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8, 192.168.1.10/32")
	if err != nil {
		t.Fatalf("parseTrustedReverseProxyCIDRs returned error: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("got %d proxies, want 2", len(proxies))
	}
}

func TestParseTrustedReverseProxyCIDRsRejectsInvalid(t *testing.T) {
	if _, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8, nope"); err == nil {
		t.Fatal("parseTrustedReverseProxyCIDRs accepted an invalid CIDR")
	}
}

func TestTrustedReverseProxyMiddlewareAppliesForwardedHeadersForTrustedRemote(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}

	var gotScheme, gotHost string
	handler := trustedReverseProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotScheme = r.URL.Scheme
		gotHost = r.Host
	}), proxies)

	req := httptest.NewRequest(http.MethodGet, "http://internal.example/healthz", nil)
	req.RemoteAddr = "10.1.2.3:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "coldarr.example")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotScheme != "https" {
		t.Fatalf("scheme = %q, want https", gotScheme)
	}
	if gotHost != "coldarr.example" {
		t.Fatalf("host = %q, want coldarr.example", gotHost)
	}
}

func TestTrustedReverseProxyMiddlewareStripsForwardedHeadersForUntrustedRemote(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}

	var gotScheme, gotHost, gotForwardedHost string
	handler := trustedReverseProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotScheme = r.URL.Scheme
		gotHost = r.Host
		gotForwardedHost = r.Header.Get("X-Forwarded-Host")
	}), proxies)

	req := httptest.NewRequest(http.MethodGet, "http://internal.example/healthz", nil)
	req.RemoteAddr = "203.0.113.4:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "coldarr.example")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotScheme != "http" {
		t.Fatalf("scheme = %q, want original http", gotScheme)
	}
	if gotHost != "internal.example" {
		t.Fatalf("host = %q, want original internal.example", gotHost)
	}
	if gotForwardedHost != "" {
		t.Fatalf("forwarded host header was not stripped: %q", gotForwardedHost)
	}
}
