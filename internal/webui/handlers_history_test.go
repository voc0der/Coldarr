package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
