package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vocoder/coldarr/internal/conncheck"
	"github.com/vocoder/coldarr/internal/secrets"
)

func newConnectionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connections",
		Short: "Manage Radarr/Sonarr/Jellyfin connections (stored encrypted, or set via env vars)",
	}
	cmd.AddCommand(newConnectionsListCmd())
	cmd.AddCommand(newConnectionsSetCmd())
	cmd.AddCommand(newConnectionsTestCmd())
	cmd.AddCommand(newConnectionsDeleteCmd())
	return cmd
}

func newConnectionsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show the effective connection for each app and where it comes from",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := connectionsStore()
			if err != nil {
				return err
			}

			for _, app := range conncheck.Apps {
				conn, source := store.Effective(app)
				if source == secrets.SourceNone {
					fmt.Printf("%-10s not configured\n", app)
					continue
				}
				fmt.Printf("%-10s %s (source: %s, enabled: %v, key: %s)\n", app, conn.URL, source, conn.Enabled, maskKey(conn.APIKey))
			}
			return nil
		},
	}
}

func maskKey(key string) string {
	if key == "" {
		return "(none)"
	}
	if len(key) <= 4 {
		return strings.Repeat("*", len(key))
	}
	return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
}

func newConnectionsSetCmd() *cobra.Command {
	var url, apiKey string
	var disabled bool

	cmd := &cobra.Command{
		Use:   "set <radarr|sonarr|jellyfin>",
		Short: "Store connection info for an app, encrypted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if !conncheck.Valid(app) {
				return fmt.Errorf("unknown app %q (expected radarr, sonarr, or jellyfin)", app)
			}
			if url == "" || apiKey == "" {
				return fmt.Errorf("--url and --api-key are required")
			}

			store, err := connectionsStore()
			if err != nil {
				return err
			}

			if err := store.Set(app, secrets.Connection{URL: url, APIKey: apiKey, Enabled: !disabled}); err != nil {
				return err
			}
			fmt.Printf("saved %s connection\n", app)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "base URL, e.g. http://radarr:7878")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "store but mark disabled (jellyfin only - radarr/sonarr are enabled whenever configured)")
	return cmd
}

func newConnectionsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <radarr|sonarr|jellyfin>",
		Short: "Test the currently effective connection for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if !conncheck.Valid(app) {
				return fmt.Errorf("unknown app %q", app)
			}

			store, err := connectionsStore()
			if err != nil {
				return err
			}

			conn, source := store.Effective(app)
			if source == secrets.SourceNone {
				return fmt.Errorf("%s is not configured", app)
			}

			version, err := conncheck.Test(app, conn)
			if err != nil {
				return fmt.Errorf("%s: %w", app, err)
			}
			fmt.Printf("%s: connected (version %s, source: %s)\n", app, version, source)
			return nil
		},
	}
}

func newConnectionsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <radarr|sonarr|jellyfin>",
		Short: "Remove a stored connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if !conncheck.Valid(app) {
				return fmt.Errorf("unknown app %q", app)
			}
			store, err := connectionsStore()
			if err != nil {
				return err
			}
			if err := store.Delete(app); err != nil {
				return err
			}
			fmt.Printf("deleted stored %s connection (env var override, if any, still applies)\n", app)
			return nil
		},
	}
}
