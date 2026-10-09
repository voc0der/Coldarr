package jellyfin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// assertAuthorized pins the credential on a request. Getting the scheme
// wrong locks Coldarr out of every Jellyfin 12 server, and no other
// assertion in this file would notice: the test servers here never check
// credentials.
func assertAuthorized(t *testing.T, r *http.Request) {
	t.Helper()
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, "MediaBrowser ") {
		t.Errorf("Authorization = %q, want the MediaBrowser scheme", got)
	}
	if !strings.Contains(got, `Token="key"`) {
		t.Errorf(`Authorization = %q, want it to carry Token="key"`, got)
	}
}

// TestClient_Post_Authorizes covers writes as well as reads: the two share
// no code path, and a refresh that 401s is easy to miss because the failure
// surfaces as an item that simply never updated.
func TestClient_Post_Authorizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorized(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).RefreshItem("item-1", FullRefreshOptions()); err != nil {
		t.Fatalf("RefreshItem: %v", err)
	}
}

func TestClient_StartScheduledTask_ResolvesKeyAndStartsOnce(t *testing.T) {
	starts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorized(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ScheduledTasks":
			_, _ = w.Write([]byte(`[
				{"Id":"other-id","Key":"RefreshLibrary","State":"Idle"},
				{"Id":"restore-runtime-id","Key":"UserDataRestore","State":"Idle"}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/ScheduledTasks/Running/restore-runtime-id":
			starts++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).StartScheduledTask(UserDataRestoreTaskKey); err != nil {
		t.Fatalf("StartScheduledTask: %v", err)
	}
	if starts != 1 {
		t.Fatalf("task starts = %d, want exactly 1", starts)
	}
}

func TestClient_StartScheduledTask_AlreadyRunningDoesNotRestart(t *testing.T) {
	starts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ScheduledTasks":
			_, _ = w.Write([]byte(`[{"Id":"restore-runtime-id","Key":"UserDataRestore","State":"Running"}]`))
		case r.Method == http.MethodPost:
			starts++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).StartScheduledTask(UserDataRestoreTaskKey); err != nil {
		t.Fatalf("StartScheduledTask: %v", err)
	}
	if starts != 0 {
		t.Fatalf("task starts = %d, want 0 for a task already running", starts)
	}
}

func TestClient_StartScheduledTask_RequiredPluginMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"Id":"other-id","Key":"RefreshLibrary","State":"Idle"}]`))
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).StartScheduledTask(UserDataRestoreTaskKey)
	if err == nil || !strings.Contains(err.Error(), "required plugin") {
		t.Fatalf("error = %v, want a required-plugin explanation", err)
	}
}

func TestClient_Ping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		assertAuthorized(t, r)
		_, _ = w.Write([]byte(`{"Version": "10.9.0", "ServerName": "home"}`))
	}))
	defer srv.Close()

	version, name, err := NewClient(srv.URL, "key").Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if version != "10.9.0" || name != "home" {
		t.Fatalf("Ping() = (%q, %q), want (10.9.0, home)", version, name)
	}
}

func TestClient_Ping_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL, "bad-key").Ping(); err == nil {
		t.Fatal("expected an error for a 401 response")
	}
}

func TestClient_RefreshLibrary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "key").RefreshLibrary(); err != nil {
		t.Fatalf("RefreshLibrary: %v", err)
	}
}

func TestClient_ServerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Id": "abc123"}`))
	}))
	defer srv.Close()

	id, err := NewClient(srv.URL, "key").ServerID()
	if err != nil {
		t.Fatalf("ServerID: %v", err)
	}
	if id != "abc123" {
		t.Fatalf("ServerID() = %q, want abc123", id)
	}
}

// testClient is a client wired for tests: logs routed to t, and polling
// fast enough that a resolve-retry test finishes in milliseconds.
func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := NewClient(baseURL, "key")
	c.Logf = t.Logf
	c.ResolvePollInterval = time.Millisecond
	c.ResolveTimeout = 200 * time.Millisecond
	return c
}

// TestClient_RefreshItem_SendsExplicitModes pins the exact query string.
// Jellyfin defaults metadataRefreshMode and imageRefreshMode to "None"
// when they're omitted, so a refresh that forgets them is a no-op that
// still answers 204 - the failure this asserts against is silent.
func TestClient_RefreshItem_SendsExplicitModes(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Items/item-1/Refresh" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		got = r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).RefreshItem("item-1", FullRefreshOptions()); err != nil {
		t.Fatalf("RefreshItem: %v", err)
	}

	want := map[string]string{
		"metadataRefreshMode": "FullRefresh",
		"imageRefreshMode":    "FullRefresh",
		"replaceAllMetadata":  "true",
		"replaceAllImages":    "true",
		"regenerateTrickplay": "false",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestClient_RefreshItem_NotFoundIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such item", http.StatusNotFound)
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).RefreshItem("stale-id", FullRefreshOptions())
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("RefreshItem error = %v, want ErrItemNotFound", err)
	}
}

