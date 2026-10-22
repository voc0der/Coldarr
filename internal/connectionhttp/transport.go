// Package connectionhttp provides HTTP transport for configured app connections.
package connectionhttp

import (
	"crypto/tls"
	"net/http"
	"net/netip"
	"strings"
)

// Transport accepts local servers' self-signed or mismatched certificates.
// Public IPs and DNS names other than localhost still require valid certificates.
// It is shared so connections can be reused across app clients.
var Transport http.RoundTripper = newTransport()

type transport struct {
	verified *http.Transport
	local    *http.Transport
}

func newTransport() *transport {
	verified := http.DefaultTransport.(*http.Transport).Clone()
	local := verified.Clone()
	// Only literal private/loopback/link-local IPs and localhost use this transport;
	// public hosts (including redirects) use verified. See transport_test.go. Go's
	// client default already refuses anything older than TLS 1.2.
	// Code scanning matches its dismissed alert by this line's text: keep the line
	// as it is, and add anything new after its existing comment.
	// codeql[go/disabled-certificate-check]
	local.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Only selected for local addresses below. // nosemgrep: problem-based-packs.insecure-transport.go-stdlib.bypass-tls-verification.bypass-tls-verification, go.lang.security.audit.crypto.missing-ssl-minversion.missing-ssl-minversion
	return &transport{verified: verified, local: local}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	ip, _ := netip.ParseAddr(host)
	ip = ip.Unmap()
	// Decide per request so a redirect to a public host restores verification.
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return t.local.RoundTrip(req)
	}
	return t.verified.RoundTrip(req)
}
