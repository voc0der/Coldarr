package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "downloads.json")
	l, err := LoadDownloadLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Recent(10); len(got) != 0 {
		t.Fatalf("a fresh log has %d events", len(got))
	}

	base := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for i, action := range []string{DownloadMoved, DownloadRelabeled, DownloadRemoved} {
		if err := l.Append(DownloadEvent{Action: action, Names: []string{action}, At: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}

	reloaded, err := LoadDownloadLog(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Recent(2)
	if len(got) != 2 || got[0].Action != DownloadRemoved || got[1].Action != DownloadRelabeled {
		t.Fatalf("Recent(2) = %+v, want the newest two, newest first", got)
	}
}

func TestDownloadLog_BadFiles(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := LoadDownloadLog(empty); err != nil || len(l.Recent(1)) != 0 {
		t.Errorf("an empty file = %v, want an empty log", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDownloadLog(bad); err == nil || !strings.Contains(err.Error(), "parsing download history") {
		t.Errorf("a corrupt file = %v", err)
	}
	if _, err := LoadDownloadLog(dir); err == nil || !strings.Contains(err.Error(), "reading download history") {
		t.Errorf("a directory = %v", err)
	}

	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	l, err := LoadDownloadLog(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(DownloadEvent{Action: DownloadMoved}); err == nil {
		t.Error("a log that can't be written should say so")
	}
}
