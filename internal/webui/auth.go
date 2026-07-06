package webui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/secrets"
)

const (
	oidcSecretApp     = "oidc"
	authSessionCookie = "coldarr_session"
	authSessionTTL    = 12 * time.Hour
	oidcStateTTL      = 10 * time.Minute
)

type effectiveOIDCConfig struct {
	config.OIDCAuthConfig
	ClientSecret    string
	ClientSecretSet bool
	EnvLocked       bool
}

type authSession struct {
	UserName string
	Email    string
	Groups   []string
	Expires  time.Time
}

type oidcLoginState struct {
	Verifier string
	Nonce    string
	ReturnTo string
	Expires  time.Time
}

type loginData struct {
	Title    string
	Error    string
	LoginURL string
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		cfg := s.effectiveOIDCConfig()
		if !cfg.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if err := validateEffectiveOIDCConfig(cfg); err != nil {
			http.Error(w, "OIDC auth is enabled but not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if _, ok := s.currentAuthSession(r); ok {
			next.ServeHTTP(w, r)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		returnTo := cleanReturnTo(r.URL.RequestURI())
		target := "/login?return_to=" + url.QueryEscape(returnTo)
		if cfg.AutoLogin {
			target = "/auth/login?return_to=" + url.QueryEscape(returnTo)
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

func authPublicPath(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case path == "/login", path == "/auth/login", path == "/auth/callback", path == "/auth/logout":
		return true
	case strings.HasPrefix(path, "/static/"):
		return true
	default:
		return false
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveOIDCConfig()
	if !cfg.Enabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if _, ok := s.currentAuthSession(r); ok {
		http.Redirect(w, r, cleanReturnTo(r.URL.Query().Get("return_to")), http.StatusFound)
		return
	}

	returnTo := cleanReturnTo(r.URL.Query().Get("return_to"))
	data := loginData{
		Title:    "Sign in",
		LoginURL: "/auth/login?return_to=" + url.QueryEscape(returnTo),
	}
	if err := validateEffectiveOIDCConfig(cfg); err != nil {
		data.Error = err.Error()
	}
	s.render(w, "login", data)
}

func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveOIDCConfig()
	if !cfg.Enabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := validateEffectiveOIDCConfig(cfg); err != nil {
		http.Error(w, "OIDC auth is enabled but not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	provider, oauthCfg, err := s.oidcOAuthConfig(r.Context(), r, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = provider

	state, err := randomURLToken(32)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nonce, err := randomURLToken(32)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	s.storeOIDCState(state, oidcLoginState{
		Verifier: verifier,
		Nonce:    nonce,
		ReturnTo: cleanReturnTo(r.URL.Query().Get("return_to")),
		Expires:  time.Now().Add(oidcStateTTL),
	})

	authURL := oauthCfg.AuthCodeURL(
		state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveOIDCConfig()
	if !cfg.Enabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := validateEffectiveOIDCConfig(cfg); err != nil {
		http.Error(w, "OIDC auth is enabled but not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if msg := r.URL.Query().Get("error"); msg != "" {
		http.Error(w, msg, http.StatusUnauthorized)
		return
	}

	state, ok := s.consumeOIDCState(r.URL.Query().Get("state"))
	if !ok {
		http.Error(w, "OIDC login state is missing or expired", http.StatusBadRequest)
		return
	}

	provider, oauthCfg, err := s.oidcOAuthConfig(r.Context(), r, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	token, err := oauthCfg.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(state.Verifier))
	if err != nil {
		http.Error(w, "exchanging OIDC code: "+err.Error(), http.StatusBadGateway)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Error(w, "OIDC provider did not return an id_token", http.StatusBadGateway)
		return
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}).Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "verifying OIDC token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if idToken.Nonce != state.Nonce {
		http.Error(w, "OIDC token nonce did not match login state", http.StatusUnauthorized)
		return
	}

	identity, err := oidcIdentityFromToken(r.Context(), provider, token, idToken, cfg.GroupsClaim)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !groupAllowed(identity.Groups, cfg.RequiredGroup) {
		http.Error(w, fmt.Sprintf("OIDC user is not a member of required group %q", cfg.RequiredGroup), http.StatusForbidden)
		return
	}

	if err := s.createAuthSession(w, r, cfg, identity); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, cleanReturnTo(state.ReturnTo), http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authSessionCookie); err == nil {
		s.authMu.Lock()
		delete(s.authSessions, c.Value)
		s.authMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, s.effectiveOIDCConfig()),
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) oidcOAuthConfig(ctx context.Context, r *http.Request, cfg effectiveOIDCConfig) (*oidc.Provider, oauth2.Config, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, oauth2.Config{}, fmt.Errorf("discovering OIDC provider: %w", err)
	}
	oauthCfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  s.oidcRedirectURL(r, cfg),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}
	return provider, oauthCfg, nil
}

func (s *Server) oidcRedirectURL(r *http.Request, cfg effectiveOIDCConfig) string {
	if cfg.RedirectURL != "" {
		return cfg.RedirectURL
	}
	scheme := "http"
	if requestIsHTTPS(r, cfg) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

func (s *Server) effectiveOIDCConfig() effectiveOIDCConfig {
	cfg := s.currentConfig().Auth.OIDC
	stored, _ := s.connStore.Get(oidcSecretApp)
	out := effectiveOIDCConfig{
		OIDCAuthConfig: cfg,
		ClientSecret:   stored.APIKey,
	}
	if out.ClientID == "" {
		out.ClientID = stored.URL
	}

	envLocked := false
	setString := func(name string, dest *string) {
		if v, ok := os.LookupEnv(name); ok {
			*dest = strings.TrimSpace(v)
			envLocked = true
		}
	}
	setBool := func(name string, dest *bool) {
		if v, ok := os.LookupEnv(name); ok {
			envLocked = true
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				*dest = b
			}
		}
	}

	setBool("COLDARR_OIDC_ENABLED", &out.Enabled)
	setString("COLDARR_OIDC_ISSUER_URL", &out.IssuerURL)
	setString("COLDARR_OIDC_CLIENT_ID", &out.ClientID)
	setString("COLDARR_OIDC_CLIENT_SECRET", &out.ClientSecret)
	setString("COLDARR_OIDC_REDIRECT_URL", &out.RedirectURL)
	setString("COLDARR_OIDC_REQUIRED_GROUP", &out.RequiredGroup)
	setString("COLDARR_OIDC_GROUPS_CLAIM", &out.GroupsClaim)
	setBool("COLDARR_OIDC_AUTO_LOGIN", &out.AutoLogin)
	if disabled, ok := boolEnv("COLDARR_OIDC_DISABLE_AUTO_LOGIN"); ok && disabled {
		out.AutoLogin = false
		envLocked = true
	}

	if out.RequiredGroup == "" {
		out.RequiredGroup = "coldarr"
	}
	if out.GroupsClaim == "" {
		out.GroupsClaim = "groups"
	}
	out.ClientSecretSet = out.ClientSecret != ""
	out.EnvLocked = envLocked
	return out
}

func boolEnv(name string) (bool, bool) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, true
	}
	return b, true
}

