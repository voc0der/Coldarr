package arrapi

import (
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
		case "/api/v3/wanted/cutoff":
			_, _ = w.Write([]byte(`{"records": [{"seriesId": 3}], "totalRecords": 1}`))
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
	if !continuing.QualityCutoffNotMet {
		t.Error("series 3 is in the fake wanted/cutoff response, must report QualityCutoffNotMet = true")
	}
	if ended.QualityCutoffNotMet {
		t.Error("series 1 is not in the fake wanted/cutoff response, must report QualityCutoffNotMet = false")
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

func TestSonarrClient_TitleSlugs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "titleSlug": "show-a"}]`))
	}))
	defer srv.Close()

	slugs, err := NewSonarrClient(srv.URL, "key").TitleSlugs()
	if err != nil {
		t.Fatalf("TitleSlugs: %v", err)
	}
	if slugs[1] != "show-a" {
		t.Fatalf("unexpected slugs: %+v", slugs)
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
