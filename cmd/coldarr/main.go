// Command coldarr is a policy-based storage-tiering balancer for
// Radarr/Sonarr libraries. It decides what belongs on hot vs. cold storage
// and asks Radarr/Sonarr to relocate it, so their databases stay correct.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/engine"
)

// version is set at build time via -ldflags "-X main.version=...". Docker
// images built by the release workflow stamp it with the release tag.
var version = "dev"

var configPath string

func main() {
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

func loadEngine() (*engine.Engine, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	return engine.New(cfg)
}
