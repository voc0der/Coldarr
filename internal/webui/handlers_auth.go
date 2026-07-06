package webui

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/vocoder/coldarr/internal/config"
)

type authData struct {
	Title           string
	Error           string
	Saved           string
	Enabled         bool
	IssuerURL       string
	ClientID        string
	RedirectURL     string
	RequiredGroup   string
	GroupsClaim     string
	AutoLogin       bool
	ClientSecretSet bool
	Locked          bool
}

func (s *Server) authPageData() authData {
	cfg := s.effectiveOIDCConfig()
	return authData{
		Title:           "Auth",
		Enabled:         cfg.Enabled,
		IssuerURL:       cfg.IssuerURL,
		ClientID:        cfg.ClientID,
		RedirectURL:     cfg.RedirectURL,
		RequiredGroup:   cfg.RequiredGroup,
		GroupsClaim:     cfg.GroupsClaim,
		AutoLogin:       cfg.AutoLogin,
		ClientSecretSet: cfg.ClientSecretSet,
		Locked:          cfg.EnvLocked,
	}
}

func (s *Server) handleAuthPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings_auth", s.authPageData())
}

func (s *Server) handleAuthSave(w http.ResponseWriter, r *http.Request) {
	data := s.authPageData()
	if data.Locked {
		data.Error = "OIDC auth is currently set through environment variables and cannot be edited here."
		s.render(w, "settings_auth", data)
		return
	}

	_ = r.ParseForm()
	authCfg := config.OIDCAuthConfig{
		Enabled:       r.FormValue("enabled") == "on",
		IssuerURL:     strings.TrimSpace(r.FormValue("issuer_url")),
		ClientID:      strings.TrimSpace(r.FormValue("client_id")),
		RedirectURL:   strings.TrimSpace(r.FormValue("redirect_url")),
		RequiredGroup: strings.TrimSpace(r.FormValue("required_group")),
		GroupsClaim:   strings.TrimSpace(r.FormValue("groups_claim")),
		AutoLogin:     r.FormValue("auto_login") == "on",
	}
	if authCfg.RequiredGroup == "" {
		authCfg.RequiredGroup = "coldarr"
	}
	if authCfg.GroupsClaim == "" {
		authCfg.GroupsClaim = "groups"
	}

	stored, _ := s.connStore.Get(oidcSecretApp)
	clientSecret := strings.TrimSpace(r.FormValue("client_secret"))
	if clientSecret == "" {
		clientSecret = stored.APIKey
	}

	effective := effectiveOIDCConfig{OIDCAuthConfig: authCfg, ClientSecret: clientSecret}
	if authCfg.Enabled {
		if err := validateEffectiveOIDCConfig(effective); err != nil {
			data := s.authPageData()
			data.Error = err.Error()
			data.Enabled = authCfg.Enabled
			data.IssuerURL = authCfg.IssuerURL
			data.ClientID = authCfg.ClientID
			data.RedirectURL = authCfg.RedirectURL
			data.RequiredGroup = authCfg.RequiredGroup
			data.GroupsClaim = authCfg.GroupsClaim
			data.AutoLogin = authCfg.AutoLogin
			data.ClientSecretSet = clientSecret != ""
			s.render(w, "settings_auth", data)
			return
		}
	}

	if err := s.updateAuthOIDC(authCfg); err != nil {
		data := s.authPageData()
		data.Error = err.Error()
		s.render(w, "settings_auth", data)
		return
	}

	if authCfg.ClientID == "" && clientSecret == "" {
		if err := s.connStore.Delete(oidcSecretApp); err != nil {
			data := s.authPageData()
			data.Error = fmt.Sprintf("auth settings saved, but removing stored OIDC secret failed: %v", err)
			s.render(w, "settings_auth", data)
			return
		}
	} else if err := s.connStore.Set(oidcSecretApp, storedOIDCSecret(authCfg.ClientID, clientSecret)); err != nil {
		data := s.authPageData()
		data.Error = fmt.Sprintf("auth settings saved, but storing OIDC secret failed: %v", err)
		s.render(w, "settings_auth", data)
		return
	}

	data = s.authPageData()
	data.Saved = "OIDC auth settings saved."
	s.render(w, "settings_auth", data)
}
