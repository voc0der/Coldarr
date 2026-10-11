package webui

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/vocoder/coldarr/internal/conncheck"
	"github.com/vocoder/coldarr/internal/secrets"
)

type connRow struct {
	App            string
	URL            string
	URLPlaceholder string
	APIKeySet      bool
	Enabled        bool
	Source         string
	Locked         bool
	// EnvVars names the variables that override this connection, shown
	// when they're what locks it.
	EnvVars []string
	// HasLinks marks the apps the Plan/History Links column can deep-link
	// into - the only use an External URL has.
	HasLinks               bool
	ExternalURL            string
	ExternalURLPlaceholder string
	// UsesLogin marks qBittorrent: its API key is optional, and a
	// username and password can stand in for it - or neither, if its
	// WebUI skips authentication for Coldarr's address.
	UsesLogin   bool
	Username    string
	PasswordSet bool
	// Hint says what Coldarr uses the app for, where that isn't obvious.
	Hint string
}

var defaultPorts = map[string]string{
	"radarr":      "7878",
	"sonarr":      "8989",
	"lidarr":      "8686",
	"jellyfin":    "8096",
	"qbittorrent": "8080",
}

var connHints = map[string]string{
	"lidarr":      "Optional. Coldarr doesn't move music libraries - it only asks Lidarr whether a music download has been imported before moving that download off the download drive.",
	"qbittorrent": "Optional. Lets Coldarr move finished downloads from the download drive to seed storage once they've settled, and remove them once their trackers' seeding requirements are met - see Settings > Downloads.",
}

type connectionsData struct {
	Title string
	Error string
	Saved string
	Rows  []connRow
}

func (s *Server) connRows() []connRow {
	rows := make([]connRow, 0, len(conncheck.Apps))
	for _, app := range conncheck.Apps {
		conn, source := s.connStore.Effective(app)
		// External URL is read from the raw stored connection, not
		// Effective - it has no env var override, so it should round-trip
		// on the settings form even in the edge case where it's set
		// before URL/API key ever are (when Effective would report
		// SourceNone and zero out everything else).
		raw, _ := s.connStore.Get(app)
		rows = append(rows, connRow{
			App:                    app,
			URL:                    conn.URL,
			URLPlaceholder:         fmt.Sprintf("http://%s:%s", app, defaultPorts[app]),
			APIKeySet:              conn.APIKey != "",
			Enabled:                conn.Enabled,
			Source:                 source,
			Locked:                 source == secrets.SourceEnv,
			EnvVars:                secrets.EnvVars(app),
			HasLinks:               app == "radarr" || app == "sonarr" || app == "jellyfin",
			ExternalURL:            raw.ExternalURL,
			ExternalURLPlaceholder: fmt.Sprintf("https://%s.mydomain.com", app),
			UsesLogin:              app == secrets.LoginApp,
			Username:               conn.Username,
			PasswordSet:            conn.Password != "",
			Hint:                   connHints[app],
		})
	}
	return rows
}

func (s *Server) handleConnectionsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "connections", connectionsData{Title: "Connections", Rows: s.connRows()})
}

type testResult struct {
	App     string
	Version string
	Error   string
}

func (s *Server) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !conncheck.Valid(app) {
		http.Error(w, "unknown app", http.StatusNotFound)
		return
	}

	_ = r.ParseForm()
	conn := secrets.Connection{
		URL:      strings.TrimSpace(r.FormValue("url")),
		APIKey:   strings.TrimSpace(r.FormValue("api_key")),
		Username: strings.TrimSpace(r.FormValue("username")),
		Password: r.FormValue("password"),
	}
	// Blank fields fall back to whatever is currently effective (saved or
	// env), so "Test connection" also works for already-configured apps
	// without retyping the key. A login is only borrowed when the form
	// didn't post one at all: a username field submitted empty means
	// "test without signing in".
	effective, _ := s.connStore.Effective(app)
	if conn.URL == "" {
		conn.URL = effective.URL
	}
	if conn.APIKey == "" {
		conn.APIKey = effective.APIKey
	}
	if !r.Form.Has("username") {
		conn.Username = effective.Username
	}
	if conn.Username != "" && conn.Password == "" {
		conn.Password = effective.Password
	}

	result := testResult{App: app}
	switch {
	case app == secrets.LoginApp && conn.URL == "":
		result.Error = "URL is required to test"
	case app != secrets.LoginApp && (conn.URL == "" || conn.APIKey == ""):
		result.Error = "URL and API key are required to test"
	default:
		if version, err := conncheck.Test(app, conn); err != nil {
			result.Error = err.Error()
		} else {
			result.Version = version
		}
	}

	s.renderPartial(w, "connections", "conn_test_result", result)
}

