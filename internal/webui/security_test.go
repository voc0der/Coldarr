package webui

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestBrowserSecurity(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, origin, fetchSite string
		want                                  int
	}{
		{"login page", "GET", "/login", "", "", 204},
		{"static asset", "GET", "/static/style.css", "", "", 204},
		{"same origin form", "POST", "/login", "https://coldarr.example", "", 204},
		{"same origin htmx", "POST", "/plan/apply", "https://coldarr.example", "same-origin", 204},
		{"cross origin login", "POST", "/login", "https://evil.example", "", 403},
		{"cross origin mutation", "POST", "/settings/tiers", "https://evil.example", "cross-site", 403},
		{"same site sibling", "POST", "/downloads/remove", "https://other.coldarr.example", "same-site", 403},
		{"opaque origin", "POST", "/login", "null", "", 403},
		{"fetch metadata without origin", "POST", "/plan/apply", "", "cross-site", 403},
		{"OIDC callback", "GET", "/auth/callback", "https://idp.example", "cross-site", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := browserSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(tc.method, "https://coldarr.example"+tc.path, nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || called != (tc.want == 204) {
				t.Fatalf("status=%d called=%v; want %d", w.Code, called, tc.want)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatal("missing browser protections")
			}
			for _, directive := range []string{"default-src 'self'", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'self'"} {
				if !strings.Contains(w.Header().Get("Content-Security-Policy"), directive) {
					t.Fatalf("missing CSP directive %q", directive)
				}
			}
			if tc.path != "/static/style.css" && w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("dynamic response may be cached")
			}
		})
	}
}

func TestBrowserSecurityBehindTrustedProxy(t *testing.T) {
	for _, peer := range []string{"192.0.2.1:1234", "198.51.100.1:1234"} {
		h := trustedReverseProxyMiddleware(browserSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})), []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
		r := httptest.NewRequest(http.MethodPost, "http://internal:8478/login", nil)
		r.RemoteAddr = peer
		r.Header.Set("Origin", "https://coldarr.example")
		r.Header.Set("X-Forwarded-Host", "coldarr.example")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusForbidden
		if peer == "192.0.2.1:1234" {
			want = http.StatusNoContent
		}
		if w.Code != want {
			t.Fatalf("peer %s: status=%d want %d", peer, w.Code, want)
		}
	}
}
