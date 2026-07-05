// Package diskusage reports filesystem usage and mount-point safety checks
// for configured tier paths. Coldarr must never mistake an unmounted
// satellite drive's empty mountpoint directory for the drive itself, so
// every usage lookup is paired with a mount check the planner is expected
// to consult before treating a path as usable storage.
package diskusage

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Usage struct {
	Path        string
	TotalBytes  uint64
	FreeBytes   uint64
	UsedBytes   uint64
	UsedPercent float64
}

// Stat returns filesystem usage for path. path must exist.
func Stat(path string) (Usage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Usage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	bsize := uint64(stat.Bsize)
	total := stat.Blocks * bsize
	free := stat.Bavail * bsize
	used := total - stat.Bfree*bsize

	var usedPercent float64
	if total > 0 {
		usedPercent = float64(used) / float64(total) * 100
	}

	return Usage{
		Path:        path,
		TotalBytes:  total,
		FreeBytes:   free,
		UsedBytes:   used,
		UsedPercent: usedPercent,
	}, nil
}

// DeviceID returns the identifier of the filesystem path resides on -
// the same underlying value tools like `du -x`/`find -xdev` use to detect
// filesystem boundaries. Two paths with the same DeviceID are on the same
// physical volume (or partition/dataset), and therefore share the same
// capacity, no matter how differently they're named or nested.
func DeviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot determine device for %s on this platform", path)
	}
	return uint64(stat.Dev), nil
}

// IsMountPoint reports whether path is a distinct mount point from its
// parent directory, i.e. crossing from the parent into path changes
// filesystem device. A path that fails this check but is expected to be a
// mounted drive is almost certainly an unmounted drive's empty mountpoint
// directory sitting on the root filesystem - writing to it would silently
// fill the root disk instead of the intended drive.
func IsMountPoint(path string) (bool, error) {
	dev, err := DeviceID(path)
	if err != nil {
		return false, err
	}
	parentDev, err := DeviceID(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	return dev != parentDev, nil
}

// CheckPath verifies path exists and, if requireMount is true, that it is a
// genuine mount point rather than a plain directory. It returns a
// human-readable error describing exactly what's wrong so operators can fix
// misconfigurations (or a missing drive) before Coldarr ever plans a move.
func CheckPath(path string, requireMount bool) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("path %s does not exist - is the drive mounted?", path)
		}
		return fmt.Errorf("checking path %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %s is not a directory", path)
	}

	if requireMount {
		isMount, err := IsMountPoint(path)
		if err != nil {
			return fmt.Errorf("checking mount status of %s: %w", path, err)
		}
		if !isMount {
			return fmt.Errorf("path %s is required to be a mount point but is not - refusing to use it (this usually means the drive is unmounted and you're looking at an empty directory on the root filesystem)", path)
		}
	}

	return nil
}
