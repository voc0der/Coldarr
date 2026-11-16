package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
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
	// oidcBrowserCookie ties an OIDC login to the browser that started it
	// (see handleOIDCLogin).
	oidcBrowserCookie = "coldarr_oidc"
	// maxOIDCLogins bounds the OIDC logins kept in flight: anyone can start
	// one. Past it, the oldest is dropped and has to start again.
	maxOIDCLogins = 10000
	// maxReturnTo is the longest return path a login keeps (see
	// cleanReturnTo); none of Coldarr's own pages comes near it.
	maxReturnTo = 2048

	oidcTokenAuthAuto        = "auto"
	oidcTokenAuthClientPost  = "client_secret_post"
	oidcTokenAuthClientBasic = "client_secret_basic"

	passwordEnvVar         = "COLDARR_PASSWORD"
	passwordFileEnvVar     = "COLDARR_PASSWORD_FILE"
	generatedPasswordBytes = 32 // -> 64 hex characters
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
	// RedirectURI is the callback address the login asked the provider for.
	// The token request repeats it, wherever the callback arrives.
	RedirectURI string
	// Browser is the login's oidcBrowserCookie value: only the browser that
	// started the login can finish it.
	Browser string
	Expires time.Time
}

type loginData struct {
	Title        string
	Error        string
	LoginURL     string
	AutoLogin    bool
	PasswordAuth bool
	ReturnTo     string
	SignedOut    bool
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		cfg := s.effectiveOIDCConfig()
		if !cfg.Enabled {
			s.requirePasswordSession(w, r, next)
			return
		}
		if err := validateEffectiveOIDCConfig(cfg); err != nil {
			http.Error(w, "OIDC auth is enabled but not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if s.currentAuthSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		redirectToLogin(w, r)
	})
}

// requirePasswordSession is authMiddleware's branch for whenever OIDC is
// disabled: password auth is the fallback gate, so the GUI is never
// reachable with zero protection just because nobody configured OIDC.
func (s *Server) requirePasswordSession(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if s.currentAuthSession(r) {
		next.ServeHTTP(w, r)
		return
	}
	redirectToLogin(w, r)
}

// redirectToLogin sends a request without a session to the login page. An
// htmx request (a status poll, a "Test connection" button) would follow a
// plain redirect inside its XHR and swap the whole login page into a corner
// of the current one, so it gets HX-Redirect instead: htmx moves the browser
// itself to the login page, returning afterwards to the page that made the
// request rather than to the partial it asked for.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		returnTo := "/"
		if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil {
			returnTo = cleanReturnTo(u.RequestURI())
		}
		w.Header().Set("HX-Redirect", loginPath(returnTo))
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	// loginPath fixes the destination to /login and query-escapes the return path.
	// nosemgrep: go.lang.security.injection.open-redirect.open-redirect
	http.Redirect(w, r, loginPath(cleanReturnTo(r.URL.RequestURI())), http.StatusFound) //nolint:gosec // always /login; the request URI only rides along as an escaped, cleanReturnTo-restricted query value
}

func loginPath(returnTo string) string {
	return "/login?return_to=" + url.QueryEscape(returnTo)
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
	if s.currentAuthSession(r) {
		// cleanReturnTo rejects foreign origins; FuzzReturnTo checks the emitted Location.
		// codeql[go/unvalidated-url-redirection]
		http.Redirect(w, r, cleanReturnTo(r.URL.Query().Get("return_to")), http.StatusFound) //nolint:gosec // cleanReturnTo restricts the target to a same-origin relative path // nosemgrep: go.lang.security.injection.open-redirect.open-redirect
		return
	}
	returnTo := cleanReturnTo(r.URL.Query().Get("return_to"))

	signedOut := r.URL.Query().Has("signed_out")

	cfg := s.effectiveOIDCConfig()
	if !cfg.Enabled {
		s.renderLogin(w, loginData{Title: "Sign in", PasswordAuth: true, ReturnTo: returnTo, SignedOut: signedOut})
		return
	}

	data := loginData{
		Title:     "Sign in",
		LoginURL:  "/auth/login?return_to=" + url.QueryEscape(returnTo),
		SignedOut: signedOut,
	}
	if err := validateEffectiveOIDCConfig(cfg); err != nil {
		data.Error = err.Error()
	}
	// Auto-login starts from this page however the browser got here - a
	// protected page's redirect, a bookmark, an installed app's start URL -
	// except straight after signing out, which would otherwise sign the user
	// straight back in through the provider's still-live session.
	//
	// It deliberately goes through this rendered page, never a redirect
	// chain: committing a same-origin Coldarr document before leaving for the
	// provider keeps the page that opened Coldarr from being the browser's
	// triggering principal, which made Firefox apply Local Network Access
	// checks to a private OIDC callback.
	data.AutoLogin = cfg.AutoLogin && data.Error == "" && !signedOut
	s.renderLogin(w, data)
}

