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
	"strconv"
	"strings"
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

	bsize := uint64(stat.Bsize) //nolint:gosec // block size is never negative in practice
	total := stat.Blocks * bsize
	free := stat.Bavail * bsize
	used := total - stat.Bfree*bsize

	return Usage{
		Path:        path,
		TotalBytes:  total,
		FreeBytes:   free,
		UsedBytes:   used,
		UsedPercent: PercentUsed(used, free),
	}, nil
}

// PercentUsed computes usage the way `df` does: used / (used + avail),
// not used / raw-total. Filesystems like ext4 reserve a slice of blocks
// (Bfree) that only root can write into, which Bavail already excludes;
// dividing by the raw total instead would count that reserved slice as
// headroom Coldarr could still pack into, so the planner would keep
// filling a tier past the point where its own writes actually start
// failing with ENOSPC - and df would already be reporting 100% used
// while Coldarr still thought there was room.
func PercentUsed(usedBytes, freeBytes uint64) float64 {
	denom := usedBytes + freeBytes
	if denom == 0 {
		return 0
	}
	return float64(usedBytes) / float64(denom) * 100
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
	return stat.Dev, nil
}

// mountInfoPath is the kernel's list of this process's mounts - read from
// inside Coldarr's own mount namespace, so in Docker it describes the
// container's view of each path. A variable so tests can use a fixture.
var mountInfoPath = "/proc/self/mountinfo"

type mountEntry struct {
	device string // "major:minor" of the filesystem behind the mount
	point  string
}

func readMounts() ([]mountEntry, error) {
	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return nil, err
	}
	var mounts []mountEntry
	for _, line := range strings.Split(string(data), "\n") {
		// Fields: mount ID, parent ID, major:minor, root, mount point, ...
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mounts = append(mounts, mountEntry{device: fields[2], point: unescapeMountField(fields[4])})
	}
	if len(mounts) == 0 {
		return nil, fmt.Errorf("%s lists no mounts", mountInfoPath)
	}
	return mounts, nil
}

// unescapeMountField undoes the kernel's octal escaping of spaces, tabs,
// newlines and backslashes in mountinfo paths (e.g. "\040" for a space).
func unescapeMountField(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// backingMount returns the mount path lives on: the longest mount point
// containing it, and among equal ones the last listed, since a later mount
// on the same point hides the earlier one.
func backingMount(mounts []mountEntry, path string) (mountEntry, bool) {
	var best mountEntry
	found := false
	for _, m := range mounts {
		if m.point != "/" && path != m.point && !strings.HasPrefix(path, m.point+"/") {
			continue
		}
		if !found || len(m.point) >= len(best.point) {
			best, found = m, true
		}
	}
	return best, found
}

// systemDisk returns the mount identifying the system disk. Inside a Docker
// container "/" is the container's own overlay, so the device behind
// Docker's per-container /etc/hostname bind mount - kept under Docker's data
// directory on the host - stands in for the host's system disk. Outside a
// container it is simply "/".
func systemDisk(mounts []mountEntry) (mountEntry, bool) {
	var root mountEntry
	foundRoot := false
	for _, m := range mounts {
		switch m.point {
		case "/etc/hostname":
			return m, true
		case "/":
			root, foundRoot = m, true
		}
	}
	return root, foundRoot
}

// checkOnOwnDrive refuses a path that isn't backed by a drive of its own -
// the state a missing or dead drive leaves behind. Its mountpoint directory
// still exists as a plain directory on the system disk, and in Docker the
// bind mount of that directory still exists too, so neither "does the
// directory exist" nor "is it a mount point" can tell the difference. Where
// the path's filesystem actually comes from can: the system disk, not the
// drive.
func checkOnOwnDrive(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", path, err)
	}
	mounts, err := readMounts()
	if err != nil {
		return fmt.Errorf("reading the mount table to confirm %s is on a mounted drive: %w - refusing to use it", path, err)
	}
	backing, ok := backingMount(mounts, resolved)
	system, sysOK := systemDisk(mounts)
	if !ok || !sysOK {
		return fmt.Errorf("could not find which filesystem %s is on in %s - refusing to use it", path, mountInfoPath)
	}
	if backing.point == "/" || backing.device == system.device {
		return fmt.Errorf("path %s is required to be on its own mounted drive but is on the system disk - refusing to use it (the drive is probably unmounted, missing, or not passed through to this machine, and this is the empty directory left behind)", path)
	}
	return nil
}

// CheckPath verifies path exists and, if requireMount is true, that it is
// backed by its own mounted drive rather than the system disk (see
// checkOnOwnDrive). It returns a human-readable error describing exactly
// what's wrong so operators can fix misconfigurations (or a missing drive)
// before Coldarr ever plans a move.
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
		return checkOnOwnDrive(path)
	}
	return nil
}
