package torrents

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/qbit"
)

// ExecFS is what the executor needs to check a landing from Coldarr's own
// side of the mount.
type ExecFS interface {
	Exists(path string) (bool, error)
	// Size is the total size of the files at path - the file itself, or
	// everything under a folder.
	Size(path string) (int64, error)
}

// Executor carries out downloads plans through qBittorrent, for the shared
// move engine (mover.Run): each move is a setLocation for the whole group,
// waited on until qBittorrent says it's done and Coldarr can see it at
// its new home - then, and only then, labeled with the seed label.
type Executor struct {
	Qbit      *qbit.Client
	SeedLabel string
	FS        ExecFS
	Log       *history.DownloadLog
	// Now defaults to time.Now.
	Now func() time.Time

	mu sync.Mutex
	// sawMoving records, per move, that qBittorrent has been seen moving
	// it - after which finding it back where it started means the move
	// gave up, not that it hasn't begun.
	sawMoving map[string]bool
	// unseen counts, per move, confirmations where qBittorrent said it had
	// landed but Coldarr couldn't see it.
	unseen map[string]int
}

// unseenLimit is how many times in a row a move qBittorrent calls done
// may be invisible to Coldarr before that's taken as the mount mismatch it
// almost certainly is - a little slack for network filesystems that show
// new files late.
const unseenLimit = 3

func (x *Executor) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

// preMoveError is a move skipped before anything was asked of
// qBittorrent - nothing from it can be writing.
type preMoveError struct{ err error }

func (e *preMoveError) Error() string { return e.err.Error() }
func (e *preMoveError) Unwrap() error { return e.err }

func skip(format string, args ...any) error {
	return &preMoveError{err: fmt.Errorf(format, args...)}
}

// Move asks qBittorrent to move the group to its new location. The plan
// was made when the run started, and a run moving one download at a time
// can reach this one hours later - so qBittorrent is first asked again
// whether every member is still finished, and still where the plan found
// it. If not, the move is skipped: a download only ever moves once it's
// done, and one that isn't right now is considered afresh by a later run.
func (x *Executor) Move(e MoveEntry) error {
	list, err := x.Qbit.TorrentsByHash(e.Hashes)
	if err != nil {
		return skip("checking %s with qBittorrent before moving it: %w", e.Name, err)
	}
	found := indexByHash(list)
	for _, h := range e.Hashes {
		t, ok := found[h]
		switch {
		case !ok:
			return skip("%s is no longer in qBittorrent", e.Name)
		case !t.Complete() || !qbit.Seeding(t.State):
			return skip("%s isn't finished right now (qBittorrent reports %s) - it moves on a later run, once it's done", t.Name, t.State)
		case filepath.Clean(t.SavePath) != filepath.Clean(e.FromPath):
			return skip("%s has moved since the plan was made - it's now at %s", t.Name, t.SavePath)
		case t.AutoTMM && !contains(e.DisableAutoTMM, h):
			return skip("Automatic Torrent Management was switched on for %s since the plan was made", t.Name)
		}
	}

	if len(e.DisableAutoTMM) > 0 {
		if err := x.Qbit.SetAutoManagement(e.DisableAutoTMM, false); err != nil {
			return skip("switching off Automatic Torrent Management for %s: %w", e.Name, err)
		}
	}
	return x.Qbit.SetLocation(e.Hashes, e.Location)
}

// Rejected: a move skipped before the request, or one qBittorrent answered
// with an error status, never started. Anything else - a request whose
// answer was lost - may have.
func (x *Executor) Rejected(err error) bool {
	var skipped *preMoveError
	var status *qbit.StatusError
	var auth *qbit.AuthError
	return errors.As(err, &skipped) || errors.As(err, &status) || errors.As(err, &auth)
}

