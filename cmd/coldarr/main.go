// Command coldarr is a policy-based storage-tiering balancer for
// Radarr/Sonarr libraries. It decides what belongs on hot vs. cold storage
// and asks Radarr/Sonarr to relocate it, so their databases stay correct.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/secrets"
)

// version is set at build time via -ldflags "-X main.version=...". Docker
// images built by the release workflow stamp it with the release tag.
var version = "dev"

var configPath string

func main() {
	// So Jellyfin's logs and Dashboard -> Devices name the Coldarr build
	// that made a request, not a bare "Coldarr".
	jellyfin.Version = version

	defaultConfig := "coldarr.yaml"
	if v := os.Getenv("COLDARR_CONFIG"); v != "" {
		defaultConfig = v
	}

	root := &cobra.Command{
		Use:   "coldarr",
		Short: "Storage-tiering balancer for Radarr/Sonarr libraries",
		Long: "Coldarr analyzes disk usage and library metadata from Radarr/Sonarr, decides which\n" +
			"movies and series belong on hot vs. cold storage, and asks those apps to relocate\n" +
			"items so their databases remain the source of truth.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfig, "path to config file (default can be set via COLDARR_CONFIG)")

	root.AddCommand(newReportCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newApplyCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newServeCmd())
	root.AddCommand(newConnectionsCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the Coldarr version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(version)
			return nil
		},
	}
}

// connectionsStore opens the encrypted connection store living alongside
// the config file (same directory).
func connectionsStore() (*secrets.Store, error) {
	dir := filepath.Dir(configPath)
	return secrets.LoadOrCreate(dir)
}

func loadEngine() (*engine.Engine, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}

	connStore, err := connectionsStore()
	if err != nil {
		return nil, err
	}

	e, err := engine.New(cfg, connStore)
	if err != nil {
		return nil, err
	}

	if e.Radarr == nil && e.Sonarr == nil {
		return nil, fmt.Errorf("no Radarr or Sonarr connection is configured - set one via `coldarr connections set`, the web GUI, or RADARR_URL/RADARR_API_KEY (or SONARR_*) env vars")
	}

	return e, nil
}
