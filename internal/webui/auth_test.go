package webui

import (
	"testing"

	"golang.org/x/oauth2"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/secrets"
)

func TestCleanReturnTo(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "", want: "/"},
		{raw: "/plan?page=1", want: "/plan?page=1"},
		{raw: "https://evil.example/plan", want: "/"},
		{raw: "//evil.example/plan", want: "/"},
		{raw: "/auth/login", want: "/"},
		{raw: "/login", want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := cleanReturnTo(tt.raw); got != tt.want {
				t.Fatalf("cleanReturnTo(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestGroupAllowed(t *testing.T) {
	if !groupAllowed([]string{"coldarr", "media"}, "coldarr") {
		t.Fatal("expected required group to be allowed")
	}
	if groupAllowed([]string{"media"}, "coldarr") {
		t.Fatal("expected missing required group to be denied")
	}
	if !groupAllowed(nil, "") {
		t.Fatal("blank required group should allow authenticated users")
	}
}

func TestClaimStringSlice(t *testing.T) {
	primary := map[string]any{"groups": []any{"coldarr", "media"}}
	fallback := map[string]any{"groups": "fallback"}

	got := claimStringSlice(primary, fallback, "groups")
	if len(got) != 2 || got[0] != "coldarr" || got[1] != "media" {
		t.Fatalf("claimStringSlice array = %#v, want coldarr/media", got)
	}

	got = claimStringSlice(map[string]any{}, map[string]any{"groups": "coldarr, media"}, "groups")
	if len(got) != 2 || got[0] != "coldarr" || got[1] != "media" {
		t.Fatalf("claimStringSlice string fallback = %#v, want coldarr/media", got)
	}
}

func TestEffectiveOIDCConfigEnvOverridesStoredValues(t *testing.T) {
	store, err := secrets.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(oidcSecretApp, storedOIDCSecret("stored-client", "stored-secret")); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		cfg: &config.Config{Auth: config.AuthConfig{OIDC: config.OIDCAuthConfig{
			Enabled:       true,
			IssuerURL:     "https://stored.example",
			ClientID:      "stored-client",
			RequiredGroup: "coldarr",
			GroupsClaim:   "groups",
			AutoLogin:     true,
		}}},
		connStore: store,
	}

	t.Setenv("COLDARR_OIDC_ISSUER_URL", "https://env.example")
	t.Setenv("COLDARR_OIDC_CLIENT_ID", "env-client")
	t.Setenv("COLDARR_OIDC_CLIENT_SECRET", "env-secret")
	t.Setenv("COLDARR_OIDC_REQUIRED_GROUP", "env-group")
	t.Setenv("COLDARR_OIDC_CLIENT_SECRET_POST", "true")
	t.Setenv("COLDARR_OIDC_DISABLE_AUTO_LOGIN", "true")

	got := s.effectiveOIDCConfig()
	if got.IssuerURL != "https://env.example" || got.ClientID != "env-client" || got.ClientSecret != "env-secret" {
		t.Fatalf("env values did not override stored config: %+v", got)
	}
	if got.RequiredGroup != "env-group" {
		t.Fatalf("RequiredGroup = %q, want env-group", got.RequiredGroup)
	}
	if got.TokenAuthMethod != oidcTokenAuthClientPost {
		t.Fatalf("TokenAuthMethod = %q, want %q", got.TokenAuthMethod, oidcTokenAuthClientPost)
	}
	if got.AutoLogin {
		t.Fatal("COLDARR_OIDC_DISABLE_AUTO_LOGIN should force AutoLogin off")
	}
	if !got.EnvLocked {
		t.Fatal("expected EnvLocked when OIDC env vars are set")
	}
}

func TestOIDCOAuth2AuthStyle(t *testing.T) {
	tests := []struct {
		method string
		want   oauth2.AuthStyle
	}{
		{method: oidcTokenAuthAuto, want: oauth2.AuthStyleAutoDetect},
		{method: oidcTokenAuthClientPost, want: oauth2.AuthStyleInParams},
		{method: oidcTokenAuthClientBasic, want: oauth2.AuthStyleInHeader},
		{method: "", want: oauth2.AuthStyleAutoDetect},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			if got := oidcOAuth2AuthStyle(tt.method); got != tt.want {
				t.Fatalf("oidcOAuth2AuthStyle(%q) = %v, want %v", tt.method, got, tt.want)
			}
		})
	}
}
