package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/orphans"
)

// TestOrphansPage_ShowsTheLastScan: the page only ever shows the stored
// result of the last scan - biggest candidates first, with the total and
// each tier path's writability - and never scans on a page view.
func TestOrphansPage_ShowsTheLastScan(t *testing.T) {
	srv := newSettingsTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleOrphansPage(rec, httptest.NewRequest(http.MethodGet, "/settings/orphans", nil))
	if !strings.Contains(rec.Body.String(), "Never scanned yet.") {
		t.Fatalf("expected the never-scanned state:\n%s", rec.Body.String())
	}

	scanned := time.Date(2026, 9, 30, 4, 5, 0, 0, time.UTC)
	cache := filepath.Join(filepath.Dir(srv.cfgPath), "seeded-orphans.json")
	snap := orphans.Snapshot{
		ScannedAt: scanned,
		Candidates: []orphans.Candidate{
			{Path: "/cold/Small Leftover", TierName: "cold1", TierRole: model.RoleCold, SizeBytes: 1 << 30},
			{Path: "/hot/Big Leftover", TierName: "hot1", TierRole: model.RoleHot, SizeBytes: 5 << 30},
		},
		TierWritable:   map[string]bool{"/hot": true, "/cold": false},
		TierWriteError: map[string]string{"/cold": "read-only file system"},
		Warnings:       []string{"skipping /sat1: path /sat1 does not exist - is the drive mounted?"},
	}
	srv.orphanStore = seededOrphanStore(t, cache, snap)

	data := srv.orphansPageData()
	if data.ScannedAt != "2026-09-30 04:05" || data.TotalBytes != 6<<30 {
		t.Errorf("orphans page = scanned %q, total %d; want 2026-09-30 04:05 and 6 GiB", data.ScannedAt, data.TotalBytes)
	}
	if len(data.Candidates) != 2 || data.Candidates[0].Path != "/hot/Big Leftover" {
		t.Errorf("candidates = %+v, want the biggest first", data.Candidates)
	}
	if len(data.Tiers) != 2 || data.Tiers[0].Path != "/cold" || data.Tiers[0].Writable || data.Tiers[0].Error != "read-only file system" {
		t.Errorf("tier writability = %+v, want paths sorted with /cold's failure reason", data.Tiers)
	}

	rec = httptest.NewRecorder()
	srv.handleOrphansPage(rec, httptest.NewRequest(http.MethodGet, "/settings/orphans", nil))
	body := rec.Body.String()
	for _, want := range []string{"Last scanned: 2026-09-30 04:05", "2 folder(s), 6.0 GB total", "read-only - read-only file system", "is the drive mounted?"} {
		if !strings.Contains(body, want) {
			t.Errorf("orphans page is missing %q", want)
		}
	}
}

// seededOrphanStore writes snap where an orphan scan would have left it and
// loads it back.
func seededOrphanStore(t *testing.T, path string, snap orphans.Snapshot) *orphans.Store {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := orphans.Load(path)
	if err != nil {
		t.Fatalf("orphans.Load: %v", err)
	}
	return store
}

func TestOrphansScanNow(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)
	if err := os.MkdirAll(filepath.Join(coldDir, "Orphaned Movie"), 0o750); err != nil {
		t.Fatal(err)
	}

	rec := serveForm(srv.handleOrphansScanNow, "/settings/orphans/scan", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Scan finished.") || !strings.Contains(body, filepath.Join(coldDir, "Orphaned Movie")) {
		t.Fatalf("expected the scan to finish and list the orphan:\n%s", body)
	}
	if srv.getLastRanScanOrphans().IsZero() {
		t.Error("a manual scan should count as the task's last run")
	}

	// A second scan while one is running is refused, not run alongside it.
	srv.scanOrphansMu.Lock()
	rec = serveForm(srv.handleOrphansScanNow, "/settings/orphans/scan", nil)
	srv.scanOrphansMu.Unlock()
	if !strings.Contains(rec.Body.String(), "a scan is already in progress") {
		t.Fatalf("expected an overlapping scan refused:\n%s", rec.Body.String())
	}

	// A service that can't be read fails the scan, and the page says why.
	radarr.Close()
	rec = serveForm(srv.handleOrphansScanNow, "/settings/orphans/scan", nil)
	if !strings.Contains(rec.Body.String(), "fetching radarr movies") {
		t.Fatalf("expected the scan failure shown:\n%s", rec.Body.String())
	}
}