func validateEffectiveOIDCConfig(cfg effectiveOIDCConfig) error {
	if strings.TrimSpace(cfg.IssuerURL) == "" {
		return fmt.Errorf("issuer URL is required")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return fmt.Errorf("client ID is required")
	}
	if strings.TrimSpace(cfg.ClientSecret) == "" {
		return fmt.Errorf("client secret is required")
	}
	if strings.TrimSpace(cfg.GroupsClaim) == "" {
		return fmt.Errorf("groups claim is required")
	}
	if _, err := url.ParseRequestURI(cfg.IssuerURL); err != nil {
		return fmt.Errorf("issuer URL is invalid: %w", err)
	}
	if cfg.RedirectURL != "" {
		if _, err := url.ParseRequestURI(cfg.RedirectURL); err != nil {
			return fmt.Errorf("redirect URL is invalid: %w", err)
		}
	}
	return nil
}

func (s *Server) currentAuthSession(r *http.Request) (authSession, bool) {
	c, err := r.Cookie(authSessionCookie)
	if err != nil || c.Value == "" {
		return authSession{}, false
	}

	now := time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	sess, ok := s.authSessions[c.Value]
	if !ok {
		return authSession{}, false
	}
	if now.After(sess.Expires) {
		delete(s.authSessions, c.Value)
		return authSession{}, false
	}
	return sess, true
}

