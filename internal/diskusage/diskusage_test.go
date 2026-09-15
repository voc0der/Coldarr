package diskusage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPercentUsed(t *testing.T) {
	cases := []struct {
		used, free uint64
		want       float64
	}{
		{used: 50, free: 50, want: 50},
		{used: 0, free: 100, want: 0},
		{used: 100, free: 0, want: 100},
		{used: 0, free: 0, want: 0}, // no division by zero
	}
	for _, c := range cases {
		if got := PercentUsed(c.used, c.free); got != c.want {
			t.Errorf("PercentUsed(%d, %d) = %v, want %v", c.used, c.free, got, c.want)
		}
	}
}

func TestDeviceID_SamePathsShareADevice(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	devDir, err := DeviceID(dir)
	if err != nil {
		t.Fatalf("DeviceID(dir): %v", err)
	}
	devSub, err := DeviceID(sub)
	if err != nil {
		t.Fatalf("DeviceID(sub): %v", err)
	}
	if devDir != devSub {
		t.Error("a plain subdirectory should report the same device ID as its parent")
	}
}

func TestDeviceID_NonexistentPath(t *testing.T) {
	if _, err := DeviceID(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("expected an error for a nonexistent path")
	}
}

func TestCheckPath_NonexistentPath(t *testing.T) {
	err := CheckPath(filepath.Join(t.TempDir(), "missing"), false)
	if err == nil {
		t.Fatal("expected an error for a nonexistent path")
	}
}

func TestCheckPath_NotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := CheckPath(file, false); err == nil {
		t.Fatal("expected an error for a path that is a file, not a directory")
	}
}

// withMountInfo points the mount-table lookup at a fixture listing the
// given mountinfo lines for the rest of the test.
func withMountInfo(t *testing.T, lines ...string) {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(fixture, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	old := mountInfoPath
	mountInfoPath = fixture
	t.Cleanup(func() { mountInfoPath = old })
}

// driveDir creates a real directory (CheckPath stats it) and returns its
// symlink-resolved path, which is what the mount table is matched against.
func driveDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

func TestCheckPath_RequireMount(t *testing.T) {
	cases := []struct {
		name    string
		mounts  func(drive string) []string
		wantErr bool
	}{
		{
			// The incident: the USB drive never reached the VM, so Docker
			// bind-mounted the empty mountpoint directory from the VM's
			// system disk - the same disk Docker's own /etc/hostname is on.
			"docker bind mount of an empty directory on the system disk",
			func(drive string) []string {
				return []string{
					"700 650 0:52 / / rw,relatime - overlay overlay rw",
					"710 700 8:2 /media/usbsatahdd " + drive + " ro,relatime - ext4 /dev/sda2 rw",
					"720 700 8:2 /var/lib/docker/containers/abc/hostname /etc/hostname rw,relatime - ext4 /dev/sda2 rw",
				}
			},
			true,
		},
		{
			"docker bind mount of the mounted drive",
			func(drive string) []string {
				return []string{
					"700 650 0:52 / / rw,relatime - overlay overlay rw",
					"710 700 8:96 / " + drive + " ro,relatime - ext4 /dev/sdg rw",
					"720 700 8:2 /var/lib/docker/containers/abc/hostname /etc/hostname rw,relatime - ext4 /dev/sda2 rw",
				}
			},
			false,
		},
		{
			"docker path that was never bind-mounted lives on the container overlay",
			func(drive string) []string {
				return []string{
					"700 650 0:52 / / rw,relatime - overlay overlay rw",
					"720 700 8:2 /var/lib/docker/containers/abc/hostname /etc/hostname rw,relatime - ext4 /dev/sda2 rw",
				}
			},
			true,
		},
		{
			"bare metal: unmounted drive's directory on the root filesystem",
			func(drive string) []string {
				return []string{"22 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw"}
			},
			true,
		},
		{
			"bare metal: mounted drive",
			func(drive string) []string {
				return []string{
					"22 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw",
					"90 22 8:96 / " + drive + " rw,relatime - ext4 /dev/sdg rw",
				}
			},
			false,
		},
		{
			"bare metal: a later mount on the same point hides the drive",
			func(drive string) []string {
				return []string{
					"22 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw",
					"90 22 8:96 / " + drive + " rw,relatime - ext4 /dev/sdg rw",
					"91 90 8:2 /srv/empty " + drive + " rw,relatime - ext4 /dev/sda2 rw",
				}
			},
			true,
		},
		{
			"bare metal: subdirectory of a mounted drive",
			func(drive string) []string {
				return []string{
					"22 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw",
					"90 22 8:96 / " + filepath.Dir(drive) + " rw,relatime - ext4 /dev/sdg rw",
				}
			},
			false,
		},
		{
			"unreadable mount table fails closed",
			func(drive string) []string { return nil },
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drive := driveDir(t)
			withMountInfo(t, c.mounts(drive)...)
			err := CheckPath(drive, true)
			if (err != nil) != c.wantErr {
				t.Fatalf("CheckPath(requireMount) error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestUnescapeMountField(t *testing.T) {
	if got, want := unescapeMountField(`/media/usb\040drive\134x`), `/media/usb drive\x`; got != want {
		t.Fatalf("unescapeMountField() = %q, want %q", got, want)
	}
}

func TestCheckPath_OKWithoutMountRequirement(t *testing.T) {
	if err := CheckPath(t.TempDir(), false); err != nil {
		t.Fatalf("CheckPath: %v", err)
	}
}
