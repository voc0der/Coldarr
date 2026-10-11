package torrents

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
)

// done is a download on tierPath that has seeded seededDays and met its
// requirement.
func done(hash, name, contentPath string, seededDays int) *Download {
	return &Download{
		Torrent:          qbit.Torrent{Hash: hash, Name: name, ContentPath: contentPath, Size: 100},
		Tier:             model.Tier{Name: "seed"},
		TierPath:         "/seed",
		State:            Parked,
		Seeding:          Seeding{Known: true, Met: true, Seeded: time.Duration(seededDays) * 24 * time.Hour, Rule: "default: 14 days"},
		ImportsConfirmed: true,
	}
}

func removalNames(rs []Removal) string {
	var out []string
	for _, r := range rs {
		out = append(out, strings.Join(r.Names, "+"))
	}
	return strings.Join(out, ",")
}

func TestPlanRemovals_Guards(t *testing.T) {
	notMet := done("b", "Still.Seeding", "/seed/Still.Seeding", 3)
	notMet.Seeding.Met = false
	active := done("c", "Busy", "/seed/Busy", 30)
	active.State = Active
	ignored := done("d", "Transfer", "/seed/Transfer", 30)
	ignored.State = Ignored
	outside := done("e", "Elsewhere", "/elsewhere/E", 30)
	outside.TierPath = ""
	unimported := done("f", "Unimported", "/downloads/Unimported", 30)
	unimported.State, unimported.ImportsConfirmed, unimported.importReason = Waiting, false, "Radarr hasn't imported it yet"
	library := done("g", "In.Library", "/data/media/In.Library", 30)
	onHot := done("h", "On.Hot", "/downloads/On.Hot", 30)
	onHot.State = Ready

	plan := PlanRemovals([]*Download{done("a", "Done", "/seed/Done", 30), notMet, active, ignored, outside, unimported, library, onHot}, []string{"/data/media/In.Library/"}, 25)

	if got := removalNames(plan.Removals); got != "Done,On.Hot" {
		t.Errorf("removals = %s, want only the finished, cleared downloads - from either drive", got)
	}
	if !plan.Removals[0].DeleteFiles || plan.Removals[0].KeptFor != "" {
		t.Errorf("a download sharing nothing = %+v, want its files deleted", plan.Removals[0])
	}
	held := map[string]string{}
	for _, h := range plan.Held {
		held[h.Name] = h.Reason
	}
	if !strings.Contains(held["Unimported"], "done seeding, but Radarr hasn't imported it yet") {
		t.Errorf("Unimported held = %q", held["Unimported"])
	}
	if !strings.Contains(held["In.Library"], "shares a folder with a library item at /data/media/In.Library/") {
		t.Errorf("In.Library held = %q", held["In.Library"])
	}
	if len(held) != 2 {
		t.Errorf("held = %v, want only the two done downloads that were blocked", held)
	}
}

