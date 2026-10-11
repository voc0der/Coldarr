package connectionhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalCertificates(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tr := newTransport()
	defer tr.local.CloseIdleConnections()
	defer tr.verified.CloseIdleConnections()
	// Keep every request on the test server, whose certificate is untrusted
	// and does not match the private addresses or public names below.
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	tr.local.DialContext, tr.verified.DialContext = dial, dial
	tr.local.Proxy, tr.verified.Proxy = nil, nil
	client := &http.Client{Transport: tr}

	for _, tc := range []struct {
		host  string
		local bool
	}{
		{"172.30.12.102", true},
		{"10.0.0.2", true},
		{"192.168.1.2", true},
		{"127.0.0.1", true},
		{"169.254.1.2", true},
		{"[::1]", true},
		{"[fd00::2]", true},
		{"[fe80::2]", true},
		{"[::ffff:172.30.12.102]", true},
		{"localhost", true},
		{"example.com", false},
		{"8.8.8.8", false},
		{"172.32.0.1", false},
		{"[2001:4860:4860::8888]", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			resp, err := client.Get("https://" + tc.host)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if tc.local {
				if err != nil {
					t.Fatalf("local certificate rejected: %v", err)
				}
			} else {
				var certErr *tls.CertificateVerificationError
				if !errors.As(err, &certErr) {
					t.Fatalf("error = %v, want certificate verification failure", err)
				}
			}
		})
	}
}
