package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/model"
)

func TestLoad_MissingFileStartsEmpty(t *testing.T) {
	s, err := Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.All()) != 0 {
		t.Fatalf("expected no records for a fresh store, got %d", len(s.All()))
	}
}

func TestAppend_PersistsAcrossLoad(t *testing.T) {
	path := t.TempDir() + "/history.json"

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := Record{ArrApp: "radarr", ItemID: 1, Title: "Movie A", MovedAt: time.Now()}
	if err := s.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load (reloaded): %v", err)
	}
	all := reloaded.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 record after reload, got %d", len(all))
	}
	if all[0].Title != "Movie A" {
		t.Fatalf("Title = %q, want Movie A", all[0].Title)
	}
}

func TestAll_MostRecentFirst(t *testing.T) {
	s, err := Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	now := time.Now()
	if err := s.Append(Record{ArrApp: "radarr", ItemID: 1, Title: "Older", MovedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(Record{ArrApp: "radarr", ItemID: 2, Title: "Newer", MovedAt: now}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	all := s.All()
	if len(all) != 2 || all[0].Title != "Newer" || all[1].Title != "Older" {
		t.Fatalf("expected [Newer, Older], got %+v", all)
	}
}

func TestLastMoved_NotFound(t *testing.T) {
	s, err := Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := s.LastMoved(model.Key{ArrApp: "radarr", ID: 1}); ok {
		t.Error("expected LastMoved to report not-found for an item with no history")
	}
}

func TestLastMoved_ReturnsMostRecent(t *testing.T) {
	s, err := Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key := model.Key{ArrApp: "radarr", ID: 1}
	now := time.Now()

	if err := s.Append(Record{ArrApp: "radarr", ItemID: 1, MovedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(Record{ArrApp: "radarr", ItemID: 1, MovedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	last, ok := s.LastMoved(key)
	if !ok {
		t.Fatal("expected a last-moved time")
	}
	if !last.Equal(now.Add(-time.Hour)) {
		t.Fatalf("LastMoved = %v, want the most recent of the two records", last)
	}
}

func TestInCooldown(t *testing.T) {
	s, err := Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key := model.Key{ArrApp: "radarr", ID: 1}
	now := time.Now()

	if s.InCooldown(key, 30*24*time.Hour, now) {
		t.Error("an item never moved should never be in cooldown")
	}

	if err := s.Append(Record{ArrApp: "radarr", ItemID: 1, MovedAt: now.Add(-5 * 24 * time.Hour)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if !s.InCooldown(key, 30*24*time.Hour, now) {
		t.Error("expected item moved 5 days ago to be in cooldown under a 30-day policy")
	}
	if s.InCooldown(key, 1*24*time.Hour, now) {
		t.Error("expected item moved 5 days ago to be out of cooldown under a 1-day policy")
	}
}

// TestLoad_EmptyFileStartsEmpty: a zero-byte history file - what a crash
// between creating the file and first writing it leaves behind - loads as
// no history rather than failing every command that reads the ledger.
func TestLoad_EmptyFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.All()) != 0 {
		t.Fatalf("expected no records from an empty file, got %d", len(s.All()))
	}
}

// TestLoad_UnreadableHistoryIsAnError: unlike a missing or empty file, a
// history that exists but can't be read must stop the caller. Reading it
// as empty would silently drop every cooldown, letting the planner move
// recently moved items straight back.
func TestLoad_UnreadableHistoryIsAnError(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(path string) error
		wantErr string
	}{
		{name: "corrupt", setup: func(path string) error { return os.WriteFile(path, []byte("[{"), 0o600) }, wantErr: "parsing history"},
		{name: "a directory", setup: func(path string) error { return os.Mkdir(path, 0o750) }, wantErr: "reading history"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.json")
			if err := tt.setup(path); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestAppend_ReportsAFailedSave: the mover records each move as it lands,
// and a ledger write that fails has to come back to it rather than vanish.
func TestAppend_ReportsAFailedSave(t *testing.T) {
	tests := []struct {
		name string
		// breakSave runs after a successful Load, so the failure is the
		// save's own, not one Load would already have reported.
		breakSave func(path string) error
		wantErr   string
	}{
		{
			name:      "directory cannot be created",
			breakSave: func(path string) error { return os.WriteFile(filepath.Dir(path), nil, 0o600) },
			wantErr:   "creating history directory",
		},
		{
			name:      "temp file cannot be written",
			breakSave: func(path string) error { return os.MkdirAll(path+".tmp", 0o750) },
			wantErr:   "writing history",
		},
		{
			name:      "file replaced by a directory",
			breakSave: func(path string) error { return os.MkdirAll(path, 0o750) },
			wantErr:   "saving history",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "history.json")
			s, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := tt.breakSave(path); err != nil {
				t.Fatalf("breakSave: %v", err)
			}
			err = s.Append(Record{ArrApp: "radarr", ItemID: 1, Title: "Movie A", MovedAt: time.Now()})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Append error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