// TestPlanRemovals_SharedFiles: files a download still needs are never
// deleted - cross-seeds that are all done go together, files and all,
// while a done one sharing with one that isn't leaves only qBittorrent.
func TestPlanRemovals_SharedFiles(t *testing.T) {
	bothA := done("a1", "Both", "/seed/Both", 30)
	bothB := done("a2", "Both", "/seed/Both", 40)
	mixedDone := done("b1", "Mixed", "/seed/Mixed", 30)
	mixedNot := done("b2", "Mixed", "/seed/Mixed", 3)
	mixedNot.Seeding.Met = false
	withIgnored := done("c1", "Shared.With.Transfer", "/seed/Shared.With.Transfer", 30)
	transfer := done("c2", "Shared.With.Transfer", "/seed/Shared.With.Transfer", 30)
	transfer.State = Ignored
	outer := done("d1", "Pack", "/seed/Pack", 30)
	inner := done("d2", "Episode", "/seed/Pack/ep1.mkv", 3)
	inner.Seeding.Met = false
	sibling := done("e1", "Pack.REPACK", "/seed/Pack.REPACK", 30)

	plan := PlanRemovals([]*Download{bothA, bothB, mixedDone, mixedNot, withIgnored, transfer, outer, inner, sibling}, nil, 25)
	byName := map[string]Removal{}
	for _, r := range plan.Removals {
		byName[r.Name] = r
	}

	if r := byName["Both"]; strings.Join(r.Hashes, ",") != "a1,a2" || !r.DeleteFiles || r.Seeded != 30*24*time.Hour {
		t.Errorf("Both = %+v, want both removed together with their files, counted by the shorter seed", r)
	}
	if r := byName["Mixed"]; strings.Join(r.Hashes, ",") != "b1" || r.DeleteFiles || r.KeptFor != "Mixed" {
		t.Errorf("Mixed = %+v, want only the done one removed, files kept", r)
	}
	if r := byName["Shared.With.Transfer"]; r.DeleteFiles || strings.Join(r.Hashes, ",") != "c1" {
		t.Errorf("Shared.With.Transfer = %+v, want the files kept for the download Coldarr leaves alone", r)
	}
	if r := byName["Pack"]; r.DeleteFiles || r.KeptFor != "Episode" {
		t.Errorf("Pack = %+v, want its folder kept: Episode's file is inside it", r)
	}
	if r := byName["Pack.REPACK"]; !r.DeleteFiles {
		t.Errorf("Pack.REPACK = %+v - a name that merely starts the same shares nothing", r)
	}
}

func TestPlanRemovals_CapsEachRun(t *testing.T) {
	pair1 := done("p1", "Pair", "/seed/Pair", 50)
	pair2 := done("p2", "Pair", "/seed/Pair", 50)
	plan := PlanRemovals([]*Download{
		done("a", "Oldest", "/seed/Oldest", 90),
		pair1, pair2,
		done("b", "Newer", "/seed/Newer", 20),
	}, nil, 2)
	if got := removalNames(plan.Removals); got != "Oldest,Newer" {
		t.Errorf("removals = %s, want the longest-seeded first, skipping what doesn't fit the cap", got)
	}
	if got := removalNames(plan.Deferred); got != "Pair+Pair" {
		t.Errorf("deferred = %s", got)
	}
}

func TestShareClusters(t *testing.T) {
	ds := []*Download{
		{Torrent: qbit.Torrent{Hash: "1", ContentPath: "/dl/Show"}},
		{Torrent: qbit.Torrent{Hash: "2", ContentPath: "/dl/Show.S01"}},
		{Torrent: qbit.Torrent{Hash: "3", ContentPath: "/dl/Show/ep.mkv"}},
		{Torrent: qbit.Torrent{Hash: "4"}},
		{Torrent: qbit.Torrent{Hash: "5", ContentPath: "/dl/Show/"}},
	}
	var got []string
	for _, c := range shareClusters(ds) {
		var hashes []string
		for _, d := range c {
			hashes = append(hashes, d.Hash)
		}
		got = append(got, strings.Join(hashes, "+"))
	}
	if strings.Join(got, ",") != "1+5+3,2" { // 5 is 1's path with a trailing slash
		t.Errorf("clusters = %v, want Show with its episode (across the .S01 sibling), and .S01 alone", got)
	}
}

