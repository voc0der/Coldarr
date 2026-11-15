package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestForwardedHeaders_FromATrustedProxy covers the RFC 7239 Forwarded
// header and the X-Forwarded-* family a trusted proxy may send - and the
// junk values that must never become the request's scheme or host.
func TestForwardedHeaders_FromATrustedProxy(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8, ::1/128")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		remote     string
		headers    http.Header
		wantScheme string
		wantHost   string
	}{
		{name: "Forwarded", headers: http.Header{"Forwarded": {`for=192.0.2.60;proto=https;host="coldarr.example"`}}, wantScheme: "https", wantHost: "coldarr.example"},
		{name: "first Forwarded element saying something wins", headers: http.Header{"Forwarded": {`for=192.0.2.60, proto=https;host=second.example`}}, wantScheme: "https", wantHost: "second.example"},
		{name: "Forwarded param without a value is skipped", headers: http.Header{"Forwarded": {`secure;proto=https`}}, wantScheme: "https", wantHost: "internal.example"},
		{name: "X-Forwarded-Proto beats Forwarded", headers: http.Header{"Forwarded": {"proto=http"}, "X-Forwarded-Proto": {"https, http"}}, wantScheme: "https", wantHost: "internal.example"},
		{name: "X-Forwarded-Ssl on", headers: http.Header{"X-Forwarded-Ssl": {"on"}}, wantScheme: "https", wantHost: "internal.example"},
		{name: "unknown proto ignored", headers: http.Header{"X-Forwarded-Proto": {"gopher"}, "Forwarded": {"proto=ftp"}}, wantScheme: "http", wantHost: "internal.example"},
		{name: "host carrying a path ignored", headers: http.Header{"X-Forwarded-Host": {"evil.example/login"}}, wantScheme: "http", wantHost: "internal.example"},
		{name: "host carrying a space ignored", headers: http.Header{"Forwarded": {`host="evil.example x"`}}, wantScheme: "http", wantHost: "internal.example"},
		{name: "IPv6 proxy without a port", remote: "::1", headers: http.Header{"X-Forwarded-Proto": {"https"}}, wantScheme: "https", wantHost: "internal.example"},
		{name: "bracketed IPv6 proxy", remote: "[::1]", headers: http.Header{"X-Forwarded-Proto": {"https"}}, wantScheme: "https", wantHost: "internal.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotScheme, gotHost string
			handler := trustedReverseProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotScheme, gotHost = r.URL.Scheme, r.Host
			}), proxies)

			req := httptest.NewRequest(http.MethodGet, "http://internal.example/healthz", nil)
			req.RemoteAddr = "10.1.2.3:54321"
			if tt.remote != "" {
				req.RemoteAddr = tt.remote
			}
			for k, v := range tt.headers {
				req.Header[k] = v
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if gotScheme != tt.wantScheme || gotHost != tt.wantHost {
				t.Fatalf("scheme, host = %q, %q; want %q, %q", gotScheme, gotHost, tt.wantScheme, tt.wantHost)
			}
		})
	}
}

