package scoring

import (
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
)

func basePolicy() config.PolicyConfig {
	return config.PolicyConfig{
		CooldownDays:               30,
		HotGraceDays:               14,
		ProtectedTags:              []string{"keep-hot"},
		ColdOkTags:                 []string{"cold-ok"},
		NeverMoveTags:              []string{"never-move"},
		ProtectContinuingSeries:    true,
		LowPriorityQualityProfiles: []string{"SD"},
		ColdScoreThreshold:         40,
	}
}

func TestEvaluate_NeverMoveTagWins(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:  model.Movie,
		Tags:  []string{"never-move"},
		Added: now.AddDate(-5, 0, 0),
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Protected {
		t.Fatalf("expected Protected, got %v", eval.Decision)
	}
}

func TestEvaluate_ActiveQueueIsProtected(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:          model.Movie,
		Added:         now.AddDate(-5, 0, 0),
		InActiveQueue: true,
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Protected {
		t.Fatalf("expected Protected for active queue item, got %v", eval.Decision)
	}
}

func TestEvaluate_RecentlyAddedStaysHot(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:      model.Movie,
		Added:     now.AddDate(0, 0, -1),
		SizeBytes: 100 << 30,
		Tags:      []string{"cold-ok"},
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Hot {
		t.Fatalf("expected Hot for item within grace period, got %v", eval.Decision)
	}
}

func TestEvaluate_ContinuingSeriesStaysHot(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:      model.TV,
		Added:     now.AddDate(-5, 0, 0),
		Ended:     false,
		SizeBytes: 500 << 30,
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Hot {
		t.Fatalf("expected Hot for continuing series, got %v (score %.1f)", eval.Decision, eval.Score)
	}
}

func TestEvaluate_UpcomingStaysHot(t *testing.T) {
	now := time.Now()
	for _, mt := range []model.MediaType{model.Movie, model.TV} {
		item := model.MediaItem{
			Type:      mt,
			Added:     now.AddDate(-5, 0, 0), // old enough to clear grace period
			Tags:      []string{"cold-ok"},   // and tagged cold-ok
			SizeBytes: 500 << 30,             // and big - would otherwise easily clear the threshold
			Upcoming:  true,
		}
		eval := Evaluate(item, basePolicy(), now)
		if eval.Decision != Hot {
			t.Fatalf("%s: expected Hot for an upcoming item despite old age/cold-ok tag/size, got %v (score %.1f)", mt, eval.Decision, eval.Score)
		}
	}
}

func TestEvaluate_QualityCutoffNotMetStaysHot(t *testing.T) {
	now := time.Now()
	for _, mt := range []model.MediaType{model.Movie, model.TV} {
		item := model.MediaItem{
			Type:                mt,
			Added:               now.AddDate(-5, 0, 0), // old enough to clear grace period
			Tags:                []string{"cold-ok"},   // and tagged cold-ok
			SizeBytes:           500 << 30,             // and big - would otherwise easily clear the threshold
			Monitored:           true,
			HasFile:             true,
			QualityCutoffNotMet: true,
		}
		eval := Evaluate(item, basePolicy(), now)
		if eval.Decision != Hot {
			t.Fatalf("%s: expected Hot for a cutoff-not-met item despite old age/cold-ok tag/size, got %v (score %.1f)", mt, eval.Decision, eval.Score)
		}
	}
}

func TestEvaluate_QualityCutoffNotMetIgnoredWhenUnmonitored(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:                model.Movie,
		Added:               now.AddDate(-5, 0, 0),
		Tags:                []string{"cold-ok"},
		SizeBytes:           500 << 30,
		Monitored:           false,
		HasFile:             true,
		QualityCutoffNotMet: true,
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Cold {
		t.Fatalf("expected Cold for an unmonitored item (cutoff-not-met doesn't matter - it won't be searched), got %v (score %.1f, reasons %v)", eval.Decision, eval.Score, eval.Reasons)
	}
}

func TestEvaluate_ColdOkTagPushesToCold(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:      model.Movie,
		Added:     now.AddDate(-1, 0, 0),
		SizeBytes: 20 << 30,
		Tags:      []string{"cold-ok"},
		Monitored: true,
		HasFile:   true,
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Cold {
		t.Fatalf("expected Cold for cold-ok tagged item, got %v (score %.1f, reasons %v)", eval.Decision, eval.Score, eval.Reasons)
	}
}

func TestEvaluate_EndedOldSeriesGoesCold(t *testing.T) {
	now := time.Now()
	lastAired := now.AddDate(-2, 0, 0)
	item := model.MediaItem{
		Type:      model.TV,
		Added:     now.AddDate(-3, 0, 0),
		Ended:     true,
		LastAired: &lastAired,
		SizeBytes: 300 << 30,
		Monitored: true,
		HasFile:   true,
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Cold {
		t.Fatalf("expected Cold for old ended series, got %v (score %.1f, reasons %v)", eval.Decision, eval.Score, eval.Reasons)
	}
}

func TestEvaluate_SmallRecentUnremarkableMovieStaysHot(t *testing.T) {
	now := time.Now()
	item := model.MediaItem{
		Type:      model.Movie,
		Added:     now.AddDate(0, -1, 0),
		SizeBytes: 2 << 30,
		Monitored: true,
		HasFile:   true,
	}
	eval := Evaluate(item, basePolicy(), now)
	if eval.Decision != Hot {
		t.Fatalf("expected Hot for unremarkable recent movie, got %v (score %.1f)", eval.Decision, eval.Score)
	}
}
