package webui

import (
	"fmt"
	"sync"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/history"
)

// verifyEntry is one history record's re-check result. History only
// records the tier-level From/To paths a move used (e.g. "/mnt/cold1/tv"),
// not the item's own subfolder, so the only reliable way to find its
// current size is to ask the owning Radarr/Sonarr item directly by the
// ArrApp+ItemID already stored in history - not to guess a folder name
// from the title.
type verifyEntry struct {
	Record history.Record
	// Status is "checking", "done", or "failed".
	Status      string
	CurrentSize int64
	// SizeStatus is only meaningful once Status is "done": "match",
	// "grew", "shrank", or "unknown" (the item is no longer in
	// Radarr/Sonarr, so there's nothing to compare against).
	SizeStatus string
	Err        string
}

type verifySnapshot struct {
	Done    bool
	Entries []verifyEntry
}

// verifyProgress is a live view of an in-progress (or completed) size
// verification run, safe to read from any goroutine (e.g. a web handler
// polling for status while the checks continue in the background).
type verifyProgress struct {
	mu      sync.Mutex
	done    bool
	entries []verifyEntry
	doneCh  chan struct{}
}

func newVerifyProgress(records []history.Record) *verifyProgress {
	entries := make([]verifyEntry, len(records))
	for i, rec := range records {
		entries[i] = verifyEntry{Record: rec, Status: "checking"}
	}
	return &verifyProgress{entries: entries, doneCh: make(chan struct{})}
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
	return verifySnapshot{Done: p.done, Entries: entries}
}

// startVerify checks every record concurrently and returns immediately
// with a *verifyProgress the caller can poll; the checks themselves
// continue in the background. Unlike mover.Apply, there's no need to
// serialize by destination volume here - these are light read-only API
// calls, not disk writes, so full concurrency is safe.
func startVerify(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, records []history.Record) *verifyProgress {
	progress := newVerifyProgress(records)

	var wg sync.WaitGroup
	wg.Add(len(records))
	for i, rec := range records {
		go func(i int, rec history.Record) {
			defer wg.Done()
			verifyOne(radarr, sonarr, rec, progress, i)
		}(i, rec)
	}

	go func() {
		wg.Wait()
		progress.markDone()
	}()

	return progress
}

func verifyOne(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, rec history.Record, progress *verifyProgress, i int) {
	var (
		currentSize int64
		found       bool
		err         error
	)

	switch rec.ArrApp {
	case "radarr":
		if radarr == nil {
			err = fmt.Errorf("radarr is not configured")
		} else {
			currentSize, found, err = radarr.GetMovieSize(rec.ItemID)
		}
	case "sonarr":
		if sonarr == nil {
			err = fmt.Errorf("sonarr is not configured")
		} else {
			currentSize, found, err = sonarr.GetSeriesSize(rec.ItemID)
		}
	default:
		err = fmt.Errorf("unknown arr app %q", rec.ArrApp)
	}

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

	sizeStatus := "match"
	switch {
	case currentSize > rec.SizeBytes:
		sizeStatus = "grew"
	case currentSize < rec.SizeBytes:
		sizeStatus = "shrank"
	}

	progress.update(i, func(e *verifyEntry) {
		e.Status = "done"
		e.CurrentSize = currentSize
		e.SizeStatus = sizeStatus
	})
}
