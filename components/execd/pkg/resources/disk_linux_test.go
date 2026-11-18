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
	"testing"
)

func overrideDiskPaths(t *testing.T, cgroup, mountinfo string, v2Dir, v1BlkioDir string) {
	t.Helper()
	origCgroup, origMountinfo := procSelfCgroupFile, procSelfMountinfo
	origV2, origV1Blkio := cgroupV2Dir, cgroupV1BlkioDir
	procSelfCgroupFile, procSelfMountinfo = cgroup, mountinfo
	cgroupV2Dir, cgroupV1BlkioDir = v2Dir, v1BlkioDir
	t.Cleanup(func() {
		procSelfCgroupFile, procSelfMountinfo = origCgroup, origMountinfo
		cgroupV2Dir, cgroupV1BlkioDir = origV2, origV1Blkio
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
