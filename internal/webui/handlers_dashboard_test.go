package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/model"
)

func TestDashboardTemplate_ShowsEffectiveHotCeiling(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	defaultTier := model.Tier{Role: model.RoleHot}
	overrideTier := model.Tier{Role: model.RoleHot, MaxUsedPercent: 99}
	defaultRow := dashboardTierRow(defaultTier, "/hot-default")
	defaultRow.TierName = "default"
	defaultRow.Available = true
	overrideRow := dashboardTierRow(overrideTier, "/hot-override")
	overrideRow.TierName = "override"
	overrideRow.Available = true
	data := dashboardData{Title: "Dashboard", Rows: []tierRow{
		defaultRow,
		overrideRow,
	}}

	s := &Server{pages: pages}
	rec := httptest.NewRecorder()
	s.render(rec, "dashboard", data)
	page := rec.Body.String()
	if !strings.Contains(page, "reclaim max 97.0% (default)") {
		t.Fatalf("dashboard does not show the effective default hot ceiling: %s", page)
	}
	if !strings.Contains(page, "reclaim max 99.0% (configured)") {
		t.Fatalf("dashboard does not distinguish an explicit hot ceiling: %s", page)
	}
}

func TestDashboardHotUsageAboveReclaimMaxIsNotAnError(t *testing.T) {
	row := dashboardTierRow(model.Tier{Role: model.RoleHot}, "/hot")
	row.UsedPercent = 98.6
	if usagePastHardLimit(row) {
		t.Fatal("hot usage above the reclaim admission limit is valid runoff, not an over-max error")
	}
}

// TestDashboard_ReportsEachTierPathAndLibraryCounts renders the dashboard
// against real tier directories and a fake Radarr: each configured path gets
// its own row - a missing one marked unavailable with the reason - paths on
// one disk share a link color, and the library is counted by decision.
func TestDashboard_ReportsEachTierPathAndLibraryCounts(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)
	unplugged := filepath.Join(dir, "unplugged")
	srv.cfg.Tiers = append(srv.cfg.Tiers, model.Tier{
		Name: "sat1", Role: model.RoleCold, Paths: []string{unplugged}, Media: []model.MediaType{model.Movie},
		MaxUsedPercent: 95, TargetUsedPercent: 90,
	})

	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}

	for _, want := range []string{
		`<div class="value">1</div><div class="label">library items</div>`,
		`<div class="value">1</div><div class="label">cold candidates</div>`,
		"Quality-cutoff status has never been scanned",
		"path " + unplugged + " does not exist - is the drive mounted?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}
	// hot1 and cold1 sit on the same temp filesystem, so both rows carry the
	// first shared-disk color; the unplugged path has no disk to share.
	if n := strings.Count(body, "tier-link tier-link-1"); n != 2 {
		t.Errorf("rows colored as sharing a disk = %d, want 2 (hot1 and cold1)", n)
	}
	if strings.Count(body, "connected") != 1 || strings.Count(body, "not configured") != 2 {
		t.Errorf("expected Radarr connected and Sonarr/Jellyfin not configured:\n%s", body)
	}
}

func TestDashboard_ShowsWhyTheLibraryCouldNotBeRead(t *testing.T) {
	t.Run("arr app failing", func(t *testing.T) {
		dir, hotDir, coldDir := testTierDirs(t)
		failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(failing.Close)
		srv := newTestServer(t, dir, hotDir, coldDir, failing.URL, "", false)

		rec := httptest.NewRecorder()
		srv.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if !strings.Contains(rec.Body.String(), "fetching movies from radarr") {
			t.Fatalf("dashboard should explain the failed fetch:\n%s", rec.Body.String())
		}
	})

	t.Run("unreadable history", func(t *testing.T) {
		dir, hotDir, coldDir := testTierDirs(t)
		srv := newTestServer(t, dir, hotDir, coldDir, "", "", false)
		if err := os.WriteFile(srv.cfg.History.Path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		rec := httptest.NewRecorder()
		srv.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if !strings.Contains(rec.Body.String(), "parsing history") {
			t.Fatalf("dashboard should explain the unreadable history:\n%s", rec.Body.String())
		}
	})
}