// renderLogin renders the login page in the portal layout: sign-in only,
// with none of the app's navigation.
func (s *Server) renderLogin(w http.ResponseWriter, data loginData) {
	s.renderTemplate(w, "login", "portal_layout", data)
}

// handlePasswordLogin verifies a submitted password against s.password
// (resolved once at startup - see resolvePassword) and, if it matches,
// creates the same kind of session OIDC login does. Only reachable while
// OIDC is disabled; requirePasswordSession is what actually gates access.
// Failed attempts are logged and limited per client (see signInLimiter).
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if s.effectiveOIDCConfig().Enabled {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	_ = r.ParseForm()
	returnTo := cleanReturnTo(r.FormValue("return_to"))
	attempt := r.FormValue("password")

	client := signInClient(r)
	if ok, report := s.signIns.allow(client, time.Now()); !ok {
		if report {
			log.Printf("auth: refusing password sign-ins from %s: too many failed attempts", clientAddr(r))
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(signInWindow/time.Second)))
		s.renderTemplateStatus(w, http.StatusTooManyRequests, "login", "portal_layout", loginData{
			Title:        "Sign in",
			PasswordAuth: true,
			ReturnTo:     returnTo,
			Error:        "Too many failed sign-in attempts. Wait 15 minutes and try again.",
		})
		return
	}

	if s.password == "" || subtle.ConstantTimeCompare([]byte(attempt), []byte(s.password)) != 1 {
		log.Printf("auth: wrong password from %s", clientAddr(r))
		s.renderLogin(w, loginData{
			Title:        "Sign in",
			PasswordAuth: true,
			ReturnTo:     returnTo,
			Error:        "Incorrect password.",
		})
		return
	}
	s.signIns.forget(client)

	if err := s.createAuthSession(w, r, effectiveOIDCConfig{}, "admin", "", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("auth: signed in with the password from %s", clientAddr(r))
	// cleanReturnTo rejects foreign origins; FuzzReturnTo checks the emitted Location.
	// codeql[go/unvalidated-url-redirection]
	http.Redirect(w, r, returnTo, http.StatusFound) //nolint:gosec // cleanReturnTo restricts the target to a same-origin relative path // nosemgrep: go.lang.security.injection.open-redirect.open-redirect
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

	state, err := randomURLToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nonce, err := randomURLToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	// The state only proves a callback answers a login Coldarr started; the
	// browser cookie proves it's this browser's. Without it, a callback for a
	// login someone started in their own browser would sign whoever opened
	// it in as them. Logins started together in several tabs share it.
	browser := oidcBrowser(r)
	if browser == "" {
		if browser, err = randomURLToken(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	s.storeOIDCState(state, oidcLoginState{
		Verifier:    verifier,
		Nonce:       nonce,
		ReturnTo:    cleanReturnTo(r.URL.Query().Get("return_to")),
		RedirectURI: oauthCfg.RedirectURL,
		Browser:     browser,
		Expires:     time.Now().Add(oidcStateTTL),
	})
	// Lax, unlike Strict, comes back on the provider's redirect to the
	// callback; Secure follows the effective HTTPS request, as the session's.
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // HttpOnly/SameSite are set below; Secure is conditional on requestIsHTTPS since plain-HTTP LAN deployments are supported
		Name:     cookieName(r, oidcBrowserCookie),
		Value:    browser,
		Path:     "/",
		MaxAge:   int(oidcStateTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, cfg),
	})

	authURL := oauthCfg.AuthCodeURL(
		state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // authURL is the configured OIDC provider's own authorization endpoint, not user input
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
	// The state comes first: until it matches a login this browser started,
	// nothing in the callback is the provider's answer to it - an error
	// message included, so a link can't put its own words on this page.
	query := r.URL.Query()
	state, ok := s.consumeOIDCState(query.Get("state"), oidcBrowser(r))
	if !ok {
		http.Error(w, "OIDC login state is missing or expired. Start the sign-in again, in this browser and from the address your identity provider sends you back to.", http.StatusBadRequest)
		return
	}
	if msg := query.Get("error"); msg != "" {
		http.Error(w, "OIDC provider refused the sign-in: "+msg, http.StatusUnauthorized)
		return
	}
	code := query.Get("code")
	if code == "" {
		http.Error(w, "OIDC callback did not include an authorization code", http.StatusBadRequest)
		return
	}

	provider, oauthCfg, err := s.oidcOAuthConfig(r.Context(), r, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// The token request repeats the redirect URI the login went out with,
	// whatever address the callback arrived at.
	oauthCfg.RedirectURL = state.RedirectURI
	token, err := oauthCfg.Exchange(r.Context(), code, oauth2.VerifierOption(state.Verifier))
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
	if err == nil {
		err = checkAuthorizedParty(idToken, cfg.ClientID)
	}
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

	if err := s.createAuthSession(w, r, cfg, identity.UserName, identity.Email, identity.Groups); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// cleanReturnTo rejects foreign origins; FuzzReturnTo checks the emitted Location.
	// nosemgrep: go.lang.security.injection.open-redirect.open-redirect
	http.Redirect(w, r, cleanReturnTo(state.ReturnTo), http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName(r, authSessionCookie)); err == nil {
		s.authMu.Lock()
		delete(s.authSessions, c.Value)
		s.authMu.Unlock()
	}
	// Secure follows the effective HTTPS request; plain HTTP on a LAN is supported.
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // HttpOnly/SameSite are set below; Secure is conditional on requestIsHTTPS since plain-HTTP LAN deployments are supported
		Name:     cookieName(r, authSessionCookie),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, s.effectiveOIDCConfig()),
	})
	http.Redirect(w, r, "/login?signed_out=1", http.StatusFound)
}

