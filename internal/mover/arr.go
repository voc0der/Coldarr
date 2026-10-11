package mover

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/planner"
)

// arrExecutor carries out library moves through Radarr/Sonarr's bulk-move
// APIs, for Movers.
type arrExecutor struct {
	m *Movers
}

// errUnknownApp is a move nobody can carry out - refused before any
// request is sent.
var errUnknownApp = errors.New("unknown arr app")

func (x arrExecutor) Move(entry planner.MoveEntry) error {
	switch entry.Item.ArrApp {
	case "radarr":
		return x.m.Radarr.MoveMovies([]int{entry.Item.ID}, entry.ToPath)
	case "sonarr":
		return x.m.Sonarr.MoveSeries([]int{entry.Item.ID}, entry.ToPath)
	default:
		return fmt.Errorf("%w %q", errUnknownApp, entry.Item.ArrApp)
	}
}

// Rejected: a concrete non-2xx response means Arr rejected the command, so
// no transfer from it is running. Transport and response-decoding failures
// are ambiguous: Arr may have accepted the move before the response was
// lost.
func (x arrExecutor) Rejected(err error) bool {
	var statusErr *arrapi.StatusError
	return errors.As(err, &statusErr) || errors.Is(err, errUnknownApp)
}

// ConfirmLanded asks Radarr/Sonarr to rescan the item's folder - forcing
// it to recompute the item's location/size from the real filesystem
// rather than a possibly-stale cached value - and reports whether it now
// considers the item fully landed: its path under entry.ToPath, AND its
// freshly-rescanned size actually matching what was planned (see
// sizeLanded). Checking the path alone isn't enough: Radarr/Sonarr can
// update an item's recorded path before the underlying file transfer is
// completely finished, so a path-only check can say "landed" while the
// file is still short. This is the ground-truth check settle() gates
// every "looks done" signal on; see settle's doc comment for why disk
// usage alone isn't enough either.
// It also returns the item's own folder path as Radarr/Sonarr now reports
// it, which is the only authoritative source for where the item actually
// ended up - see EntryProgress.LandedPath.
func (x arrExecutor) ConfirmLanded(entry planner.MoveEntry) (bool, string, error) {
	switch entry.Item.ArrApp {
	case "radarr":
		if err := x.m.Radarr.RescanMovie(entry.Item.ID); err != nil {
			return false, "", fmt.Errorf("rescanning movie: %w", err)
		}
		size, path, found, err := x.m.Radarr.GetMovieSize(entry.Item.ID)
		return landingMatches("Radarr", entry, size, path, found, err)
	case "sonarr":
		if err := x.m.Sonarr.RescanSeries(entry.Item.ID); err != nil {
			return false, "", fmt.Errorf("rescanning series: %w", err)
		}
		size, path, found, err := x.m.Sonarr.GetSeriesSize(entry.Item.ID)
		return landingMatches("Sonarr", entry, size, path, found, err)
	default:
		return false, "", fmt.Errorf("unknown arr app %q", entry.Item.ArrApp)
	}
}

// Landed reports the move downstream, then records it in the history -
// the cooldown ledger. A history failure is shown on the entry, but the
// move has already happened.
func (x arrExecutor) Landed(entry planner.MoveEntry, landedPath string) (bool, error) {
	reported := x.reportMoved(entry, landedPath)

	if err := x.m.History.Append(history.Record{
		ArrApp:    entry.Item.ArrApp,
		ItemID:    entry.Item.ID,
		Title:     entry.Item.Title,
		FromTier:  entry.FromTier,
		FromPath:  entry.FromPath,
		ToTier:    entry.ToTier,
		ToPath:    entry.ToPath,
		SizeBytes: entry.Item.SizeBytes,
		MovedAt:   time.Now(),
	}); err != nil {
		return reported, fmt.Errorf("moved, but recording history failed: %w", err)
	}
	return reported, nil
}

// reportMoved hands this item's old and new folders to Reporter inline
// with the move that just landed, rather than leaving every item in the
// run to be reported in one batch at the end, and reports whether that
// succeeded.
//
// Timing is the whole point. A consumer's indexing work is slow and
// starts late - Jellyfin sits on a reported path for a minute before
// touching it, then re-validates the entire containing library root - so
// a batch delivered after the last move means all of that begins from a
// standstill, with nothing left to overlap it. Delivered here it runs
// against the remaining moves, which take minutes to hours apiece.
//
// Synchronous, not fired into a goroutine: this is one small POST to a
// service the run is going to poll anyway, bounded by that client's own
// timeout, which is nothing beside the transfer it follows. Waiting for it
// is also what makes the result trustworthy enough to record, so the
// end-of-run pass can pick up precisely the items still needing a report
// instead of re-reporting everything and resetting their debounce.
//
// Never fatal, and deliberately not reflected in the entry's status: a
// consumer being unreachable says nothing about whether the media moved,
// which Radarr/Sonarr already confirmed.
func (x arrExecutor) reportMoved(entry planner.MoveEntry, landedPath string) bool {
	if x.m.Reporter == nil {
		return false
	}
	if err := x.m.Reporter.ReportMoved(entry.Item.Path, landedPath); err != nil {
		x.m.logf("mover: reporting %q moved to %s failed, leaving it for the end of the run: %v", entry.Item.Title, landedPath, err)
		return false
	}
	return true
}

func landingMatches(app string, entry planner.MoveEntry, size int64, path string, found bool, err error) (bool, string, error) {
	if err != nil {
		return false, "", fmt.Errorf("reading item from %s after rescan: %w", app, err)
	}
	if !found {
		return false, "", fmt.Errorf("%s no longer reports the item", app)
	}
	if !isUnderPath(path, entry.ToPath) {
		return false, "", fmt.Errorf("%s reports path %q, expected it under %q", app, path, entry.ToPath)
	}
	if !sizeLanded(size, entry.Item.SizeBytes) {
		return false, "", fmt.Errorf("%s reports %d bytes, expected approximately %d", app, size, entry.Item.SizeBytes)
	}
	return true, path, nil
}

// sizeLanded reports whether gotBytes - Radarr/Sonarr's freshly-rescanned,
// real read of the file on disk - is close enough to wantBytes (the
// item's expected size from the plan) to trust the transfer is actually
// finished, rather than a partial file that merely happens to already
// sit at the right path. A plain move never legitimately shrinks a file,
// so anything meaningfully short of wantBytes means it's still copying;
// a small tolerance absorbs harmless slack (e.g. a filesystem block-size
// rounding difference between how Coldarr and Radarr/Sonarr each compute
// "size on disk"), not genuine incompleteness.
func sizeLanded(gotBytes, wantBytes int64) bool {
	if wantBytes <= 0 {
		// No usable expected size to compare against - fall back to
		// trusting the rescanned path alone rather than blocking forever
		// on a check that can never be satisfied.
		return true
	}
	return gotBytes >= wantBytes*99/100
}

// isUnderPath reports whether path is root itself or a descendant of it.
func isUnderPath(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