func (s *Server) createAuthSession(w http.ResponseWriter, r *http.Request, cfg effectiveOIDCConfig, identity oidcIdentity) error {
	id, err := randomURLToken(32)
	if err != nil {
		return err
	}
	expires := time.Now().Add(authSessionTTL)

	s.authMu.Lock()
	s.cleanupAuthLocked(time.Now())
	s.authSessions[id] = authSession{
		UserName: identity.UserName,
		Email:    identity.Email,
		Groups:   identity.Groups,
		Expires:  expires,
	}
	s.authMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookie,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(authSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, cfg),
	})
	return nil
}

func (s *Server) storeOIDCState(state string, loginState oidcLoginState) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.cleanupAuthLocked(time.Now())
	s.oidcStates[state] = loginState
}

func (s *Server) consumeOIDCState(state string) (oidcLoginState, bool) {
	if state == "" {
		return oidcLoginState{}, false
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	loginState, ok := s.oidcStates[state]
	delete(s.oidcStates, state)
	if !ok || time.Now().After(loginState.Expires) {
		return oidcLoginState{}, false
	}
	return loginState, true
}

func (s *Server) cleanupAuthLocked(now time.Time) {
	for id, sess := range s.authSessions {
		if now.After(sess.Expires) {
			delete(s.authSessions, id)
		}
	}
	for state, loginState := range s.oidcStates {
		if now.After(loginState.Expires) {
			delete(s.oidcStates, state)
		}
	}
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func cleanReturnTo(raw string) string {
	if raw == "" {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if strings.HasPrefix(u.Path, "/auth/") || u.Path == "/login" {
		return "/"
	}
	return raw
}

func requestIsHTTPS(r *http.Request, cfg effectiveOIDCConfig) bool {
	if r.TLS != nil || r.URL.Scheme == "https" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(cfg.RedirectURL), "https://")
}

type oidcIdentity struct {
	Subject  string
	UserName string
	Email    string
	Groups   []string
}

func oidcIdentityFromToken(ctx context.Context, provider *oidc.Provider, token *oauth2.Token, idToken *oidc.IDToken, groupsClaim string) (oidcIdentity, error) {
	var idClaims map[string]any
	if err := idToken.Claims(&idClaims); err != nil {
		return oidcIdentity{}, fmt.Errorf("reading OIDC token claims: %w", err)
	}

	userClaims := map[string]any{}
	userInfo, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err == nil {
		_ = userInfo.Claims(&userClaims)
	}

	identity := oidcIdentity{
		Subject: claimString(idClaims, userClaims, "sub"),
		UserName: firstNonEmpty(
			claimString(idClaims, userClaims, "preferred_username"),
			claimString(idClaims, userClaims, "name"),
			claimString(idClaims, userClaims, "email"),
			claimString(idClaims, userClaims, "sub"),
		),
		Email:  claimString(idClaims, userClaims, "email"),
		Groups: claimStringSlice(idClaims, userClaims, groupsClaim),
	}
	if identity.Subject == "" {
		return oidcIdentity{}, fmt.Errorf("OIDC token did not include a subject")
	}
	return identity, nil
}

func claimString(primary, fallback map[string]any, name string) string {
	if s := stringClaim(primary[name]); s != "" {
		return s
	}
	return stringClaim(fallback[name])
}

func stringClaim(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

func claimStringSlice(primary, fallback map[string]any, name string) []string {
	if values := stringSliceClaim(primary[name]); len(values) > 0 {
		return values
	}
	return stringSliceClaim(fallback[name])
}

func stringSliceClaim(v any) []string {
	switch t := v.(type) {
	case []string:
		return cleanStringSlice(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, v := range t {
			if s := strings.TrimSpace(stringClaim(v)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		return cleanStringSlice(strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' }))
	default:
		return nil
	}
}

func cleanStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func groupAllowed(groups []string, required string) bool {
	required = strings.TrimSpace(required)
	if required == "" {
		return true
	}
	for _, group := range groups {
		if group == required {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func storedOIDCSecret(clientID, clientSecret string) secrets.Connection {
	return secrets.Connection{URL: clientID, APIKey: clientSecret, Enabled: true}
}