func (s *Server) handleConnectionSave(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !conncheck.Valid(app) {
		http.Error(w, "unknown app", http.StatusNotFound)
		return
	}

	_ = r.ParseForm()
	url := strings.TrimSpace(r.FormValue("url"))
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	enabled := r.FormValue("enabled") == "on"
	if app != "jellyfin" {
		enabled = true
	}

	if url == "" {
		s.render(w, "connections", connectionsData{Title: "Connections", Error: "URL is required", Rows: s.connRows()})
		return
	}

	// Saved over what's already stored rather than replacing it, so the
	// External URL - which has its own form - survives a connection save.
	conn, _ := s.connStore.Get(app)

	// Blank API key on save means "keep the existing one" (the field is
	// left blank in the form deliberately, so a saved key never round-trips
	// back into the browser). qBittorrent's key is optional, so it alone
	// has a box to drop the saved one.
	if apiKey == "" {
		apiKey = conn.APIKey
	}
	if app == secrets.LoginApp && r.FormValue("clear_api_key") == "on" {
		apiKey = ""
	}
	if apiKey == "" && app != secrets.LoginApp {
		s.render(w, "connections", connectionsData{Title: "Connections", Error: "API key is required", Rows: s.connRows()})
		return
	}

	conn.URL, conn.APIKey, conn.Enabled = url, apiKey, enabled
	if app == secrets.LoginApp {
		// No username means no login at all, so a stale password must not
		// linger; a username with the password left blank keeps the saved
		// one, the same way the API key does.
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		switch {
		case username == "":
			password = ""
		case password == "":
			password = conn.Password
		}
		conn.Username, conn.Password = username, password
	}
	if err := s.connStore.Set(app, conn); err != nil {
		s.render(w, "connections", connectionsData{Title: "Connections", Error: err.Error(), Rows: s.connRows()})
		return
	}

	s.render(w, "connections", connectionsData{Title: "Connections", Saved: app + " connection saved.", Rows: s.connRows()})
}

// handleConnectionExternalURLSave saves just the External URL field,
// independent of Locked state (there's no env var override for it) and
// without the URL/API-key validation handleConnectionSave applies -
// External URL is optional and purely cosmetic (it builds outbound
// browser links, never a Coldarr API call), so an empty value just means
// "fall back to the URL above" rather than an error.
func (s *Server) handleConnectionExternalURLSave(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !conncheck.Valid(app) {
		http.Error(w, "unknown app", http.StatusNotFound)
		return
	}

	_ = r.ParseForm()
	externalURL := strings.TrimSpace(r.FormValue("external_url"))

	conn, _ := s.connStore.Get(app)
	conn.ExternalURL = externalURL
	if err := s.connStore.Set(app, conn); err != nil {
		s.render(w, "connections", connectionsData{Title: "Connections", Error: err.Error(), Rows: s.connRows()})
		return
	}

	s.render(w, "connections", connectionsData{Title: "Connections", Saved: app + " external URL saved.", Rows: s.connRows()})
}

func (s *Server) handleConnectionDelete(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !conncheck.Valid(app) {
		http.Error(w, "unknown app", http.StatusNotFound)
		return
	}

	if err := s.connStore.Delete(app); err != nil {
		s.render(w, "connections", connectionsData{Title: "Connections", Error: err.Error(), Rows: s.connRows()})
		return
	}

	s.render(w, "connections", connectionsData{Title: "Connections", Saved: app + " connection removed.", Rows: s.connRows()})
}
