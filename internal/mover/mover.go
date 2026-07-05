// Package mover executes a planner.Plan by asking Radarr/Sonarr to relocate
// items - Coldarr never touches media files directly, so the owning Arr
// app's database stays the source of truth.
package mover

import (
	"fmt"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/planner"
)

type Movers struct {
	Radarr  *arrapi.RadarrClient
	Sonarr  *arrapi.SonarrClient
	History *history.Store
}

type FailedMove struct {
	Entry planner.MoveEntry
	Err   error
}

type Result struct {
	Moved  []planner.MoveEntry
	Failed []FailedMove
}

type groupKey struct {
	arrApp string
	toPath string
}

// Apply groups plan entries by (owning app, destination path) and issues
// one bulk move call per group, so N items moving to the same satellite
// folder become one Radarr/Sonarr request instead of N. Each successful
// group is recorded to history immediately, so a failure partway through a
// large plan still leaves the cooldown ledger accurate for what did move.
func (m *Movers) Apply(plan *planner.Plan, now time.Time) (Result, error) {
	groups := map[groupKey][]planner.MoveEntry{}
	var order []groupKey
	for _, e := range plan.Entries {
		k := groupKey{arrApp: e.Item.ArrApp, toPath: e.ToPath}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}

	var result Result

	for _, k := range order {
		entries := groups[k]
		ids := make([]int, len(entries))
		for i, e := range entries {
			ids[i] = e.Item.ID
		}

		var err error
		switch k.arrApp {
		case "radarr":
			err = m.Radarr.MoveMovies(ids, k.toPath)
		case "sonarr":
			err = m.Sonarr.MoveSeries(ids, k.toPath)
		default:
			err = fmt.Errorf("unknown arr app %q", k.arrApp)
		}

		if err != nil {
			for _, e := range entries {
				result.Failed = append(result.Failed, FailedMove{Entry: e, Err: err})
			}
			continue
		}

		for _, e := range entries {
			result.Moved = append(result.Moved, e)
			histErr := m.History.Append(history.Record{
				ArrApp:    e.Item.ArrApp,
				ItemID:    e.Item.ID,
				Title:     e.Item.Title,
				FromTier:  e.FromTier,
				FromPath:  e.FromPath,
				ToTier:    e.ToTier,
				ToPath:    e.ToPath,
				SizeBytes: e.Item.SizeBytes,
				MovedAt:   now,
			})
			if histErr != nil {
				return result, fmt.Errorf("recording move history for %q: %w (move already happened in %s - fix history file manually)", e.Item.Title, histErr, k.arrApp)
			}
		}
	}

	return result, nil
}
