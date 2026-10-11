package arrapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// downloadsAPI fakes one *arr app's queue (paginated) and history, under
// the app's own API root.
func downloadsAPI(t *testing.T, root, unknownParam string, queue []string, history []historyRecord) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case root + "/queue":
			if r.URL.Query().Get(unknownParam) != "true" {
				t.Errorf("queue request %s lacks %s=true - downloads it can't match to a library item would be missed", r.URL, unknownParam)
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
			start := min((page-1)*size, len(queue))
			end := min(start+size, len(queue))
			records := []map[string]string{}
			for _, id := range queue[start:end] {
				records = append(records, map[string]string{"downloadId": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": records, "totalRecords": len(queue)})
		case root + "/history":
			want := r.URL.Query().Get("downloadId")
			if want != strings.ToUpper(want) {
				t.Errorf("history asked for %q - the apps store download IDs upper-cased and match exactly", want)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": history})
		case root + "/system/status":
			_, _ = w.Write([]byte(`{"version": "2.9.6"}`))
		case root + "/artist":
			_, _ = w.Write([]byte(`[{"path": "/music/Artist A"}, {"path": ""}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQueueDownloadIDs_ReadsEveryPage(t *testing.T) {
	queue := make([]string, 0, 300)
	for i := range 300 {
		queue = append(queue, "hash"+strconv.Itoa(i))
	}
	queue = append(queue, "") // a usenet item without an ID is skipped
	srv := downloadsAPI(t, "/api/v3", "includeUnknownMovieItems", queue, nil)

	ids, err := NewRadarrClient(srv.URL, "key").QueueDownloadIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 300 || !ids["HASH299"] {
		t.Fatalf("got %d IDs (HASH299 present: %v), want all 300 upper-cased across pages", len(ids), ids["HASH299"])
	}
}

func TestImported(t *testing.T) {
	history := []historyRecord{
		{DownloadID: "OTHER", EventType: "downloadFolderImported"},
		{DownloadID: "ABC123", EventType: "grabbed"},
	}
	srv := downloadsAPI(t, "/api/v3", "includeUnknownSeriesItems", nil, history)
	sonarr := NewSonarrClient(srv.URL, "key")

	// Only a grab for this download, and an import for a different one -
	// which is also what an app that ignored the filter would send back.
	if ok, err := sonarr.Imported("abc123"); err != nil || ok {
		t.Fatalf("Imported = %v, %v; want false: nothing imported this download", ok, err)
	}

	history = append(history, historyRecord{DownloadID: "abc123", EventType: "downloadFolderImported"})
	srv = downloadsAPI(t, "/api/v3", "includeUnknownSeriesItems", nil, history)
	if ok, err := NewSonarrClient(srv.URL, "key").Imported("abc123"); err != nil || !ok {
		t.Fatalf("Imported = %v, %v; want true", ok, err)
	}
	if ok, err := NewRadarrClient(srv.URL, "key").Imported("ABC123"); err != nil || !ok {
		t.Fatalf("Radarr Imported = %v, %v; want true", ok, err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	if _, err := NewRadarrClient(broken.URL, "key").Imported("x"); err == nil {
		t.Error("a failed history lookup must be an error, not \"not imported\"")
	}
	if _, err := NewSonarrClient(broken.URL, "key").QueueDownloadIDs(); err == nil {
		t.Error("a failed queue lookup must be an error, not an empty queue")
	}
}

func TestLidarr(t *testing.T) {
	history := []historyRecord{
		{DownloadID: "ALBUM1", EventType: "trackFileImported"},
		{DownloadID: "ALBUM1", EventType: "albumImportIncomplete"},
		{DownloadID: "ALBUM2", EventType: "downloadImported"},
	}
	srv := downloadsAPI(t, "/api/v1", "includeUnknownArtistItems", []string{"album3"}, history)
	lidarr := NewLidarrClient(srv.URL, "key")

	if v, err := lidarr.Ping(); err != nil || v != "2.9.6" {
		t.Fatalf("Ping = %q, %v", v, err)
	}
	if ok, err := lidarr.Imported("album1"); err != nil || ok {
		t.Errorf("a partly imported album = %v, %v; want not imported", ok, err)
	}
	if ok, err := lidarr.Imported("album2"); err != nil || !ok {
		t.Errorf("a fully imported album = %v, %v; want imported", ok, err)
	}
	if ids, err := lidarr.QueueDownloadIDs(); err != nil || !ids["ALBUM3"] {
		t.Errorf("QueueDownloadIDs = %v, %v", ids, err)
	}
	if paths, err := lidarr.ArtistPaths(); err != nil || strings.Join(paths, ",") != "/music/Artist A" {
		t.Errorf("ArtistPaths = %v, %v", paths, err)
	}
}
