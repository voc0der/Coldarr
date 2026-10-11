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
	local.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Only selected for local addresses below.
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