func TestClient_ReportMediaUpdated_SendsPaths(t *testing.T) {
	var body struct {
		Updates []mediaUpdate `json:"Updates"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Media/Updated" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).ReportMediaUpdated([]string{"/cold/Movie A", ""}, "Created"); err != nil {
		t.Fatalf("ReportMediaUpdated: %v", err)
	}

	// The empty path is dropped rather than sent - Jellyfin's handler
	// throws on a null path and rejects the whole batch.
	if len(body.Updates) != 1 {
		t.Fatalf("sent %d updates, want 1: %+v", len(body.Updates), body.Updates)
	}
	if body.Updates[0].Path != "/cold/Movie A" || body.Updates[0].UpdateType != "Created" {
		t.Errorf("update = %+v", body.Updates[0])
	}
}

// TestClient_ResolveAndRefresh_ResolvesNewPathThenRefreshes covers the
// whole point of the sequence: the item ID is re-resolved from the item's
// NEW path (Jellyfin hashes paths into IDs, so the pre-move ID is dead),
// and the refresh targets that ID. The Movie's Path is the video file, one
// level below the folder Radarr reports, which is what makes the
// itemFolderPath normalization load-bearing here.
//
// It also pins that resolving reports nothing itself, which is what makes
// reporting each item mid-run safe: Jellyfin folds a repeat report into
// the refresher already pending for that path and restarts its timer, so
// re-reporting here would push back by another LibraryMonitorDelay the
// very rescan this is waiting on.
func TestClient_ResolveAndRefresh_ResolvesNewPathThenRefreshes(t *testing.T) {
	var refreshed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "cold-movie-1", "Path": "/cold/Movie A/Movie A.mkv", "Type": "Movie"}]}`))
		case strings.HasSuffix(r.URL.Path, "/Refresh"):
			refreshed = append(refreshed, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/Items/"), "/Refresh"))
			if r.URL.Query().Get("imageRefreshMode") != "FullRefresh" || r.URL.Query().Get("replaceAllImages") != "true" {
				t.Errorf("refresh did not request image replacement: %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			// Catches /Library/Media/Updated in particular.
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "Movie A", OldPath: "/hot/Movie A", NewPath: "/cold/Movie A"},
	})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	if len(refreshed) != 1 || refreshed[0] != "cold-movie-1" {
		t.Errorf("refreshed = %v, want [cold-movie-1] (the ID at the NEW path)", refreshed)
	}
}

// TestClient_ResolveAndRefresh_UnresolvedItemIsReported guards the case
// that used to be invisible: Jellyfin never surfaces the item at its new
// path, and the caller has to learn about it rather than get a cheerful
// nil.
func TestClient_ResolveAndRefresh_UnresolvedItemIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": []}`))
		default:
			t.Errorf("nothing should be refreshed, got %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "Movie A", OldPath: "/hot/Movie A", NewPath: "/cold/Movie A"},
	})
	if err == nil {
		t.Fatal("expected an error when the item never appears at its new path")
	}
	if !strings.Contains(err.Error(), "Movie A") {
		t.Errorf("error should name the unrefreshed item, got: %v", err)
	}
}

// TestClient_ResolveAndRefresh_SameTitledItemsReportedSeparately covers a
// real library shape: two distinct items sharing a title (a remake, or the
// same show tracked under two roots). They are different files in
// different folders, so both can fail independently and an operator needs
// to see both - keying the failure set by title silently collapsed them
// into one, under-reporting how much artwork was left stale.
func TestClient_ResolveAndRefresh_SameTitledItemsReportedSeparately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": []}`))
		default:
			t.Errorf("nothing should be refreshed, got %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "The Thing", OldPath: "/hot/The Thing (1982)", NewPath: "/cold/The Thing (1982)"},
		{Title: "The Thing", OldPath: "/hot/The Thing (2011)", NewPath: "/cold/The Thing (2011)"},
	})
	if err == nil {
		t.Fatal("expected an error when neither item appears at its new path")
	}
	if !strings.Contains(err.Error(), "2 item(s)") {
		t.Errorf("both same-titled items must be counted, got: %v", err)
	}
	for _, want := range []string{"/cold/The Thing (1982)", "/cold/The Thing (2011)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q so it's actionable, got: %v", want, err)
		}
	}
}

