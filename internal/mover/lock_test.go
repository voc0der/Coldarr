package mover

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAcquireLock_OnlyOneApplyAtATime pins the guard the CLI and the web
// GUI share: while one apply holds the lock a second fails at once instead
// of queueing behind it, and releasing it lets the next apply in.
func TestAcquireLock_OnlyOneApplyAtATime(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := AcquireLock(dir); err == nil || !strings.Contains(err.Error(), "another apply is already running") {
		t.Fatalf("second AcquireLock while held = %v, want it refused", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireLock_MissingDirectory(t *testing.T) {
	_, err := AcquireLock(filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(err.Error(), "opening apply lock") {
		t.Fatalf("AcquireLock in a missing directory = %v, want an open error", err)
	}
}
