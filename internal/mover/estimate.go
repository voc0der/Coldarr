package mover

import (
	"math"
	"time"
)

// Estimate describes progress through the move plan, excluding any downstream
// library refresh. Percent is size-weighted work resolved (including failures),
// never a claim that an unconfirmed transfer has finished copying.
type Estimate struct {
	Percent   int
	Elapsed   time.Duration
	Remaining time.Duration
	Known     bool
	Overdue   bool
}

type movePace struct {
	bytes        float64
	sizedSeconds float64
	seconds      float64
	count        int
}

func (p *movePace) observe(size int64, started, finished time.Time) {
	seconds := finished.Sub(started).Seconds()
	p.seconds += seconds
	p.count++
	if size > 0 {
		p.bytes += float64(size)
		p.sizedSeconds += seconds
	}
}

func (p movePace) duration(size int64) float64 {
	if size > 0 && p.bytes > 0 {
		return float64(size) * p.sizedSeconds / p.bytes
	}
	// Missing sizes still take time; use observed time per item instead.
	if p.count > 0 {
		return p.seconds / float64(p.count)
	}
	return 0
}

// Estimate uses successful move sizes and durations from this run. A destination
// learns its own pace, falling back to the run's average until it has a sample.
// Serial queues add; parallel volumes take the longest queue; dependent phases
// add again. No disk reads or requests are made when the UI polls this snapshot.
func (s RunSnapshot[E]) Estimate(now time.Time) Estimate {
	end := now
	if s.Done {
		end = s.Finished
	}
	estimate := Estimate{}
	if !s.Started.IsZero() {
		estimate.Elapsed = max(0, end.Sub(s.Started))
	}
	if s.Done {
		estimate.Percent = 100
		estimate.Known = true
		return estimate
	}

	var overall movePace
	byVolume := map[string]*movePace{}
	var totalBytes float64
	var sizedCount int
	for _, e := range s.Entries {
		if size := e.Entry.MoveSize(); size > 0 {
			totalBytes += float64(size)
			sizedCount++
		}
		// Rejections and skipped prerequisites must not look like fast copies.
		if e.Status != StatusDone || e.Started.IsZero() || !e.Finished.After(e.Started) {
			continue
		}
		overall.observe(e.Entry.MoveSize(), e.Started, e.Finished)
		if byVolume[e.volume] == nil {
			byVolume[e.volume] = &movePace{}
		}
		byVolume[e.volume].observe(e.Entry.MoveSize(), e.Started, e.Finished)
	}

	// Assign unknown-size items the average planned size for percentage only;
	// an entirely unsized plan naturally falls back to item-count progress.
	unknownWeight := 1.0
	if sizedCount > 0 {
		unknownWeight = totalBytes / float64(sizedCount)
	}
	var totalWeight, resolvedWeight float64
	queues := map[int]map[string]float64{}
	estimate.Known = overall.count > 0
	for _, e := range s.Entries {
		weight := float64(e.Entry.MoveSize())
		if weight <= 0 {
			weight = unknownWeight
		}
		totalWeight += weight
		if e.Status == StatusDone || e.Status == StatusFailed {
			resolvedWeight += weight
			continue
		}

		pace := overall
		if local := byVolume[e.volume]; local != nil {
			pace = *local
		}
		seconds := pace.duration(e.Entry.MoveSize())
		if seconds <= 0 {
			estimate.Known = false
			continue
		}
		if e.Status == StatusMoving && !e.Started.IsZero() {
			seconds -= max(0, now.Sub(e.Started).Seconds())
			if seconds <= 0 {
				// A slow or stalled transfer has outlived the available evidence.
				// Wait for a fresh sample instead of promising zero seconds left.
				estimate.Overdue = true
				estimate.Known = false
			}
		}
		if queues[e.Entry.MovePhase()] == nil {
			queues[e.Entry.MovePhase()] = map[string]float64{}
		}
		queues[e.Entry.MovePhase()][e.volume] += max(0, seconds)
	}
	if totalWeight > 0 {
		estimate.Percent = min(99, int(100*resolvedWeight/totalWeight))
	}
	if estimate.Known {
		var remaining float64
		for _, volumes := range queues {
			var longest float64
			for _, seconds := range volumes {
				longest = max(longest, seconds)
			}
			remaining += longest
		}
		// Saturate before converting to nanoseconds, even for implausible sizes.
		estimate.Remaining = time.Duration(min(math.Ceil(remaining), float64(math.MaxInt64/int64(time.Second)))) * time.Second
	}
	return estimate
}
