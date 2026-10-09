package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServe_ReportsWhyItCannotStart: serve only returns when the GUI can't
// run, and each reason is reported - a config it can't load, listen
// options that don't fit together, or an address that's taken.
func TestServe_ReportsWhyItCannotStart(t *testing.T) {
	t.Setenv("COLDARR_PASSWORD", "pw")
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	broken := filepath.Join(t.TempDir(), "coldarr.yaml")
	if err := os.WriteFile(broken, []byte("tiers: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "coldarr.yaml") // a fresh install: no file yet

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "unreadable config", args: []string{"--config", broken, "serve"}, wantErr: "parsing config"},
		{name: "certificate without a key", args: []string{"--config", fresh, "serve", "--listen", "127.0.0.1:0", "--tls-cert-file", "cert.pem"}, wantErr: "both TLS certificate and key files"},
		{name: "bad proxy CIDR", args: []string{"--config", fresh, "serve", "--trusted-reverse-proxies-cidr", "10.0.0.0/33"}, wantErr: "parsing trusted reverse proxy CIDR"},
		{name: "address taken", args: []string{"--config", fresh, "serve", "--listen", taken.Addr().String()}, wantErr: "address already in use"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := runColdarr(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("coldarr %v = %v, want an error containing %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestServe_FlagDefaultsComeFromTheEnvironment: every serve flag can be set
// through the container's environment instead, with the COLDARR_-prefixed
// proxy variable winning over the bare one.
func TestServe_FlagDefaultsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("COLDARR_LISTEN_ADDR", "127.0.0.1:9000")
	t.Setenv("COLDARR_TLS_CERT_FILE", "/certs/cert.pem")
	t.Setenv("COLDARR_TLS_KEY_FILE", "/certs/key.pem")
	t.Setenv("TRUSTED_REVERSE_PROXIES_CIDR", "192.168.0.0/16")
	t.Setenv("COLDARR_TRUSTED_REVERSE_PROXIES_CIDR", "10.0.0.0/8")

	flags := newServeCmd().Flags()
	for flag, want := range map[string]string{
		"listen":                       "127.0.0.1:9000",
		"tls-cert-file":                "/certs/cert.pem",
		"tls-key-file":                 "/certs/key.pem",
		"trusted-reverse-proxies-cidr": "10.0.0.0/8",
	} {
		if got := flags.Lookup(flag).DefValue; got != want {
			t.Errorf("--%s default = %q, want %q", flag, got, want)
		}
	}

	t.Setenv("COLDARR_TRUSTED_REVERSE_PROXIES_CIDR", "")
	if got := envFirst("COLDARR_TRUSTED_REVERSE_PROXIES_CIDR", "TRUSTED_REVERSE_PROXIES_CIDR"); got != "192.168.0.0/16" {
		t.Errorf("envFirst fallback = %q, want the bare variable's value", got)
	}
	if got := envFirst("COLDARR_TEST_UNSET_1", "COLDARR_TEST_UNSET_2"); got != "" {
		t.Errorf("envFirst with nothing set = %q, want empty", got)
	}
}

func TestNormalizeListenAddr(t *testing.T) {
	for addr, want := range map[string]string{
		"8478":           ":8478",
		":8478":          ":8478",
		"0.0.0.0:8478":   "0.0.0.0:8478",
		"localhost:8478": "localhost:8478",
		"":               "",
	} {
		if got := normalizeListenAddr(addr); got != want {
			t.Errorf("normalizeListenAddr(%q) = %q, want %q", addr, got, want)
		}
	}
}