// fakeJellyfinServer returns a server with two users, where a Movie is
// favorited only by user 2 and a Series is visible to both - exercising
// FavoritePaths/LibraryItemIDs' per-user union-and-dedup logic. The Movie's
// Path points at the video file itself (real Jellyfin behavior), one level
// inside "/hot/Movie A" - the folder Radarr actually reports - unlike the
// Series' Path, which is already its folder.
func fakeJellyfinServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}, {"Id": "u2"}]`))
		case "/Users/u1/Items":
			if r.URL.Query().Get("Filters") == "IsFavorite" {
				_, _ = w.Write([]byte(`{"Items": []}`))
				return
			}
			_, _ = w.Write([]byte(`{"Items": [{"Id": "series-1", "Path": "/hot/Show A", "Type": "Series"}]}`))
		case "/Users/u2/Items":
			if r.URL.Query().Get("Filters") == "IsFavorite" {
				_, _ = w.Write([]byte(`{"Items": [{"Id": "movie-1", "Path": "/hot/Movie A/Movie A.mkv", "Type": "Movie"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"Items": [{"Id": "movie-1", "Path": "/hot/Movie A/Movie A.mkv", "Type": "Movie"}, {"Id": "series-1", "Path": "/hot/Show A", "Type": "Series"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestClient_FavoritePaths(t *testing.T) {
	srv := fakeJellyfinServer(t)
	defer srv.Close()

	paths, err := NewClient(srv.URL, "key").FavoritePaths()
	if err != nil {
		t.Fatalf("FavoritePaths: %v", err)
	}
	if !paths["/hot/Movie A"] {
		t.Errorf("expected /hot/Movie A (favorited by u2) to be present, got %+v", paths)
	}
	if paths["/hot/Show A"] {
		t.Error("Show A is not favorited by anyone and must not appear")
	}
}

func TestClient_LibraryItemIDs_DedupsAcrossUsers(t *testing.T) {
	srv := fakeJellyfinServer(t)
	defer srv.Close()

	ids, err := NewClient(srv.URL, "key").LibraryItemIDs()
	if err != nil {
		t.Fatalf("LibraryItemIDs: %v", err)
	}
	// Both users see Movie A - it must appear exactly once (map keys can't
	// duplicate, but the underlying union logic could still overwrite with
	// wrong data if broken).
	if ids["/hot/Movie A"] != "movie-1" {
		t.Errorf("ids[/hot/Movie A] = %q, want movie-1", ids["/hot/Movie A"])
	}
	if ids["/hot/Show A"] != "series-1" {
		t.Errorf("ids[/hot/Show A] = %q, want series-1", ids["/hot/Show A"])
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 distinct items, got %d: %+v", len(ids), ids)
	}
}

// TestClient_ReportMoved_SendsVacatedAndOccupiedPaths pins both halves of
// the hint: the folder the item left, so the stale entry there gets
// revalidated away, and the folder it now occupies.
func TestClient_ReportMoved_SendsVacatedAndOccupiedPaths(t *testing.T) {
	byType := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Media/Updated" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Updates []mediaUpdate `json:"Updates"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, u := range body.Updates {
			byType[u.UpdateType] = append(byType[u.UpdateType], u.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ReportMoved([]MovedItem{
		{Title: "Movie A", OldPath: "/hot/movies/Movie A", NewPath: "/cold/movies/Movie A"},
	})
	if err != nil {
		t.Fatalf("ReportMoved: %v", err)
	}

	if got := byType["Deleted"]; len(got) != 1 || got[0] != "/hot/movies/Movie A" {
		t.Errorf("vacated paths = %v, want [/hot/movies/Movie A]", got)
	}
	if got := byType["Created"]; len(got) != 1 || got[0] != "/cold/movies/Movie A" {
		t.Errorf("occupied paths = %v, want [/cold/movies/Movie A]", got)
	}
}

// TestClient_ResolveAndRefresh_BacksOffBetweenPolls guards what the long
// resolve budget costs. Every poll lists the entire library once per user,
// and it runs while Jellyfin is busy with the scan being waited for, so
// holding a short fixed interval for the whole timeout would aim this
// function's heaviest read load at precisely the worst moment.
func TestClient_ResolveAndRefresh_BacksOffBetweenPolls(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			mu.Lock()
			polls++
			mu.Unlock()
			// Never resolves, so this runs the full timeout.
			_, _ = w.Write([]byte(`{"Items": []}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// testClient polls every 1ms with a 200ms budget, so a fixed interval
	// would be ~200 listings; backing off to the 8ms cap is ~30.
	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "Movie A", NewPath: "/cold/movies/Movie A"},
	})
	if err == nil {
		t.Fatal("expected an error naming the item that never appeared")
	}

	mu.Lock()
	defer mu.Unlock()
	if polls > 60 {
		t.Errorf("polled %d times in the resolve budget, want the interval to back off", polls)
	}
	if polls < 2 {
		t.Errorf("polled %d times, want it to retry rather than give up after one look", polls)
	}
}

// TestResolveBackoffCeiling_NeverBelowConfiguredInterval pins the
// direction backoff is allowed to move. Driving this through
// ResolveAndRefresh would mean a test that sleeps for real minutes, since
// the case only bites at intervals above maxResolvePollInterval.
func TestResolveBackoffCeiling_NeverBelowConfiguredInterval(t *testing.T) {
	cases := []struct {
		name string
		base time.Duration
		want time.Duration
	}{
		{"default interval backs off to the cap", 10 * time.Second, maxResolvePollInterval},
		{"short interval backs off to 8x, under the cap", time.Millisecond, 8 * time.Millisecond},
		{"interval at the cap stays there", maxResolvePollInterval, maxResolvePollInterval},
		// The regression: an operator asking for less polling pressure than
		// the cap allows must not be sped back up to it.
		{"interval above the cap is never shortened", 5 * time.Minute, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBackoffCeiling(tc.base); got != tc.want {
				t.Errorf("resolveBackoffCeiling(%s) = %s, want %s", tc.base, got, tc.want)
			}
			if got := resolveBackoffCeiling(tc.base); got < tc.base {
				t.Errorf("resolveBackoffCeiling(%s) = %s, which polls faster than configured", tc.base, got)
			}
		})
	}
}

// assertAddedDates compares snapshots entry by entry with time.Equal, since
// the same instant can be two different time.Time values.
func assertAddedDates(t *testing.T, got, want map[string]AddedDates) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("snapshot has %d folder(s), want %d: %v", len(got), len(want), got)
	}
	for folder, wantDates := range want {
		gotDates, ok := got[folder]
		if !ok {
			t.Errorf("snapshot is missing %s: %v", folder, got)
			continue
		}
		if len(gotDates) != len(wantDates) {
			t.Errorf("%s has %d date(s), want %d: %v", folder, len(gotDates), len(wantDates), gotDates)
		}
		for rel, wantDate := range wantDates {
			if gotDate, ok := gotDates[rel]; !ok || !gotDate.Equal(wantDate) {
				t.Errorf("%s: %s = %v, want %v", folder, rel, gotDate, wantDate)
			}
		}
	}
}

// TestClient_SnapshotAddedDates_RecordsMoviesAndEpisodes pins what a
// snapshot is keyed by: the folder Radarr/Sonarr name, then each file's
// path inside it, which is the part a move leaves alone. A series
// contributes its episodes rather than itself, since Recently Added dates
// episodes, and they are listed as an administrator, whom parental
// controls don't hide episodes from.
func TestClient_SnapshotAddedDates_RecordsMoviesAndEpisodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "kid"}, {"Id": "admin", "Policy": {"IsAdministrator": true}}]`))
		case q.Get("ParentId") != "":
			if r.URL.Path != "/Users/admin/Items" || q.Get("ParentId") != "series-1" || q.Get("IncludeItemTypes") != "Episode" {
				t.Errorf("episodes listed via %s?%s, want the admin's episodes of series-1", r.URL.Path, r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"Items": [
				{"Id": "ep-1", "Type": "Episode", "Path": "/hot/Show A/Season 01/Show A - S01E01.mkv", "DateCreated": "2020-01-02T03:04:05.0000000Z"},
				{"Id": "ep-2", "Type": "Episode", "Path": "/hot/Show A/Season 01/Show A - S01E02.mkv", "DateCreated": "2020-01-09T03:04:05.1234567"},
				{"Id": "ep-missing", "Type": "Episode", "DateCreated": "2020-01-16T00:00:00.0000000Z"}
			]}`))
		case r.URL.Path == "/Users/kid/Items" || r.URL.Path == "/Users/admin/Items":
			if !strings.Contains(q.Get("Fields"), "DateCreated") {
				t.Errorf("library listed without DateCreated: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"Items": [
				{"Id": "movie-1", "Type": "Movie", "Path": "/hot/Movie A/Movie A.mkv", "DateCreated": "2021-03-14T12:00:00.0000000Z"},
				{"Id": "series-1", "Type": "Series", "Path": "/hot/Show A", "DateCreated": "2019-05-01T00:00:00.0000000Z"}
			]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	snapshot, err := testClient(t, srv.URL).SnapshotAddedDates([]string{"/hot/Movie A/", "/hot/Show A", "/hot/Not In Jellyfin"})
	if err != nil {
		t.Fatalf("SnapshotAddedDates: %v", err)
	}

	assertAddedDates(t, snapshot, map[string]AddedDates{
		"/hot/Movie A": {"Movie A.mkv": time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)},
		"/hot/Show A": {
			filepath.Join("Season 01", "Show A - S01E01.mkv"): time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
			// No zone designator: read as the UTC Jellyfin stores.
			filepath.Join("Season 01", "Show A - S01E02.mkv"): time.Date(2020, 1, 9, 3, 4, 5, 123456700, time.UTC),
		},
	})
}

// TestClient_ResolveAndRefresh_PutsBackDateAddedBeforeRefreshing covers the
// write and its place in the sequence. It goes before the full refresh, so
// that refresh re-derives anything a scan racing the write could have
// reverted. And it sends the item back exactly as read, with only the date
// changed and the optional collections dropped, so it can neither blank a
// field Jellyfin always overwrites nor rewrite the cast or provider IDs.
func TestClient_ResolveAndRefresh_PutsBackDateAddedBeforeRefreshing(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var update map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "cold-movie-1", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv", "DateCreated": "2026-09-30T02:15:00.0000000Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/Users/u1/Items/cold-movie-1":
			calls = append(calls, "read")
			_, _ = w.Write([]byte(`{"Id": "cold-movie-1", "Name": "Movie A", "Overview": "A <film> & more",
				"LockData": true, "CommunityRating": 7.4, "DateCreated": "2026-09-30T02:15:00.0000000Z",
				"People": [{"Name": "Someone", "Type": "Actor"}], "Genres": ["Drama"], "Tags": ["kept"],
				"ProviderIds": {"Tmdb": "603"}, "LockedFields": [], "Studios": [], "Taglines": [],
				"ProductionLocations": [], "FieldColdarrNeverHeardOf": {"kept": 1}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/Items/cold-movie-1":
			calls = append(calls, "update")
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Errorf("decoding update: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/Items/cold-movie-1/Refresh":
			calls = append(calls, "refresh")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{{
		Title: "Movie A", OldPath: "/hot/Movie A", NewPath: "/cold/Movie A",
		AddedDates: AddedDates{"Movie A.mkv": time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)},
	}})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"read", "update", "refresh"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if got := string(update["DateCreated"]); got != `"2021-03-14T12:00:00.0000000Z"` {
		t.Errorf("DateCreated sent = %s, want the snapshot's date in Jellyfin's UTC form", got)
	}
	for _, field := range itemUpdateOptionalFields {
		if _, ok := update[field]; ok {
			t.Errorf("update carries %s, which Jellyfin would rewrite", field)
		}
	}
	for field, want := range map[string]string{
		"Name":                     `"Movie A"`,
		"LockData":                 `true`,
		"CommunityRating":          `7.4`,
		"FieldColdarrNeverHeardOf": `{"kept":1}`,
	} {
		if got := string(update[field]); got != want {
			t.Errorf("update %s = %s, want %s as read", field, got, want)
		}
	}
	var overview string
	if err := json.Unmarshal(update["Overview"], &overview); err != nil || overview != "A <film> & more" {
		t.Errorf("update Overview = %q (%v), want it as read", overview, err)
	}
}

// TestClient_ResolveAndRefresh_LeavesDatesTheMoveDidNotChange covers the
// dates that are not the move's doing. A move within one filesystem keeps a
// file's creation time, and a date older than the snapshot was set by
// something else. Neither is written, so a move that disturbed nothing
// costs no item update at all.
func TestClient_ResolveAndRefresh_LeavesDatesTheMoveDidNotChange(t *testing.T) {
	var mu sync.Mutex
	var refreshed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [
				{"Id": "same", "Type": "Movie", "Path": "/cold/Same/Same.mkv", "DateCreated": "2021-03-14T12:00:00.5000000Z"},
				{"Id": "older", "Type": "Movie", "Path": "/cold/Older/Older.mkv", "DateCreated": "2019-01-01T00:00:00.0000000Z"}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/Refresh"):
			refreshed = append(refreshed, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("no date should be read or written, got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	snapshot := time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)
	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "Same", NewPath: "/cold/Same", AddedDates: AddedDates{"Same.mkv": snapshot}},
		{Title: "Older", NewPath: "/cold/Older", AddedDates: AddedDates{"Older.mkv": snapshot}},
	})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	slices.Sort(refreshed)
	if want := []string{"/Items/older/Refresh", "/Items/same/Refresh"}; !slices.Equal(refreshed, want) {
		t.Errorf("refreshed = %v, want %v", refreshed, want)
	}
}

// seriesServer fakes a series moved to /cold/Show A whose episodes Jellyfin
// lists in stages: listing n gets episodesAt(n). Series updates are an
// error - updating a series rewrites every episode's rating.
func seriesServer(t *testing.T, episodesAt func(listing int) string, calls *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	listings := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items" && r.URL.Query().Get("ParentId") == "cold-series-1":
			listings++
			_, _ = w.Write([]byte(`{"Items": [` + episodesAt(listings) + `]}`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "cold-series-1", "Type": "Series", "Path": "/cold/Show A", "DateCreated": "2026-09-30T02:15:00.0000000Z"}]}`))
		case r.Method == http.MethodGet && movedEpisodeItems[r.URL.Path] != "":
			_, _ = w.Write([]byte(movedEpisodeItems[r.URL.Path]))
		case r.Method == http.MethodPost && r.URL.Path == "/Items/cold-series-1/Refresh":
			*calls = append(*calls, "refresh")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/Items/cold-series-1":
			t.Error("the series itself was updated, which rewrites the rating of every episode under it")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/Items/"):
			*calls = append(*calls, "update "+strings.TrimPrefix(r.URL.Path, "/Items/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// movedEpisodeItems is what reading each moved episode returns, by path.
var movedEpisodeItems = map[string]string{
	"/Users/u1/Items/ep-1": `{"Id": "ep-1", "Name": "Episode 1", "DateCreated": "2026-09-30T02:15:00.0000000Z"}`,
	"/Users/u1/Items/ep-2": `{"Id": "ep-2", "Name": "Episode 2", "DateCreated": "2026-09-30T02:15:00.0000000Z"}`,
}

const (
	movedEpisode1 = `{"Id": "ep-1", "Type": "Episode", "Path": "/cold/Show A/Season 01/Show A - S01E01.mkv", "DateCreated": "2026-09-30T02:15:00.0000000Z"}`
	movedEpisode2 = `{"Id": "ep-2", "Type": "Episode", "Path": "/cold/Show A/Season 01/Show A - S01E02.mkv", "DateCreated": "2026-09-30T02:15:00.0000000Z"}`
)

func showAAddedDates() AddedDates {
	return AddedDates{
		filepath.Join("Season 01", "Show A - S01E01.mkv"): time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC),
		filepath.Join("Season 01", "Show A - S01E02.mkv"): time.Date(2020, 1, 9, 0, 0, 0, 0, time.UTC),
	}
}

// TestClient_ResolveAndRefresh_WaitsForEveryEpisodeBeforePuttingBack covers
// a series listed before its episodes. Jellyfin lists a new series as soon
// as its scan creates it, and only reaches the episodes later in the same
// scan; putting back what was there and refreshing straight away would
// leave the rest dated by the move.
func TestClient_ResolveAndRefresh_WaitsForEveryEpisodeBeforePuttingBack(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := seriesServer(t, func(listing int) string {
		if listing < 3 {
			return movedEpisode1
		}
		return movedEpisode1 + "," + movedEpisode2
	}, &calls, &mu)
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "Show A", NewPath: "/cold/Show A", AddedDates: showAAddedDates()},
	})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 || calls[2] != "refresh" {
		t.Fatalf("calls = %v, want both episode updates and then the series refresh", calls)
	}
	updates := slices.Sorted(slices.Values(calls[:2]))
	if want := []string{"update ep-1", "update ep-2"}; !slices.Equal(updates, want) {
		t.Errorf("updates = %v, want %v", updates, want)
	}
}

// TestClient_ResolveAndRefresh_PutsBackWhatArrivedByTheDeadline covers an
// episode that never reappears, renamed or deleted mid-run. It must cost
// neither the rest of the series its dates nor the series its artwork
// refresh, and the series is not reported as unrefreshed.
func TestClient_ResolveAndRefresh_PutsBackWhatArrivedByTheDeadline(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := seriesServer(t, func(int) string { return movedEpisode1 }, &calls, &mu)
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{
		{Title: "Show A", NewPath: "/cold/Show A", AddedDates: showAAddedDates()},
	})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"update ep-1", "refresh"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

// TestClient_ResolveAndRefresh_RoundOverrunningTheDeadlineIsNotTheLast
// pins the last round to the deadline itself. A series' episode listing
// runs after the round has decided whether it's the last, so one slow
// enough to carry that round past the deadline used to end the wait right
// there - with no last round, the series kept the move's dates, missed its
// refresh, and was reported as never appearing at all.
func TestClient_ResolveAndRefresh_RoundOverrunningTheDeadlineIsNotTheLast(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	episodeListings := 0
	srv := seriesServer(t, func(int) string {
		// seriesServer holds mu while it calls this.
		episodeListings++
		if episodeListings == 1 {
			time.Sleep(100 * time.Millisecond) // well past the 50ms budget
		}
		return movedEpisode1
	}, &calls, &mu)
	defer srv.Close()

	c := testClient(t, srv.URL)
	c.ResolvePollInterval = time.Hour
	c.ResolveTimeout = 50 * time.Millisecond
	err := c.ResolveAndRefresh([]MovedItem{
		{Title: "Show A", NewPath: "/cold/Show A", AddedDates: showAAddedDates()},
	})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"update ep-1", "refresh"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v - the round after the overrun is the last, putting back what arrived", calls, want)
	}
}

func TestJellyfinTime_ReadsWithAndWithoutZone(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{`"2021-03-14T12:00:00.0000000Z"`, time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)},
		{`"2021-03-14T12:00:00.1234567"`, time.Date(2021, 3, 14, 12, 0, 0, 123456700, time.UTC)},
		{`"2021-03-14T14:00:00+02:00"`, time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)},
		{`null`, time.Time{}},
	}
	for _, tc := range cases {
		var got jellyfinTime
		if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
			t.Errorf("decoding %s: %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("decoding %s = %v, want %v", tc.in, got.Time, tc.want)
		}
	}

	var bad jellyfinTime
	if err := json.Unmarshal([]byte(`"last Tuesday"`), &bad); err == nil {
		t.Error("expected an error for a date Jellyfin would never send")
	}
}

func TestClient_StartScheduledTask_Failures(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		tasks       string // GET /ScheduledTasks body; "" answers 500
		startStatus int
		wantErr     string
	}{
		{name: "blank key", key: "  ", wantErr: "scheduled task key is required"},
		{name: "listing fails", key: UserDataRestoreTaskKey, wantErr: "listing scheduled tasks"},
		{name: "malformed listing", key: UserDataRestoreTaskKey, tasks: `{"Id": "not a list"}`, wantErr: "decoding response"},
		{name: "task without a runtime ID", key: UserDataRestoreTaskKey, tasks: `[{"Key": "UserDataRestore", "State": "Idle"}]`, wantErr: "has no runtime ID"},
		{name: "start refused", key: UserDataRestoreTaskKey, tasks: `[{"Id": "rid", "Key": "UserDataRestore", "State": "Idle"}]`, startStatus: http.StatusForbidden, wantErr: "unexpected status 403"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch {
				case r.URL.Path == "/ScheduledTasks" && tt.tasks != "":
					_, _ = w.Write([]byte(tt.tasks))
				case r.URL.Path == "/ScheduledTasks/Running/rid":
					w.WriteHeader(tt.startStatus)
				default:
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			err := testClient(t, srv.URL).StartScheduledTask(tt.key)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("StartScheduledTask error = %v, want one containing %q", err, tt.wantErr)
			}
			if tt.name == "blank key" && requests != 0 {
				t.Errorf("a blank key made %d request(s), want none", requests)
			}
		})
	}
}

// TestClient_WritesWithNothingToSayAreSkipped: a refresh with no item ID
// or a report with no paths would at best be a no-op request, and an empty
// item ID would refresh the wrong endpoint entirely - none are sent.
func TestClient_WritesWithNothingToSayAreSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	if err := c.RefreshItem("", FullRefreshOptions()); err == nil || !strings.Contains(err.Error(), "empty item ID") {
		t.Errorf("RefreshItem(\"\") = %v, want an empty-ID error", err)
	}
	if err := c.ReportMediaUpdated([]string{"", ""}, "Created"); err != nil {
		t.Errorf("ReportMediaUpdated(no paths) = %v, want nil", err)
	}
	if err := c.ReportMoved([]MovedItem{{Title: "Never landed", OldPath: "/hot/A"}}); err != nil {
		t.Errorf("ReportMoved(nothing landed) = %v, want nil", err)
	}
	if err := c.ResolveAndRefresh([]MovedItem{{Title: "Never landed", OldPath: "/hot/A"}}); err != nil {
		t.Errorf("ResolveAndRefresh(nothing landed) = %v, want nil", err)
	}
}

func TestClient_ReportMoved_BothHalvesFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ReportMoved([]MovedItem{{Title: "Movie A", OldPath: "/hot/Movie A", NewPath: "/cold/Movie A"}})
	if err == nil || !strings.Contains(err.Error(), "reporting vacated paths") || !strings.Contains(err.Error(), "reporting new paths") {
		t.Fatalf("ReportMoved error = %v, want both halves' failures", err)
	}
}

// TestClient_FavoritePaths_ListingFailuresAreErrors: favorites are what
// keep an item on hot storage, and the engine refuses to plan when it can't
// read them - so every way the listing can break has to come back as an
// error, never as "nobody has favorites".
func TestClient_FavoritePaths_ListingFailuresAreErrors(t *testing.T) {
	tests := []struct {
		name    string
		users   string // "" answers 500
		items   map[string]string
		wantErr string
	}{
		{name: "users unavailable", wantErr: "listing users"},
		{name: "users malformed", users: `{"Id": "u1"}`, wantErr: "listing users: decoding response"},
		{name: "one user's items unavailable", users: `[{"Id": "u1"}, {"Id": "u2"}]`, items: map[string]string{"u1": `{"Items": []}`}, wantErr: "listing items for user u2"},
		{name: "items malformed", users: `[{"Id": "u1"}]`, items: map[string]string{"u1": `{"Items": {}}`}, wantErr: "listing items for user u1: decoding response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Users" && tt.users != "" {
					_, _ = w.Write([]byte(tt.users))
					return
				}
				user := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/Users/"), "/Items")
				if body, ok := tt.items[user]; ok {
					_, _ = w.Write([]byte(body))
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()

			paths, err := testClient(t, srv.URL).FavoritePaths()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("FavoritePaths() = (%v, %v), want an error containing %q", paths, err, tt.wantErr)
			}
		})
	}
}

// TestClient_ResolveAndRefresh_EpisodeListingFailureStillRefreshes: a
// series whose episodes can't be listed keeps waiting while there's time -
// the listing may have caught Jellyfin mid-scan - and at the deadline still
// gets its artwork refreshed, only its dates staying as the move set them.
func TestClient_ResolveAndRefresh_EpisodeListingFailureStillRefreshes(t *testing.T) {
	var mu sync.Mutex
	episodeListings, refreshes := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items" && r.URL.Query().Get("ParentId") != "":
			episodeListings++
			http.Error(w, "scan in progress", http.StatusInternalServerError)
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "cold-series-1", "Type": "Series", "Path": "/cold/Show A"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/Items/cold-series-1/Refresh":
			refreshes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("no date should be written, got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// One look, then a single wait that runs to the deadline, then the
	// final look: the poll interval outlasts the whole budget, so the
	// deadline always falls during the wait, never mid-listing.
	c := testClient(t, srv.URL)
	c.ResolvePollInterval = time.Hour
	c.ResolveTimeout = 300 * time.Millisecond
	err := c.ResolveAndRefresh([]MovedItem{{Title: "Show A", NewPath: "/cold/Show A", AddedDates: showAAddedDates()}})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want the series refreshed once at the deadline", refreshes)
	}
	if episodeListings != 2 {
		t.Errorf("episode listings = %d, want one look before the deadline and one at it", episodeListings)
	}
}

// TestClient_ResolveAndRefresh_FailedDateWriteDoesNotBlockTheRefresh: a
// date that can't be put back is logged, not fatal - the item still gets
// the artwork refresh the whole follow-up exists for.
func TestClient_ResolveAndRefresh_FailedDateWriteDoesNotBlockTheRefresh(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "cold-movie-1", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv", "DateCreated": "2026-09-30T02:15:00.0000000Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/Users/u1/Items/cold-movie-1":
			calls = append(calls, "read")
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/Items/cold-movie-1/Refresh":
			calls = append(calls, "refresh")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{{
		Title: "Movie A", NewPath: "/cold/Movie A",
		AddedDates: AddedDates{"Movie A.mkv": time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)},
	}})
	if err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"read", "refresh"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

// TestClient_ResolveAndRefresh_RecoversFromStaleLookups: a failed library
// listing is one bad look, and an item that resolves but 404s on refresh
// was listed from a stale index. Neither gives up on the item - both leave
// it pending for the next round.
func TestClient_ResolveAndRefresh_RecoversFromStaleLookups(t *testing.T) {
	var mu sync.Mutex
	listings := 0
	var refreshed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			listings++
			switch listings {
			case 1:
				http.Error(w, "busy", http.StatusServiceUnavailable)
			case 2:
				_, _ = w.Write([]byte(`{"Items": [{"Id": "stale-id", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv"}]}`))
			default:
				_, _ = w.Write([]byte(`{"Items": [{"Id": "fresh-id", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv"}]}`))
			}
		case "/Items/stale-id/Refresh":
			refreshed = append(refreshed, "stale-id")
			http.NotFound(w, r)
		case "/Items/fresh-id/Refresh":
			refreshed = append(refreshed, "fresh-id")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{{Title: "Movie A", NewPath: "/cold/Movie A"}}); err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"stale-id", "fresh-id"}; !slices.Equal(refreshed, want) {
		t.Errorf("refreshed = %v, want %v", refreshed, want)
	}
}

// TestClient_ResolveAndRefresh_RefreshFailureIsReported: a refresh
// Jellyfin rejects for any reason but "no such item" is reported against
// that item, so the caller can fall back to a library scan.
func TestClient_ResolveAndRefresh_RefreshFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "id-a", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv"}]}`))
		default:
			http.Error(w, "refresh queue full", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).ResolveAndRefresh([]MovedItem{{Title: "Movie A", NewPath: "/cold/Movie A"}})
	if err == nil || !strings.Contains(err.Error(), "could not refresh 1 item(s)") || !strings.Contains(err.Error(), "Movie A") {
		t.Fatalf("ResolveAndRefresh error = %v, want the failed refresh reported against Movie A", err)
	}
}

// TestClient_ResolveAndRefresh_ZeroPollSettingsFallBackToDefaults: a
// client built without poll settings still resolves an item that is
// already there rather than treating a zero timeout as "out of time".
func TestClient_ResolveAndRefresh_ZeroPollSettingsFallBackToDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "id-a", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv"}]}`))
		case "/Items/id-a/Refresh":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	c.Logf = t.Logf
	c.ResolvePollInterval, c.ResolveTimeout = 0, 0
	if err := c.ResolveAndRefresh([]MovedItem{{Title: "Movie A", NewPath: "/cold/Movie A"}}); err != nil {
		t.Fatalf("ResolveAndRefresh: %v", err)
	}
}

func TestClient_SystemInfoFailures(t *testing.T) {
	for name, respond := range map[string]http.HandlerFunc{
		"unavailable": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusBadGateway) },
		"malformed":   func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"Id": `)) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(respond)
			defer srv.Close()
			c := testClient(t, srv.URL)
			if id, err := c.ServerID(); err == nil {
				t.Errorf("ServerID() = %q, want an error", id)
			}
			if version, _, err := c.Ping(); err == nil {
				t.Errorf("Ping() version = %q, want an error", version)
			}
		})
	}
}

func TestClient_TransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	unreachable := srv.URL
	srv.Close()

	var logged []string
	c := testClient(t, unreachable)
	c.Logf = func(format string, args ...any) { logged = append(logged, format) }
	if _, _, err := c.Ping(); err == nil || !strings.Contains(err.Error(), "GET /System/Info") {
		t.Errorf("Ping error = %v, want the failed GET named", err)
	}
	if err := c.RefreshLibrary(); err == nil || !strings.Contains(err.Error(), "POST /Library/Refresh") {
		t.Errorf("RefreshLibrary error = %v, want the failed POST named", err)
	}
	if len(logged) == 0 {
		t.Error("a failed write must be logged - its outcome is otherwise invisible")
	}

	malformed := testClient(t, "http://jellyfin host:8096")
	if _, _, err := malformed.Ping(); err == nil || !strings.Contains(err.Error(), "building request") {
		t.Errorf("Ping error = %v, want a request-building error", err)
	}
	if err := malformed.RefreshLibrary(); err == nil || !strings.Contains(err.Error(), "building request") {
		t.Errorf("RefreshLibrary error = %v, want a request-building error", err)
	}
}

func TestJellyfinTime_RejectsNonStrings(t *testing.T) {
	var got jellyfinTime
	if err := json.Unmarshal([]byte(`1615723200`), &got); err == nil || !strings.Contains(err.Error(), "decoding Jellyfin date") {
		t.Fatalf("decoding a number = %v, want a decoding error", err)
	}
}
