package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/secrets"
)

// TestHistoryPage_LinksToJellyfinItem pins that History rows get a Jellyfin
// link. A record only stores the tier roots a move went between, and the
// page used to look the destination root up as if it were the item's own
// folder - which never matches a Jellyfin item, so no row ever had one. The
// link has to follow the item to wherever it lives now, including after a
// later move - and be left out, not pointed at the folder the item left,
// when the move happened after the links cache last looked.
func TestHistoryPage_LinksToJellyfinItem(t *testing.T) {
	cases := []struct {
		name              string
		nowOnHot          bool // moved back to hot since the recorded move
		movedAfterRefresh bool // the links cache predates the move
		wantJFID          string
		staleJFID         string
	}{
		{name: "item still where the move left it", wantJFID: "jf-on-cold", staleJFID: "jf-on-hot"},
		{name: "item moved again since", nowOnHot: true, wantJFID: "jf-on-hot", staleJFID: "jf-on-cold"},
		// The cache saw Movie A on hot, then it moved: hot's item is the one
		// it left, and cold's isn't known to the cache yet - so no link.
		{name: "moved after the last links refresh", nowOnHot: true, movedAfterRefresh: true, staleJFID: "jf-on-hot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, hotDir, coldDir := testTierDirs(t)
			root := coldDir
			if tc.nowOnHot {
				root = hotDir
			}
			radarr := newFakeRadarr(t, root)
			srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

			// Until it re-indexes, Jellyfin still knows Movie A at both folders,
			// under a different ID at each.
			items := []map[string]any{
				{"Id": "jf-on-hot", "Path": hotDir + "/Movie A"},
				{"Id": "jf-on-cold", "Path": coldDir + "/Movie A"},
			}
			jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/System/Info":
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "srv-1"})
				case "/Users":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "u1"}})
				case "/Users/u1/Items":
					_ = json.NewEncoder(w).Encode(map[string]any{"Items": items})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(jf.Close)
			if err := srv.connStore.Set("jellyfin", secrets.Connection{URL: jf.URL, APIKey: "test", Enabled: true}); err != nil {
				t.Fatalf("connStore.Set jellyfin: %v", err)
			}

			// Recorded the way the mover records it: tier roots, not the item.
			recordMove := func() {
				hist, err := history.Load(srv.cfg.History.Path)
				if err != nil {
					t.Fatalf("history.Load: %v", err)
				}
				if err := hist.Append(history.Record{
					ArrApp: "radarr", ItemID: 1, Title: "Movie A",
					FromTier: "hot1", FromPath: hotDir, ToTier: "cold1", ToPath: coldDir,
					SizeBytes: 5_000_000, MovedAt: time.Now(),
				}); err != nil {
					t.Fatalf("history.Append: %v", err)
				}
			}
			refreshLinks := func() {
				eng, err := srv.newEngine()
				if err != nil {
					t.Fatalf("newEngine: %v", err)
				}
				if err := srv.linkCache.Refresh(eng.Radarr, eng.Sonarr, eng.JellyfinClient()); err != nil {
					t.Fatalf("linkCache.Refresh: %v", err)
				}
			}
			if tc.movedAfterRefresh {
				refreshLinks()
				time.Sleep(time.Millisecond) // a move strictly after the refresh
				recordMove()
			} else {
				recordMove()
				refreshLinks()
			}

			rec := httptest.NewRecorder()
			srv.handleHistoryPage(rec, httptest.NewRequest(http.MethodGet, "/history", nil))
			body := rec.Body.String()

			if !strings.Contains(body, "/movie/movie-a-2020") {
				t.Fatalf("row should still link to Radarr, got:\n%s", body)
			}
			if tc.wantJFID != "" && !strings.Contains(body, "details?id="+tc.wantJFID) {
				t.Fatalf("row should link to Jellyfin item %s, got:\n%s", tc.wantJFID, body)
			}
			if tc.wantJFID == "" && strings.Contains(body, "details?id=") {
				t.Fatalf("row should have no Jellyfin link yet, got:\n%s", body)
			}
			if tc.staleJFID != "" && strings.Contains(body, "details?id="+tc.staleJFID) {
				t.Fatalf("row links to Jellyfin item %s, where the item no longer is:\n%s", tc.staleJFID, body)
			}
		})
	}
}