func (s *Server) oidcOAuthConfig(ctx context.Context, r *http.Request, cfg effectiveOIDCConfig) (*oidc.Provider, oauth2.Config, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, oauth2.Config{}, fmt.Errorf("discovering OIDC provider: %w", err)
	}
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = oidcOAuth2AuthStyle(cfg.TokenAuthMethod)
	oauthCfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     endpoint,
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
	setString("COLDARR_OIDC_TOKEN_AUTH_METHOD", &out.TokenAuthMethod)
	if post, ok := boolEnv("COLDARR_OIDC_CLIENT_SECRET_POST"); ok {
		envLocked = true
		if post {
			out.TokenAuthMethod = oidcTokenAuthClientPost
		} else {
			out.TokenAuthMethod = oidcTokenAuthClientBasic
		}
	}
	setBool("COLDARR_OIDC_AUTO_LOGIN", &out.AutoLogin)

	if out.RequiredGroup == "" {
		out.RequiredGroup = "coldarr"
	}
	if out.GroupsClaim == "" {
		out.GroupsClaim = "groups"
	}
	if out.TokenAuthMethod == "" {
		out.TokenAuthMethod = oidcTokenAuthAuto
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
	if !validOIDCTokenAuthMethod(cfg.TokenAuthMethod) {
		return fmt.Errorf("token auth method must be %q, %q, or %q", oidcTokenAuthAuto, oidcTokenAuthClientPost, oidcTokenAuthClientBasic)
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

func oidcOAuth2AuthStyle(method string) oauth2.AuthStyle {
	switch strings.TrimSpace(method) {
	case oidcTokenAuthClientPost:
		return oauth2.AuthStyleInParams
	case oidcTokenAuthClientBasic:
		return oauth2.AuthStyleInHeader
	default:
		return oauth2.AuthStyleAutoDetect
	}
}

func validOIDCTokenAuthMethod(method string) bool {
	switch strings.TrimSpace(method) {
	case "", oidcTokenAuthAuto, oidcTokenAuthClientPost, oidcTokenAuthClientBasic:
		return true
	default:
		return false
	}
}

func (s *Server) currentAuthSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName(r, authSessionCookie))
	if err != nil || c.Value == "" {
		return false
	}

	now := time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	sess, ok := s.authSessions[c.Value]
	if !ok {
		return false
	}
	if now.After(sess.Expires) {
		delete(s.authSessions, c.Value)
		return false
	}
	return true
}

