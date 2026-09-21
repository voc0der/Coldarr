package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// arrKind is what differs between the Radarr and Sonarr APIs for the
// subset Coldarr calls.
type arrKind struct {
	app        string // "radarr" or "sonarr"
	appName    string
	collection string // "movie" or "series"
	idField    string // "movieId" or "seriesId"
	moveName   string // the command Radarr/Sonarr's bulk editor queues
}

var (
	radarr = arrKind{app: "radarr", appName: "Radarr", collection: "movie", idField: "movieId", moveName: "BulkMoveMovie"}
	sonarr = arrKind{app: "sonarr", appName: "Sonarr", collection: "series", idField: "seriesId", moveName: "BulkMoveSeries"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (l *library) arrHandler(k arrKind, version string) http.Handler {
	mux := http.NewServeMux()
	api := "/api/v3/"

	mux.HandleFunc("GET "+api+"system/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"appName": k.appName, "version": version})
	})

	mux.HandleFunc("GET "+api+k.collection, func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		out := []map[string]any{}
		for _, it := range l.items(k.app) {
			out = append(out, l.render(it))
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET "+api+k.collection+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		l.mu.Lock()
		defer l.mu.Unlock()
		it := l.find(k.app, id)
		if it == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "NotFound"})
			return
		}
		writeJSON(w, http.StatusOK, l.render(it))
	})

	mux.HandleFunc("GET "+api+"tag", func(w http.ResponseWriter, _ *http.Request) {
		out := []map[string]any{}
		for i, t := range l.tags {
			out = append(out, map[string]any{"id": i + 1, "label": t})
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET "+api+"qualityprofile", func(w http.ResponseWriter, _ *http.Request) {
		out := []map[string]any{}
		for i, p := range l.profiles {
			out = append(out, map[string]any{"id": i + 1, "name": p})
		}
		writeJSON(w, http.StatusOK, out)
	})

	// The download queue: only items the fixture marks as downloading.
	mux.HandleFunc("GET "+api+"queue", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		records := []map[string]any{}
		for _, it := range l.items(k.app) {
			if it.downloading {
				records = append(records, map[string]any{"id": 1000 + it.id, k.idField: it.id, "status": "downloading"})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"page": 1, "totalRecords": len(records), "records": records})
	})

	// Radarr's cutoff list is movies; Sonarr's is episodes, one standing in
	// for each cutoff-unmet series here.
	mux.HandleFunc("GET "+api+"wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		var unmet []*item
		for _, it := range l.items(k.app) {
			if it.cutoffUnmet && it.monitored {
				unmet = append(unmet, it)
			}
		}
		l.mu.Unlock()

		page := max(atoiOr(r.URL.Query().Get("page"), 1), 1)
		size := max(atoiOr(r.URL.Query().Get("pageSize"), 10), 1)
		start, end := min((page-1)*size, len(unmet)), min(page*size, len(unmet))
		records := []map[string]any{}
		for _, it := range unmet[start:end] {
			if k.app == "radarr" {
				records = append(records, map[string]any{"id": it.id, "title": it.title})
			} else {
				records = append(records, map[string]any{"id": 5000 + it.id, "seriesId": it.id})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"page": page, "pageSize": size, "totalRecords": len(unmet), "records": records})
	})

	mux.HandleFunc("GET "+api+"command", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		out := []command{}
		for _, c := range l.commands[k.app] {
			out = append(out, *c)
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET "+api+"command/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, c := range l.commands[k.app] {
			if c.ID == id {
				writeJSON(w, http.StatusOK, *c)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "NotFound"})
	})

	mux.HandleFunc("POST "+api+"command", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "command name required"})
			return
		}
		writeJSON(w, http.StatusCreated, *l.runCommand(k.app, body.Name))
	})

	mux.HandleFunc("PUT "+api+k.collection+"/editor", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MovieIDs       []int  `json:"movieIds"`
			SeriesIDs      []int  `json:"seriesIds"`
			RootFolderPath string `json:"rootFolderPath"`
			MoveFiles      bool   `json:"moveFiles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		ids := append(body.MovieIDs, body.SeriesIDs...)
		root, ok := l.rootFolder(body.RootFolderPath)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "root folder " + body.RootFolderPath + " is not a configured root folder"})
			return
		}
		l.mu.Lock()
		var targets []*item
		for _, id := range ids {
			if it := l.find(k.app, id); it != nil {
				targets = append(targets, it)
			}
		}
		l.mu.Unlock()
		for _, it := range targets {
			if body.MoveFiles {
				l.startMove(k.app, k.moveName, it, root)
			}
		}
		writeJSON(w, http.StatusAccepted, []any{})
	})

	return logRequests(k.app, mux)
}

// render is one item as Radarr's /movie or Sonarr's /series returns it.
// Callers hold l.mu.
func (l *library) render(it *item) map[string]any {
	tagIDs := []int{}
	for _, t := range it.tags {
		tagIDs = append(tagIDs, l.tagID(t))
	}
	m := map[string]any{
		"id":               it.id,
		"title":            it.title,
		"titleSlug":        it.slug(),
		"year":             it.year,
		"path":             it.path(),
		"rootFolderPath":   it.root,
		"qualityProfileId": l.profileID(it.profile),
		"monitored":        it.monitored,
		"added":            it.added.UTC().Format(time.RFC3339),
		"tags":             tagIDs,
		"status":           it.status,
	}
	if it.app == "radarr" {
		m["hasFile"] = it.hasFile()
		m["sizeOnDisk"] = it.size
		return m
	}
	m["ended"] = it.status == "ended"
	m["seasonCount"] = max(it.seasons, 1)
	if it.lastAired != nil {
		m["previousAiring"] = it.lastAired.UTC().Format(time.RFC3339)
	}
	m["statistics"] = map[string]any{
		"sizeOnDisk":       it.size,
		"episodeFileCount": it.episodeFileCount(),
		"seasonCount":      max(it.seasons, 1),
	}
	return m
}

const jellyfinServerID = "5c0d1a6e3f2b4a8c9d7e6f5a4b3c2d1e"

// jellyfinUsers: favorites belong to the first, so Coldarr's per-user
// union has something to union.
var jellyfinUsers = []map[string]string{
	{"Id": "8a2f6c1d4b3e4f5a9c8d7e6b5a4f3e2d", "Name": "alex"},
	{"Id": "1b9e8d7c6a5f4e3d2c1b0a9f8e7d6c5b", "Name": "sam"},
}

func (l *library) jellyfinHandler(version string) http.Handler {
	mux := http.NewServeMux()
	noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

	info := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"Id": jellyfinServerID, "ServerName": "jellyfin", "Version": version, "ProductName": "Jellyfin Server",
		})
	}
	mux.HandleFunc("GET /System/Info", info)
	mux.HandleFunc("GET /System/Info/Public", info)

	mux.HandleFunc("GET /Users", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, jellyfinUsers)
	})

	// Jellyfin's view of the library is whatever Radarr/Sonarr have on disk
	// right now. A Movie's Path is its video file; a Series' is its folder.
	mux.HandleFunc("GET /Users/{user}/Items", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		favoritesOnly := strings.Contains(q.Get("Filters"), "IsFavorite")
		types := strings.Split(q.Get("IncludeItemTypes"), ",")
		isFirstUser := r.PathValue("user") == jellyfinUsers[0]["Id"]

		l.mu.Lock()
		defer l.mu.Unlock()
		items := []map[string]any{}
		for _, it := range l.allItems() {
			kind, path := "Series", it.path()
			if it.app == "radarr" {
				files := it.files()
				if len(files) == 0 {
					continue
				}
				kind, path = "Movie", it.path()+"/"+files[0].rel
			}
			if q.Get("IncludeItemTypes") != "" && !slices.Contains(types, kind) {
				continue
			}
			if favoritesOnly && (!it.favorite || !isFirstUser) {
				continue
			}
			items = append(items, map[string]any{"Id": jellyfinID(path), "Name": it.title, "Path": path, "Type": kind})
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	})

	mux.HandleFunc("POST /Library/Media/Updated", noContent)
	mux.HandleFunc("POST /Library/Refresh", noContent)
	mux.HandleFunc("POST /Items/{id}/Refresh", noContent)

	mux.HandleFunc("GET /ScheduledTasks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{{
			"Id": "e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6", "Key": "UserDataRestore",
			"Name": "Restore user data after move", "State": "Idle",
		}})
	})
	mux.HandleFunc("POST /ScheduledTasks/Running/{id}", noContent)

	return logRequests("jellyfin", mux)
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return fallback
}

// logRequests logs writes and anything that fails, not the steady stream
// of reads a dashboard load makes.
func logRequests(app string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method != http.MethodGet || rec.status >= 400 {
			logf("%s: %s %q -> %d", app, r.Method, r.URL.RequestURI(), rec.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