// TestHistoryPage_PaginatesNewestFirst: the History page shows a fixed-size
// page of the newest moves first - which is also all a "Verify sizes" click
// checks - and clamps an out-of-range page instead of showing nothing.
func TestHistoryPage_PaginatesNewestFirst(t *testing.T) {
	sonarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "titleSlug": "show-1", "path": "/cold/Show 1"}]`))
	}))
	t.Cleanup(sonarr.Close)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var records []history.Record
	for i := 1; i <= historyPageSize+5; i++ {
		records = append(records, history.Record{ArrApp: "sonarr", ItemID: i, Title: fmt.Sprintf("Show %d", i), SizeBytes: 1 << 30, MovedAt: start.Add(time.Duration(i) * time.Hour)})
	}
	srv := newVerifyTestServer(t, "", sonarr.URL, records...)
	eng, err := srv.newEngine()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.linkCache.Refresh(nil, eng.Sonarr, nil); err != nil {
		t.Fatalf("linkCache.Refresh: %v", err)
	}

	first := srv.buildHistoryData(1)
	if first.TotalCount != historyPageSize+5 || first.TotalPages != 2 || len(first.Rows) != historyPageSize || !first.HasNext || first.HasPrev {
		t.Fatalf("page 1 = %d rows of %d (pages %d, prev %v, next %v), want a full first page of two", len(first.Rows), first.TotalCount, first.TotalPages, first.HasPrev, first.HasNext)
	}
	if newest := first.Rows[0].Title; newest != fmt.Sprintf("Show %d", historyPageSize+5) {
		t.Errorf("first row = %q, want the newest move first", newest)
	}

	last := srv.buildHistoryData(2)
	if len(last.Rows) != 5 || !last.HasPrev || last.HasNext || last.PrevPage != 1 {
		t.Fatalf("page 2 = %d rows (prev %v, next %v), want the 5 oldest", len(last.Rows), last.HasPrev, last.HasNext)
	}
	oldest := last.Rows[len(last.Rows)-1]
	if oldest.Title != "Show 1" || len(oldest.Links) != 1 || oldest.Links[0].URL != sonarr.URL+"/series/show-1" {
		t.Errorf("oldest row = %+v, want Show 1 linking to its Sonarr page", oldest)
	}

	if clamped := srv.buildHistoryData(99); clamped.Page != 2 || len(clamped.Rows) != 5 {
		t.Errorf("page 99 = page %d with %d rows, want it clamped to the last page", clamped.Page, len(clamped.Rows))
	}
	for raw, want := range map[string]int{"": 1, "abc": 1, "0": 1, "-3": 1, "2": 2} {
		if got := parsePage(raw); got != want {
			t.Errorf("parsePage(%q) = %d, want %d", raw, got, want)
		}
	}

	rec := httptest.NewRecorder()
	srv.handleHistoryPage(rec, httptest.NewRequest(http.MethodGet, "/history?page=2", nil))
	if body := rec.Body.String(); !strings.Contains(body, fmt.Sprintf("Page 2 of 2 (%d moves total)", historyPageSize+5)) || !strings.Contains(body, `href="/history?page=1"`) {
		t.Errorf("page 2 should say where it is and link back to page 1:\n%s", body)
	}
}

func TestHistoryPage_UnreadableHistory(t *testing.T) {
	srv := newVerifyTestServer(t, "", "")
	if err := os.WriteFile(srv.cfg.History.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data := srv.buildHistoryData(1); !strings.Contains(data.Error, "parsing history") || len(data.Rows) != 0 {
		t.Fatalf("buildHistoryData = %+v, want the history error and no rows", data)
	}
}
