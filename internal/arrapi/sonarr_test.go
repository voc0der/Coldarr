package arrapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSonarrClient_FetchSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series":
			_, _ = w.Write([]byte(`[
				{"id": 1, "title": "Ended Show", "titleSlug": "ended-show", "path": "/hot/Ended Show", "rootFolderPath": "/hot", "status": "ended", "ended": true, "statistics": {"sizeOnDisk": 500, "episodeFileCount": 10}},
				{"id": 2, "title": "Upcoming Show", "titleSlug": "upcoming-show", "path": "/hot/Upcoming Show", "rootFolderPath": "/hot", "status": "upcoming", "ended": false, "statistics": {"sizeOnDisk": 0, "episodeFileCount": 0}},
				{"id": 3, "title": "Continuing Show", "titleSlug": "continuing-show", "path": "/hot/Continuing Show", "rootFolderPath": "/hot", "status": "continuing", "ended": false, "statistics": {"sizeOnDisk": 200, "episodeFileCount": 5}}
			]`))
		case "/api/v3/tag":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	items, err := NewSonarrClient(srv.URL, "key").FetchSeries()
	if err != nil {
		t.Fatalf("FetchSeries: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	byID := map[int]int{}
	for i, item := range items {
		byID[item.ID] = i
	}

	ended := items[byID[1]]
	if !ended.Ended || ended.Upcoming {
		t.Errorf("ended show: Ended=%v Upcoming=%v, want Ended=true Upcoming=false", ended.Ended, ended.Upcoming)
	}
	if !ended.HasFile {
		t.Error("a series with episodeFileCount > 0 must report HasFile = true")
	}

	upcoming := items[byID[2]]
	if upcoming.Ended || !upcoming.Upcoming {
		t.Errorf("upcoming show: Ended=%v Upcoming=%v, want Ended=false Upcoming=true", upcoming.Ended, upcoming.Upcoming)
	}
	if upcoming.HasFile {
		t.Error("a series with episodeFileCount = 0 must report HasFile = false")
	}

	continuing := items[byID[3]]
	if continuing.Ended || continuing.Upcoming {
		t.Errorf("continuing show: Ended=%v Upcoming=%v, want both false", continuing.Ended, continuing.Upcoming)
	}
}

func TestSonarrClient_CutoffUnmetSeriesIDs_Paginates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"records": [{"seriesId": 1}, {"seriesId": 2}], "totalRecords": 3}`))
		case "2":
			_, _ = w.Write([]byte(`{"records": [{"seriesId": 3}], "totalRecords": 3}`))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	unmet, err := NewSonarrClient(srv.URL, "key").CutoffUnmetSeriesIDs()
	if err != nil {
		t.Fatalf("CutoffUnmetSeriesIDs: %v", err)
	}
	if pages != 2 {
		t.Fatalf("expected 2 pages fetched, got %d", pages)
	}
	if !unmet[1] || !unmet[2] || !unmet[3] {
		t.Fatalf("unexpected result: %+v", unmet)
	}
}

func TestSonarrClient_LinkTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "titleSlug": "show-a", "path": "/cold/Show A"}]`))
	}))
	defer srv.Close()

	targets, err := NewSonarrClient(srv.URL, "key").LinkTargets()
	if err != nil {
		t.Fatalf("LinkTargets: %v", err)
	}
	if targets[1] != (LinkTarget{TitleSlug: "show-a", Path: "/cold/Show A"}) {
		t.Fatalf("unexpected targets: %+v", targets)
	}
}

func TestSonarrClient_MoveSeries_EmptyIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	if err := NewSonarrClient(srv.URL, "key").MoveSeries(nil, "/cold"); err != nil {
		t.Fatalf("MoveSeries: %v", err)
	}
	if called {
		t.Error("MoveSeries with no IDs must not make a request")
	}
}

func TestSonarrClient_GetSeriesSize_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, _, found, err := NewSonarrClient(srv.URL, "key").GetSeriesSize(99)
	if err != nil {
		t.Fatalf("expected a 404 to be reported as not-found, not an error: %v", err)
	}
	if found {
		t.Fatal("expected found = false for a deleted series")
	}
}

func TestSonarrClient_Ping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/system/status" || r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("unexpected request %s with key %q", r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		_, _ = w.Write([]byte(`{"version": "4.0.1"}`))
	}))
	defer srv.Close()

	version, err := NewSonarrClient(srv.URL, "key").Ping()
	if err != nil || version != "4.0.1" {
		t.Fatalf("Ping() = (%q, %v), want 4.0.1", version, err)
	}
}

func TestSonarrClient_GetSeriesSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series/7":
			_, _ = w.Write([]byte(`{"id": 7, "path": "/cold/Show A", "statistics": {"sizeOnDisk": 1234}}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewSonarrClient(srv.URL, "key")

	size, path, found, err := c.GetSeriesSize(7)
	if err != nil || !found || size != 1234 || path != "/cold/Show A" {
		t.Fatalf("GetSeriesSize(7) = (%d, %q, %v, %v), want (1234, /cold/Show A, true, nil)", size, path, found, err)
	}

	// Anything but a 404 is a real failure, not "the series is gone".
	if _, _, found, err := c.GetSeriesSize(8); err == nil || found {
		t.Fatalf("GetSeriesSize on a 500 = (found %v, err %v), want an error", found, err)
	}
}

func TestSonarrClient_MoveSeries(t *testing.T) {
	var got struct {
		SeriesIDs      []int  `json:"seriesIds"`
		RootFolderPath string `json:"rootFolderPath"`
		MoveFiles      bool   `json:"moveFiles"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v3/series/editor" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
	}))
	defer srv.Close()

	if err := NewSonarrClient(srv.URL, "key").MoveSeries([]int{3, 4}, "/cold/tv"); err != nil {
		t.Fatalf("MoveSeries: %v", err)
	}
	if len(got.SeriesIDs) != 2 || got.RootFolderPath != "/cold/tv" || !got.MoveFiles {
		t.Fatalf("request body = %+v, want both series moved to /cold/tv with moveFiles", got)
	}
}

func TestSonarrClient_ActiveMoveCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "name": "MoveSeries", "status": "started"}, {"id": 2, "name": "RefreshSeries", "status": "started"}, {"id": 3, "name": "MoveSeries", "status": "completed"}]`))
	}))
	defer srv.Close()

	n, err := NewSonarrClient(srv.URL, "key").ActiveMoveCommands()
	if err != nil || n != 1 {
		t.Fatalf("ActiveMoveCommands() = (%d, %v), want 1", n, err)
	}
}

func TestArrLookups_FailuresAreErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	radarr, sonarr := NewRadarrClient(srv.URL, "key"), NewSonarrClient(srv.URL, "key")

	checks := map[string]func() error{
		"radarr LinkTargets":          func() error { _, err := radarr.LinkTargets(); return err },
		"radarr CutoffUnmetMovieIDs":  func() error { _, err := radarr.CutoffUnmetMovieIDs(); return err },
		"radarr GetMovieSize":         func() error { _, _, _, err := radarr.GetMovieSize(1); return err },
		"sonarr LinkTargets":          func() error { _, err := sonarr.LinkTargets(); return err },
		"sonarr CutoffUnmetSeriesIDs": func() error { _, err := sonarr.CutoffUnmetSeriesIDs(); return err },
		"sonarr BusySeriesIDs":        func() error { _, err := sonarr.BusySeriesIDs(); return err },
	}
	for name, check := range checks {
		if err := check(); err == nil {
			t.Errorf("%s: expected an error from a failing server", name)
		}
	}
}
