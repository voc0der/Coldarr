package jellyfin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClient_Ping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Emby-Token") != "key" {
			t.Errorf("missing/incorrect X-Emby-Token header: %q", r.Header.Get("X-Emby-Token"))
		}
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

// TestClient_NotifyMoved_ResolvesNewPathThenRefreshes covers the whole
// point of the sequence: the item ID is re-resolved from the item's NEW
// path (Jellyfin hashes paths into IDs, so the pre-move ID is dead), and
// the refresh targets that ID. The Movie's Path is the video file, one
// level below the folder Radarr reports, which is what makes the
// itemFolderPath normalization load-bearing here.
func TestClient_NotifyMoved_ResolvesNewPathThenRefreshes(t *testing.T) {
	var refreshed []string
	var reported []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Library/Media/Updated":
			var body struct {
				Updates []mediaUpdate `json:"Updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, u := range body.Updates {
				reported = append(reported, u.Path)
			}
			w.WriteHeader(http.StatusNoContent)
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
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).NotifyMoved([]MovedItem{
		{Title: "Movie A", OldPath: "/hot/Movie A", NewPath: "/cold/Movie A"},
	})
	if err != nil {
		t.Fatalf("NotifyMoved: %v", err)
	}

	if len(refreshed) != 1 || refreshed[0] != "cold-movie-1" {
		t.Errorf("refreshed = %v, want [cold-movie-1] (the ID at the NEW path)", refreshed)
	}
	// Both the vacated and the new path are reported, so Jellyfin drops
	// the old entry instead of leaving a phantom behind.
	for _, want := range []string{"/hot/Movie A", "/cold/Movie A"} {
		found := false
		for _, p := range reported {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("path %q was never reported to Jellyfin, got %v", want, reported)
		}
	}
}

// TestClient_NotifyMoved_UnresolvedItemIsReported guards the case that
// used to be invisible: Jellyfin never surfaces the item at its new path,
// and the caller has to learn about it rather than get a cheerful nil.
func TestClient_NotifyMoved_UnresolvedItemIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Library/Media/Updated":
			w.WriteHeader(http.StatusNoContent)
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": []}`))
		default:
			t.Errorf("nothing should be refreshed, got %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	err := testClient(t, srv.URL).NotifyMoved([]MovedItem{
		{Title: "Movie A", OldPath: "/hot/Movie A", NewPath: "/cold/Movie A"},
	})
	if err == nil {
		t.Fatal("expected an error when the item never appears at its new path")
	}
	if !strings.Contains(err.Error(), "Movie A") {
		t.Errorf("error should name the unrefreshed item, got: %v", err)
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
