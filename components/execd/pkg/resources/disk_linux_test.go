//go:build linux

// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resources

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func overrideDiskPaths(t *testing.T, cgroup, mountinfo string, v2Dir, v1BlkioDir string) {
	t.Helper()
	origCgroup, origMountinfo := procSelfCgroupFile, procSelfMountinfo
	origV2, origV1Blkio := cgroupV2Dir, cgroupV1BlkioDir
	origSysDevBlock := sysDevBlockDir
	procSelfCgroupFile, procSelfMountinfo = cgroup, mountinfo
	cgroupV2Dir, cgroupV1BlkioDir = v2Dir, v1BlkioDir
	// No device resolves, so every cgroup IO line counts as it does on a
	// host without sysfs.
	sysDevBlockDir = t.TempDir()
	t.Cleanup(func() {
		procSelfCgroupFile, procSelfMountinfo = origCgroup, origMountinfo
		cgroupV2Dir, cgroupV1BlkioDir = origV2, origV1Blkio
		sysDevBlockDir = origSysDevBlock
	})
}

func TestDiskUsagesSkipsPseudoMounts(t *testing.T) {
	dir := t.TempDir()
	data := "1 0 0:1 / / rw - tmpfs tmpfs rw\n2 0 0:2 / " + dir + " rw - ext4 /dev/vda rw\n3 0 0:3 / /proc rw - proc proc rw\n"
	mountinfoPath := filepath.Join(t.TempDir(), "mountinfo")
	writeFile(t, mountinfoPath, data)
	overrideDiskPaths(t,
		filepath.Join(t.TempDir(), "cgroup"),
		mountinfoPath,
		filepath.Join(t.TempDir(), "v2"),
		filepath.Join(t.TempDir(), "blkio"),
	)

	usages, err := DiskUsages()
	if err != nil {
		t.Fatalf("DiskUsages: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("usages = %+v, want 2 real mounts", usages)
	}
	// The mountinfo fixture claims ext4, but statfs on the temp dir always
	// succeeds and yields a sane snapshot regardless of the claimed type.
	byPoint := make(map[string]DiskUsage, len(usages))
	for _, u := range usages {
		byPoint[u.Mountpoint] = u
		if u.TotalBytes <= 0 {
			t.Fatalf("usage %v has non-positive total", u)
		}
	}
	if _, ok := byPoint["/proc"]; ok {
		t.Fatal("proc pseudo mount leaked into usages")
	}
	if _, ok := byPoint[dir]; !ok {
		t.Fatalf("temp dir mount missing from usages: %+v", usages)
	}
}

// statfs reports block counts in f_frsize units, which is what df uses. Most
// filesystems set f_bsize to the same value, but FUSE passes both through
// from the daemon: GlusterFS, for one, reports a 128 KiB f_bsize over 4 KiB
// fragments, so sizing by f_bsize would overstate the mount 32 times.
func TestDiskUsagesSizesBlocksByFragmentSize(t *testing.T) {
	mountpoint := t.TempDir()
	mountinfoPath := filepath.Join(t.TempDir(), "mountinfo")
	writeFile(t, mountinfoPath, "1 0 0:1 / "+mountpoint+" rw - fuse.glusterfs host:/vol rw\n")
	overrideDiskPaths(t,
		filepath.Join(t.TempDir(), "cgroup"),
		mountinfoPath,
		filepath.Join(t.TempDir(), "v2"),
		filepath.Join(t.TempDir(), "blkio"),
	)
	origSyscall := statfsSyscall
	statfsSyscall = func(_ string, st *unix.Statfs_t) error {
		st.Bsize, st.Frsize = 128<<10, 4<<10
		st.Blocks, st.Bfree, st.Bavail = 1000, 400, 300
		return nil
	}
	t.Cleanup(func() { statfsSyscall = origSyscall })

	usages, err := DiskUsages()
	if err != nil {
		t.Fatalf("DiskUsages: %v", err)
	}
	want := DiskUsage{
		Mountpoint: mountpoint,
		FSType:     "fuse.glusterfs",
		Source:     "host:/vol",
		TotalBytes: 1000 * 4096,
		UsedBytes:  600 * 4096,
		AvailBytes: 300 * 4096,
	}
	if len(usages) != 1 || usages[0] != want {
		t.Fatalf("usages = %+v, want [%+v]", usages, want)
	}
}

func TestNewDiskIOSourceFallsBackToDiskstats(t *testing.T) {
	root := t.TempDir()
	// gopsutil resolves /proc/diskstats through the HOST_PROC override.
	t.Setenv("HOST_PROC", root)
	writeFile(t, filepath.Join(root, "diskstats"),
		"   7       0 loop0 111 0 222 10 333 0 444 20 0 30 500\n 259       0 vda 100 0 200 10 300 0 400 20 0 30 500\n")
	overrideDiskPaths(t,
		filepath.Join(root, "cgroup"), // absent: no cgroup view
		filepath.Join(root, "mountinfo"),
		filepath.Join(root, "v2"),
		filepath.Join(root, "blkio"),
	)

	reader := newDiskIOSource()
	io, err := reader()
	if err != nil {
		t.Fatalf("disk IO reader: %v", err)
	}
	if io.ReadOps != 100 || io.WriteOps != 300 {
		t.Fatalf("ops = (%d, %d), want (100, 300) from diskstats only", io.ReadOps, io.WriteOps)
	}
	if io.ReadBytes != 200*512 || io.WrittenBytes != 400*512 {
		t.Fatalf("bytes = (%d, %d), want diskstats sectors*512", io.ReadBytes, io.WrittenBytes)
	}
}

// TestStatfsTimeoutReusesPendingCall pins the hung-mount contract: a timed
// out statfs keeps its in-flight goroutine, later collections reuse it
// instead of stacking new ones, and a recovered mount unblocks cleanly.
func TestStatfsTimeoutReusesPendingCall(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	origSyscall, origTimeout := statfsSyscall, statfsTimeout
	realStatfs := unix.Statfs // captured: the woken goroutine must not read the package var while cleanup restores it
	statfsSyscall = func(_ string, st *unix.Statfs_t) error {
		<-release
		return realStatfs(dir, st)
	}
	statfsTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		statfsSyscall, statfsTimeout = origSyscall, origTimeout
	})

	// First call times out; the blocked syscall goroutine stays pending.
	if _, ok := statfsWithTimeout("/hung"); ok {
		t.Fatal("first call should time out")
	}
	afterFirst := runtime.NumGoroutine()

	// Later collections skip the hung mount at O(1) cost instead of
	// re-paying the timeout, and never stack a new goroutine.
	start := time.Now()
	if _, ok := statfsWithTimeout("/hung"); ok {
		t.Fatal("hung mount should be skipped fast")
	}
	if elapsed := time.Since(start); elapsed > statfsTimeout/2 {
		t.Fatalf("hung skip took %s, want well under the %s timeout", elapsed, statfsTimeout)
	}
	if got := runtime.NumGoroutine(); got != afterFirst {
		t.Fatalf("goroutines = %d, want %d (no new statfs goroutine)", got, afterFirst)
	}

	// When the blocked syscall finally returns, the pending result is
	// consumed by the next collection and normal scanning resumes. The
	// peek is non-blocking by design, so the recovery may take a round
	// to land; retry within a generous budget.
	statfsTimeout = origTimeout
	close(release)
	deadline := time.Now().Add(time.Second)
	var st unix.Statfs_t
	var ok bool
	for {
		if st, ok = statfsWithTimeout("/hung"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered result should surface")
		}
	}
	if st.Blocks == 0 {
		t.Fatal("expected real statfs values after recovery")
	}
	st, ok = statfsWithTimeout("/hung")
	if !ok || st.Blocks == 0 {
		t.Fatal("subsequent call should statfs normally after recovery")
	}
}

