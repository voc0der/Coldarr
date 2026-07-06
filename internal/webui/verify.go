package webui

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/history"
)

// verifyMode controls how thorough a size check is.
type verifyMode string

const (
	// verifyQuick trusts whatever size Radarr/Sonarr already has cached
	// in its own database - one API call, fast, but that cache is only
	// as fresh as Radarr/Sonarr's own last look at the file.
	verifyQuick verifyMode = "quick"
	// verifyComplete asks Radarr/Sonarr to rescan the folder first, then
	// independently walks the real media file(s) on disk itself rather
	// than trusting any cached value - slower, but reflects exactly
	// what's physically there right now.
	verifyComplete verifyMode = "complete"
)

func parseVerifyMode(raw string) verifyMode {
	if verifyMode(raw) == verifyComplete {
		return verifyComplete
	}
	return verifyQuick
}

// verifyEntry is one history record's re-check result. History only
// records the tier-level From/To paths a move used (e.g. "/mnt/cold1/tv"),
// not the item's own subfolder, so the only reliable way to find its
// current size is to ask the owning Radarr/Sonarr item directly by the
// ArrApp+ItemID already stored in history - not to guess a folder name
// from the title.
type verifyEntry struct {
	Record history.Record
	// Status is "checking", "done", or "failed".
	Status string
	// CurrentSize is the size the verdict (SizeStatus) is judged
	// against: Radarr/Sonarr's cached value in quick mode, or the real
	// bytes found on disk in complete mode.
	CurrentSize int64
	// ArrSize and HasArrSize are only populated in complete mode, once
	// Status is "done" - Radarr/Sonarr's own (freshly rescanned) cached
	// size, shown alongside the physical count so a discrepancy between
	// what Radarr/Sonarr believes and physical reality is visible too.
	ArrSize    int64
	HasArrSize bool
	// SizeStatus is only meaningful once Status is "done": "match",
	// "grew", "shrank", or "unknown" (the item is no longer in
	// Radarr/Sonarr, so there's nothing to compare against).
	SizeStatus string
	// Note is an informational aside on an otherwise "done" entry (e.g.
	// the rescan request itself failed but the physical check proceeded
	// anyway) - unlike Err, it doesn't mean the check failed.
	Note string
	Err  string
}

type verifySnapshot struct {
	Done    bool
	Mode    verifyMode
	Entries []verifyEntry
}

// verifyProgress is a live view of an in-progress (or completed) size
// verification run, safe to read from any goroutine (e.g. a web handler
// polling for status while the checks continue in the background).
type verifyProgress struct {
	mu      sync.Mutex
	done    bool
	mode    verifyMode
	entries []verifyEntry
	doneCh  chan struct{}
}

func newVerifyProgress(records []history.Record, mode verifyMode) *verifyProgress {
	entries := make([]verifyEntry, len(records))
	for i, rec := range records {
		entries[i] = verifyEntry{Record: rec, Status: "checking"}
	}
	return &verifyProgress{entries: entries, mode: mode, doneCh: make(chan struct{})}
}

func (p *verifyProgress) update(i int, fn func(*verifyEntry)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.entries[i])
}

func (p *verifyProgress) markDone() {
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
	close(p.doneCh)
}

func (p *verifyProgress) Snapshot() verifySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]verifyEntry, len(p.entries))
	copy(entries, p.entries)
	return verifySnapshot{Done: p.done, Mode: p.mode, Entries: entries}
}

// startVerify checks every record concurrently and returns immediately
// with a *verifyProgress the caller can poll; the checks themselves
// continue in the background. Unlike mover.Apply, there's no need to
// serialize by destination volume here - these are light read-only API
// calls (and, in complete mode, disk reads), not disk writes, so full
// concurrency is safe.
func startVerify(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, records []history.Record, mode verifyMode) *verifyProgress {
	progress := newVerifyProgress(records, mode)

	var wg sync.WaitGroup
	wg.Add(len(records))
	for i, rec := range records {
		go func(i int, rec history.Record) {
			defer wg.Done()
			verifyOne(radarr, sonarr, rec, mode, progress, i)
		}(i, rec)
	}

	go func() {
		wg.Wait()
		progress.markDone()
	}()

	return progress
}

