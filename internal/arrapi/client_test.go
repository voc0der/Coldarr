package arrapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStatusError_TruncatesLongBodies: Radarr/Sonarr can answer an error
// with a whole HTML page, which would otherwise swamp the log line or GUI
// banner the error lands in.
func TestStatusError_TruncatesLongBodies(t *testing.T) {
	long := &StatusError{Method: http.MethodGet, Path: "/api/v3/movie", Code: http.StatusBadGateway, Body: strings.Repeat("x", 600)}
	got := long.Error()
	if !strings.HasPrefix(got, "GET /api/v3/movie: unexpected status 502: ") {
		t.Errorf("Error() = %q, want method, path and status first", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("x", 500)+"...") || strings.Contains(got, strings.Repeat("x", 501)) {
		t.Errorf("Error() carries %d body bytes, want the first 500 and an ellipsis", strings.Count(got, "x"))
	}

	short := &StatusError{Method: http.MethodGet, Path: "/api/v3/movie", Code: http.StatusUnauthorized, Body: "Unauthorized"}
	if got := short.Error(); !strings.HasSuffix(got, ": Unauthorized") {
		t.Errorf("Error() = %q, want a short body shown whole", got)
	}
}

func TestClient_TransportAndDecodingErrors(t *testing.T) {
	t.Run("invalid JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"version": `))
		}))
		defer srv.Close()
		if _, err := NewRadarrClient(srv.URL, "key").Ping(); err == nil || !strings.Contains(err.Error(), "decoding response") {
			t.Fatalf("Ping error = %v, want a decoding error", err)
		}
	})

	t.Run("server unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		if _, err := NewSonarrClient(url, "key").Ping(); err == nil || !strings.Contains(err.Error(), "GET /api/v3/system/status") {
			t.Fatalf("Ping error = %v, want the request named in a transport error", err)
		}
	})

	t.Run("malformed base URL", func(t *testing.T) {
		if _, err := NewRadarrClient("http://radarr host:7878", "key").Ping(); err == nil || !strings.Contains(err.Error(), "building request") {
			t.Fatalf("Ping error = %v, want a request-building error", err)
		}
	})
}

// commandAPI fakes Radarr/Sonarr's command queue: POST starts a command,
// and GET /api/v3/command/{id} reports statuses in order, repeating the
// last one.
type commandAPI struct {
	*httptest.Server
	mu       sync.Mutex
	started  map[string]any
	statuses []string
	polls    int
}

