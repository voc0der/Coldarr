package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadCrossSeedsMoveTogether(t *testing.T) {
	base := t.TempDir()
	hot, cold := filepath.Join(base, "downloads"), filepath.Join(base, "seed")
	destination := filepath.Join(cold, "movies")
	l := &library{
		drives: []*drive{{mount: hot}, {mount: cold}}, seedRoots: []string{cold},
		rootFolders: []string{hot, cold},
		sim:         simulation{MoveThroughput: 1 << 30, MinMoveSeconds: 0.25, MaxMoveSeconds: 0.25},
	}
	spec := downloadSpec{Name: "Gallery.Film", Root: filepath.Join(hot, "movies"), Size: 4096, Category: "movies", CompletedDaysAgo: 5}
	if err := l.seedDownloads([]downloadSpec{spec, spec}, time.Now()); err != nil {
		t.Fatal(err)
	}
	handler := l.qbitHandler("v5.2.0")
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	hashes := l.downloads[0].hash + "|" + l.downloads[1].hash
	form := url.Values{"hashes": {hashes}, "location": {filepath.Join(base, "outside")}}
	if w := post("/api/v2/torrents/setLocation", form); w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured destination accepted: %d %s", w.Code, w.Body.String())
	}
	form.Set("location", destination)
	if w := post("/api/v2/torrents/setLocation", form); w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/torrents/info?hashes="+l.downloads[0].hash, nil))
		var list []struct {
			State    string `json:"state"`
			SavePath string `json:"save_path"`
			Category string `json:"category"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("hash filter returned %d downloads", len(list))
		}
		if list[0].State == "stalledUP" && list[0].SavePath == destination {
			if list[0].Category != "movies" {
				t.Fatal("move relabeled the download before Coldarr confirmed it")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("move did not finish: %+v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(spec.Root, spec.Name)); !os.IsNotExist(err) {
		t.Fatalf("source remains after move: %v", err)
	}
	info, err := os.Stat(filepath.Join(destination, spec.Name, "content.mkv"))
	if err != nil || info.Size() != int64(spec.Size) {
		t.Fatalf("destination size: %v, %v", info, err)
	}
	if w := post("/api/v2/torrents/setCategory", url.Values{"hashes": {hashes}, "category": {"seed"}}); w.Code != http.StatusOK {
		t.Fatalf("relabel: %d %s", w.Code, w.Body.String())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range l.downloads {
		if d.Root != destination || d.Category != "seed" || d.State != "stalledUP" {
			t.Fatalf("cross-seed did not follow the move and relabel: %+v", d)
		}
	}
}

func TestDownloadImportEvidence(t *testing.T) {
	l := &library{downloads: []*download{
		{hash: "aabb", downloadSpec: downloadSpec{ImportedBy: "radarr", Imported: true}},
		{hash: "ccdd", downloadSpec: downloadSpec{ImportedBy: "sonarr", Queued: true}},
	}}
	for _, tt := range []struct {
		name, path string
		app        arrKind
		want       int
	}{
		{"imported hash", "/api/v3/history?downloadId=AABB", radarr, 1},
		{"different hash", "/api/v3/history?downloadId=CCDD", radarr, 0},
		{"different importer", "/api/v3/history?downloadId=AABB", sonarr, 0},
		{"still queued", "/api/v3/queue", sonarr, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			l.arrHandler(tt.app, "gallery").ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
			var body struct {
				Records []struct {
					DownloadID string `json:"downloadId"`
				} `json:"records"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || len(body.Records) != tt.want {
				t.Fatalf("import evidence: %d %s", w.Code, w.Body.String())
			}
			if tt.want > 0 && body.Records[0].DownloadID == "" {
				t.Fatal("import evidence must identify the download by hash")
			}
		})
	}
}
