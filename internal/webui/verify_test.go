package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/secrets"
)

func TestCompareSizesUsesTolerance(t *testing.T) {
	tests := []struct {
		name     string
		current  int64
		recorded int64
		want     string
	}{
		{
			name:     "exact match",
			current:  10 << 30,
			recorded: 10 << 30,
			want:     "match",
		},
		{
			name:     "tiny growth is still a match",
			current:  (10 << 30) + sizeCompareToleranceBytes,
			recorded: 10 << 30,
			want:     "match",
		},
		{
			name:     "tiny shrink is still a match",
			current:  (10 << 30) - sizeCompareToleranceBytes,
			recorded: 10 << 30,
			want:     "match",
		},
		{
			name:     "meaningful growth is flagged",
			current:  (10 << 30) + sizeCompareToleranceBytes + 1,
			recorded: 10 << 30,
			want:     "grew",
		},
		{
			name:     "meaningful shrink is flagged",
			current:  (10 << 30) - sizeCompareToleranceBytes - 1,
			recorded: 10 << 30,
			want:     "shrank",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareSizes(tt.current, tt.recorded); got != tt.want {
				t.Fatalf("compareSizes(%d, %d) = %q, want %q", tt.current, tt.recorded, got, tt.want)
			}
		})
	}
}

// arrSizeServer fakes the per-item lookups and rescans size verification
// makes. routes maps "METHOD /path" to a status and body; anything else is
// unexpected.
func arrSizeServer(t *testing.T, routes map[string]struct {
	status int
	body   string
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(route.status)
		_, _ = w.Write([]byte(route.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type route = struct {
	status int
	body   string
}

// newVerifyTestServer builds a Server whose history holds records (newest
// last), with Radarr and Sonarr at the given URLs.
func newVerifyTestServer(t *testing.T, radarrURL, sonarrURL string, records ...history.Record) *Server {
	t.Helper()
	dir, hotDir, coldDir := testTierDirs(t)
	srv := newTestServer(t, dir, hotDir, coldDir, radarrURL, "", false)
	if sonarrURL != "" {
		if err := srv.connStore.Set("sonarr", secrets.Connection{URL: sonarrURL, APIKey: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := history.Load(srv.cfg.History.Path)
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	for _, rec := range records {
		if err := hist.Append(rec); err != nil {
			t.Fatalf("history.Append: %v", err)
		}
	}
	return srv
}

// runVerify starts verification from the History page's form and waits for
// it to finish.
func runVerify(t *testing.T, srv *Server, mode string) verifyStatusData {
	t.Helper()
	rec := serveForm(srv.handleVerifyStart, "/history/verify", url.Values{"page": {"1"}, "mode": {mode}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/history/verify/status" {
		t.Fatalf("POST /history/verify = %d %q, want a redirect to the status page", rec.Code, rec.Header().Get("Location"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		data := srv.currentVerifyStatus()
		if !data.Running {
			return data
		}
		if time.Now().After(deadline) {
			t.Fatal("verification never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func entryByTitle(t *testing.T, data verifyStatusData, title string) verifyStatusEntryView {
	t.Helper()
	for _, e := range data.Entries {
		if e.Title == title {
			return e
		}
	}
	t.Fatalf("no verify entry for %q in %+v", title, data.Entries)
	return verifyStatusEntryView{}
}

// TestVerify_QuickModeJudgesWhatArrHasCached: a quick check compares each
// record against the size Radarr/Sonarr already has on file, flagging
// growth and shrinkage beyond the tolerance, items that are gone, and
// lookups that failed - each distinctly.
func TestVerify_QuickModeJudgesWhatArrHasCached(t *testing.T) {
	const gib = int64(1) << 30
	radarr := arrSizeServer(t, map[string]route{
		"GET /api/v3/movie/1": {200, `{"id": 1, "path": "/cold/Match", "sizeOnDisk": 10737418240}`},
		"GET /api/v3/movie/2": {200, `{"id": 2, "path": "/cold/Grew", "sizeOnDisk": 12884901888}`},
		"GET /api/v3/movie/4": {404, `{"message": "NotFound"}`},
		"GET /api/v3/movie/5": {500, `boom`},
	})
	sonarr := arrSizeServer(t, map[string]route{
		"GET /api/v3/series/3": {200, `{"id": 3, "path": "/cold/Shrank", "statistics": {"sizeOnDisk": 5368709120}}`},
	})
	moved := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	srv := newVerifyTestServer(t, radarr.URL, sonarr.URL,
		history.Record{ArrApp: "radarr", ItemID: 1, Title: "Match", SizeBytes: 10 * gib, MovedAt: moved},
		history.Record{ArrApp: "radarr", ItemID: 2, Title: "Grew", SizeBytes: 10 * gib, MovedAt: moved.Add(time.Hour)},
		history.Record{ArrApp: "sonarr", ItemID: 3, Title: "Shrank", SizeBytes: 10 * gib, MovedAt: moved.Add(2 * time.Hour)},
		history.Record{ArrApp: "radarr", ItemID: 4, Title: "Deleted", SizeBytes: 10 * gib, MovedAt: moved.Add(3 * time.Hour)},
		history.Record{ArrApp: "radarr", ItemID: 5, Title: "Unreachable", SizeBytes: 10 * gib, MovedAt: moved.Add(4 * time.Hour)},
		history.Record{ArrApp: "lidarr", ItemID: 6, Title: "Unknown App", SizeBytes: gib, MovedAt: moved.Add(5 * time.Hour)},
	)

	data := runVerify(t, srv, "quick")
	if data.Mode != "quick" || data.MatchCount != 1 || data.MismatchCount != 2 {
		t.Fatalf("verify summary = mode %q, %d match, %d mismatched; want quick, 1 and 2", data.Mode, data.MatchCount, data.MismatchCount)
	}

	checks := []struct {
		title, sizeStatus, currentGB, note, err string
	}{
		{title: "Match", sizeStatus: "match", currentGB: "10.0 GB"},
		{title: "Grew", sizeStatus: "grew", currentGB: "12.0 GB", note: "larger than when moved"},
		{title: "Shrank", sizeStatus: "shrank", currentGB: "5.0 GB", note: "smaller than when moved"},
		{title: "Deleted", sizeStatus: "unknown", note: "no longer found in radarr"},
		{title: "Unreachable", err: "500"},
		{title: "Unknown App", err: `unknown arr app "lidarr"`},
	}
	for _, c := range checks {
		e := entryByTitle(t, data, c.title)
		if c.err != "" {
			if e.Status != "failed" || !strings.Contains(e.Err, c.err) {
				t.Errorf("%s = %+v, want failed with %q", c.title, e, c.err)
			}
			continue
		}
		if e.Status != "done" || e.SizeStatus != c.sizeStatus || e.CurrentGB != c.currentGB || !strings.Contains(e.Note, c.note) {
			t.Errorf("%s = %+v, want done/%s at %q noting %q", c.title, e, c.sizeStatus, c.currentGB, c.note)
		}
		if e.RecordedGB == "" || e.ArrGB != "" {
			t.Errorf("%s = %+v, want the recorded size shown and no separate Arr size in quick mode", c.title, e)
		}
	}

	page := httptest.NewRecorder()
	srv.handleVerifyStatus(page, httptest.NewRequest(http.MethodGet, "/history/verify/status", nil))
	if body := page.Body.String(); !strings.Contains(body, "1 match, 2 mismatched") || !strings.Contains(body, "quick check") || strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Errorf("status page should show the finished quick check without polling:\n%s", body)
	}
	partial := httptest.NewRecorder()
	srv.handleVerifyStatusPartial(partial, httptest.NewRequest(http.MethodGet, "/history/verify/status/partial", nil))
	if body := partial.Body.String(); !strings.HasPrefix(strings.TrimSpace(body), `<div id="verify-status"`) || strings.Contains(body, "<html") {
		t.Errorf("the partial should be just the status table:\n%s", body)
	}
}

// TestVerify_CompleteModeCountsTheFilesOnDisk: a complete check has
// Radarr/Sonarr rescan, then measures the media files itself - ignoring the
// artwork and metadata beside them - and judges the record by what's
// physically there, showing the Arr's own figure alongside.
func TestVerify_CompleteModeCountsTheFilesOnDisk(t *testing.T) {
	movieDir := t.TempDir()
	mustSparseFile(t, filepath.Join(movieDir, "Movie A.mkv"), 3<<20)
	mustSparseFile(t, filepath.Join(movieDir, "poster.jpg"), 1<<20)
	mustSparseFile(t, filepath.Join(movieDir, "movie.nfo"), 4<<10)
	showDir := t.TempDir()
	mustSparseFile(t, filepath.Join(showDir, "Season 01", "S01E01.MP4"), 2<<20)

	radarr := arrSizeServer(t, map[string]route{
		"POST /api/v3/command":  {201, `{"id": 7, "status": "queued"}`},
		"GET /api/v3/command/7": {200, `{"id": 7, "status": "completed"}`},
		// Radarr's cached figure is stale; the verdict must come from disk.
		"GET /api/v3/movie/1": {200, `{"id": 1, "path": "` + movieDir + `", "sizeOnDisk": 999}`},
		"GET /api/v3/movie/2": {200, `{"id": 2, "path": "` + filepath.Join(movieDir, "gone") + `", "sizeOnDisk": 5}`},
	})
	sonarr := arrSizeServer(t, map[string]route{
		"POST /api/v3/command": {503, `busy`},
		"GET /api/v3/series/3": {200, `{"id": 3, "path": "` + showDir + `", "statistics": {"sizeOnDisk": 2097152}}`},
	})
	srv := newVerifyTestServer(t, radarr.URL, sonarr.URL,
		history.Record{ArrApp: "radarr", ItemID: 1, Title: "Movie A", SizeBytes: 3 << 20, MovedAt: time.Now()},
		history.Record{ArrApp: "radarr", ItemID: 2, Title: "Vanished Folder", SizeBytes: 5, MovedAt: time.Now()},
		history.Record{ArrApp: "sonarr", ItemID: 3, Title: "Show A", SizeBytes: 2 << 20, MovedAt: time.Now()},
	)

	data := runVerify(t, srv, "complete")
	if data.Mode != "complete" || data.MatchCount != 2 {
		t.Fatalf("verify summary = mode %q, %d match; want complete and 2", data.Mode, data.MatchCount)
	}

	movie := entryByTitle(t, data, "Movie A")
	if movie.SizeStatus != "match" || movie.CurrentGB != "0.0 GB" || movie.ArrGB != "0.0 GB" || movie.Note != "" {
		t.Errorf("Movie A = %+v, want a clean match judged on disk, with Radarr's size alongside", movie)
	}
	show := entryByTitle(t, data, "Show A")
	if show.SizeStatus != "match" || !strings.Contains(show.Note, "rescan request failed") || !strings.Contains(show.Note, "checked the files on disk anyway") {
		t.Errorf("Show A = %+v, want a match noting the failed rescan", show)
	}
	if gone := entryByTitle(t, data, "Vanished Folder"); gone.Status != "failed" || !strings.Contains(gone.Err, "walking") {
		t.Errorf("Vanished Folder = %+v, want it failed walking a folder that isn't there", gone)
	}

	// The exact byte counts behind those rounded figures.
	snap := srv.currentVerify.Snapshot()
	for _, e := range snap.Entries {
		if e.Record.Title == "Movie A" && (e.CurrentSize != 3<<20 || e.ArrSize != 999) {
			t.Errorf("Movie A measured %d bytes on disk (Radarr says %d), want %d and 999", e.CurrentSize, e.ArrSize, 3<<20)
		}
	}
}

// TestVerifyStart_OneRunAtATime: while a check is still running, starting
// another just shows the one already in progress.
func TestVerifyStart_OneRunAtATime(t *testing.T) {
	srv := newVerifyTestServer(t, "", "")
	running := newVerifyProgress([]history.Record{{Title: "Still checking"}}, verifyQuick)
	srv.currentVerify = running

	rec := serveForm(srv.handleVerifyStart, "/history/verify", url.Values{"mode": {"complete"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/history/verify/status" {
		t.Fatalf("POST /history/verify = %d %q, want a redirect to the running check", rec.Code, rec.Header().Get("Location"))
	}
	if srv.currentVerify != running {
		t.Fatal("a second check must not replace the one still running")
	}
}

func TestVerifyStatus_BeforeAnyRun(t *testing.T) {
	srv := newVerifyTestServer(t, "", "")
	rec := httptest.NewRecorder()
	srv.handleVerifyStatus(rec, httptest.NewRequest(http.MethodGet, "/history/verify/status", nil))
	if !strings.Contains(rec.Body.String(), "No verification has been run yet") {
		t.Fatalf("expected the empty state:\n%s", rec.Body.String())
	}
}

func TestVerifyStart_UnreadableHistory(t *testing.T) {
	srv := newVerifyTestServer(t, "", "")
	if err := os.WriteFile(srv.cfg.History.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := serveForm(srv.handleVerifyStart, "/history/verify", url.Values{"mode": {"quick"}})
	if !strings.Contains(rec.Body.String(), "parsing history") || srv.currentVerify != nil {
		t.Fatalf("expected the history error shown and no check started:\n%s", rec.Body.String())
	}
}

func mustSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}
