package torrents

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// fakeFS answers Exists from a set of paths, or fails for those in errs.
type fakeFS struct {
	exists map[string]bool
	errs   map[string]error
}

func (f fakeFS) Exists(path string) (bool, error) {
	if err := f.errs[path]; err != nil {
		return false, err
	}
	return f.exists[path], nil
}

// everything is an FS where every path exists.
type everything struct{}

func (everything) Exists(string) (bool, error) { return true, nil }

func testTiers() []model.Tier {
	return []model.Tier{
		{Name: "library", Role: model.RoleHot, Paths: []string{"/media"}, Media: []model.MediaType{model.Movie}},
		{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{"/downloads"}},
		{Name: "seed", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{"/seed"}, TargetUsedPercent: 92, MaxUsedPercent: 97},
	}
}

func testConfig() config.TorrentsConfig {
	return config.TorrentsConfig{SettleDays: 3, SeedLabel: "seed", Labels: []config.LabelRule{
		{Label: "movies", Action: config.LabelMove, ImportedBy: "radarr"},
		{Label: "tv", Action: config.LabelMove, ImportedBy: "sonarr"},
		{Label: "manual", Action: config.LabelMove},
		{Label: "transfer", Action: config.LabelIgnore},
	}}
}

// settled is a finished, seeding download that completed long enough ago
// to move.
func settled(hash, name, category string) qbit.Torrent {
	return qbit.Torrent{
		Hash: hash, Name: name, State: "stalledUP", Category: category,
		SavePath: "/downloads", ContentPath: "/downloads/" + name,
		CompletedOn: now.Add(-5 * 24 * time.Hour), Progress: 1, Size: 10 << 30,
		SeedingTime: 5 * 24 * time.Hour, SeedingTimeKnown: true,
	}
}

func input(torrents ...qbit.Torrent) Input {
	return Input{
		Torrents:  torrents,
		Tiers:     testTiers(),
		Config:    testConfig(),
		Queued:    map[string]string{},
		Imports:   map[ImportKey]ImportResult{},
		Connected: map[string]bool{"radarr": true, "sonarr": true},
		FS:        everything{},
		Now:       now,
	}
}

func byHash(t *testing.T, downloads []*Download, hash string) *Download {
	t.Helper()
	for _, d := range downloads {
		if d.Hash == hash {
			return d
		}
	}
	t.Fatalf("no download %q", hash)
	return nil
}