func verifyOne(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, rec history.Record, mode verifyMode, progress *verifyProgress, i int) {
	getSize := func() (size int64, path string, found bool, err error) {
		switch rec.ArrApp {
		case "radarr":
			if radarr == nil {
				return 0, "", false, fmt.Errorf("radarr is not configured")
			}
			return radarr.GetMovieSize(rec.ItemID)
		case "sonarr":
			if sonarr == nil {
				return 0, "", false, fmt.Errorf("sonarr is not configured")
			}
			return sonarr.GetSeriesSize(rec.ItemID)
		default:
			return 0, "", false, fmt.Errorf("unknown arr app %q", rec.ArrApp)
		}
	}

	arrSize, path, found, err := getSize()
	if err != nil {
		progress.update(i, func(e *verifyEntry) {
			e.Status = "failed"
			e.Err = err.Error()
		})
		return
	}
	if !found {
		progress.update(i, func(e *verifyEntry) {
			e.Status = "done"
			e.SizeStatus = "unknown"
		})
		return
	}

	if mode == verifyQuick {
		progress.update(i, func(e *verifyEntry) {
			e.Status = "done"
			e.CurrentSize = arrSize
			e.SizeStatus = compareSizes(arrSize, rec.SizeBytes)
		})
		return
	}

	// Complete: ask Radarr/Sonarr to rescan the folder first (best
	// effort - the physical walk below is authoritative regardless of
	// whether this succeeds or times out), then independently walk the
	// real media file(s) on disk rather than trusting any cached value.
	var rescanErr error
	switch rec.ArrApp {
	case "radarr":
		rescanErr = radarr.RescanMovie(rec.ItemID)
	case "sonarr":
		rescanErr = sonarr.RescanSeries(rec.ItemID)
	}

	var note string
	if rescanErr != nil {
		note = fmt.Sprintf("rescan request failed (%v) - checked the files on disk anyway", rescanErr)
	} else if refreshedSize, refreshedPath, refreshedFound, err := getSize(); err == nil && refreshedFound {
		arrSize, path = refreshedSize, refreshedPath
	}

	diskSize, walkErr := walkMediaSize(path)
	if walkErr != nil {
		progress.update(i, func(e *verifyEntry) {
			e.Status = "failed"
			e.Err = fmt.Sprintf("walking %s: %v", path, walkErr)
		})
		return
	}

	progress.update(i, func(e *verifyEntry) {
		e.Status = "done"
		e.CurrentSize = diskSize
		e.ArrSize = arrSize
		e.HasArrSize = true
		e.Note = note
		e.SizeStatus = compareSizes(diskSize, rec.SizeBytes)
	})
}

func compareSizes(current, recorded int64) string {
	switch {
	case current > recorded:
		return "grew"
	case current < recorded:
		return "shrank"
	default:
		return "match"
	}
}

// mediaExtensions is the same set of file extensions Radarr/Sonarr
// themselves recognize as an importable media file (see
// MediaFileExtensions.cs in their source) - Radarr/Sonarr's own
// "sizeOnDisk" only ever counts these files, never posters/.nfo/
// subtitles/other extras sitting in the same folder, so walkMediaSize
// filters to the same set to stay a fair, apples-to-apples comparison
// against a size Radarr/Sonarr itself recorded.
var mediaExtensions = map[string]bool{
	".webm": true, ".m4v": true, ".3gp": true, ".nsv": true, ".ty": true,
	".strm": true, ".rm": true, ".rmvb": true, ".m3u": true, ".ifo": true,
	".mov": true, ".qt": true, ".divx": true, ".xvid": true, ".bivx": true,
	".nrg": true, ".pva": true, ".wmv": true, ".asf": true, ".asx": true,
	".ogm": true, ".ogv": true, ".m2v": true, ".avi": true, ".bin": true,
	".dat": true, ".dvr-ms": true, ".mpg": true, ".mpeg": true, ".mp4": true,
	".avc": true, ".vp3": true, ".svq3": true, ".nuv": true, ".viv": true,
	".dv": true, ".fli": true, ".flv": true, ".wpl": true, ".img": true,
	".iso": true, ".vob": true, ".mkv": true, ".mk3d": true, ".ts": true,
	".wtv": true, ".m2ts": true,
}

// walkMediaSize returns the total size in bytes of every recognized media
// file found recursively under root - the real physical footprint of the
// movie/episode file(s), independent of whatever Radarr/Sonarr's own
// database has cached.
func walkMediaSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && mediaExtensions[strings.ToLower(filepath.Ext(path))] {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}
