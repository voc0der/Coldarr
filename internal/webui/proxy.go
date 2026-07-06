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
			applyForwardedHeaders(r2)
		} else {
			stripForwardedHeaders(r2.Header)
		}
		next.ServeHTTP(w, r2)
	})
}

func remoteAddrTrusted(remoteAddr string, proxies []netip.Prefix) bool {
	addr, ok := remoteIP(remoteAddr)
	if !ok {
		return false
	}
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

func applyForwardedHeaders(r *http.Request) {
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