// ConfirmLanded reports whether qBittorrent has finished moving every
// member - none of them still moving or checking, each saved at the new
// location and complete - and whether Coldarr, looking for itself, finds
// it there at about the right size. qBittorrent reporting the move went
// wrong ends the wait: a member gone, in an error state, incomplete after
// the move, or back where it started after being seen moving - and a
// landing that stays invisible to Coldarr, which means the drive isn't
// mounted in Coldarr the way it is in qBittorrent.
func (x *Executor) ConfirmLanded(e MoveEntry) (bool, string, error) {
	list, err := x.Qbit.TorrentsByHash(e.Hashes)
	if err != nil {
		return false, "", fmt.Errorf("asking qBittorrent about %s: %w", e.Name, err)
	}
	key := e.Hashes[0]
	found := indexByHash(list)
	for _, h := range e.Hashes {
		t, ok := found[h]
		if !ok {
			return false, "", fmt.Errorf("%w: qBittorrent no longer has %s", mover.ErrMoveFailed, e.Name)
		}
		switch {
		case qbit.Errored(t.State):
			return false, "", fmt.Errorf("%w: qBittorrent reports %s for %s", mover.ErrMoveFailed, t.State, t.Name)
		case t.State == "moving":
			x.markMoving(key)
			return false, "", fmt.Errorf("qBittorrent is still moving %s", t.Name)
		case qbit.InFlight(t.State):
			return false, "", fmt.Errorf("qBittorrent is checking %s", t.Name)
		case filepath.Clean(t.SavePath) != filepath.Clean(e.Location):
			if x.wasMoving(key) && filepath.Clean(t.SavePath) == filepath.Clean(e.FromPath) {
				return false, "", fmt.Errorf("%w: qBittorrent stopped moving %s and left it at %s", mover.ErrMoveFailed, t.Name, t.SavePath)
			}
			return false, "", fmt.Errorf("qBittorrent still has %s at %s", t.Name, t.SavePath)
		case !t.Complete():
			return false, "", fmt.Errorf("%w: %s is %.0f%% complete after the move - qBittorrent will download what's missing", mover.ErrMoveFailed, t.Name, t.Progress*100)
		}
	}

	exists, err := x.FS.Exists(e.NewContentPath)
	if err != nil {
		return false, "", fmt.Errorf("checking %s: %w", e.NewContentPath, err)
	}
	if !exists {
		if x.notSeen(key) >= unseenLimit {
			return false, "", fmt.Errorf("%w: qBittorrent reports %s moved to %s, but Coldarr can't see it there - is %s mounted into Coldarr from the same drive, at the same path, as in qBittorrent?", mover.ErrMoveFailed, e.Name, e.NewContentPath, e.ToPath)
		}
		return false, "", fmt.Errorf("qBittorrent reports %s moved, but Coldarr can't see %s yet", e.Name, e.NewContentPath)
	}
	size, err := x.FS.Size(e.NewContentPath)
	if err != nil {
		return false, "", fmt.Errorf("sizing %s: %w", e.NewContentPath, err)
	}
	if e.SizeBytes > 0 && size < e.SizeBytes*99/100 {
		return false, "", fmt.Errorf("%s is %d bytes at %s so far, expected about %d", e.Name, size, e.NewContentPath, e.SizeBytes)
	}
	return true, e.NewContentPath, nil
}

// Landed gives the group the seed label - only now that the move has
// landed - and records the move. A failed relabel is retried by the next
// run (see Download.Relabel); either failure leaves the move counted as
// landed, since its data has moved.
func (x *Executor) Landed(e MoveEntry, landedPath string) (bool, error) {
	var errs []error
	if err := x.applySeedLabel(e.Hashes); err != nil {
		errs = append(errs, fmt.Errorf("moved, but giving it the %q label failed - the next run tries again: %w", x.SeedLabel, err))
	}
	if err := x.Log.Append(history.DownloadEvent{
		Action:    history.DownloadMoved,
		Names:     e.Names,
		Hashes:    e.Hashes,
		FromPath:  e.ContentPath,
		ToPath:    landedPath,
		SizeBytes: e.SizeBytes,
		At:        x.now(),
	}); err != nil {
		errs = append(errs, fmt.Errorf("moved, but recording it failed: %w", err))
	}
	return false, errors.Join(errs...)
}

// Relabel gives a download already on seed storage the seed label (see
// Download.Relabel), switching off Automatic Torrent Management first
// when that's what it takes - with it on, a category change would move
// the download.
func (x *Executor) Relabel(r Relabel) error {
	if r.DisableAutoTMM {
		if err := x.Qbit.SetAutoManagement([]string{r.Hash}, false); err != nil {
			return fmt.Errorf("switching off Automatic Torrent Management for %s: %w", r.Name, err)
		}
	}
	if err := x.applySeedLabel([]string{r.Hash}); err != nil {
		return fmt.Errorf("giving %s the %q label: %w", r.Name, x.SeedLabel, err)
	}
	return x.Log.Append(history.DownloadEvent{
		Action: history.DownloadRelabeled,
		Names:  []string{r.Name},
		Hashes: []string{r.Hash},
		Detail: fmt.Sprintf("%q to %q", r.Category, x.SeedLabel),
		At:     x.now(),
	})
}

// applySeedLabel sets the seed label as the downloads' category, creating
// it first if qBittorrent doesn't have it - with no save path of its own,
// so the category change alone never moves anything.
func (x *Executor) applySeedLabel(hashes []string) error {
	categories, err := x.Qbit.Categories()
	if err != nil {
		return fmt.Errorf("reading qBittorrent's categories: %w", err)
	}
	if _, ok := categories[x.SeedLabel]; !ok {
		if err := x.Qbit.CreateCategory(x.SeedLabel); err != nil {
			return fmt.Errorf("creating the %q category: %w", x.SeedLabel, err)
		}
	}
	return x.Qbit.SetCategory(hashes, x.SeedLabel)
}

func (x *Executor) markMoving(key string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.sawMoving == nil {
		x.sawMoving = map[string]bool{}
	}
	x.sawMoving[key] = true
}

func (x *Executor) wasMoving(key string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.sawMoving[key]
}

func (x *Executor) notSeen(key string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.unseen == nil {
		x.unseen = map[string]int{}
	}
	x.unseen[key]++
	return x.unseen[key]
}

func indexByHash(list []qbit.Torrent) map[string]qbit.Torrent {
	out := make(map[string]qbit.Torrent, len(list))
	for _, t := range list {
		out[t.Hash] = t
	}
	return out
}
