package webui

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
)

func TestTierEditForm_HotMaxRemainsVisibleAndSubmitted(t *testing.T) {
	s, cfgPath, hotPath := newHotTierFormTestServer(t, 93)

	getReq := httptest.NewRequest(http.MethodGet, "/settings/tiers/hot/edit", nil)
	getReq.SetPathValue("name", "hot")
	getRec := httptest.NewRecorder()
	s.handleTierEditForm(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET edit status = %d, want 200: %s", getRec.Code, getRec.Body.String())
	}
	page := getRec.Body.String()
	if !strings.Contains(page, `id="max" name="max_used_percent" step="0.1" min="0" max="100" value="93"`) {
		t.Fatalf("hot edit form does not expose its configured max_used_percent: %s", page)
	}
	if strings.Contains(page, `document.getElementById("max").disabled`) {
		t.Fatal("hot edit form disables max_used_percent, so the browser would omit the YAML override on save")
	}
	if !strings.Contains(page, `document.getElementById("target").disabled = !isCold`) {
		t.Fatal("target_used_percent should remain disabled for hot tiers")
	}
	if !strings.Contains(page, "97.0% default") {
		t.Fatal("hot max hint does not explain the effective default ceiling")
	}

	form := url.Values{
		"name":                {"hot"},
		"role":                {"hot"},
		"paths":               {hotPath},
		"media_movie":         {"on"},
		"target_used_percent": {"42"}, // hot targets are ignored even if a client injects one
		"max_used_percent":    {"93"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/settings/tiers/hot", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.SetPathValue("name", "hot")
	postRec := httptest.NewRecorder()
	s.handleTierUpdate(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST edit status = %d, want 303: %s", postRec.Code, postRec.Body.String())
	}
	if got := s.currentConfig().Tiers[0].MaxUsedPercent; got != 93 {
		t.Fatalf("live hot max_used_percent = %v after save, want 93", got)
	}
	if got := s.currentConfig().Tiers[0].TargetUsedPercent; got != 0 {
		t.Fatalf("hot target_used_percent = %v after save, want 0", got)
	}

	reloaded, err := config.LoadForServer(cfgPath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if got := reloaded.Tiers[0].MaxUsedPercent; got != 93 {
		t.Fatalf("persisted hot max_used_percent = %v after save, want 93", got)
	}
}

func TestTierUpdate_BlankOrZeroHotMaxSelectsDefault(t *testing.T) {
	for _, submittedMax := range []string{"", "0"} {
		name := "blank"
		if submittedMax != "" {
			name = submittedMax
		}
		t.Run(name, func(t *testing.T) {
			s, _, hotPath := newHotTierFormTestServer(t, 93)
			form := url.Values{
				"name":             {"hot"},
				"role":             {"hot"},
				"paths":            {hotPath},
				"media_movie":      {"on"},
				"max_used_percent": {submittedMax},
			}
			req := httptest.NewRequest(http.MethodPost, "/settings/tiers/hot", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("name", "hot")
			rec := httptest.NewRecorder()
			s.handleTierUpdate(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("update status = %d, want 303: %s", rec.Code, rec.Body.String())
			}
			tier := s.currentConfig().Tiers[0]
			if tier.MaxUsedPercent != 0 {
				t.Fatalf("stored max_used_percent = %v, want 0", tier.MaxUsedPercent)
			}
			if tier.EffectiveMaxUsedPercent() != model.DefaultHotMaxUsedPercent {
				t.Fatalf("effective max_used_percent = %v, want default %v", tier.EffectiveMaxUsedPercent(), model.DefaultHotMaxUsedPercent)
			}
		})
	}
}

func TestTierUpdate_RejectsInvalidHotMaxWithoutChangingConfig(t *testing.T) {
	tests := []string{"not-a-number", "NaN", "+Inf", "-1", "100.1"}
	for _, submittedMax := range tests {
		t.Run(submittedMax, func(t *testing.T) {
			s, _, hotPath := newHotTierFormTestServer(t, 93)
			form := url.Values{
				"name":             {"hot"},
				"role":             {"hot"},
				"paths":            {hotPath},
				"media_movie":      {"on"},
				"max_used_percent": {submittedMax},
			}
			req := httptest.NewRequest(http.MethodPost, "/settings/tiers/hot", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("name", "hot")
			rec := httptest.NewRecorder()
			s.handleTierUpdate(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("invalid update status = %d, want rendered form status 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "max used percent") && !strings.Contains(body, "max_used_percent") {
				t.Fatalf("error page does not identify max used percent: %s", rec.Body.String())
			}
			if got := s.currentConfig().Tiers[0].MaxUsedPercent; got != 93 {
				t.Fatalf("invalid submission changed hot max_used_percent to %v, want 93", got)
			}
			if !strings.Contains(body, `name="name" value="hot"`) || !strings.Contains(html.UnescapeString(body), hotPath) {
				t.Fatalf("invalid max submission did not preserve the rest of the tier form: %s", body)
			}
		})
	}
}

func TestTiersPage_ShowsEffectiveHotCeiling(t *testing.T) {
	root := t.TempDir()
	defaultPath := filepath.Join(root, "hot-default")
	overridePath := filepath.Join(root, "hot-override")
	for _, path := range []string{defaultPath, overridePath} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	pages, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	s := &Server{pages: pages, cfg: &config.Config{Tiers: []model.Tier{
		{Name: "default", Role: model.RoleHot, Paths: []string{defaultPath}, Media: []model.MediaType{model.Movie}},
		{Name: "override", Role: model.RoleHot, Paths: []string{overridePath}, Media: []model.MediaType{model.Movie}, MaxUsedPercent: 99},
	}}}

	rec := httptest.NewRecorder()
	s.handleTiersPage(rec, httptest.NewRequest(http.MethodGet, "/settings/tiers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("tiers page status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	if !strings.Contains(page, "reclaim max 97.0% (default)") {
		t.Fatalf("tiers page does not show the effective default hot ceiling: %s", page)
	}
	if !strings.Contains(page, "reclaim max 99.0% (configured)") {
		t.Fatalf("tiers page does not distinguish an explicit hot ceiling: %s", page)
	}
}

func newHotTierFormTestServer(t *testing.T, max float64) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	hotPath := filepath.Join(root, "hot")
	if err := os.Mkdir(hotPath, 0o750); err != nil {
		t.Fatalf("mkdir hot tier: %v", err)
	}
	cfgPath := filepath.Join(root, "coldarr.yaml")
	raw := fmt.Sprintf("tiers:\n  - name: hot\n    role: hot\n    paths: [%q]\n    media_types: [movie]\n    max_used_percent: %v\n", hotPath, max)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadForServer(cfgPath)
	if err != nil {
		t.Fatalf("LoadForServer: %v", err)
	}
	pages, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	return &Server{cfgPath: cfgPath, cfg: cfg, pages: pages}, cfgPath, hotPath
}
