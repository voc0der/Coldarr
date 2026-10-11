// Package conncheck tests a Radarr/Sonarr/Lidarr/Jellyfin/qBittorrent
// connection against the app's own API - shared by the CLI's `connections
// test` command and the web GUI's "Test connection" button so both
// exercise the exact same check.
package conncheck

import (
	"fmt"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/qbit"
	"github.com/vocoder/coldarr/internal/secrets"
)

// Apps is the fixed set of connections Coldarr knows how to test.
var Apps = []string{"radarr", "sonarr", "lidarr", "jellyfin", "qbittorrent"}

func Valid(app string) bool {
	for _, a := range Apps {
		if a == app {
			return true
		}
	}
	return false
}

// Test attempts to connect using conn's URL and credentials and returns
// whatever version string the app reports. It never touches stored config - the
// caller decides whether conn came from a form, the encrypted store, or an
// env var.
func Test(app string, conn secrets.Connection) (version string, err error) {
	switch app {
	case "radarr":
		return arrapi.NewRadarrClient(conn.URL, conn.APIKey).Ping()
	case "sonarr":
		return arrapi.NewSonarrClient(conn.URL, conn.APIKey).Ping()
	case "lidarr":
		return arrapi.NewLidarrClient(conn.URL, conn.APIKey).Ping()
	case "qbittorrent":
		return QbitClient(conn).Version()
	case "jellyfin":
		v, _, err := jellyfin.NewClient(conn.URL, conn.APIKey).Ping()
		return v, err
	default:
		return "", fmt.Errorf("unknown app %q", app)
	}
}

// QbitClient builds a qBittorrent client from a stored or effective
// connection - the one place secrets.Connection's fields are mapped onto
// qBittorrent's three ways of signing in.
func QbitClient(conn secrets.Connection) *qbit.Client {
	return qbit.NewClient(conn.URL, qbit.Auth{APIKey: conn.APIKey, Username: conn.Username, Password: conn.Password})
}
