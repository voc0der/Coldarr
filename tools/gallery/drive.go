package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const blockSize = 4096

// drive is one fake disk: a FUSE loopback of a plain directory, mounted
// where a real drive would be, whose statfs reports the fixture's capacity
// instead of the backing filesystem's.
//
// Used space is a fixed baseline (everything a full drive holds that the
// fixture doesn't bother to model) plus the apparent size of whatever is
// actually in the backing directory. Media files are sparse, so a 58 GiB
// "remux" costs nothing, yet copying one onto the drive grows its usage by
// 58 GiB, exactly what Coldarr's settle check watches for.
//
// Because each drive is a separate mount, Coldarr sees what it would in
// production: its own device ID (so shared-volume detection works), its
// own /proc/self/mountinfo entry (so require_mount passes), and, once
// unmounted, an empty directory on the system disk (so the dead-drive
// check fires).
type drive struct {
	name    string
	mount   string
	backing string
	total   uint64
	wantPct float64

	mu         sync.Mutex
	base       uint64
	apparentAt time.Time
	apparentN  uint64

	server *fuse.Server
}

func newDrive(spec driveSpec, backingRoot string) *drive {
	total := uint64(spec.Size) / blockSize * blockSize //nolint:gosec // loadFixture rejects sizes <= 0
	return &drive{
		name:    spec.Name,
		mount:   filepath.Clean(spec.Mount),
		backing: filepath.Join(backingRoot, spec.Name),
		total:   total,
		wantPct: spec.UsedPercent,
	}
}

// contains reports whether path lives on this drive.
func (d *drive) contains(path string) bool {
	path = filepath.Clean(path)
	return path == d.mount || len(path) > len(d.mount) && path[:len(d.mount)+1] == d.mount+"/"
}

func (d *drive) mountFS() error {
	if err := os.RemoveAll(d.backing); err != nil {
		return err
	}
	if err := os.MkdirAll(d.backing, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(d.mount, 0o750); err != nil {
		return err
	}

	loopback, err := fusefs.NewLoopbackRoot(d.backing)
	if err != nil {
		return err
	}
	root := &driveNode{LoopbackNode: loopback.(*fusefs.LoopbackNode), drive: d}

	// No kernel caching: the fake Radarr/Sonarr change these trees while
	// Coldarr is watching them, and a stale entry would hide a move.
	var zero time.Duration
	server, err := fusefs.Mount(d.mount, root, &fusefs.Options{
		EntryTimeout:    &zero,
		AttrTimeout:     &zero,
		NegativeTimeout: &zero,
		MountOptions: fuse.MountOptions{
			FsName:            "gallery-" + d.name,
			Name:              "gallery",
			AllowOther:        true,
			DirectMountStrict: true,
		},
	})
	if err != nil {
		return fmt.Errorf("mounting fake drive %s at %s: %w (the container needs --device /dev/fuse --cap-add SYS_ADMIN)", d.name, d.mount, err)
	}
	d.server = server
	go func() {
		server.Wait()
		logf("drive %s: unmounted from %s", d.name, d.mount)
	}()
	return nil
}

func (d *drive) unmount() {
	if d.server != nil {
		_ = d.server.Unmount()
	}
}

// calibrate sets the baseline so the drive reports the fixture's used
// percentage with everything seeded so far counted in it.
func (d *drive) calibrate() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.apparentAt = time.Time{}
	have := d.apparentLocked()
	want := uint64(float64(d.total) * d.wantPct / 100)
	if have > want {
		return fmt.Errorf("drive %s: the fixture puts %s of media on it, more than the %.1f%% used it asks for", d.name, gib(have), d.wantPct)
	}
	d.base = want - have
	return nil
}

func (d *drive) usage() (used, total uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	used = min(d.base+d.apparentLocked(), d.total)
	return used, d.total
}

// apparentLocked sums the apparent size of every file on the drive. A walk
// per statfs is fine at fixture scale; the short cache just keeps a burst
// of statfs calls (one per tier path) from walking once each.
func (d *drive) apparentLocked() uint64 {
	if time.Since(d.apparentAt) < 200*time.Millisecond {
		return d.apparentN
	}
	var n uint64
	_ = filepath.WalkDir(d.backing, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return nil //nolint:nilerr // a file vanishing mid-walk is a move finishing, not an error
		}
		if info, err := e.Info(); err == nil {
			n += uint64(info.Size()) //nolint:gosec // file sizes are never negative
		}
		return nil
	})
	d.apparentAt, d.apparentN = time.Now(), n
	return n
}

// driveNode is go-fuse's loopback node with statfs answered by the drive.
// WrapChild makes every node on the mount one of these, since the kernel
// sends statfs for whichever inode the caller named, not the mount root.
type driveNode struct {
	*fusefs.LoopbackNode
	drive *drive
}

var (
	_ fusefs.NodeStatfser    = (*driveNode)(nil)
	_ fusefs.NodeWrapChilder = (*driveNode)(nil)
)

func (n *driveNode) WrapChild(_ context.Context, ops fusefs.InodeEmbedder) fusefs.InodeEmbedder {
	return &driveNode{LoopbackNode: ops.(*fusefs.LoopbackNode), drive: n.drive}
}

func (n *driveNode) Statfs(_ context.Context, out *fuse.StatfsOut) syscall.Errno {
	used, total := n.drive.usage()
	free := (total - used) / blockSize
	out.Bsize = blockSize
	out.Frsize = blockSize
	out.Blocks = total / blockSize
	out.Bfree = free
	out.Bavail = free
	out.Files = 1 << 32
	out.Ffree = 1 << 31
	out.NameLen = 255
	return fusefs.OK
}
