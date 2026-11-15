package webui

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func parseTrustedReverseProxyCIDRs(raw string) ([]netip.Prefix, error) {
	parts := strings.Split(raw, ",")
	proxies := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("parsing trusted reverse proxy CIDR %q: %w", part, err)
		}
		proxies = append(proxies, prefix.Masked())
	}
	if len(proxies) == 0 {
		return nil, fmt.Errorf("trusted reverse proxy CIDRs must include at least one CIDR")
	}
	return proxies, nil
}

func trustedReverseProxyMiddleware(next http.Handler, proxies []netip.Prefix) http.Handler {
	if len(proxies) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		if remoteAddrTrusted(r.RemoteAddr, proxies) {
			applyForwardedHeaders(r2, proxies)
		} else {
			stripForwardedHeaders(r2.Header)
		}
		next.ServeHTTP(w, r2)
	})
}

func remoteAddrTrusted(remoteAddr string, proxies []netip.Prefix) bool {
	addr, ok := remoteIP(remoteAddr)
	return ok && addrTrusted(addr, proxies)
}

func addrTrusted(addr netip.Addr, proxies []netip.Prefix) bool {
	for _, proxy := range proxies {
		if proxy.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = strings.Trim(remoteAddr, "[]")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func applyForwardedHeaders(r *http.Request, proxies []netip.Prefix) {
	forwarded := parseForwardedHeader(r.Header.Values("Forwarded"))
	if forwarded.proto != "" {
		r.URL.Scheme = forwarded.proto
	}
	if forwarded.host != "" {
		r.Host = forwarded.host
	}

	if proto := cleanForwardedProto(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		r.URL.Scheme = proto
	} else if strings.EqualFold(firstCommaPart(r.Header.Get("X-Forwarded-Ssl")), "on") {
		r.URL.Scheme = "https"
	}

	if host := cleanForwardedHost(r.Header.Get("X-Forwarded-Host")); host != "" {
		r.Host = host
	}

	// The request's remote address becomes the client's, so what counts per
	// client - failed sign-ins - counts each one behind the proxy separately.
	if client, ok := forwardedClient(r.Header.Values("X-Forwarded-For"), proxies); ok {
		r.RemoteAddr = client.String()
	}
}

// forwardedClient is the client a trusted proxy passed the request on for,
// read from X-Forwarded-For. Each proxy appends the address it heard from, so
// reading back from the end, past the trusted proxies' own addresses, reaches
// the client. Anything further left, the client sent itself, so the reading
// stops there, and at an entry that isn't an address.
func forwardedClient(values []string, proxies []netip.Prefix) (netip.Addr, bool) {
	var hops []string
	for _, value := range values {
		hops = append(hops, strings.Split(value, ",")...)
	}
	var client netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseForwardedAddr(hops[i])
		if !ok {
			break
		}
		client = addr
		if !addrTrusted(addr, proxies) {
			break
		}
	}
	return client, client.IsValid()
}

// parseForwardedAddr reads one X-Forwarded-For entry: an IP address, perhaps
// in brackets or with a port, as some proxies write them.
func parseForwardedAddr(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if addr, err := netip.ParseAddr(strings.Trim(raw, "[]")); err == nil {
		return addr.Unmap().WithZone(""), true
	}
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		return addrPort.Addr().Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}

func stripForwardedHeaders(h http.Header) {
	h.Del("Forwarded")
	h.Del("X-Forwarded-For")
	h.Del("X-Forwarded-Host")
	h.Del("X-Forwarded-Proto")
	h.Del("X-Forwarded-Ssl")
}

type forwardedValues struct {
	proto string
	host  string
}

func parseForwardedHeader(values []string) forwardedValues {
	for _, value := range values {
		for _, element := range strings.Split(value, ",") {
			var out forwardedValues
			for _, param := range strings.Split(element, ";") {
				key, val, ok := strings.Cut(param, "=")
				if !ok {
					continue
				}
				key = strings.ToLower(strings.TrimSpace(key))
				val = strings.Trim(strings.TrimSpace(val), `"`)
				switch key {
				case "proto":
					out.proto = cleanForwardedProto(val)
				case "host":
					out.host = cleanForwardedHost(val)
				}
			}
			if out.proto != "" || out.host != "" {
				return out
			}
		}
	}
	return forwardedValues{}
}

func cleanForwardedProto(raw string) string {
	proto := strings.ToLower(firstCommaPart(raw))
	switch proto {
	case "http", "https":
		return proto
	default:
		return ""
	}
}

func cleanForwardedHost(raw string) string {
	host := firstCommaPart(raw)
	if host == "" || strings.ContainsAny(host, " \t\r\n/") {
		return ""
	}
	return host
}

func firstCommaPart(raw string) string {
	part, _, _ := strings.Cut(raw, ",")
	return strings.TrimSpace(part)
}