func TestClassify_States(t *testing.T) {
	imported := func(in *Input, app, hash string) {
		in.Imports[ImportKey{App: app, Hash: strings.ToUpper(hash)}] = ImportResult{Imported: true}
	}
	tests := []struct {
		name       string
		torrent    qbit.Torrent
		setup      func(*Input)
		wantState  State
		wantReason string
	}{
		{name: "manual label, settled", torrent: settled("a", "Linux.ISO", "manual"), wantState: Ready},
		{name: "outside every downloads tier", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.SavePath = "/elsewhere"; return t }(), wantState: Ignored, wantReason: "not in a downloads tier"},
		{name: "library tier path is not a downloads tier", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.SavePath = "/media"; return t }(), wantState: Ignored, wantReason: "not in a downloads tier"},
		{name: "no labels", torrent: settled("a", "X", ""), wantState: Ignored, wantReason: "no category or tag"},
		{name: "unmapped label fails closed", torrent: settled("a", "X", "books"), wantState: Ignored, wantReason: `no rule for "books"`},
		{name: "ignore wins over move", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.Tags = []string{"transfer"}; return t }(), wantState: Ignored, wantReason: `labeled "transfer"`},
		{name: "a move tag manages an unmapped category", torrent: func() qbit.Torrent { t := settled("a", "X", "books"); t.Tags = []string{"manual"}; return t }(), wantState: Ready},
		{name: "downloading", torrent: func() qbit.Torrent {
			t := settled("a", "X", "manual")
			t.State, t.Progress, t.AmountLeft = "downloading", 0.4, 100
			return t
		}(), wantState: Active, wantReason: "downloading, 40% done"},
		{name: "moving", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.State = "moving"; return t }(), wantState: Active, wantReason: "qBittorrent is moving it"},
		{name: "checking", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.State = "checkingUP"; return t }(), wantState: Active, wantReason: "checking"},
		{name: "allocating", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.State = "allocating"; return t }(), wantState: Active, wantReason: "allocating"},
		{name: "missing files", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.State = "missingFiles"; return t }(), wantState: Active, wantReason: "missingFiles"},
		{name: "a state from a newer qBittorrent", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.State = "hibernatingUP"; return t }(), wantState: Active, wantReason: `doesn't recognize ("hibernatingUP")`},
		{name: "paused complete downloads still move", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.State = "stoppedUP"; return t }(), wantState: Ready},
		{name: "in an arr queue", torrent: settled("abc", "X", "movies"), setup: func(in *Input) { in.Queued["ABC"] = "radarr" }, wantState: Active, wantReason: "still in Radarr's queue"},
		{name: "settling", torrent: func() qbit.Torrent {
			t := settled("a", "X", "manual")
			t.CompletedOn = now.Add(-24 * time.Hour)
			return t
		}(), wantState: Waiting},
		{name: "settling from seeding time when completion is unknown", torrent: func() qbit.Torrent {
			t := settled("a", "X", "manual")
			t.CompletedOn, t.SeedingTime = time.Time{}, time.Hour
			return t
		}(), wantState: Waiting},
		{name: "no clock at all", torrent: func() qbit.Torrent {
			t := settled("a", "X", "manual")
			t.CompletedOn, t.SeedingTimeKnown = time.Time{}, false
			return t
		}(), wantState: Held, wantReason: "doesn't say when it finished"},
		{name: "import not checked yet", torrent: settled("abc", "X", "movies"), wantState: Held, wantReason: "not yet checked whether Radarr imported it"},
		{name: "imported", torrent: settled("abc", "X", "movies"), setup: func(in *Input) { imported(in, "radarr", "abc") }, wantState: Ready},
		{name: "not imported", torrent: settled("abc", "X", "movies"), setup: func(in *Input) { in.Imports[ImportKey{App: "radarr", Hash: "ABC"}] = ImportResult{} }, wantState: Held, wantReason: "Radarr hasn't imported it yet"},
		{name: "import check failed", torrent: settled("abc", "X", "movies"), setup: func(in *Input) { in.Imports[ImportKey{App: "radarr", Hash: "ABC"}] = ImportResult{Err: "boom"} }, wantState: Held, wantReason: "couldn't ask Radarr whether it imported this download: boom"},
		{name: "importer not connected", torrent: settled("abc", "X", "movies"), setup: func(in *Input) { in.Connected = map[string]bool{} }, wantState: Held, wantReason: "Radarr isn't connected"},
		{name: "two importers both needed", torrent: func() qbit.Torrent { t := settled("abc", "X", "movies"); t.Tags = []string{"tv"}; return t }(), setup: func(in *Input) { imported(in, "radarr", "abc") }, wantState: Held, wantReason: "whether Sonarr imported it"},
		{name: "AutoTMM refused", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.AutoTMM = true; return t }(), wantState: Held, wantReason: "Automatic Torrent Management is on"},
		{name: "AutoTMM allowed", torrent: func() qbit.Torrent { t := settled("a", "X", "manual"); t.AutoTMM = true; return t }(), setup: func(in *Input) { in.Config.AllowDisablingAutoTMM = true }, wantState: Ready},
		{name: "not visible to Coldarr", torrent: settled("a", "X", "manual"), setup: func(in *Input) { in.FS = fakeFS{} }, wantState: Held, wantReason: "Coldarr can't see it at /downloads/X - is /downloads mounted into Coldarr"},
		{name: "visibility check failed", torrent: settled("a", "X", "manual"), setup: func(in *Input) {
			in.FS = fakeFS{errs: map[string]error{"/downloads/X": errors.New("permission denied")}}
		}, wantState: Held, wantReason: "checking /downloads/X: permission denied"},
		{name: "on seed storage", torrent: func() qbit.Torrent {
			t := settled("a", "X", "seed")
			t.SavePath, t.ContentPath = "/seed", "/seed/X"
			return t
		}(), wantState: Parked},
		{name: "seed label is managed without a rule", torrent: func() qbit.Torrent { t := settled("a", "X", "seed"); return t }(), wantState: Ready},
		{name: "a category subfolder is still on its tier", torrent: func() qbit.Torrent {
			t := settled("a", "X", "manual")
			t.SavePath, t.ContentPath = "/downloads/manual", "/downloads/manual/X"
			return t
		}(), wantState: Ready},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(tt.torrent)
			if tt.setup != nil {
				tt.setup(&in)
			}
			got := Classify(in)[0]
			if got.State != tt.wantState || !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("state = %q (%q), want %q with a reason containing %q", got.State, got.Reason, tt.wantState, tt.wantReason)
			}
		})
	}
}

