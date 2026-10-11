package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Download actions, as recorded in a DownloadEvent.
const (
	DownloadMoved     = "moved"
	DownloadRelabeled = "relabeled"
	DownloadRemoved   = "removed"
	// DownloadUnlisted is a removal that left the files in place, because
	// another download still seeds them.
	DownloadUnlisted = "unlisted"
)

// DownloadEvent records one thing Coldarr had qBittorrent do to downloads.
// It's a log of its own rather than library Records: downloads have no
// Radarr/Sonarr item to verify or put on cooldown, and their removals
// matter to see even more than their moves.
type DownloadEvent struct {
	Action string `json:"action"`
	// Names and Hashes list every download the action covered - more than
	// one for cross-seeds sharing their files.
	Names     []string  `json:"names"`
	Hashes    []string  `json:"hashes"`
	FromPath  string    `json:"from_path,omitempty"`
	ToPath    string    `json:"to_path,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// DownloadLog is an append-only, file-backed log of DownloadEvents, safe
// for concurrent use within one process.
type DownloadLog struct {
	path string

	mu     sync.Mutex
	events []DownloadEvent
}

// LoadDownloadLog reads the log at path, starting empty if it doesn't
// exist.
func LoadDownloadLog(path string) (*DownloadLog, error) {
	l := &DownloadLog{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fmt.Errorf("reading download history %s: %w", path, err)
	}
	if len(data) == 0 {
		return l, nil
	}
	if err := json.Unmarshal(data, &l.events); err != nil {
		return nil, fmt.Errorf("parsing download history %s: %w", path, err)
	}
	return l, nil
}

// Append records ev and persists the log at once, so a crash mid-run
// doesn't lose what already happened.
func (l *DownloadLog) Append(ev DownloadEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)

	data, err := json.MarshalIndent(l.events, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding download history: %w", err)
	}
	if dir := filepath.Dir(l.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating download history directory %s: %w", dir, err)
		}
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing download history %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("saving download history %s: %w", l.path, err)
	}
	return nil
}

// Recent returns up to n events, most recent first.
func (l *DownloadLog) Recent(n int) []DownloadEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]DownloadEvent, len(l.events))
	copy(out, l.events)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
