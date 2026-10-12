package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// downloadSpec describes a multi-file torrent. Repeating its name and root
// creates a cross-seed of the same sparse files, with a distinct hash.
type downloadSpec struct {
	Name             string   `yaml:"name"`
	Root             string   `yaml:"root"`
	Size             byteSize `yaml:"size"`
	Category         string   `yaml:"category"`
	Tags             []string `yaml:"tags"`
	State            string   `yaml:"state"`
	CompletedDaysAgo float64  `yaml:"completed_days_ago"`
	SeededDays       float64  `yaml:"seeded_days"`
	Trackers         []string `yaml:"trackers"`
	ImportedBy       string   `yaml:"imported_by"`
	Imported         bool     `yaml:"imported"`
	Queued           bool     `yaml:"queued"`
	AutoTMM          bool     `yaml:"auto_tmm"`
	Missing          bool     `yaml:"missing"`
}

type download struct {
	downloadSpec
	hash      string
	completed int64
}

func (d *download) path() string { return filepath.Join(d.Root, d.Name) }
func (d *download) files() []fileSpec {
	return []fileSpec{{rel: "content.mkv", size: int64(d.Size)}}
}

func (l *library) seedDownloads(specs []downloadSpec, now time.Time) error {
	l.seedTargets = slices.Clone(l.seedRoots)
	for i, spec := range specs {
		if spec.Name == "" || spec.Name == "." || spec.Name == ".." || filepath.Base(spec.Name) != spec.Name ||
			!filepath.IsAbs(spec.Root) || l.driveOf(spec.Root) == nil || spec.Size <= 0 {
			return fmt.Errorf("download %d needs a plain name, a root on a fixture drive and a positive size", i+1)
		}
		d := &download{downloadSpec: spec, hash: fmt.Sprintf("%040x", i+1)}
		// Precompute the category destinations from the fixture and tier
		// paths. Requests can only select one of these stored paths.
		for _, tierPath := range l.rootFolders {
			rel, err := filepath.Rel(tierPath, d.Root)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			for _, seed := range l.seedRoots {
				l.seedTargets = append(l.seedTargets, filepath.Join(seed, rel))
			}
		}
		if d.State == "" {
			d.State = "stalledUP"
		}
		if d.CompletedDaysAgo > 0 {
			d.completed = daysAgo(now, d.CompletedDaysAgo).Unix()
		}
		if len(d.Trackers) == 0 {
			d.Trackers = []string{"tracker.example.org"}
		}
		if !d.Missing {
			for _, f := range d.files() {
				if err := writeSparse(filepath.Join(d.path(), f.rel), f.size); err != nil {
					return err
				}
			}
		}
		l.downloads = append(l.downloads, d)
	}
	return nil
}

// qbitHandler implements the API surface used by the gallery scenes. All
// requests are local to the disposable container; deletion is unsupported.
func (l *library) qbitHandler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/app/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, version)
	})
	mux.HandleFunc("GET /api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		out := []map[string]any{}
		for _, d := range l.downloads {
			if hashes := r.URL.Query().Get("hashes"); hashes != "" && !slices.Contains(strings.Split(hashes, "|"), d.hash) {
				continue
			}
			progress, left := 1.0, int64(0)
			if d.completed == 0 {
				progress, left = 0.45, int64(d.Size)*55/100
			}
			out = append(out, map[string]any{
				"hash": d.hash, "name": d.Name, "state": d.State, "category": d.Category,
				"tags": strings.Join(d.Tags, ", "), "save_path": d.Root, "content_path": d.path(),
				"completion_on": d.completed, "seeding_time": int64(d.SeededDays * 86400),
				"size": int64(d.Size), "amount_left": left, "progress": progress, "auto_tmm": d.AutoTMM,
				"tracker": "https://" + d.Trackers[0] + "/announce",
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v2/torrents/categories", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		out := map[string]map[string]string{"seed": {"name": "seed", "savePath": ""}}
		for _, d := range l.downloads {
			out[d.Category] = map[string]string{"name": d.Category, "savePath": ""}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v2/torrents/tags", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		out := []string{}
		for _, d := range l.downloads {
			out = append(out, d.Tags...)
		}
		sort.Strings(out)
		writeJSON(w, http.StatusOK, slices.Compact(out))
	})
	for _, resource := range []string{"trackers", "files"} {
		mux.HandleFunc("GET /api/v2/torrents/"+resource, func(w http.ResponseWriter, r *http.Request) {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, d := range l.downloads {
				if d.hash != r.URL.Query().Get("hash") {
					continue
				}
				out := []map[string]string{}
				if resource == "trackers" {
					for _, host := range d.Trackers {
						out = append(out, map[string]string{"url": "https://" + host + "/announce"})
					}
				} else {
					for _, f := range d.files() {
						out = append(out, map[string]string{"name": filepath.Join(d.Name, f.rel)})
					}
				}
				writeJSON(w, http.StatusOK, out)
				return
			}
			http.NotFound(w, r)
		})
	}
	mux.HandleFunc("POST /api/v2/torrents/setCategory", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.FormValue("category") != "seed" {
			http.Error(w, "gallery only supports the seed category", http.StatusConflict)
			return
		}
		for _, d := range l.downloads {
			if slices.Contains(strings.Split(r.FormValue("hashes"), "|"), d.hash) {
				d.Category = "seed"
			}
		}
	})
	mux.HandleFunc("POST /api/v2/torrents/setLocation", l.moveDownloads)
	return logRequests("qbittorrent", mux)
}

func (l *library) moveDownloads(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Coldarr preserves category subdirectories (e.g. /downloads/movies
	// becomes /seed/movies), but every destination must stay on seed storage.
	index := slices.Index(l.seedTargets, filepath.Clean(r.FormValue("location")))
	if index < 0 {
		http.Error(w, "not a configured seed path", http.StatusBadRequest)
		return
	}
	root := l.seedTargets[index]
	var group []*download
	for _, hash := range strings.Split(r.FormValue("hashes"), "|") {
		index := slices.IndexFunc(l.downloads, func(d *download) bool { return d.hash == hash })
		if index < 0 {
			http.NotFound(w, r)
			return
		}
		d := l.downloads[index]
		if d.completed == 0 || d.AutoTMM || d.State == "moving" || len(group) > 0 && d.path() != group[0].path() {
			http.Error(w, "downloads must be settled and share the same content", http.StatusConflict)
			return
		}
		group = append(group, d)
	}
	first := group[0]
	src, dst, files, size := first.path(), filepath.Join(root, first.Name), first.files(), int64(first.Size)
	for _, d := range group {
		d.State = "moving"
	}
	go func() {
		err := l.relocateFiles(src, dst, files, size)
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, d := range group {
			if err != nil {
				d.State = "error"
			} else {
				d.Root, d.State = root, "stalledUP"
			}
		}
		if err != nil {
			logf("qBittorrent: moving %q failed: %v", first.Name, err)
		}
	}()
}
