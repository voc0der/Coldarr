package torrents

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
)

// planFS answers Exists and HasHardLinks from sets of paths.
type planFS struct {
	exists map[string]bool
	linked map[string]bool
	err    error
}

func (f planFS) Exists(path string) (bool, error) { return f.exists[path], f.err }
func (f planFS) HasHardLinks(path string) (bool, error) {
	return f.linked[path], nil
}

func ready(hash, name, savePath string, size int64, finishedAgo time.Duration) *Download {
	return &Download{
		Torrent: qbit.Torrent{
			Hash: hash, Name: name, State: "stalledUP", SavePath: savePath, ContentPath: savePath + "/" + name,
			Size: size, Progress: 1, CompletedOn: now.Add(-finishedAgo),
		},
		Tier:     model.Tier{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot},
		TierPath: "/downloads",
		State:    Ready,
		Group:    savePath + "/" + name,
	}
}

func planTiers() []model.Tier {
	return []model.Tier{
		{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{"/downloads"}},
		{Name: "seed", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{"/seed1/", "/seed2"}, TargetUsedPercent: 80, MaxUsedPercent: 90},
		{Name: "library", Role: model.RoleCold, Paths: []string{"/library"}, Media: []model.MediaType{model.Movie}, TargetUsedPercent: 99, MaxUsedPercent: 100},
	}
}

func usage(used, total uint64) diskusage.Usage {
	return diskusage.Usage{TotalBytes: total, UsedBytes: used, FreeBytes: total - used, UsedPercent: diskusage.PercentUsed(used, total-used)}
}

