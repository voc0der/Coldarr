// Package conncheck tests a Radarr/Sonarr/Jellyfin connection against the
// app's own API - shared by the CLI's `connections test` command and the
// web GUI's "Test connection" button so both exercise the exact same
// check.
package conncheck

import (
	"fmt"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/secrets"
)

// Apps is the fixed set of connections Coldarr knows how to test.
var Apps = []string{"radarr", "sonarr", "jellyfin"}

func Valid(app string) bool {
	for _, a := range Apps {
		if a == app {
			return true
		}
	}
	return false
}

// Test attempts to connect using conn's URL/API key and returns whatever
// version string the app reports. It never touches stored config - the
// caller decides whether conn came from a form, the encrypted store, or an
// env var.
func Test(app string, conn secrets.Connection) (version string, err error) {
	switch app {
	case "radarr":
		return arrapi.NewRadarrClient(conn.URL, conn.APIKey).Ping()
	case "sonarr":
		return arrapi.NewSonarrClient(conn.URL, conn.APIKey).Ping()
	case "jellyfin":
		v, _, err := jellyfin.NewClient(conn.URL, conn.APIKey).Ping()
		return v, err
	default:
		return "", fmt.Errorf("unknown app %q", app)
	}
}