func TestNewDiskIOSourcePrefersV2IOStat(t *testing.T) {
	root := t.TempDir()
	v2Dir := filepath.Join(root, "cgroup2")
	if err := os.MkdirAll(filepath.Join(v2Dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(v2Dir, "cgroup.controllers"), "")
	writeFile(t, filepath.Join(v2Dir, "sandbox", "io.stat"), "8:0 rbytes=100 wbytes=200 rios=10 wios=20\n")
	cgroupPath := filepath.Join(root, "cgroup")
	writeFile(t, cgroupPath, "0::/sandbox\n")
	overrideDiskPaths(t,
		cgroupPath,
		filepath.Join(root, "mountinfo"),
		v2Dir,
		filepath.Join(root, "blkio"),
	)

	reader := newDiskIOSource()
	io, err := reader()
	if err != nil {
		t.Fatalf("disk IO reader: %v", err)
	}
	if io.ReadBytes != 100 || io.WrittenBytes != 200 || io.ReadOps != 10 || io.WriteOps != 20 {
		t.Fatalf("io = %+v, want v2 io.stat values", io)
	}
}

func TestNewDiskIOSourcePrefersV1Blkio(t *testing.T) {
	root := t.TempDir()
	blkioDir := filepath.Join(root, "blkio")
	sandbox := filepath.Join(blkioDir, "sandbox")
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sandbox, "blkio.throttle.io_service_bytes"), "8:0 Read 1000\n8:0 Write 2000\n")
	writeFile(t, filepath.Join(sandbox, "blkio.throttle.io_serviced"), "8:0 Read 10\n8:0 Write 20\n")
	cgroupPath := filepath.Join(root, "cgroup")
	writeFile(t, cgroupPath, "12:blkio:/sandbox\n")
	overrideDiskPaths(t,
		cgroupPath,
		filepath.Join(root, "mountinfo"),
		filepath.Join(root, "v2"),
		blkioDir,
	)

	reader := newDiskIOSource()
	io, err := reader()
	if err != nil {
		t.Fatalf("disk IO reader: %v", err)
	}
	if io.ReadBytes != 1000 || io.WrittenBytes != 2000 || io.ReadOps != 10 || io.WriteOps != 20 {
		t.Fatalf("io = %+v, want v1 blkio throttle values", io)
	}
}