func (s *Server) createAuthSession(w http.ResponseWriter, r *http.Request, cfg effectiveOIDCConfig, userName, email string, groups []string) error {
	id, err := randomURLToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(authSessionTTL)

	s.authMu.Lock()
	s.cleanupAuthLocked(time.Now())
	s.authSessions[id] = authSession{
		UserName: userName,
		Email:    email,
		Groups:   groups,
		Expires:  expires,
	}
	s.authMu.Unlock()

	// Secure follows the effective HTTPS request; plain HTTP on a LAN is supported.
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // HttpOnly/SameSite are set below; Secure is conditional on requestIsHTTPS since plain-HTTP LAN deployments are supported
		Name:     cookieName(r, authSessionCookie),
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

// storeOIDCState keeps a login in flight until its callback. Anyone can
// start one, so at most maxOIDCLogins are kept: past that, the oldest goes.
func (s *Server) storeOIDCState(state string, loginState oidcLoginState) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.cleanupAuthLocked(time.Now())
	for len(s.oidcStates) >= maxOIDCLogins {
		oldest := ""
		for key, kept := range s.oidcStates {
			if oldest == "" || kept.Expires.Before(s.oidcStates[oldest].Expires) {
				oldest = key
			}
		}
		delete(s.oidcStates, oldest)
	}
	s.oidcStates[state] = loginState
}

