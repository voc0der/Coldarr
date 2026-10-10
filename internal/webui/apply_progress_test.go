package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/planner"
)

func TestApplyProgress(t *testing.T) {
	now := time.Now()
	snap := mover.ProgressSnapshot{
		Started: now.Add(-6 * time.Minute),
		Entries: []mover.EntryProgress{
			{Entry: planner.MoveEntry{Item: model.MediaItem{SizeBytes: 50 << 30}}, Status: mover.StatusDone, Started: now.Add(-6 * time.Minute), Finished: now.Add(-time.Minute)},
			{Entry: planner.MoveEntry{Item: model.MediaItem{SizeBytes: 50 << 30}}, Status: mover.StatusMoving, Started: now.Add(-time.Minute)},
		},
	}
	got := buildApplyProgress(snap, now)
	if got.Percent != 50 || got.Remaining != "About 4m left" || got.Elapsed != "6m" || got.Total != 2 || got.Resolved != 1 || got.TotalSize != "100.0 GB" {
		t.Fatalf("progress view = %+v", got)
	}
	if got := buildApplyProgress(snap, now.Add(5*time.Minute)); got.Remaining != "Re-estimating…" {
		t.Fatalf("overdue view = %+v", got)
	}
	snap.Entries[0].Status = mover.StatusFailed
	if got := buildApplyProgress(snap, now); got.Remaining != "Estimating…" || got.Resolved != 1 {
		t.Fatalf("failed-only sample view = %+v", got)
	}
	snap.Done = true
	snap.Finished = now
	if got := buildApplyProgress(snap, now.Add(time.Hour)); got.Remaining != "Moves finished" || got.Percent != 100 || got.Elapsed != "6m" {
		t.Fatalf("finished view = %+v", got)
	}
	if got := buildApplyProgress(mover.ProgressSnapshot{}, now); got.TotalSize != "" || got.Percent != 0 {
		t.Fatalf("empty view = %+v", got)
	}
}

func TestFormatMoveDuration(t *testing.T) {
	for duration, want := range map[time.Duration]string{
		0: "< 1m", 59 * time.Second: "< 1m", time.Minute: "1m",
		61 * time.Second: "2m", time.Hour: "1h 0m", 90 * time.Minute: "1h 30m",
		24 * time.Hour: "1d 0h", 25*time.Hour + time.Minute: "1d 2h",
		48*time.Hour - time.Minute: "2d 0h",
	} {
		if got := formatMoveDuration(duration); got != want {
			t.Errorf("formatMoveDuration(%v) = %q, want %q", duration, got, want)
		}
	}
}

func TestApplyProgressRendering(t *testing.T) {
	srv := newSettingsTestServer(t)
	tests := []struct {
		name    string
		status  applyStatusData
		want    []string
		missing []string
	}{
		{
			name: "running with estimate",
			status: applyStatusData{Running: true, MovedCount: 33, Progress: applyProgressView{
				Percent: 42, Remaining: "About 1h 12m left", Elapsed: "54m", Total: 80, Resolved: 33, TotalSize: "1400.0 GB",
			}},
			want: []string{`role="progressbar"`, `aria-valuenow="42"`, `width: 42%`, "About 1h 12m left", "33 of 80 moves processed", "1400.0 GB planned", `hx-trigger="every 2s"`},
		},
		{
			name: "initial learning",
			status: applyStatusData{Running: true, Progress: buildApplyProgress(mover.ProgressSnapshot{
				Entries: []mover.EntryProgress{{Status: mover.StatusPending}},
			}, time.Now())},
			want:    []string{`aria-valuenow="0"`, "Estimating…", "Waiting for the first completed move"},
			missing: []string{"About 0", "GB planned"},
		},
		{
			name:    "Jellyfin is still working",
			status:  applyStatusData{Running: true, SyncingJellyfin: true, Progress: applyProgressView{Percent: 100, Remaining: "Moves finished"}},
			want:    []string{`aria-valuenow="100"`, "Final library update", "Updating Jellyfin…", `hx-trigger="every 2s"`},
			missing: []string{"Estimated time remaining", "About 0", "apply-progress-complete"},
		},
		{
			name:    "finished successfully",
			status:  applyStatusData{MovedCount: 1, Progress: applyProgressView{Percent: 100, Remaining: "Moves finished"}},
			want:    []string{`aria-valuenow="100"`, "Moves finished", "apply-progress-complete"},
			missing: []string{"Estimated time remaining", "hx-trigger", "is-running"},
		},
		{
			name:    "finished with failures",
			status:  applyStatusData{FailedCount: 1, Progress: applyProgressView{Percent: 100}},
			want:    []string{`aria-valuenow="100"`, "Finished with errors", "apply-progress-errors", "0 moved, 1 failed"},
			missing: []string{"apply-progress-complete", "hx-trigger"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.renderPartial(rec, "plan", "apply_status_table", tt.status)
			if rec.Code != http.StatusOK {
				t.Fatalf("render failed: %s", rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in rendered progress", want)
				}
			}
			for _, missing := range tt.missing {
				if strings.Contains(body, missing) {
					t.Errorf("unexpected %q in rendered progress", missing)
				}
			}
		})
	}
}