func TestClassify_ImportChecksRoundTrip(t *testing.T) {
	in := input(settled("abc", "Movie", "movies"), settled("def", "Show", "tv"), settled("ghi", "ISO", "manual"))
	in.Connected["lidarr"] = true

	first := Classify(in)
	keys := ImportChecksNeeded(first)
	if len(keys) != 2 || keys[0] != (ImportKey{App: "radarr", Hash: "ABC"}) || keys[1] != (ImportKey{App: "sonarr", Hash: "DEF"}) {
		t.Fatalf("ImportChecksNeeded = %+v, want Radarr's and Sonarr's, upper-cased", keys)
	}
	if byHash(t, first, "ghi").State != Ready {
		t.Error("a download no app imports needs no check")
	}

	in.Imports[keys[0]] = ImportResult{Imported: true}
	in.Imports[keys[1]] = ImportResult{Imported: true}
	second := Classify(in)
	if len(ImportChecksNeeded(second)) != 0 {
		t.Error("nothing should be left to check")
	}
	for _, d := range second {
		if d.State != Ready {
			t.Errorf("%s = %s (%s), want ready once imported", d.Name, d.State, d.Reason)
		}
	}
}

func TestClassify_Relabel(t *testing.T) {
	onSeed := func(hash, category string) qbit.Torrent {
		t := settled(hash, "X-"+hash, category)
		t.SavePath, t.ContentPath = "/seed", "/seed/X-"+hash
		return t
	}
	autoTMM := onSeed("d", "manual")
	autoTMM.AutoTMM = true
	in := input(onSeed("a", "seed"), onSeed("b", "manual"), onSeed("c", "movies"), autoTMM)
	got := Classify(in)

	if d := byHash(t, got, "a"); d.Relabel {
		t.Error("a download already labeled seed needs no relabel")
	}
	if d := byHash(t, got, "b"); !d.Relabel || d.State != Parked {
		t.Errorf("b = %+v, want parked and due a relabel", d)
	}
	if d := byHash(t, got, "c"); d.Relabel || !strings.Contains(d.Reason, "not relabeled yet") {
		t.Errorf("c = %q %q, want its relabel waiting on Radarr's import", d.State, d.Reason)
	}
	if d := byHash(t, got, "d"); d.Relabel || !strings.Contains(d.Reason, "Automatic Torrent Management") {
		t.Errorf("d = %q, want an AutoTMM download not relabeled - the category change would move it", d.Reason)
	}
	if keys := ImportChecksNeeded(got); len(keys) != 1 || keys[0].Hash != "C" {
		t.Errorf("ImportChecksNeeded = %+v, want the relabel's import check", keys)
	}
}

