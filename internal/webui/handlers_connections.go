package webui

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/vocoder/coldarr/internal/conncheck"
	"github.com/vocoder/coldarr/internal/secrets"
)

type connRow struct {
	App                    string
	URL                    string
	URLPlaceholder         string
	APIKeySet              bool
	Enabled                bool
	Source                 string
	Locked                 bool
	EnvURLVar              string
	EnvKeyVar              string
	ExternalURL            string
	ExternalURLPlaceholder string
}

var defaultPorts = map[string]string{
	"radarr":   "7878",
	"sonarr":   "8989",
	"jellyfin": "8096",
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
		prefix := strings.ToUpper(app)
		rows = append(rows, connRow{
			App:                    app,
			URL:                    conn.URL,
			URLPlaceholder:         fmt.Sprintf("http://%s:%s", app, defaultPorts[app]),
			APIKeySet:              conn.APIKey != "",
			Enabled:                conn.Enabled,
			Source:                 source,
			Locked:                 source == secrets.SourceEnv,
			EnvURLVar:              prefix + "_URL",
			EnvKeyVar:              prefix + "_API_KEY",
			ExternalURL:            raw.ExternalURL,
			ExternalURLPlaceholder: fmt.Sprintf("https://%s.mydomain.com", app),
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
		URL:    strings.TrimSpace(r.FormValue("url")),
		APIKey: strings.TrimSpace(r.FormValue("api_key")),
	}
	if conn.URL == "" || conn.APIKey == "" {
		// No (or partial) form input submitted - fall back to whatever is
		// currently effective (saved or env), so "Test connection" also
		// works for already-configured apps without retyping the key.
		effective, _ := s.connStore.Effective(app)
		if conn.URL == "" {
			conn.URL = effective.URL
		}
		if conn.APIKey == "" {
			conn.APIKey = effective.APIKey
		}
	}

	result := testResult{App: app}
	if conn.URL == "" || conn.APIKey == "" {
		result.Error = "URL and API key are required to test"
	} else if version, err := conncheck.Test(app, conn); err != nil {
		result.Error = err.Error()
	} else {
		result.Version = version
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

	// Blank API key on save means "keep the existing one" (the field is
	// left blank in the form deliberately, so a saved key never round-trips
	// back into the browser).
	if apiKey == "" {
		if existing, ok := s.connStore.Get(app); ok {
			apiKey = existing.APIKey
		}
	}
	if apiKey == "" {
		s.render(w, "connections", connectionsData{Title: "Connections", Error: "API key is required", Rows: s.connRows()})
		return
	}

	if err := s.connStore.Set(app, secrets.Connection{URL: url, APIKey: apiKey, Enabled: enabled}); err != nil {
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
