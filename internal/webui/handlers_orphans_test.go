package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/orphans"
	"github.com/vocoder/coldarr/internal/secrets"
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

// TestOrphans_DownloadFolders: with qBittorrent connected, the scan
// compares the download folders against it and the page lists what it
// doesn't know about - deletable only where the tier is marked as not
// read-only, and gone from disk once deleted.
func TestOrphans_DownloadFolders(t *testing.T) {
	srv := newDownloadsTestServer(t)
	srv.qb.add(t, finishedDownload("a1", "Seeding", "manual", srv.ssd))
	if err := os.MkdirAll(filepath.Join(srv.ssd, "Leftover"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.seed, "stray.mkv"), make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := serveForm(srv.handleOrphansScanNow, "/settings/orphans/scan", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Download folders") || !strings.Contains(body, filepath.Join(srv.ssd, "Leftover")) || !strings.Contains(body, filepath.Join(srv.seed, "stray.mkv")) {
		t.Fatalf("panel should list both leftovers:\n%s", body)
	}
	if strings.Contains(body, filepath.Join(srv.ssd, "Seeding")) {
		t.Error("a download qBittorrent has is not an orphan")
	}
	if strings.Contains(body, `action="/settings/orphans/delete"`) || !strings.Contains(body, "Read-only, so no Delete button") {
		t.Error("read-only download folders offer no Delete button")
	}

	// A crafted delete against a read-only tier is still refused.
	rec = serveForm(srv.handleOrphanDelete, "/settings/orphans/delete", url.Values{"path": {filepath.Join(srv.ssd, "Leftover")}})
	if !strings.Contains(rec.Body.String(), "is read-only") {
		t.Fatalf("delete on a read-only tier:\n%s", rec.Body.String())
	}

	srv.cfg.Tiers[0].AllowDeletes = true
	page := httptest.NewRecorder()
	srv.handleOrphansPage(page, httptest.NewRequest(http.MethodGet, "/settings/orphans", nil))
	if strings.Count(page.Body.String(), `action="/settings/orphans/delete"`) != 1 {
		t.Errorf("only the not-read-only tier's orphan should be deletable:\n%s", page.Body.String())
	}

	rec = serveForm(srv.handleOrphanDelete, "/settings/orphans/delete", url.Values{"path": {filepath.Join(srv.ssd, "Leftover")}})
	if !strings.Contains(rec.Body.String(), "Deleted "+filepath.Join(srv.ssd, "Leftover")) {
		t.Fatalf("delete:\n%s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(srv.ssd, "Leftover")); !os.IsNotExist(err) {
		t.Error("the orphan should be gone from disk")
	}

	// Never alongside a scan.
	srv.scanOrphansMu.Lock()
	rec = serveForm(srv.handleOrphanDelete, "/settings/orphans/delete", url.Values{"path": {filepath.Join(srv.seed, "stray.mkv")}})
	srv.scanOrphansMu.Unlock()
	if !strings.Contains(rec.Body.String(), "a scan is in progress") {
		t.Errorf("delete during a scan:\n%s", rec.Body.String())
	}
}

// TestOrphans_NoDownloadPanelWithoutQbittorrent: a library-only install
// sees the page exactly as before.
func TestOrphans_NoDownloadPanelWithoutQbittorrent(t *testing.T) {
	srv := newSettingsTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleOrphansPage(rec, httptest.NewRequest(http.MethodGet, "/settings/orphans", nil))
	if strings.Contains(rec.Body.String(), "Download folders") || !strings.Contains(rec.Body.String(), "Candidates") {
		t.Errorf("library-only orphans page changed:\n%s", rec.Body.String())
	}
}

// TestScheduledOrphanScan_AlertsOnDownloadLeftovers: the scheduled scan's
// notification calls out download-folder leftovers, one by one when
// verbose.
func TestScheduledOrphanScan_AlertsOnDownloadLeftovers(t *testing.T) {
	srv := newDownloadsTestServer(t)
	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Notifications.Verbose = true
	if err := os.MkdirAll(filepath.Join(srv.ssd, "Leftover"), 0o750); err != nil {
		t.Fatal(err)
	}

	srv.runScheduledScanOrphans(time.Now())
	got := apprise.notifications()
	if len(got) != 2 || !strings.Contains(got[0]["body"], "1 item(s) in download folders") || got[0]["type"] != "warning" || !strings.Contains(got[1]["title"], "Leftover") {
		t.Fatalf("notifications = %+v, want a warning summary and the leftover itself", got)
	}
}