// TestClassify_CrossSeeds: downloads sharing a content path move together
// or not at all, and a download nested inside another's folder can't be
// moved safely either way.
func TestClassify_CrossSeeds(t *testing.T) {
	a := settled("a", "Show.S01", "manual")
	b := settled("b", "Show.S01", "manual") // cross-seed of a, same files
	waitingTwin := settled("c", "Film", "manual")
	waitingTwin2 := settled("d", "Film", "manual")
	waitingTwin2.CompletedOn = now.Add(-time.Hour)
	ignoredTwin := settled("e", "Album", "manual")
	ignoredTwin2 := settled("f", "Album", "transfer")
	nestedOuter := settled("g", "Pack", "manual")
	nestedInner := settled("h", "Episode", "manual")
	nestedInner.SavePath, nestedInner.ContentPath = "/downloads/Pack", "/downloads/Pack/Episode.mkv"
	sibling := settled("i", "Pack.REPACK", "manual") // shares a name prefix, not files
	downloadingTwin := settled("j", "Doc", "manual")
	downloadingTwin2 := settled("k", "Doc", "manual") // added since, still fetching its share
	downloadingTwin2.State, downloadingTwin2.Progress = "downloading", 0.5

	got := Classify(input(a, b, waitingTwin, waitingTwin2, ignoredTwin, ignoredTwin2, nestedOuter, nestedInner, sibling, downloadingTwin, downloadingTwin2))

	for _, h := range []string{"a", "b", "i"} {
		if d := byHash(t, got, h); d.State != Ready {
			t.Errorf("%s = %s (%s), want ready", h, d.State, d.Reason)
		}
	}
	if d := byHash(t, got, "c"); d.State != Held || !strings.Contains(d.Reason, "cross-seeded with Film, which is still settling") {
		t.Errorf("c = %s (%s), want held for its settling twin", d.State, d.Reason)
	}
	if k := byHash(t, got, "k"); k.State != Active {
		t.Errorf("k = %s, want active", k.State)
	} else if d := byHash(t, got, "j"); d.State != Held || !strings.Contains(d.Reason, "cross-seeded with Doc, which is "+k.Reason) {
		t.Errorf("j = %s (%s), want held for its twin, saying why (%s)", d.State, d.Reason, k.Reason)
	}
	if d := byHash(t, got, "e"); d.State != Held || !strings.Contains(d.Reason, "left alone") {
		t.Errorf("e = %s (%s), want held: its twin is one Coldarr must not touch", d.State, d.Reason)
	}
	if d := byHash(t, got, "g"); d.State != Held || !strings.Contains(d.Reason, "Episode, which sits inside its folder") {
		t.Errorf("g = %s (%s), want held for the download nested inside it", d.State, d.Reason)
	}
	if d := byHash(t, got, "h"); d.State != Held || !strings.Contains(d.Reason, "Pack, whose folder holds them") {
		t.Errorf("h = %s (%s), want held for the folder it sits in", d.State, d.Reason)
	}
}

func TestClassify_SortedByName(t *testing.T) {
	got := Classify(input(settled("1", "beta", "manual"), settled("2", "Alpha", "manual"), settled("3", "alpha", "manual")))
	if got[0].Hash != "2" || got[1].Hash != "3" || got[2].Name != "beta" {
		t.Errorf("order = %s %s %s, want case-insensitive names, ties by hash", got[0].Hash, got[1].Hash, got[2].Hash)
	}
	if got[0].Managed() != true {
		t.Error("a ready download is managed")
	}
}

func TestLocate_DeepestPathWins(t *testing.T) {
	tiers := []model.Tier{
		{Name: "outer", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{"/data"}},
		{Name: "inner", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{"/data/seed/"}},
	}
	if tier, path, ok := locate("/data/seed/movies", tiers); !ok || tier.Name != "inner" || path != "/data/seed" {
		t.Errorf("locate = %s %s %v, want the inner tier", tier.Name, path, ok)
	}
	if _, _, ok := locate("/database", tiers); ok {
		t.Error("a path merely starting with a tier path's characters is not on it")
	}
	if _, _, ok := locate("", tiers); ok {
		t.Error("no save path, no tier")
	}
}
