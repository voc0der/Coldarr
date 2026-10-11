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
		Short: "Manage Radarr/Sonarr/Lidarr/Jellyfin/qBittorrent connections (stored encrypted, or set via env vars)",
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
					fmt.Printf("%-12s not configured\n", app)
					continue
				}
				login := ""
				if app == secrets.LoginApp {
					login = fmt.Sprintf(", login: %s", orNone(conn.Username))
				}
				fmt.Printf("%-12s %s (source: %s, enabled: %v, key: %s%s)\n", app, conn.URL, source, conn.Enabled, maskKey(conn.APIKey), login)
			}
			return nil
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
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
	var url, apiKey, username, password string
	var disabled bool

	cmd := &cobra.Command{
		Use:   "set <" + strings.Join(conncheck.Apps, "|") + ">",
		Short: "Store connection info for an app, encrypted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if !conncheck.Valid(app) {
				return fmt.Errorf("unknown app %q (expected one of %s)", app, strings.Join(conncheck.Apps, ", "))
			}
			// qBittorrent can be reachable with a URL alone (its WebUI can
			// skip authentication), and signs in with a login rather than
			// needing an API key; every other app needs its key.
			if app == secrets.LoginApp {
				if url == "" {
					return fmt.Errorf("--url is required")
				}
				if password != "" && username == "" {
					return fmt.Errorf("--password needs --username")
				}
			} else {
				if url == "" || apiKey == "" {
					return fmt.Errorf("--url and --api-key are required")
				}
				if username != "" || password != "" {
					return fmt.Errorf("--username and --password are only for qbittorrent")
				}
			}

			store, err := connectionsStore()
			if err != nil {
				return err
			}

			// Saved over what's already stored rather than replacing it,
			// so the External URL - set only in the web GUI - survives.
			conn, _ := store.Get(app)
			conn.URL, conn.APIKey, conn.Enabled = url, apiKey, !disabled
			conn.Username, conn.Password = username, password
			if err := store.Set(app, conn); err != nil {
				return err
			}
			fmt.Printf("saved %s connection\n", app)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "base URL, e.g. http://radarr:7878")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key (optional for qbittorrent, which needs 5.2+ for keys)")
	cmd.Flags().StringVar(&username, "username", "", "WebUI username (qbittorrent only - leave out if its WebUI skips authentication)")
	cmd.Flags().StringVar(&password, "password", "", "WebUI password (qbittorrent only)")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "store but mark disabled (jellyfin only - every other app is enabled whenever configured)")
	return cmd
}

func newConnectionsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <" + strings.Join(conncheck.Apps, "|") + ">",
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
		Use:   "delete <" + strings.Join(conncheck.Apps, "|") + ">",
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
