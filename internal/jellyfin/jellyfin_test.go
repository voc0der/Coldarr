package jellyfin

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
