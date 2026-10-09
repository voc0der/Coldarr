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

func coldTierForm(name, path string, edits map[string]string) url.Values {
	form := url.Values{
		"name":                {name},
		"role":                {"cold"},
		"paths":               {path},
		"media_movie":         {"on"},
		"media_tv":            {"on"},
		"require_mount":       {"on"},
		"target_used_percent": {"90"},
		"max_used_percent":    {"95"},
	}
	for k, v := range edits {
		form.Set(k, v)
	}
	return form
}

// TestTierCreate: a valid tier is added and saved, while anything
// ValidateTiers would reject - or that isn't a number at all - comes back
// on the form with what was typed, leaving the config alone.
func TestTierCreate(t *testing.T) {
	srv := newSettingsTestServer(t)
	path := t.TempDir()
	create := func(form url.Values) *httptest.ResponseRecorder {
		return serveForm(srv.handleTierCreate, "/settings/tiers", form)
	}

	rejected := []struct {
		name    string
		form    url.Values
		wantErr string
	}{
		{name: "no name", form: coldTierForm(" ", path, nil), wantErr: "name is required"},
		{name: "target not a number", form: coldTierForm("sat1", path, map[string]string{"target_used_percent": "ninety"}), wantErr: "target used percent must be a number"},
		{name: "max not a number", form: coldTierForm("sat1", path, map[string]string{"max_used_percent": "95%"}), wantErr: "max used percent must be a number"},
		{name: "target above max", form: coldTierForm("sat1", path, map[string]string{"target_used_percent": "97"}), wantErr: "target_used_percent must be in"},
		{name: "relative path", form: coldTierForm("sat1", "mnt/sat1", nil), wantErr: "must be absolute"},
	}
	for _, tt := range rejected {
		rec := create(tt.form)
		if rec.Code != http.StatusOK || !strings.Contains(html.UnescapeString(rec.Body.String()), tt.wantErr) {
			t.Errorf("%s: create = %d, want the form back with %q:\n%s", tt.name, rec.Code, tt.wantErr, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Add storage tier") {
			t.Errorf("%s: expected the Add form re-rendered", tt.name)
		}
	}
	if len(srv.currentConfig().Tiers) != 0 {
		t.Fatalf("rejected creates changed the config: %+v", srv.currentConfig().Tiers)
	}

	rec := create(coldTierForm("sat1", path+"\n\n  "+path+"/more  \n", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/tiers" {
		t.Fatalf("create = %d %q, want a redirect to the tiers page:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	reloaded, err := config.LoadForServer(srv.cfgPath)
	if err != nil {
		t.Fatalf("reloading saved config: %v", err)
	}
	for where, tiers := range map[string][]model.Tier{"live": srv.currentConfig().Tiers, "saved": reloaded.Tiers} {
		if len(tiers) != 1 {
			t.Fatalf("%s tiers = %+v, want the new tier", where, tiers)
		}
		tier := tiers[0]
		if tier.Name != "sat1" || tier.Role != model.RoleCold || len(tier.Paths) != 2 || tier.Paths[1] != path+"/more" ||
			!tier.AcceptsMediaType(model.Movie) || !tier.AcceptsMediaType(model.TV) || !tier.RequireMount ||
			tier.TargetUsedPercent != 90 || tier.MaxUsedPercent != 95 {
			t.Errorf("%s tier = %+v, want sat1 as submitted (blank path lines dropped)", where, tier)
		}
	}

	if rec := create(coldTierForm("sat1", t.TempDir(), nil)); !strings.Contains(html.UnescapeString(rec.Body.String()), `a storage tier named "sat1" already exists`) {
		t.Fatalf("a second tier with the same name should be refused:\n%s", rec.Body.String())
	}
}

func TestTierUpdate_RenamesAndRefusesConflicts(t *testing.T) {
	srv := newSettingsTestServer(t)
	hotPath, coldPath := t.TempDir(), t.TempDir()
	srv.cfg.Tiers = []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{hotPath}, Media: []model.MediaType{model.Movie}},
		{Name: "cold", Role: model.RoleCold, Paths: []string{coldPath}, Media: []model.MediaType{model.Movie}, MaxUsedPercent: 95, TargetUsedPercent: 90},
	}
	update := func(orig string, form url.Values) *httptest.ResponseRecorder {
		return serveForm(srv.handleTierUpdate, "/settings/tiers/"+orig, form, "name", orig)
	}

	if rec := update("cold", coldTierForm("hot", coldPath, nil)); !strings.Contains(html.UnescapeString(rec.Body.String()), `a storage tier named "hot" already exists`) {
		t.Errorf("renaming onto another tier's name should be refused:\n%s", rec.Body.String())
	}
	if rec := update("archive", coldTierForm("archive", coldPath, nil)); !strings.Contains(html.UnescapeString(rec.Body.String()), `storage tier "archive" not found`) {
		t.Errorf("updating a tier that doesn't exist should say so:\n%s", rec.Body.String())
	}
	if rec := update("cold", coldTierForm("", coldPath, nil)); !strings.Contains(rec.Body.String(), "name is required") || !strings.Contains(rec.Body.String(), "Edit storage tier") {
		t.Errorf("an update without a name should come back on the Edit form:\n%s", rec.Body.String())
	}

	if rec := update("cold", coldTierForm("cold-archive", coldPath, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("rename = %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	if tiers := srv.currentConfig().Tiers; len(tiers) != 2 || tiers[0].Name != "hot" || tiers[1].Name != "cold-archive" {
		t.Fatalf("tiers = %+v, want hot and the renamed cold-archive, in order", tiers)
	}
}

func TestTierDelete(t *testing.T) {
	srv := newSettingsTestServer(t)
	srv.cfg.Tiers = []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{t.TempDir()}, Media: []model.MediaType{model.Movie}},
		{Name: "cold", Role: model.RoleCold, Paths: []string{t.TempDir()}, Media: []model.MediaType{model.Movie}, MaxUsedPercent: 95, TargetUsedPercent: 90},
	}

	if rec := serveForm(srv.handleTierDelete, "/settings/tiers/cold/delete", nil, "name", "cold"); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	if tiers := srv.currentConfig().Tiers; len(tiers) != 1 || tiers[0].Name != "hot" {
		t.Fatalf("tiers = %+v, want only hot left", tiers)
	}

	// A config that can't be written leaves the tier in place and says so.
	if err := os.Mkdir(srv.cfgPath+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	rec := serveForm(srv.handleTierDelete, "/settings/tiers/hot/delete", nil, "name", "hot")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alert-error") {
		t.Fatalf("a failed delete should re-render the tiers page with the error:\n%s", rec.Body.String())
	}
	if len(srv.currentConfig().Tiers) != 1 {
		t.Fatal("a failed delete must leave the tier in place")
	}
}

func TestTierForms(t *testing.T) {
	srv := newSettingsTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleTierNewForm(rec, httptest.NewRequest(http.MethodGet, "/settings/tiers/new", nil))
	if !strings.Contains(rec.Body.String(), "Add storage tier") {
		t.Errorf("expected the Add form:\n%s", rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/settings/tiers/missing/edit", nil)
	req.SetPathValue("name", "missing")
	rec = httptest.NewRecorder()
	srv.handleTierEditForm(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("editing a tier that doesn't exist = %d, want 404", rec.Code)
	}
}

// TestTiersPage_FlagsMissingPathsAndSharedDisks: each path is checked live
// on the tiers page - a missing one shows why, and paths that are really
// the same disk are each told which others they share it with.
func TestTiersPage_FlagsMissingPathsAndSharedDisks(t *testing.T) {
	srv := newSettingsTestServer(t)
	root := t.TempDir()
	hotPath, coldPath, missing := filepath.Join(root, "hot"), filepath.Join(root, "cold"), filepath.Join(root, "unplugged")
	for _, p := range []string{hotPath, coldPath} {
		if err := os.Mkdir(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	srv.cfg.Tiers = []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{hotPath}, Media: []model.MediaType{model.Movie}},
		{Name: "cold", Role: model.RoleCold, Paths: []string{coldPath, missing}, Media: []model.MediaType{model.Movie}, MaxUsedPercent: 95, TargetUsedPercent: 90},
	}

	rows := srv.tierListRows()
	if len(rows) != 2 || len(rows[1].Paths) != 2 {
		t.Fatalf("rows = %+v, want both tiers with cold's two paths", rows)
	}
	if hot := rows[0].Paths[0]; !hot.Available || len(hot.SharesVolumeWith) != 1 || hot.SharesVolumeWith[0] != coldPath+" (cold)" {
		t.Errorf("hot path = %+v, want it available and sharing a disk with cold", hot)
	}
	if gone := rows[1].Paths[1]; gone.Available || !strings.Contains(gone.Err, "does not exist") || gone.SharesVolumeWith != nil {
		t.Errorf("missing path = %+v, want it unavailable with the reason and no disk to share", gone)
	}

	rec := httptest.NewRecorder()
	srv.handleTiersPage(rec, httptest.NewRequest(http.MethodGet, "/settings/tiers", nil))
	if body := rec.Body.String(); !strings.Contains(body, "shares a disk") || !strings.Contains(body, "is the drive mounted?") {
		t.Errorf("tiers page should flag the shared disk and the missing path:\n%s", body)
	}
}
