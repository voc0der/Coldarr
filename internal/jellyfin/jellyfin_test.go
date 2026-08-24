package jellyfin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
