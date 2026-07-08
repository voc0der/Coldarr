package mover

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is a crash-safe, cross-process guard ensuring only one apply runs
// at a time system-wide - the CLI and the web GUI both respect it. It's
// backed by flock() on a file in the config directory: the kernel releases
// the lock automatically the moment the holding process exits for any
// reason, including a crash, so there's no stale-lock file to clean up by
// hand after exactly the kind of incident this exists to prevent.
type Lock struct {
	file *os.File
}

// AcquireLock takes the apply lock in dir. It never blocks - if another
// process already holds it, it returns an error immediately.
func AcquireLock(dir string) (*Lock, error) {
	path := filepath.Join(dir, ".apply.lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening apply lock %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another apply is already running - wait for it to finish before starting another")
	}

	return &Lock{file: f}, nil
}

// Release releases the lock. Safe to call once; the kernel releases it
// automatically on process exit even if this is never called.
func (l *Lock) Release() error {
	defer func() { _ = l.file.Close() }()
	return syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
}
