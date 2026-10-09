package webui

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/secrets"
)

// oidcSigningKey is shared by every fake issuer: an RSA key is the slowest
// thing these tests make, and none of them needs a fresh one.
var oidcSigningKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// fakeIssuer is just enough of an OpenID provider for Coldarr's login:
// discovery, signing keys, an authorization-code token endpoint that checks
// the PKCE verifier against the challenge from the login redirect, and
// userinfo. beginOIDCLogin arms it for one login.
type fakeIssuer struct {
	*httptest.Server

	mu        sync.Mutex
	challenge string         // the PKCE challenge the token request must answer
	claims    map[string]any // the ID token's claims, over iss/aud/iat/exp
	userinfo  map[string]any
	refuse    bool     // answer every token request with invalid_grant
	noIDToken bool     // answer without an id_token
	tokenAuth []string // how each token request sent the client secret
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{userinfo: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.URL,
			"authorization_endpoint":                f.URL + "/authorize",
			"token_endpoint":                        f.URL + "/token",
			"jwks_uri":                              f.URL + "/keys",
			"userinfo_endpoint":                     f.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		pub := oidcSigningKey().PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "test-key", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", f.serveToken)
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.userinfo)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeIssuer) serveToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	if _, _, ok := r.BasicAuth(); ok {
		f.tokenAuth = append(f.tokenAuth, "basic")
	} else if r.PostForm.Get("client_secret") != "" {
		f.tokenAuth = append(f.tokenAuth, "post")
	}

	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if f.refuse || r.PostForm.Get("grant_type") != "authorization_code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
		return
	}

	resp := map[string]any{"access_token": "access-1", "token_type": "Bearer", "expires_in": 3600}
	if !f.noIDToken {
		now := time.Now()
		claims := map[string]any{"iss": f.URL, "aud": "coldarr", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
		maps.Copy(claims, f.claims)
		raw, err := json.Marshal(claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp["id_token"] = oidctest.SignIDToken(oidcSigningKey(), "test-key", "RS256", string(raw))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// newOIDCTestServer builds a Server with OIDC enabled against issuerURL for
// client "coldarr", requiring group "coldarr". edit, if set, adjusts the
// OIDC config first.
func newOIDCTestServer(t *testing.T, issuerURL string, edit func(*config.OIDCAuthConfig)) *Server {
	t.Helper()
	dir := t.TempDir()
	connStore, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("secrets.LoadOrCreate: %v", err)
	}
	if err := connStore.Set(oidcSecretApp, storedOIDCSecret("coldarr", "client-secret")); err != nil {
		t.Fatalf("connStore.Set: %v", err)
	}
	oidcCfg := config.OIDCAuthConfig{
		Enabled: true, IssuerURL: issuerURL, ClientID: "coldarr",
		RequiredGroup: "coldarr", GroupsClaim: "groups", TokenAuthMethod: oidcTokenAuthAuto,
	}
	if edit != nil {
		edit(&oidcCfg)
	}
	srv, err := New(filepath.Join(dir, "coldarr.yaml"), &config.Config{Auth: config.AuthConfig{OIDC: oidcCfg}}, connStore)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// beginOIDCLogin starts a login the way the login page's link does and
// returns the authorization request Coldarr sent the browser to the
// provider with. It arms issuer to answer that request's PKCE challenge
// with an ID token carrying claims - plus the request's own nonce, unless
// claims sets a different one.
func beginOIDCLogin(t *testing.T, handler http.Handler, issuer *fakeIssuer, returnTo string, claims map[string]any) url.Values {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?return_to="+url.QueryEscape(returnTo), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login = %d %q, want a redirect to the provider", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host != issuer.URL || loc.Path != "/authorize" {
		t.Fatalf("GET /auth/login redirected to %q, want %s/authorize", rec.Header().Get("Location"), issuer.URL)
	}
	auth := loc.Query()

	idClaims := map[string]any{"nonce": auth.Get("nonce")}
	maps.Copy(idClaims, claims)
	issuer.mu.Lock()
	issuer.challenge = auth.Get("code_challenge")
	issuer.claims = idClaims
	issuer.mu.Unlock()
	return auth
}

// finishOIDCLogin plays the provider's redirect back to Coldarr.
func finishOIDCLogin(handler http.Handler, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?"+query, nil))
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == authSessionCookie {
			return c
		}
	}
	return nil
}

func TestOIDCLogin_FullFlowGrantsASession(t *testing.T) {
	issuer := newFakeIssuer(t)
	srv := newOIDCTestServer(t, issuer.URL, nil)
	handler := srv.routes()

	auth := beginOIDCLogin(t, handler, issuer, "/history", map[string]any{
		"sub": "user-1", "preferred_username": "alex", "email": "alex@example.com", "groups": []string{"media", "coldarr"},
	})
	for param, want := range map[string]string{
		"client_id":             "coldarr",
		"response_type":         "code",
		"redirect_uri":          "http://example.com/auth/callback",
		"code_challenge_method": "S256",
	} {
		if got := auth.Get(param); got != want {
			t.Errorf("authorization request %s = %q, want %q", param, got, want)
		}
	}
	for _, param := range []string{"state", "nonce", "code_challenge"} {
		if auth.Get(param) == "" {
			t.Errorf("authorization request has no %s", param)
		}
	}
	if scopes := strings.Fields(auth.Get("scope")); !slices.Contains(scopes, "openid") || !slices.Contains(scopes, "groups") {
		t.Errorf("scopes = %v, want openid and groups requested", scopes)
	}

	callback := "code=code-1&state=" + url.QueryEscape(auth.Get("state"))
	rec := finishOIDCLogin(handler, callback)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/history" {
		t.Fatalf("callback = %d %q (%s), want a redirect back to /history", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	cookie := sessionCookie(rec)
	if cookie == nil || !cookie.HttpOnly || cookie.Secure {
		t.Fatalf("session cookie = %+v, want an HttpOnly cookie (not Secure over plain HTTP)", cookie)
	}

	srv.authMu.Lock()
	sess := srv.authSessions[cookie.Value]
	srv.authMu.Unlock()
	if sess.UserName != "alex" || sess.Email != "alex@example.com" || !slices.Equal(sess.Groups, []string{"media", "coldarr"}) {
		t.Errorf("session = %+v, want alex's identity and groups", sess)
	}

	// The session now opens protected pages.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings/auth", nil)
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/auth with the OIDC session = %d, want 200", rec.Code)
	}

	// A login state is single-use: replaying the provider's redirect fails.
	if rec := finishOIDCLogin(handler, callback); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", rec.Code)
	}
}

// TestOIDCCallback_RefusesWhatItCannotTrust walks every way a callback can
// fail to prove who is signing in. None of them may leave a session behind.
func TestOIDCCallback_RefusesWhatItCannotTrust(t *testing.T) {
	member := map[string]any{"sub": "user-1", "groups": []string{"coldarr"}}
	with := func(edits map[string]any) map[string]any {
		claims := maps.Clone(member)
		maps.Copy(claims, edits)
		return claims
	}

	tests := []struct {
		name     string
		claims   map[string]any
		arm      func(*fakeIssuer)
		query    func(auth url.Values) string
		wantCode int
		wantBody string
	}{
		{name: "provider reported an error", query: func(url.Values) string { return "error=access_denied" }, wantCode: http.StatusUnauthorized, wantBody: "access_denied"},
		{name: "forged state", query: func(url.Values) string { return "code=code-1&state=forged" }, wantCode: http.StatusBadRequest, wantBody: "missing or expired"},
		{name: "token exchange refused", arm: func(f *fakeIssuer) { f.refuse = true }, wantCode: http.StatusBadGateway, wantBody: "exchanging OIDC code"},
		{name: "no ID token", arm: func(f *fakeIssuer) { f.noIDToken = true }, wantCode: http.StatusBadGateway, wantBody: "did not return an id_token"},
		{name: "token for another client", claims: with(map[string]any{"aud": "someone-else"}), wantCode: http.StatusUnauthorized, wantBody: "verifying OIDC token"},
		{name: "token from another issuer", claims: with(map[string]any{"iss": "https://elsewhere.example"}), wantCode: http.StatusUnauthorized, wantBody: "verifying OIDC token"},
		{name: "expired token", claims: with(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), wantCode: http.StatusUnauthorized, wantBody: "verifying OIDC token"},
		{name: "nonce from another login", claims: with(map[string]any{"nonce": "stale"}), wantCode: http.StatusUnauthorized, wantBody: "nonce did not match"},
		{name: "not in the required group", claims: with(map[string]any{"groups": []string{"family"}}), wantCode: http.StatusForbidden, wantBody: `required group "coldarr"`},
		{name: "no subject", claims: map[string]any{"groups": []string{"coldarr"}}, wantCode: http.StatusBadGateway, wantBody: "did not include a subject"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newFakeIssuer(t)
			srv := newOIDCTestServer(t, issuer.URL, nil)
			handler := srv.routes()

			claims := tt.claims
			if claims == nil {
				claims = member
			}
			auth := beginOIDCLogin(t, handler, issuer, "/", claims)
			if tt.arm != nil {
				issuer.mu.Lock()
				tt.arm(issuer)
				issuer.mu.Unlock()
			}
			query := "code=code-1&state=" + url.QueryEscape(auth.Get("state"))
			if tt.query != nil {
				query = tt.query(auth)
			}

			rec := finishOIDCLogin(handler, query)
			if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("callback = %d %q, want %d containing %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
			if cookie := sessionCookie(rec); cookie != nil {
				t.Fatalf("a refused callback set a session cookie: %+v", cookie)
			}
			srv.authMu.Lock()
			defer srv.authMu.Unlock()
			if len(srv.authSessions) != 0 {
				t.Fatalf("a refused callback created %d session(s)", len(srv.authSessions))
			}
		})
	}
}

// TestOIDCCallback_FallsBackToUserinfoClaims: providers that keep group
// membership (or a display name) out of the ID token still sign their
// members in - every claim the token leaves out is looked for in userinfo.
func TestOIDCCallback_FallsBackToUserinfoClaims(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.userinfo = map[string]any{"sub": "user-1", "name": "Alex Doe", "groups": "media, coldarr"}
	srv := newOIDCTestServer(t, issuer.URL, nil)
	handler := srv.routes()

	auth := beginOIDCLogin(t, handler, issuer, "/plan", map[string]any{"sub": "user-1"})
	rec := finishOIDCLogin(handler, "code=code-1&state="+url.QueryEscape(auth.Get("state")))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/plan" {
		t.Fatalf("callback = %d %q (%s), want a redirect to /plan", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	cookie := sessionCookie(rec)
	if cookie == nil {
		t.Fatal("expected a session cookie")
	}
	srv.authMu.Lock()
	sess := srv.authSessions[cookie.Value]
	srv.authMu.Unlock()
	if sess.UserName != "Alex Doe" || !slices.Equal(sess.Groups, []string{"media", "coldarr"}) {
		t.Fatalf("session = %+v, want the name and groups from userinfo", sess)
	}
}

// TestOIDCCallback_TokenRequestUsesTheConfiguredClientAuth: a provider that
// registered the client for client_secret_post rejects the secret in a
// Basic header, and vice versa - which is why the method is configurable.
func TestOIDCCallback_TokenRequestUsesTheConfiguredClientAuth(t *testing.T) {
	for method, want := range map[string]string{oidcTokenAuthClientPost: "post", oidcTokenAuthClientBasic: "basic"} {
		t.Run(method, func(t *testing.T) {
			issuer := newFakeIssuer(t)
			srv := newOIDCTestServer(t, issuer.URL, func(c *config.OIDCAuthConfig) { c.TokenAuthMethod = method })
			handler := srv.routes()

			auth := beginOIDCLogin(t, handler, issuer, "/", map[string]any{"sub": "user-1", "groups": []string{"coldarr"}})
			if rec := finishOIDCLogin(handler, "code=code-1&state="+url.QueryEscape(auth.Get("state"))); rec.Code != http.StatusFound {
				t.Fatalf("callback = %d %q, want a successful login", rec.Code, rec.Body.String())
			}
			issuer.mu.Lock()
			defer issuer.mu.Unlock()
			if !slices.Equal(issuer.tokenAuth, []string{want}) {
				t.Fatalf("token requests authenticated as %v, want [%s]", issuer.tokenAuth, want)
			}
		})
	}
}

// TestOIDCLogin_ConfiguredRedirectURLMakesTheSessionSecure: behind a TLS
// proxy Coldarr itself sees plain HTTP, so the configured https redirect
// URL is both what the provider is told to return to and the signal that
// the session cookie must be Secure.
func TestOIDCLogin_ConfiguredRedirectURLMakesTheSessionSecure(t *testing.T) {
	const redirect = "https://coldarr.example.com/auth/callback"
	issuer := newFakeIssuer(t)
	srv := newOIDCTestServer(t, issuer.URL, func(c *config.OIDCAuthConfig) { c.RedirectURL = redirect })
	handler := srv.routes()

	auth := beginOIDCLogin(t, handler, issuer, "/", map[string]any{"sub": "user-1", "groups": []string{"coldarr"}})
	if got := auth.Get("redirect_uri"); got != redirect {
		t.Fatalf("redirect_uri = %q, want the configured %q", got, redirect)
	}
	rec := finishOIDCLogin(handler, "code=code-1&state="+url.QueryEscape(auth.Get("state")))
	if cookie := sessionCookie(rec); cookie == nil || !cookie.Secure {
		t.Fatalf("session cookie = %+v, want it marked Secure", cookie)
	}
}

func TestOIDCLogin_UnreachableProvider(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.Close()
	srv := newOIDCTestServer(t, issuer.URL, nil)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "discovering OIDC provider") {
		t.Fatalf("GET /auth/login = %d %q, want 502 naming the discovery failure", rec.Code, rec.Body.String())
	}
}

// TestOIDC_MisconfiguredProviderFailsClosed: OIDC switched on without
// everything it needs must not leave the GUI open. Protected pages and both
// auth endpoints refuse with the reason, and the login page shows it rather
// than auto-starting a login that can't work.
func TestOIDC_MisconfiguredProviderFailsClosed(t *testing.T) {
	srv := newOIDCTestServer(t, "https://auth.example.com", func(c *config.OIDCAuthConfig) { c.AutoLogin = true })
	if err := srv.connStore.Delete(oidcSecretApp); err != nil {
		t.Fatalf("connStore.Delete: %v", err)
	}
	handler := srv.routes()

	for _, target := range []string{"/plan", "/auth/login", "/auth/callback?code=c&state=s"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "client secret is required") {
			t.Errorf("GET %s = %d %q, want 503 naming the missing secret", target, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "client secret is required") {
		t.Fatalf("GET /login = %d, want the configuration error shown:\n%s", rec.Code, body)
	}
	if strings.Contains(body, "data-auto-login") {
		t.Fatal("the login page must not auto-start a login that can't work")
	}
}

func TestOIDCEndpoints_GoHomeWhenOIDCIsOff(t *testing.T) {
	t.Setenv(passwordEnvVar, "pw")
	handler := newAuthTestServer(t, false).routes()

	for _, target := range []string{"/auth/login", "/auth/callback?code=c&state=s"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
			t.Errorf("GET %s with OIDC off = %d %q, want a redirect home", target, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestValidateEffectiveOIDCConfig(t *testing.T) {
	valid := effectiveOIDCConfig{
		OIDCAuthConfig: config.OIDCAuthConfig{IssuerURL: "https://auth.example.com", ClientID: "coldarr", GroupsClaim: "groups", TokenAuthMethod: oidcTokenAuthAuto},
		ClientSecret:   "secret",
	}
	if err := validateEffectiveOIDCConfig(valid); err != nil {
		t.Fatalf("validateEffectiveOIDCConfig(valid) = %v", err)
	}

	tests := []struct {
		name    string
		edit    func(*effectiveOIDCConfig)
		wantErr string
	}{
		{name: "no issuer", edit: func(c *effectiveOIDCConfig) { c.IssuerURL = " " }, wantErr: "issuer URL is required"},
		{name: "no client ID", edit: func(c *effectiveOIDCConfig) { c.ClientID = "" }, wantErr: "client ID is required"},
		{name: "no client secret", edit: func(c *effectiveOIDCConfig) { c.ClientSecret = "" }, wantErr: "client secret is required"},
		{name: "no groups claim", edit: func(c *effectiveOIDCConfig) { c.GroupsClaim = "" }, wantErr: "groups claim is required"},
		{name: "unknown token auth method", edit: func(c *effectiveOIDCConfig) { c.TokenAuthMethod = "private_key_jwt" }, wantErr: "token auth method must be"},
		{name: "issuer not a URL", edit: func(c *effectiveOIDCConfig) { c.IssuerURL = "auth.example.com" }, wantErr: "issuer URL is invalid"},
		{name: "redirect not a URL", edit: func(c *effectiveOIDCConfig) { c.RedirectURL = "coldarr/callback" }, wantErr: "redirect URL is invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.edit(&cfg)
			if err := validateEffectiveOIDCConfig(cfg); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateEffectiveOIDCConfig error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEffectiveOIDCConfig_ClientSecretPostEnvVar(t *testing.T) {
	store, err := secrets.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:       &config.Config{Auth: config.AuthConfig{OIDC: config.OIDCAuthConfig{TokenAuthMethod: oidcTokenAuthClientPost}}},
		connStore: store,
	}

	t.Setenv("COLDARR_OIDC_CLIENT_SECRET_POST", "false")
	if got := s.effectiveOIDCConfig(); got.TokenAuthMethod != oidcTokenAuthClientBasic || !got.EnvLocked {
		t.Fatalf("COLDARR_OIDC_CLIENT_SECRET_POST=false: method %q, locked %v, want %q and locked", got.TokenAuthMethod, got.EnvLocked, oidcTokenAuthClientBasic)
	}

	// An unparseable value still counts as set: it locks the settings and
	// reads as false, overriding the stored client_secret_post.
	t.Setenv("COLDARR_OIDC_CLIENT_SECRET_POST", "sometimes")
	if got := s.effectiveOIDCConfig(); got.TokenAuthMethod != oidcTokenAuthClientBasic || !got.EnvLocked {
		t.Fatalf("COLDARR_OIDC_CLIENT_SECRET_POST=sometimes: method %q, locked %v, want %q and locked", got.TokenAuthMethod, got.EnvLocked, oidcTokenAuthClientBasic)
	}
}