// fakeSysDevBlock points sysDevBlockDir at a directory of MAJ:MIN links
// named like /sys/dev/block, each resolving to a device directory.
func fakeSysDevBlock(t *testing.T, devices map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for majMin, target := range devices {
		if err := os.Symlink(target, filepath.Join(dir, majMin)); err != nil {
			t.Fatal(err)
		}
	}
	orig := sysDevBlockDir
	sysDevBlockDir = dir
	t.Cleanup(func() { sysDevBlockDir = orig })
}

// The cgroup charges a bio on every block layer it passes through, so a
// device-mapper (or loop) device and the disk under it both report the
// same I/O. Like the /proc/diskstats fallback, the cgroup sources count only
// the bottom layer; a device that cannot be resolved is still counted.
func TestDiskIOSourcesSkipStackedDevices(t *testing.T) {
	fakeSysDevBlock(t, map[string]string{
		"252:0": "../../devices/virtual/block/dm-0",
		"7:0":   "../../devices/virtual/block/loop0",
		"259:0": "../../devices/pci0000:00/0000:00:04.0/nvme/nvme0/nvme0n1",
	})

	t.Run("v2", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "io.stat")
		writeFile(t, path, "252:0 rbytes=4096 wbytes=33554432 rios=1 wios=32\n"+
			"259:0 rbytes=4096 wbytes=33554432 rios=1 wios=64\n"+
			"7:0 rbytes=0 wbytes=1048576 rios=0 wios=8\n"+
			"8:16 rbytes=100 wbytes=200 rios=10 wios=20\n")

		io, err := v2IOReader(path)()
		if err != nil {
			t.Fatalf("v2 reader: %v", err)
		}
		want := DiskIO{ReadBytes: 4196, WrittenBytes: 33554632, ReadOps: 11, WriteOps: 84}
		if io != want {
			t.Fatalf("io = %+v, want %+v", io, want)
		}
	})

	t.Run("v1", func(t *testing.T) {
		dir := t.TempDir()
		bytesPath := filepath.Join(dir, "blkio.throttle.io_service_bytes")
		opsPath := filepath.Join(dir, "blkio.throttle.io_serviced")
		writeFile(t, bytesPath, "252:0 Read 4096\n252:0 Write 33554432\n259:0 Read 4096\n259:0 Write 33554432\n"+
			"8:16 Read 100\n8:16 Write 200\nTotal 67109064\n")
		writeFile(t, opsPath, "252:0 Read 1\n252:0 Write 32\n259:0 Read 1\n259:0 Write 64\n8:16 Read 10\n8:16 Write 20\nTotal 128\n")

		io, err := v1IOReader(bytesPath, opsPath)()
		if err != nil {
			t.Fatalf("v1 reader: %v", err)
		}
		want := DiskIO{ReadBytes: 4196, WrittenBytes: 33554632, ReadOps: 11, WriteOps: 84}
		if io != want {
			t.Fatalf("io = %+v, want %+v", io, want)
		}
	})
}
