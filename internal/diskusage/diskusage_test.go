package diskusage

import (
	"os"
	"path/filepath"
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

func TestIsMountPoint_PlainSubdirectoryIsNotAMountPoint(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	isMount, err := IsMountPoint(sub)
	if err != nil {
		t.Fatalf("IsMountPoint: %v", err)
	}
	if isMount {
		t.Error("a plain subdirectory on the same filesystem must not report as a mount point")
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

func TestCheckPath_RequireMountRejectsPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := CheckPath(sub, true); err == nil {
		t.Fatal("expected CheckPath to refuse a plain subdirectory when requireMount is true")
	}
}

func TestCheckPath_OKWithoutMountRequirement(t *testing.T) {
	if err := CheckPath(t.TempDir(), false); err != nil {
		t.Fatalf("CheckPath: %v", err)
	}
}