func TestRemover(t *testing.T) {
	trackers := trackerMap{"a": {"other.net"}, "b": {"other.net"}}
	seeding := config.SeedingConfig{MinSeedDays: 1}
	finished := func(hash string) execTorrent {
		return execTorrent{Hash: hash, Name: "Done", State: "stalledUP", SavePath: "/seed", ContentPath: "/seed/Done", Progress: 1, SeedingTime: 30 * 24 * 3600, CompletionOn: now.Add(-30 * 24 * time.Hour).Unix()}
	}
	newRemover := func(qb *execQbit) (*Remover, *history.DownloadLog) {
		log, err := history.LoadDownloadLog(filepath.Join(t.TempDir(), "downloads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return &Remover{Qbit: qbit.NewClient(qb.URL, qbit.Auth{}), Trackers: trackers, Seeding: seeding, Log: log, Now: func() time.Time { return now }}, log
	}
	r := Removal{Name: "Done", Names: []string{"Done", "Done"}, Hashes: []string{"a", "b"}, ContentPath: "/seed/Done", SizeBytes: 500, DeleteFiles: true, Seeded: 30 * 24 * time.Hour, Rule: "default: 1 days"}

	qb := newExecQbit(t, finished("a"), finished("b"))
	x, log := newRemover(qb)
	if err := x.Remove(r); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := strings.Join(qb.written(), "\n"); got != "delete?deleteFiles=true&hashes=a%7Cb" {
		t.Errorf("writes = %s", got)
	}
	if ev := log.Recent(1); len(ev) != 1 || ev[0].Action != history.DownloadRemoved || ev[0].SizeBytes != 500 || !strings.Contains(ev[0].Detail, "seeded 30 days") {
		t.Errorf("logged = %+v", ev)
	}

	// Files kept: the download leaves qBittorrent, its files stay.
	qb = newExecQbit(t, finished("a"), finished("b"))
	x, log = newRemover(qb)
	kept := r
	kept.DeleteFiles, kept.KeptFor = false, "Other"
	if err := x.Remove(kept); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(qb.written(), "\n"); got != "delete?deleteFiles=false&hashes=a%7Cb" {
		t.Errorf("writes = %s", got)
	}
	if ev := log.Recent(1); ev[0].Action != history.DownloadUnlisted || ev[0].SizeBytes != 0 || !strings.Contains(ev[0].Detail, "files kept for Other") {
		t.Errorf("logged = %+v", ev[0])
	}

	// Anything changed since the plan stops the removal, deleting nothing.
	tests := []struct {
		name    string
		edit    func(*execTorrent)
		wantErr string
	}{
		{name: "downloading again", edit: func(t *execTorrent) { t.State, t.Progress, t.AmountLeft = "downloading", 0.5, 1 }, wantErr: "isn't finished right now"},
		{name: "moved", edit: func(t *execTorrent) { t.ContentPath = "/elsewhere/Done" }, wantErr: "has moved since the plan"},
		{name: "seeding time reset", edit: func(t *execTorrent) { t.SeedingTime = 60 }, wantErr: "isn't done seeding any more"},
	}
	for _, tt := range tests {
		qb := newExecQbit(t, finished("a"), finished("b"))
		qb.set("b", tt.edit)
		x, _ := newRemover(qb)
		if err := x.Remove(r); err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "nothing was removed") {
			t.Errorf("%s: err = %v", tt.name, err)
		}
		if len(qb.written()) != 0 {
			t.Errorf("%s: wrote %v", tt.name, qb.written())
		}
	}

	gone := newExecQbit(t, finished("a"))
	x, _ = newRemover(gone)
	if err := x.Remove(r); err == nil || !strings.Contains(err.Error(), "no longer in qBittorrent") {
		t.Errorf("a member gone: %v", err)
	}

	// qBittorrent answers 200 to every delete, so the removal is checked.
	stuck := newExecQbit(t, finished("a"), finished("b"))
	stuck.ignoreDeletes = true
	x, _ = newRemover(stuck)
	if err := x.Remove(r); err == nil || !strings.Contains(err.Error(), "still lists Done after removing it") {
		t.Errorf("a delete that didn't happen: %v", err)
	}

	failing := newExecQbit(t, finished("a"), finished("b"))
	failing.failPath, failing.failCode = "/api/v2/torrents/delete", http.StatusInternalServerError
	x, _ = newRemover(failing)
	if err := x.Remove(r); err == nil || !strings.Contains(err.Error(), "removing Done") {
		t.Errorf("a failed delete: %v", err)
	}

	x.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	if err := x.Remove(r); err == nil || !strings.Contains(err.Error(), "before removing it") {
		t.Errorf("unreachable: %v", err)
	}
	if days(36*time.Hour) != "1.5 days" {
		t.Errorf("days(36h) = %q", days(36*time.Hour))
	}
}