// TestForwardedClient walks X-Forwarded-For back from the trusted proxy:
// past the trusted proxies' own addresses to the client, never into what the
// client wrote itself.
func TestForwardedClient(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8, fd00::/8")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		values []string
		want   string // "" for none
	}{
		{name: "one client", values: []string{"203.0.113.7"}, want: "203.0.113.7"},
		{name: "what the client sent is skipped", values: []string{"198.51.100.1, 203.0.113.7"}, want: "203.0.113.7"},
		{name: "past a chain of trusted proxies", values: []string{"198.51.100.1, 203.0.113.7, 10.0.0.2, 10.0.0.3"}, want: "203.0.113.7"},
		{name: "header lines in order", values: []string{"198.51.100.1", "203.0.113.7"}, want: "203.0.113.7"},
		{name: "a client inside the trusted ranges", values: []string{"10.9.9.9, 10.0.0.2"}, want: "10.9.9.9"},
		{name: "a port", values: []string{"203.0.113.7:4711"}, want: "203.0.113.7"},
		{name: "a bracketed IPv6 address with a port", values: []string{"[2001:db8::7]:4711"}, want: "2001:db8::7"},
		{name: "a bracketed IPv6 address", values: []string{"[2001:db8::7]"}, want: "2001:db8::7"},
		{name: "an IPv4-mapped address", values: []string{"::ffff:203.0.113.7"}, want: "203.0.113.7"},
		{name: "a zone is dropped", values: []string{"fe80::7%eth0"}, want: "fe80::7"},
		{name: "IPv6 past a trusted IPv6 proxy", values: []string{"2001:db8::7, fd00::2"}, want: "2001:db8::7"},
		{name: "junk where the proxy's entry should be", values: []string{"203.0.113.7, unknown"}, want: ""},
		{name: "junk stops the walk at the last trusted hop", values: []string{"unknown, 10.0.0.2"}, want: "10.0.0.2"},
		{name: "no header", values: nil, want: ""},
		{name: "empty header", values: []string{""}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := forwardedClient(tt.values, proxies)
			if tt.want == "" {
				if ok {
					t.Fatalf("forwardedClient(%q) = %v, want none", tt.values, got)
				}
				return
			}
			if !ok || got.String() != tt.want {
				t.Fatalf("forwardedClient(%q) = %v, %v; want %s", tt.values, got, ok, tt.want)
			}
		})
	}
}

// TestForwardedHeaders_ClientAddress: a trusted proxy's X-Forwarded-For
// becomes the request's remote address; without one, or from anyone else,
// the remote address stays the peer's.
func TestForwardedHeaders_ClientAddress(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, peer, forwardedFor, want string
	}{
		{name: "trusted proxy", peer: "10.1.2.3:54321", forwardedFor: "198.51.100.9, 203.0.113.7", want: "203.0.113.7"},
		{name: "trusted proxy without the header", peer: "10.1.2.3:54321", want: "10.1.2.3:54321"},
		{name: "trusted proxy with an unusable header", peer: "10.1.2.3:54321", forwardedFor: "unknown", want: "10.1.2.3:54321"},
		{name: "untrusted peer", peer: "203.0.113.4:54321", forwardedFor: "198.51.100.9", want: "203.0.113.4:54321"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			handler := trustedReverseProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
			}), proxies)
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = tt.peer
			if tt.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.forwardedFor)
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)
			if got != tt.want {
				t.Fatalf("remote address = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestForwardedHeaders_UnparseableRemoteIsUntrusted: a remote address
// that isn't an IP can't be matched against the trusted ranges, so its
// forwarded headers are stripped like any untrusted client's.
func TestForwardedHeaders_UnparseableRemoteIsUntrusted(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs("0.0.0.0/0")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded []string
	handler := trustedReverseProxyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Ssl"} {
			forwarded = append(forwarded, r.Header.Values(h)...)
		}
	}), proxies)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "unix-socket"
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Ssl"} {
		req.Header.Set(h, "x")
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if len(forwarded) != 0 {
		t.Fatalf("forwarded headers reached the handler: %v", forwarded)
	}
}

func TestParseTrustedReverseProxyCIDRs_EntriesAndMasking(t *testing.T) {
	proxies, err := parseTrustedReverseProxyCIDRs(" , 10.1.2.3/8 ,")
	if err != nil {
		t.Fatalf("parseTrustedReverseProxyCIDRs: %v", err)
	}
	if len(proxies) != 1 || proxies[0].String() != "10.0.0.0/8" {
		t.Fatalf("proxies = %v, want the one CIDR, masked to 10.0.0.0/8", proxies)
	}
	if _, err := parseTrustedReverseProxyCIDRs(" , "); err == nil || !strings.Contains(err.Error(), "at least one CIDR") {
		t.Fatalf("a list of nothing = %v, want an error", err)
	}

	// No trusted ranges: requests pass through untouched.
	var host string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { host = r.Header.Get("X-Forwarded-Host") })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Forwarded-Host", "kept.example")
	trustedReverseProxyMiddleware(next, nil).ServeHTTP(httptest.NewRecorder(), req)
	if host != "kept.example" {
		t.Fatalf("X-Forwarded-Host = %q, want it passed through with no proxies configured", host)
	}
}