func newCommandAPI(t *testing.T, statuses ...string) *commandAPI {
	t.Helper()
	f := &commandAPI{statuses: statuses}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			if err := json.NewDecoder(r.Body).Decode(&f.started); err != nil {
				t.Errorf("decoding command: %v", err)
			}
			_, _ = w.Write([]byte(`{"id": 42, "status": "queued"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/command/42":
			status := f.statuses[min(f.polls, len(f.statuses)-1)]
			f.polls++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": status})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func TestRadarrClient_RescanMovie_StartsTheCommandForThatMovie(t *testing.T) {
	api := newCommandAPI(t, "completed")
	if err := NewRadarrClient(api.URL, "key").RescanMovie(5); err != nil {
		t.Fatalf("RescanMovie: %v", err)
	}
	if api.started["name"] != "RescanMovie" || api.started["movieId"] != float64(5) {
		t.Fatalf("command sent = %v, want RescanMovie for movieId 5", api.started)
	}
}

// TestSonarrClient_RescanSeries_WaitsForTheCommandToFinish: a rescan is
// only useful once Sonarr has actually finished it - returning on the
// first "started" would have the caller read the size Sonarr had before.
func TestSonarrClient_RescanSeries_WaitsForTheCommandToFinish(t *testing.T) {
	api := newCommandAPI(t, "started", "completed")

	start := time.Now()
	if err := NewSonarrClient(api.URL, "key").RescanSeries(7); err != nil {
		t.Fatalf("RescanSeries: %v", err)
	}
	if api.started["name"] != "RescanSeries" || api.started["seriesId"] != float64(7) {
		t.Fatalf("command sent = %v, want RescanSeries for seriesId 7", api.started)
	}
	if api.polls != 2 {
		t.Fatalf("polls = %d, want 2 (started, then completed)", api.polls)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("returned after %v, want it to have waited between polls", elapsed)
	}
}

func TestRunCommand_TerminalFailureIsAnError(t *testing.T) {
	for _, status := range []string{"failed", "aborted", "cancelled", "orphaned"} {
		t.Run(status, func(t *testing.T) {
			api := newCommandAPI(t, status)
			err := NewRadarrClient(api.URL, "key").RescanMovie(5)
			if err == nil || !strings.Contains(err.Error(), `ended with status "`+status+`"`) {
				t.Fatalf("RescanMovie error = %v, want the terminal status reported", err)
			}
		})
	}
}

func TestRunCommand_FailedStartIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "commands disabled", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := NewSonarrClient(srv.URL, "key").RescanSeries(7); err == nil || !strings.Contains(err.Error(), "POST /api/v3/command") {
		t.Fatalf("RescanSeries error = %v, want the failed POST reported", err)
	}
}

// arrFetchServer answers the four lookups FetchMovies/FetchSeries make,
// with failing naming the one path that answers 500 instead.
func arrFetchServer(t *testing.T, itemsPath, items, queue, failing string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == failing {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case itemsPath:
			_, _ = w.Write([]byte(items))
		case "/api/v3/tag":
			_, _ = w.Write([]byte(`[{"id": 1, "label": "keep-hot"}, {"id": 2, "label": "kids"}]`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id": 10, "name": "HD-1080p"}]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(queue))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetch_AnyFailedLookupFailsTheFetch: every lookup a fetch makes is
// load-bearing. Without the download queue in particular, an item still
// being downloaded or imported would look idle and get planned for a move,
// so a failed lookup has to fail the whole fetch.
func TestFetch_AnyFailedLookupFailsTheFetch(t *testing.T) {
	for _, failing := range []string{"/api/v3/movie", "/api/v3/tag", "/api/v3/qualityprofile", "/api/v3/queue"} {
		t.Run("radarr "+failing, func(t *testing.T) {
			srv := arrFetchServer(t, "/api/v3/movie", `[]`, `{"records": []}`, failing)
			if _, err := NewRadarrClient(srv.URL, "key").FetchMovies(); err == nil || !strings.Contains(err.Error(), failing) {
				t.Fatalf("FetchMovies error = %v, want the failed %s lookup", err, failing)
			}
		})
	}
	for _, failing := range []string{"/api/v3/series", "/api/v3/tag", "/api/v3/qualityprofile", "/api/v3/queue"} {
		t.Run("sonarr "+failing, func(t *testing.T) {
			srv := arrFetchServer(t, "/api/v3/series", `[]`, `{"records": []}`, failing)
			if _, err := NewSonarrClient(srv.URL, "key").FetchSeries(); err == nil || !strings.Contains(err.Error(), failing) {
				t.Fatalf("FetchSeries error = %v, want the failed %s lookup", err, failing)
			}
		})
	}
}

func TestSonarrClient_FetchSeries_ResolvesTagsProfilesAndQueue(t *testing.T) {
	srv := arrFetchServer(t, "/api/v3/series",
		`[{"id": 3, "title": "Show A", "path": "/hot/Show A", "qualityProfileId": 10, "tags": [2, 99], "status": "continuing", "statistics": {"sizeOnDisk": 100, "episodeFileCount": 4}},
		  {"id": 4, "title": "Show B", "path": "/hot/Show B", "qualityProfileId": 11, "tags": [], "status": "ended", "statistics": {"sizeOnDisk": 50, "episodeFileCount": 1}}]`,
		`{"records": [{"seriesId": 3}, {"seriesId": 0}]}`, "")

	items, err := NewSonarrClient(srv.URL, "key").FetchSeries()
	if err != nil {
		t.Fatalf("FetchSeries: %v", err)
	}
	byID := map[int]int{}
	for i, item := range items {
		byID[item.ID] = i
	}

	showA := items[byID[3]]
	if len(showA.Tags) != 1 || showA.Tags[0] != "kids" {
		t.Errorf("Tags = %v, want [kids] (unknown tag IDs dropped)", showA.Tags)
	}
	if showA.QualityProfileName != "HD-1080p" {
		t.Errorf("QualityProfileName = %q, want HD-1080p", showA.QualityProfileName)
	}
	if !showA.InActiveQueue {
		t.Error("series 3 is in the queue and must be marked InActiveQueue")
	}

	showB := items[byID[4]]
	if showB.InActiveQueue {
		t.Error("series 4 is not in the queue - and a queue record for an unknown series (0) must not mark anything")
	}
	if showB.QualityProfileName != "" {
		t.Errorf("QualityProfileName = %q for an unknown profile ID, want empty", showB.QualityProfileName)
	}
}
