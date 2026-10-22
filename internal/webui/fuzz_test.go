package webui

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func FuzzReturnTo(f *testing.F) {
	for _, target := range []string{
		"", "/plan?page=1", "//evil.example", `/\evil.example`, "/.//evil.example",
		"/a/../..//evil.example", "/%2f%2fevil.example", "/\t/evil.example",
		"https://evil.example", "/auth/callback", "/login", "/history%zz",
	} {
		f.Add(target)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		target := cleanReturnTo(raw)
		r := httptest.NewRequest(http.MethodGet, "https://coldarr.example/auth/callback", nil)
		w := httptest.NewRecorder()
		http.Redirect(w, r, target, http.StatusFound)
		// Browsers trim C0 controls, remove tabs/newlines, and treat backslashes
		// as slashes in HTTP URLs. Check the actual Location after Go cleans it.
		location := strings.TrimFunc(w.Header().Get("Location"), func(r rune) bool { return r <= ' ' })
		location = strings.NewReplacer("\t", "", "\r", "", "\n", "", `\`, "/").Replace(location)
		u, err := url.Parse(location)
		if err != nil || !strings.HasPrefix(location, "/") || strings.HasPrefix(location, "//") || u.IsAbs() || u.Host != "" {
			t.Fatalf("return target %q became an unsafe Location %q", raw, location)
		}
	})
}

func FuzzUntrustedProxy(f *testing.F) {
	f.Add("https", "evil.example", "for=127.0.0.1;proto=https;host=evil.example")
	f.Add("http, https", "example.com:443", "")
	proxies := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	f.Fuzz(func(t *testing.T, proto, host, forwarded string) {
		r := httptest.NewRequest(http.MethodGet, "http://coldarr.example/login", nil)
		r.RemoteAddr = "198.51.100.1:1234"
		r.Header.Set("Forwarded", forwarded)
		r.Header.Set("X-Forwarded-Proto", proto)
		r.Header.Set("X-Forwarded-Host", host)
		r.Header.Set("X-Forwarded-For", "192.0.2.1")
		r.Header.Set("X-Forwarded-Ssl", "on")
		called := false
		next := http.HandlerFunc(func(_ http.ResponseWriter, got *http.Request) {
			called = true
			if got.Host != "coldarr.example" || got.URL.Scheme != "http" || requestIsHTTPS(got, effectiveOIDCConfig{}) {
				t.Fatal("an untrusted peer changed the request origin")
			}
			for _, name := range []string{"Forwarded", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-For", "X-Forwarded-Ssl"} {
				if len(got.Header.Values(name)) != 0 {
					t.Fatalf("untrusted header %s survived", name)
				}
			}
		})
		trustedReverseProxyMiddleware(next, proxies).ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Fatal("middleware did not call the handler")
		}
		if r.Header.Get("X-Forwarded-Ssl") != "on" {
			t.Fatal("middleware modified the original request")
		}
	})
}

func FuzzProxyCIDRs(f *testing.F) {
	for _, raw := range []string{"192.0.2.1/24", "::1/128", "192.0.2.1/024", "fe80::1%eth0/64", ",", "10.0.0.0/8, ::1/128"} {
		f.Add(raw, "198.51.100.1")
	}
	f.Fuzz(func(t *testing.T, raw, peer string) {
		proxies, err := parseTrustedReverseProxyCIDRs(raw)
		if err != nil {
			return
		}
		if len(proxies) == 0 {
			t.Fatal("accepted an empty trust list")
		}
		for _, p := range proxies {
			if !p.IsValid() || p != p.Masked() || p.Addr().Zone() != "" {
				t.Fatalf("invalid trust prefix %v", p)
			}
		}
		// Bare valid addresses must agree with netip's address-family-aware containment.
		addr, err := netip.ParseAddr(peer)
		if err != nil {
			return
		}
		want := false
		for _, p := range proxies {
			want = want || p.Contains(addr.Unmap())
		}
		if remoteAddrTrusted(peer, proxies) != want {
			t.Fatalf("trust decision for %q disagrees with netip", peer)
		}
	})
}