// consumeOIDCState hands over the login in flight that state names, once,
// if browser (its oidcBrowserCookie value) is the one that started it.
// Another browser's callback leaves the login alone: a link someone else
// made can't cancel it.
func (s *Server) consumeOIDCState(state, browser string) (oidcLoginState, bool) {
	if state == "" || browser == "" {
		return oidcLoginState{}, false
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	loginState, ok := s.oidcStates[state]
	if !ok {
		return oidcLoginState{}, false
	}
	if time.Now().After(loginState.Expires) {
		delete(s.oidcStates, state)
		return oidcLoginState{}, false
	}
	if subtle.ConstantTimeCompare([]byte(browser), []byte(loginState.Browser)) != 1 {
		return oidcLoginState{}, false
	}
	delete(s.oidcStates, state)
	return loginState, true
}

// oidcBrowser is the request's oidcBrowserCookie value, or "" when it has
// none that Coldarr could have made.
func oidcBrowser(r *http.Request) string {
	c, err := r.Cookie(cookieName(r, oidcBrowserCookie))
	if err != nil {
		return ""
	}
	if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err != nil || len(raw) != urlTokenBytes {
		return ""
	}
	return c.Value
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

// resolvePassword determines the password-auth secret used whenever OIDC
// is disabled, in order of precedence: COLDARR_PASSWORD_FILE (read from a
// docker secret or bind-mounted file - trimmed, since mounted secrets
// commonly carry a trailing newline), then COLDARR_PASSWORD, then - if
// neither is set - a freshly generated one-time password. The generated
// case is the only one that needs surfacing to the operator (via the
// returned bool): it lives only in this process's memory and is gone the
// moment it restarts.
func resolvePassword() (password string, generated bool, err error) {
	if path := strings.TrimSpace(os.Getenv(passwordFileEnvVar)); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // path is the operator's own COLDARR_PASSWORD_FILE env var, never derived from a request
		if err != nil {
			return "", false, fmt.Errorf("reading %s %s: %w", passwordFileEnvVar, path, err)
		}
		pw := strings.TrimSpace(string(data))
		if pw == "" {
			return "", false, fmt.Errorf("%s %s is empty", passwordFileEnvVar, path)
		}
		return pw, false, nil
	}
	if pw := strings.TrimSpace(os.Getenv(passwordEnvVar)); pw != "" {
		return pw, false, nil
	}

	b := make([]byte, generatedPasswordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("generating a random password: %w", err)
	}
	return hex.EncodeToString(b), true, nil
}

func logGeneratedPassword(password string) {
	log.Printf("======================================================================")
	log.Printf("OIDC is disabled and COLDARR_PASSWORD/COLDARR_PASSWORD_FILE aren't set -")
	log.Printf("generated a random password for this run. It will NOT persist: a new one")
	log.Printf("is generated every restart. Set COLDARR_PASSWORD (or COLDARR_PASSWORD_FILE")
	log.Printf("to read it from a mounted file/docker secret) for a stable password.")
	log.Printf("")
	// The one-time generated bootstrap password must be visible to the operator.
	// Configured passwords are never logged; this fallback changes on every restart.
	// codeql[go/clear-text-logging]
	log.Printf("    password: %s", password)
	log.Printf("======================================================================")
}

// urlTokenBytes is how many random bytes a token carries: a session ID, an
// OIDC login's state and nonce, and its browser cookie.
const urlTokenBytes = 32

func randomURLToken() (string, error) {
	b := make([]byte, urlTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func cleanReturnTo(raw string) string {
	// Logins in flight keep their return path, and anyone can start one: a
	// path longer than Coldarr's own ever get isn't kept.
	if raw == "" || len(raw) > maxReturnTo {
		return "/"
	}
	// Require a genuine single-segment path: raw[0] must be '/', and
	// raw[1] (if any) must be neither '/' nor '\' - some browsers treat a
	// leading "/\" the same as "//", so checking only for a literal "//"
	// prefix leaves a protocol-relative-redirect bypass open.
	if raw[0] != '/' || (len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\')) {
		return "/"
	}
	// A backslash further in can still end up leading: http.Redirect runs
	// path.Clean over everything before the first "?", a "#" fragment
	// included, so a "../" in the path or the fragment can pop the leading
	// segment and bring the backslash to the front. A "//" never survives
	// path.Clean, but a "\" does, and so does a control byte such as NUL,
	// which url.Parse accepts in a fragment and which then lands in a
	// malformed Location header. Reject both wherever they appear.
	if strings.ContainsFunc(raw, func(r rune) bool { return r == '\\' || r < ' ' || r == '\x7f' }) {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" {
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

// cookieName gives a cookie the __Host- prefix when the request came over
// HTTPS, to Coldarr itself or by a trusted proxy's account. A browser takes
// such a cookie only over HTTPS, from this host alone, for the whole site,
// so neither a sibling subdomain nor a plain-HTTP response can plant one or
// hide it. Unlike requestIsHTTPS, it goes by how the request came, never by
// settings, so changing them never changes which cookie a request reads.
func cookieName(r *http.Request, name string) string {
	if r.TLS != nil || r.URL.Scheme == "https" {
		return "__Host-" + name
	}
	return name
}

type oidcIdentity struct {
	Subject  string
	UserName string
	Email    string
	Groups   []string
}

// checkAuthorizedParty applies OIDC's rules for the client an ID token was
// issued to, which go-oidc leaves to its caller: an azp claim must name this
// client, and a token for several audiences must have one. Otherwise a token
// another client asked for, which merely lists this one among its audiences,
// would sign its user in here.
func checkAuthorizedParty(idToken *oidc.IDToken, clientID string) error {
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return fmt.Errorf("reading OIDC token claims: %w", err)
	}
	azp, named := claims["azp"]
	switch {
	case named:
		if party, _ := azp.(string); party != clientID {
			return fmt.Errorf("token was issued to another client (azp)")
		}
	case len(idToken.Audience) > 1:
		return fmt.Errorf("token has several audiences but no authorized party (azp)")
	}
	return nil
}

func oidcIdentityFromToken(ctx context.Context, provider *oidc.Provider, token *oauth2.Token, idToken *oidc.IDToken, groupsClaim string) (oidcIdentity, error) {
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return oidcIdentity{}, fmt.Errorf("reading OIDC token claims: %w", err)
	}

	// Userinfo isn't signed: it only fills in claims the ID token leaves out,
	// and only when it's about the user the ID token names.
	if userInfo, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token)); err == nil {
		var userClaims map[string]any
		switch {
		case userInfo.Subject == "" || userInfo.Subject != idToken.Subject:
			log.Printf("auth: ignored OIDC userinfo that wasn't about the signed-in user")
		case userInfo.Claims(&userClaims) == nil:
			for name, value := range userClaims {
				if _, signed := claims[name]; !signed {
					claims[name] = value
				}
			}
		}
	}

	identity := oidcIdentity{
		Subject: idToken.Subject,
		UserName: firstNonEmpty(
			stringClaim(claims["preferred_username"]),
			stringClaim(claims["name"]),
			stringClaim(claims["email"]),
			idToken.Subject,
		),
		Email:  stringClaim(claims["email"]),
		Groups: stringSliceClaim(claims[groupsClaim]),
	}
	if identity.Subject == "" {
		return oidcIdentity{}, fmt.Errorf("OIDC token did not include a subject")
	}
	return identity, nil
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
