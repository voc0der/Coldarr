// Package scoring implements Coldarr's policy engine: given a media item
// and the configured policy, decide whether it must stay put (Protected),
// should stay on hot storage (Hot), or is safe to relocate to cold storage
// (Cold), along with a human-readable trail of why.
package scoring

import (
	"fmt"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
)

type Decision string

const (
	Protected Decision = "protected"
	Hot       Decision = "hot"
	Cold      Decision = "cold"
)

type Evaluation struct {
	Decision Decision
	// Score is only meaningful for Cold items: higher means colder
	// (safer/better to move first). It has no fixed unit - it's a
	// relative ranking, not a percentage.
	Score   float64
	Reasons []string
}

const (
	dayHours = 24.0

	scoreEnded        = 30.0
	scoreColdOkTag    = 60.0
	scoreLowPriority  = 15.0
	scoreUnmonitored  = 10.0
	maxAgeScore       = 30.0
	ageScorePerMonth  = 5.0
	maxAiredScore     = 40.0
	airedScorePerWeek = 4.0
	maxSizeScore      = 20.0
	sizeScorePer10GB  = 2.0
)

// Evaluate scores a single item against policy. now is passed in explicitly
// so scoring is deterministic and testable.
func Evaluate(item model.MediaItem, policy config.PolicyConfig, now time.Time) Evaluation {
	if hasAnyTag(item.Tags, policy.NeverMoveTags) {
		return Evaluation{Decision: Protected, Reasons: []string{"tagged never-move"}}
	}
	if hasAnyTag(item.Tags, policy.ProtectedTags) {
		return Evaluation{Decision: Protected, Reasons: []string{"tagged protected/keep-hot"}}
	}
	if item.InActiveQueue {
		return Evaluation{Decision: Protected, Reasons: []string{"active download/import in progress"}}
	}
	if item.JellyfinFavorite {
		return Evaluation{Decision: Protected, Reasons: []string{"marked Favorite in Jellyfin"}}
	}

	age := now.Sub(item.Added)
	graceCutoff := time.Duration(policy.HotGraceDays) * dayHours * time.Hour
	if age < graceCutoff {
		return Evaluation{Decision: Hot, Reasons: []string{fmt.Sprintf("added within the last %d days (grace period)", policy.HotGraceDays)}}
	}

	if item.Type == model.TV && !item.Ended && policy.ProtectContinuingSeries {
		return Evaluation{Decision: Hot, Reasons: []string{"series is continuing/currently airing"}}
	}

	var score float64
	var reasons []string

	if hasAnyTag(item.Tags, policy.ColdOkTags) {
		score += scoreColdOkTag
		reasons = append(reasons, "tagged cold-ok")
	}

	if item.Type == model.TV && item.Ended {
		score += scoreEnded
		reasons = append(reasons, "series has ended")
	}

	if item.LastAired != nil {
		weeksSinceAired := now.Sub(*item.LastAired).Hours() / (dayHours * 7)
		if weeksSinceAired > 0 {
			bonus := min(weeksSinceAired*airedScorePerWeek, maxAiredScore)
			score += bonus
			reasons = append(reasons, fmt.Sprintf("last aired %.0f weeks ago", weeksSinceAired))
		}
	}

	ageMonths := age.Hours() / (dayHours * 30)
	ageBonus := min(ageMonths*ageScorePerMonth, maxAgeScore)
	score += ageBonus
	reasons = append(reasons, fmt.Sprintf("added %.1f months ago", ageMonths))

	sizeGB := float64(item.SizeBytes) / (1 << 30)
	sizeBonus := min((sizeGB/10)*sizeScorePer10GB, maxSizeScore)
	score += sizeBonus
	if sizeGB >= 1 {
		reasons = append(reasons, fmt.Sprintf("%.1f GB on disk", sizeGB))
	}

	if isLowPriorityProfile(item.QualityProfileName, policy.LowPriorityQualityProfiles) {
		score += scoreLowPriority
		reasons = append(reasons, fmt.Sprintf("low-priority quality profile %q", item.QualityProfileName))
	}

	if !item.Monitored || !item.HasFile {
		score += scoreUnmonitored
		reasons = append(reasons, "unmonitored or missing file")
	}

	if score >= policy.ColdScoreThreshold {
		return Evaluation{Decision: Cold, Score: score, Reasons: reasons}
	}

	reasons = append(reasons, fmt.Sprintf("score %.1f below cold threshold %.1f", score, policy.ColdScoreThreshold))
	return Evaluation{Decision: Hot, Score: score, Reasons: reasons}
}

func hasAnyTag(itemTags, watchTags []string) bool {
	for _, t := range itemTags {
		for _, w := range watchTags {
			if t == w {
				return true
			}
		}
	}
	return false
}

func isLowPriorityProfile(profile string, lowPriority []string) bool {
	for _, p := range lowPriority {
		if p == profile {
			return true
		}
	}
	return false
}
