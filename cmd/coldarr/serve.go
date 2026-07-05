package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/webui"
)

func newServeCmd() *cobra.Command {
	defaultAddr := ":8080"
	if v := os.Getenv("COLDARR_LISTEN_ADDR"); v != "" {
		defaultAddr = v
	}

	var addr string

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

			return srv.ListenAndServe(addr)
		},
	}

	cmd.Flags().StringVar(&addr, "listen", defaultAddr, "address to listen on (default can be set via COLDARR_LISTEN_ADDR)")
	return cmd
}
