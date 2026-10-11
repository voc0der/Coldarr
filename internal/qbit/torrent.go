package qbit

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// Torrent is one qBittorrent torrent, normalized from /api/v2/torrents/info.
type Torrent struct {
	// Hash is lower-case, as qBittorrent reports it. Radarr, Sonarr and
	// Lidarr record the same hash upper-cased as a download's ID.
	Hash     string
	Name     string
	State    string
	Category string
	Tags     []string
	// SavePath is the directory the torrent's content sits in; ContentPath
	// is the content itself - the root folder of a multi-file torrent, or
	// the file of a single-file one. Both are as qBittorrent sees them.
	SavePath    string
	ContentPath string
	AddedOn     time.Time
	// CompletedOn is the zero Time until the torrent has finished
	// downloading (qBittorrent reports -1, 0, or 2^32-1 for that).
	CompletedOn time.Time
	// SeedingTime is how long the torrent has spent complete and running.
	// SeedingTimeKnown is false on qBittorrent versions too old to report
	// it, which is not the same thing as zero.
	SeedingTime      time.Duration
	SeedingTimeKnown bool
	// AmountLeft is how many bytes are still to download.
	AmountLeft int64
	// Size counts the files selected for download - what the torrent
	// occupies on disk once complete. Deselected files are excluded.
	Size     int64
	Progress float64
	AutoTMM  bool
	// TrackerHost is the host of the tracker currently working for the
	// torrent, or "" when none is. Never the announce URL, which carries
	// the account's passkey.
	TrackerHost string
	Ratio       float64
}

// Labels returns the torrent's category followed by its tags - the names a
// label rule can match.
func (t Torrent) Labels() []string {
	labels := make([]string, 0, 1+len(t.Tags))
	if t.Category != "" {
		labels = append(labels, t.Category)
	}
	return append(labels, t.Tags...)
}

// Complete reports whether every selected byte is on disk.
func (t Torrent) Complete() bool {
	return t.AmountLeft == 0 && t.Progress >= 1
}

type torrentResource struct {
	Hash         string    `json:"hash"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Category     string    `json:"category"`
	Tags         labelList `json:"tags"`
	SavePath     string    `json:"save_path"`
	ContentPath  string    `json:"content_path"`
	AddedOn      int64     `json:"added_on"`
	CompletionOn int64     `json:"completion_on"`
	SeedingTime  *int64    `json:"seeding_time"`
	AmountLeft   int64     `json:"amount_left"`
	Size         int64     `json:"size"`
	Progress     float64   `json:"progress"`
	AutoTMM      bool      `json:"auto_tmm"`
	Tracker      string    `json:"tracker"`
	Ratio        float64   `json:"ratio"`
}

func (r torrentResource) normalize() Torrent {
	t := Torrent{
		Hash:        strings.ToLower(r.Hash),
		Name:        r.Name,
		State:       r.State,
		Category:    r.Category,
		Tags:        []string(r.Tags),
		SavePath:    r.SavePath,
		ContentPath: r.ContentPath,
		AddedOn:     unixTime(r.AddedOn),
		CompletedOn: unixTime(r.CompletionOn),
		AmountLeft:  r.AmountLeft,
		Size:        r.Size,
		Progress:    r.Progress,
		AutoTMM:     r.AutoTMM,
		TrackerHost: TrackerHost(r.Tracker),
		Ratio:       r.Ratio,
	}
	if r.SeedingTime != nil && *r.SeedingTime >= 0 {
		t.SeedingTime = time.Duration(*r.SeedingTime) * time.Second
		t.SeedingTimeKnown = true
	}
	return t
}

// unixTime converts qBittorrent's epoch seconds, treating its "never"
// markers - -1, 0, and 2^32-1 from builds that stored it unsigned - as the
// zero Time.
func unixTime(sec int64) time.Time {
	if sec <= 0 || sec >= math.MaxUint32 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// labelList accepts tags as qBittorrent sends them - one comma-separated
// string - and as a JSON array, so a future API that switches to an array
// isn't mistaken for a torrent with no tags.
type labelList []string

func (l *labelList) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*l = cleanLabels(list)
		return nil
	}
	var joined string
	if err := json.Unmarshal(data, &joined); err != nil {
		return err
	}
	*l = cleanLabels(strings.Split(joined, ","))
	return nil
}

func cleanLabels(raw []string) labelList {
	out := labelList{}
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// InFlight reports whether qBittorrent is doing disk I/O on the torrent's
// data that changes or depends on where it lives: moving it, hash-checking
// it, or allocating it. Disk numbers read meanwhile are unreliable, so no
// plan is built or applied while anything is in one of these states.
func InFlight(state string) bool {
	switch state {
	case "moving", "checkingUP", "checkingDL", "checkingResumeData", "allocating":
		return true
	}
	return false
}

// Seeding reports whether the state is one of the complete ones -
// qBittorrent 5 renamed pausedUP to stoppedUP, so both spellings count.
func Seeding(state string) bool {
	switch state {
	case "uploading", "stalledUP", "queuedUP", "forcedUP", "pausedUP", "stoppedUP":
		return true
	}
	return false
}

// Errored reports whether qBittorrent has given up on the torrent's data
// until someone intervenes.
func Errored(state string) bool {
	return state == "error" || state == "missingFiles"
}

// Paused reports whether the torrent is stopped by hand, so it won't grow
// or move until someone resumes it. pausedDL/pausedUP became stoppedDL/
// stoppedUP in qBittorrent 5.
func Paused(state string) bool {
	switch state {
	case "pausedDL", "stoppedDL", "pausedUP", "stoppedUP":
		return true
	}
	return false
}
