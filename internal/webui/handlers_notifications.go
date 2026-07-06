package webui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/vocoder/coldarr/internal/notify"
	"github.com/vocoder/coldarr/internal/secrets"
)

// notificationsData is the Settings > Notifications page's view model. The
// Apprise URL itself is never round-tripped back to the browser (only
// whether one is set) - unlike the Radarr/Sonarr/Jellyfin URL fields on
// the Connections page, an Apprise URL can itself function as a bearer
// credential, so it's treated the way Connections treats an API key.
type notificationsData struct {
	Title         string
	Error         string
	Saved         string
	AppriseURLSet bool
	Verbose       bool
}

func (s *Server) notificationsData() notificationsData {
	conn, _ := s.connStore.Get("apprise")
	cfg := s.currentConfig()
	return notificationsData{
		Title:         "Notifications",
		AppriseURLSet: conn.URL != "",
		Verbose:       cfg.Notifications.Verbose,
	}
}

func (s *Server) handleNotificationsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings_notifications", s.notificationsData())
}

func (s *Server) handleNotificationsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rawURL := strings.TrimSpace(r.FormValue("apprise_url"))
	verbose := r.FormValue("verbose") == "on"

	// A blank URL means "keep whatever's already saved" (the field is
	// deliberately left blank in the form, same as Connections' API key)
	// - clearing it is a separate, explicit action (the Remove button).
	if rawURL != "" {
		if !validHTTPURL(rawURL) {
			data := s.notificationsData()
			data.Error = "Apprise URL must be a valid http:// or https:// URL"
			s.render(w, "settings_notifications", data)
			return
		}
		if err := s.connStore.Set("apprise", secrets.Connection{URL: rawURL}); err != nil {
			data := s.notificationsData()
			data.Error = err.Error()
			s.render(w, "settings_notifications", data)
			return
		}
	}

	if err := s.updateNotifications(verbose); err != nil {
		data := s.notificationsData()
		data.Error = err.Error()
		s.render(w, "settings_notifications", data)
		return
	}

	data := s.notificationsData()
	data.Saved = "Notification settings saved."
	s.render(w, "settings_notifications", data)
}

func (s *Server) handleNotificationsDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.connStore.Delete("apprise"); err != nil {
		data := s.notificationsData()
		data.Error = err.Error()
		s.render(w, "settings_notifications", data)
		return
	}
	data := s.notificationsData()
	data.Saved = "Apprise URL removed."
	s.render(w, "settings_notifications", data)
}

type notifyTestResult struct {
	Error string
}

func (s *Server) handleNotificationsTest(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rawURL := strings.TrimSpace(r.FormValue("apprise_url"))
	if rawURL == "" {
		// No (or blank) URL submitted - fall back to whatever's already
		// saved, so testing also works without retyping it first, same
		// as Connections' "Test connection" fallback.
		conn, _ := s.connStore.Get("apprise")
		rawURL = conn.URL
	}

	result := notifyTestResult{}
	switch {
	case rawURL == "":
		result.Error = "Enter an Apprise URL (or save one first) to test"
	case !validHTTPURL(rawURL):
		result.Error = "Apprise URL must be a valid http:// or https:// URL"
	default:
		if err := notify.Test(rawURL); err != nil {
			result.Error = err.Error()
		}
	}

	s.renderPartial(w, "settings_notifications", "notify_test_result", result)
}

func validHTTPURL(raw string) bool {
	u, err := url.ParseRequestURI(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