func TestBuildPlan_OldestFirstOntoTheFullestSeedPathUnderTarget(t *testing.T) {
	downloads := []*Download{
		ready("n", "Newer", "/downloads", 10, 4*24*time.Hour),
		ready("o", "Older", "/downloads", 10, 9*24*time.Hour),
		ready("b", "Older.Bigger", "/downloads", 20, 9*24*time.Hour),
		{Torrent: qbit.Torrent{Hash: "w", Name: "Waiting"}, State: Waiting},
	}
	downloads[1].CompletedOn = downloads[2].CompletedOn // equal finish times: bigger first
	plan := BuildPlan(PlanInput{
		Downloads: downloads,
		Tiers:     planTiers(),
		Usage: map[string]diskusage.Usage{
			"/downloads": usage(500, 1000),
			"/seed1/":    usage(700, 1000), // fuller: preferred while under target
			"/seed2":     usage(100, 1000),
			"/library":   usage(0, 1000), // a library cold tier is never a destination
		},
		FS: planFS{},
	})

	var got []string
	for _, e := range plan.Entries {
		got = append(got, e.Name+"->"+e.ToPath)
	}
	if want := "Older.Bigger->/seed1,Older->/seed1,Newer->/seed1"; strings.Join(got, ",") != want {
		t.Fatalf("entries = %s, want %s", strings.Join(got, ","), want)
	}
	if e := plan.Entries[0]; e.Location != "/seed1" || e.NewContentPath != "/seed1/Older.Bigger" || e.FromPath != "/downloads" || e.ToTier != "seed" || e.MaxUsedPercent != 90 {
		t.Errorf("first entry = %+v", e)
	}
	if u := plan.FinalUsage["/seed1"]; u.UsedBytes != 740 {
		t.Errorf("projected /seed1 used = %d, want 740", u.UsedBytes)
	}
	if u := plan.FinalUsage["/downloads"]; u.UsedBytes != 460 {
		t.Errorf("projected /downloads used = %d, want 460", u.UsedBytes)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestBuildPlan_FallsBackToMaxAndWarnsWhenNothingFits(t *testing.T) {
	plan := BuildPlan(PlanInput{
		Downloads: []*Download{ready("a", "Fits.Under.Max", "/downloads", 50, 5*24*time.Hour), ready("b", "Too.Big", "/downloads", 200, 4*24*time.Hour)},
		Tiers:     planTiers(),
		Usage:     map[string]diskusage.Usage{"/seed2": usage(820, 1000)},
		FS:        planFS{},
	})
	if len(plan.Entries) != 1 || plan.Entries[0].Name != "Fits.Under.Max" {
		t.Fatalf("entries = %+v, want only the one that fits under max", plan.Entries)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "no seed storage has room for Too.Big") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

// TestBuildPlan_KeepsSubfoldersAndNeverLandsOnSomethingElse: a download in
// a category folder lands in the same folder on seed storage, and one
// whose landing spot is already taken goes to the next seed path - or
// nowhere, with a warning, if every spot is taken.
func TestBuildPlan_KeepsSubfoldersAndNeverLandsOnSomethingElse(t *testing.T) {
	cat := ready("c", "Show.S01", "/downloads/tv", 10, 5*24*time.Hour)
	taken := ready("t", "Taken", "/downloads", 10, 5*24*time.Hour)
	blocked := ready("x", "Blocked", "/downloads", 10, 5*24*time.Hour)
	plan := BuildPlan(PlanInput{
		Downloads: []*Download{cat, taken, blocked},
		Tiers:     planTiers(),
		Usage:     map[string]diskusage.Usage{"/seed1": usage(500, 1000), "/seed2": usage(100, 1000)},
		FS: planFS{exists: map[string]bool{
			"/seed1/Taken":   true,
			"/seed1/Blocked": true,
			"/seed2/Blocked": true,
		}},
	})
	byName := map[string]MoveEntry{}
	for _, e := range plan.Entries {
		byName[e.Name] = e
	}
	if e := byName["Show.S01"]; e.Location != "/seed1/tv" || e.NewContentPath != "/seed1/tv/Show.S01" {
		t.Errorf("category download = %+v, want it kept in its tv folder", e)
	}
	if e := byName["Taken"]; e.ToPath != "/seed2" {
		t.Errorf("Taken = %+v, want the next seed path, since /seed1 already has one", e)
	}
	if _, ok := byName["Blocked"]; ok || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "already exists at /seed2/Blocked") {
		t.Errorf("Blocked should stay put with a warning; warnings = %v", plan.Warnings)
	}

	// A disk error looking is treated like an existing file: no move.
	plan = BuildPlan(PlanInput{Downloads: []*Download{ready("a", "A", "/downloads", 10, 5*24*time.Hour)}, Tiers: planTiers(), Usage: map[string]diskusage.Usage{"/seed2": usage(100, 1000)}, FS: planFS{err: errors.New("io")}})
	if len(plan.Entries) != 0 {
		t.Error("a destination Coldarr can't check is never moved onto")
	}
}

func TestBuildPlan_CrossSeedsMoveAsOne(t *testing.T) {
	a := ready("a", "Album", "/downloads", 100, 5*24*time.Hour)
	b := ready("b", "Album", "/downloads", 120, 6*24*time.Hour)
	b.DisableAutoTMM = true
	plan := BuildPlan(PlanInput{Downloads: []*Download{a, b}, Tiers: planTiers(), Usage: map[string]diskusage.Usage{"/seed2": usage(0, 1000)}, FS: planFS{}})
	if len(plan.Entries) != 1 {
		t.Fatalf("entries = %+v, want one move for the pair", plan.Entries)
	}
	e := plan.Entries[0]
	if strings.Join(e.Hashes, ",") != "a,b" || e.SizeBytes != 120 || strings.Join(e.DisableAutoTMM, ",") != "b" || !e.CompletedAt.Equal(a.CompletedOn) {
		t.Errorf("entry = %+v, want both hashes, the larger size, b's AutoTMM switched off, and the later finish", e)
	}
	if e.MoveTitle() != "Album" || e.MoveSize() != 120 || e.MoveDestination() != "/seed2" || e.MovePhase() != 0 || e.MoveCeiling() != 90 {
		t.Error("the entry describes itself wrongly to the move engine")
	}
}

func TestBuildPlan_WarningsAndExclusions(t *testing.T) {
	linked := ready("l", "Linked", "/downloads", 10, 5*24*time.Hour)
	odd := ready("o", "Odd", "/downloads", 10, 5*24*time.Hour)
	odd.ContentPath = "/elsewhere/Odd"
	parked := &Download{Torrent: qbit.Torrent{Hash: "p", Name: "Parked", Category: "movies", ContentPath: "/seed1/Parked"}, State: Parked, Relabel: true, DisableAutoTMM: true}

	plan := BuildPlan(PlanInput{
		Downloads: []*Download{linked, odd, parked},
		Tiers:     planTiers(),
		Usage:     map[string]diskusage.Usage{"/seed1": usage(0, 1000), "/seed2": usage(0, 1000)},
		Unseen:    map[string]bool{"/seed1": true},
		FS:        planFS{linked: map[string]bool{"/downloads/Linked": true}},
	})
	if len(plan.Entries) != 1 || plan.Entries[0].ToPath != "/seed2" {
		t.Fatalf("entries = %+v, want Linked moved, and never to the unseen /seed1", plan.Entries)
	}
	joined := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(joined, "Linked has hard links") || !strings.Contains(joined, "Odd: its content (/elsewhere/Odd) isn't inside its save path") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	if len(plan.Relabels) != 1 || plan.Relabels[0] != (Relabel{Name: "Parked", Hash: "p", Category: "movies", ContentPath: "/seed1/Parked", DisableAutoTMM: true}) {
		t.Errorf("relabels = %+v", plan.Relabels)
	}
}

func TestFits(t *testing.T) {
	if fits(diskusage.Usage{}, 1, 100) {
		t.Error("an unmeasured path fits nothing")
	}
	if fits(usage(0, 100), -1, 100) || fits(usage(95, 100), 10, 100) {
		t.Error("a negative size, or one bigger than the free space, doesn't fit")
	}
	if !fits(usage(50, 100), 30, 80) || fits(usage(50, 100), 31, 80) {
		t.Error("the ceiling is inclusive and never crossed")
	}
}

// TestBuildPlan_LeavesWhatsAboutToBeRemoved: with removal on, a download
// done seeding isn't copied to seed storage only to be deleted - unless a
// cross-seed sharing its files still has to move, which takes it along.
func TestBuildPlan_LeavesWhatsAboutToBeRemoved(t *testing.T) {
	done := ready("d", "Done", "/downloads", 10, 30*24*time.Hour)
	pairDone := ready("p1", "Pair", "/downloads", 10, 30*24*time.Hour)
	pairNot := ready("p2", "Pair", "/downloads", 10, 30*24*time.Hour)
	plan := BuildPlan(PlanInput{
		Downloads: []*Download{done, pairDone, pairNot, ready("k", "Keep", "/downloads", 10, 30*24*time.Hour)},
		Tiers:     planTiers(),
		Usage:     map[string]diskusage.Usage{"/seed2": usage(0, 1000)},
		Leave:     map[string]bool{"d": true, "p1": true},
		FS:        planFS{},
	})
	var moved []string
	for _, e := range plan.Entries {
		moved = append(moved, e.Name)
	}
	if strings.Join(moved, ",") != "Keep,Pair" {
		t.Errorf("moved = %v, want Done left for removal and the pair moved together", moved)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "1 download(s) on the download drive are done seeding") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}
