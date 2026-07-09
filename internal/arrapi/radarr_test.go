package arrapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRadarrClient_Ping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/system/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("missing/incorrect X-Api-Key header: %q", r.Header.Get("X-Api-Key"))
		}
		_, _ = w.Write([]byte(`{"version": "5.1.0"}`))
	}))
	defer srv.Close()

	version, err := NewRadarrClient(srv.URL, "key").Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if version != "5.1.0" {
		t.Fatalf("version = %q, want 5.1.0", version)
	}
}

func TestRadarrClient_Ping_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := NewRadarrClient(srv.URL, "bad-key").Ping(); err == nil {
		t.Fatal("expected an error for a 401 response")
	}
}

func TestRadarrClient_FetchMovies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[
				{"id": 1, "title": "Released Movie", "titleSlug": "released-movie", "path": "/hot/Released Movie", "rootFolderPath": "/hot", "qualityProfileId": 10, "monitored": true, "hasFile": true, "tags": [1], "sizeOnDisk": 1000, "status": "released"},
				{"id": 2, "title": "Unreleased Movie", "titleSlug": "unreleased-movie", "path": "/hot/Unreleased Movie", "rootFolderPath": "/hot", "qualityProfileId": 10, "monitored": true, "hasFile": false, "tags": [], "sizeOnDisk": 0, "status": "tba"}
			]`))
		case "/api/v3/tag":
			_, _ = w.Write([]byte(`[{"id": 1, "label": "kids"}]`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id": 10, "name": "HD-1080p"}]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": [{"movieId": 2}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	items, err := NewRadarrClient(srv.URL, "key").FetchMovies()
	if err != nil {
		t.Fatalf("FetchMovies: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	byID := map[int]int{}
	for i, item := range items {
		byID[item.ID] = i
	}

	released := items[byID[1]]
	if released.QualityProfileName != "HD-1080p" {
		t.Errorf("QualityProfileName = %q, want HD-1080p", released.QualityProfileName)
	}
	if len(released.Tags) != 1 || released.Tags[0] != "kids" {
		t.Errorf("Tags = %v, want [kids]", released.Tags)
	}
	if released.Upcoming {
		t.Error("a released movie must not be marked Upcoming")
	}
	if released.InActiveQueue {
		t.Error("movie 1 is not in the fake queue, must not be marked InActiveQueue")
	}

	unreleased := items[byID[2]]
	if !unreleased.Upcoming {
		t.Error("a movie with status \"tba\" must be marked Upcoming")
	}
	if !unreleased.InActiveQueue {
		t.Error("movie 2 is in the fake queue, must be marked InActiveQueue")
	}
}

func TestRadarrClient_CutoffUnmetMovieIDs_Paginates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"records": [{"id": 1}, {"id": 2}], "totalRecords": 3}`))
		case "2":
			_, _ = w.Write([]byte(`{"records": [{"id": 3}], "totalRecords": 3}`))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	unmet, err := NewRadarrClient(srv.URL, "key").CutoffUnmetMovieIDs()
	if err != nil {
		t.Fatalf("CutoffUnmetMovieIDs: %v", err)
	}
	if pages != 2 {
		t.Fatalf("expected 2 pages fetched, got %d", pages)
	}
	if !unmet[1] || !unmet[2] || !unmet[3] {
		t.Fatalf("unexpected result: %+v", unmet)
	}
}

func TestRadarrClient_TitleSlugs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "titleSlug": "movie-a"}, {"id": 2, "titleSlug": "movie-b"}]`))
	}))
	defer srv.Close()

	slugs, err := NewRadarrClient(srv.URL, "key").TitleSlugs()
	if err != nil {
		t.Fatalf("TitleSlugs: %v", err)
	}
	if slugs[1] != "movie-a" || slugs[2] != "movie-b" {
		t.Fatalf("unexpected slugs: %+v", slugs)
	}
}

func TestRadarrClient_GetMovieSize_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie/5" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id": 5, "sizeOnDisk": 12345, "path": "/hot/Movie"}`))
	}))
	defer srv.Close()

	size, path, found, err := NewRadarrClient(srv.URL, "key").GetMovieSize(5)
	if err != nil {
		t.Fatalf("GetMovieSize: %v", err)
	}
	if !found || size != 12345 || path != "/hot/Movie" {
		t.Fatalf("got (size=%d, path=%q, found=%v)", size, path, found)
	}
}

func TestRadarrClient_GetMovieSize_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, _, found, err := NewRadarrClient(srv.URL, "key").GetMovieSize(99)
	if err != nil {
		t.Fatalf("expected a 404 to be reported as not-found, not an error: %v", err)
	}
	if found {
		t.Fatal("expected found = false for a deleted movie")
	}
}

func TestRadarrClient_MoveMovies_EmptyIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	if err := NewRadarrClient(srv.URL, "key").MoveMovies(nil, "/cold"); err != nil {
		t.Fatalf("MoveMovies: %v", err)
	}
	if called {
		t.Error("MoveMovies with no IDs must not make a request")
	}
}

func TestRadarrClient_MoveMovies(t *testing.T) {
	var got struct {
		MovieIDs       []int  `json:"movieIds"`
		RootFolderPath string `json:"rootFolderPath"`
		MoveFiles      bool   `json:"moveFiles"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v3/movie/editor" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewRadarrClient(srv.URL, "key").MoveMovies([]int{1, 2}, "/cold"); err != nil {
		t.Fatalf("MoveMovies: %v", err)
	}
	if len(got.MovieIDs) != 2 || got.RootFolderPath != "/cold" || !got.MoveFiles {
		t.Fatalf("unexpected request body: %+v", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) should be false")
	}
	if IsNotFound(&StatusError{Code: http.StatusInternalServerError}) {
		t.Error("a 500 must not be reported as not-found")
	}
	if !IsNotFound(&StatusError{Code: http.StatusNotFound}) {
		t.Error("a 404 StatusError must be reported as not-found")
	}
}
