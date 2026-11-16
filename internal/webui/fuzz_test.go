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
		`/x/../\evil.example`, "/#/../\x000",
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
			if got.RemoteAddr != "198.51.100.1:1234" {
				t.Fatalf("an untrusted peer changed the client address to %q", got.RemoteAddr)
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

// FuzzForwardedClient: whatever X-Forwarded-For a client sends, a trusted
// proxy (and any trusted proxies after it) append the address they heard
// from, and that address - not one the client wrote - is the client's.
func FuzzForwardedClient(f *testing.F) {
	f.Add("", "203.0.113.7", uint8(0))
	f.Add("198.51.100.1, 10.0.0.1", "203.0.113.7", uint8(1))
	f.Add("unknown, [2001:db8::1]:80", "2001:db8::7", uint8(2))
	f.Add("203.0.113.7,", "198.51.100.9", uint8(3))
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	f.Fuzz(func(t *testing.T, sent, client string, hops uint8) {
		addr, err := netip.ParseAddr(client)
		if err != nil || addr.Zone() != "" || addrTrusted(addr.Unmap(), proxies) {
			return
		}
		want := addr.Unmap()
		chain := []string{addr.String()}
		for i := range int(hops % 4) {
			chain = append(chain, netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 1)}).String())
		}
		header := strings.Join(chain, ", ")
		if sent != "" {
			header = sent + ", " + header
		}
		got, ok := forwardedClient([]string{header}, proxies)
		if !ok || got != want {
			t.Fatalf("X-Forwarded-For %q gave client %v, %v; want %v", header, got, ok, want)
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
