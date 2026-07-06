package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/webui"
)

func newServeCmd() *cobra.Command {
	defaultAddr := ":8478"
	if v := os.Getenv("COLDARR_LISTEN_ADDR"); v != "" {
		defaultAddr = v
	}
	defaultTLSCertFile := os.Getenv("COLDARR_TLS_CERT_FILE")
	defaultTLSKeyFile := os.Getenv("COLDARR_TLS_KEY_FILE")
	defaultTrustedProxies := envFirst("COLDARR_TRUSTED_REVERSE_PROXIES_CIDR", "TRUSTED_REVERSE_PROXIES_CIDR")

	var addr, tlsCertFile, tlsKeyFile, trustedProxies string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Coldarr web GUI",
		Long: "Runs a persistent web server for configuring connections and tiers, viewing\n" +
			"disk usage, previewing move plans, and applying them - a browser-based\n" +
			"alternative to the report/plan/apply CLI commands, backed by the same config.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadForServer(configPath)
			if err != nil {
				return err
			}

			connStore, err := connectionsStore()
			if err != nil {
				return err
			}

			srv, err := webui.New(configPath, cfg, connStore)
			if err != nil {
				return err
			}

			opts := webui.ListenOptions{
				Addr:                     addr,
				TLSCertFile:              tlsCertFile,
				TLSKeyFile:               tlsKeyFile,
				TrustedReverseProxyCIDRs: trustedProxies,
			}
			if err := opts.Validate(); err != nil {
				return err
			}

			srv.StartScheduler()
			return srv.ListenAndServe(opts)
		},
	}

	cmd.Flags().StringVar(&addr, "listen", defaultAddr, "address to listen on (default can be set via COLDARR_LISTEN_ADDR)")
	cmd.Flags().StringVar(&tlsCertFile, "tls-cert-file", defaultTLSCertFile, "TLS certificate file for serving HTTPS (default can be set via COLDARR_TLS_CERT_FILE)")
	cmd.Flags().StringVar(&tlsKeyFile, "tls-key-file", defaultTLSKeyFile, "TLS private key file for serving HTTPS (default can be set via COLDARR_TLS_KEY_FILE)")
	cmd.Flags().StringVar(&trustedProxies, "trusted-reverse-proxies-cidr", defaultTrustedProxies, "comma-separated reverse proxy CIDRs whose forwarded headers should be trusted (default can be set via COLDARR_TRUSTED_REVERSE_PROXIES_CIDR or TRUSTED_REVERSE_PROXIES_CIDR)")
	return cmd
}

func envFirst(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
