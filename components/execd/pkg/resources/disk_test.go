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
	"slices"
	"testing"
)

// A trimmed-down kata-guest mountinfo: virtiofs rootfs, network and FUSE
// data mounts, k8s bind files, and the proc/sys/dev plumbing.
const kataMountinfo = `22 19 0:21 / / rw,relatime shared:1 - virtiofs kataShared[/passthrough/abc/rootfs] rw
25 22 0:26 /skills /mnt/platform/skills ro,nosuid,nodev,relatime - fuse.ossfs2 ossfs2 rw
26 22 0:27 / /mnt/nas_browser_profile rw,relatime vers=3 - nfs 09aa765f-77ox.example.nas.aliyuncs.com:/share/vacuum-prod/users/profile
27 22 0:28 / /home/admin/des/AI00003952 rw,relatime vers=3 - nfs 09aa765f-77ox.example.nas.aliyuncs.com:/share/vacuum-prod/des/AI00003952
28 22 0:29 / /opt/opensandbox rw,nodev,relatime - virtiofs virtiofs-default[/passthrough/sandbox-abc]
29 22 0:30 / /mnt/my\040data rw - ext4 /dev/vdb rw
30 22 0:31 / /dev rw,nosuid,mode=755 - tmpfs dev rw
31 30 0:32 /fuse /dev/fuse ro,size=4104736k - devtmpfs devtmpfs[/fuse] ro
32 30 0:33 / /dev/shm rw,nosuid,nodev,noexec,relatime,size=65536k - tmpfs shm rw
33 30 0:34 /run /run rw,nosuid,nodev - tmpfs run rw
34 22 0:35 /hostname /etc/hostname rw,nosuid,nodev,mode=755 - tmpfs tmpfs[/kata-containers/sandbox/66309-hostname] rw
35 22 0:36 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw
40 22 0:40 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
41 40 0:23 /null /proc/kcore rw,mode=755 - tmpfs tmpfs[/null] rw
50 22 0:50 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
51 50 0:51 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - tmpfs cgroup rw
52 51 0:52 /memory /sys/fs/cgroup/memory ro,relatime - cgroup cgroup rw
60 22 0:60 /first /data rw - ext4 /dev/vdc1 rw
61 22 0:60 /second /data rw - ext4 /dev/vdc2 rw
`

func TestParseMountinfo(t *testing.T) {
	entries := parseMountinfo([]byte(kataMountinfo))

	if len(entries) != 19 {
		t.Fatalf("parseMountinfo got %d entries, want 19: %+v", len(entries), entries)
	}

	byPoint := make(map[string]mountEntry, len(entries))
	for _, e := range entries {
		byPoint[e.Mountpoint] = e
	}

	virtiofs := byPoint["/"]
	if virtiofs.FSType != "virtiofs" || virtiofs.Source != "kataShared[/passthrough/abc/rootfs]" {
		t.Fatalf("root entry = %+v", virtiofs)
	}

	nfs := byPoint["/mnt/nas_browser_profile"]
	if nfs.FSType != "nfs" || nfs.Source != "09aa765f-77ox.example.nas.aliyuncs.com:/share/vacuum-prod/users/profile" {
		t.Fatalf("nfs entry = %+v", nfs)
	}

	// The fixture line carries "/mnt/my\040data"; the kernel escape decodes
	// to a plain space so sandbox processes see "/mnt/my data".
	if got := byPoint["/mnt/my data"]; got.FSType != "ext4" {
		t.Fatalf("escaped mountpoint lost: %+v", byPoint)
	}
}

func TestRealMountEntries(t *testing.T) {
	entries := realMountEntries([]byte(kataMountinfo))

	var points []string
	for _, e := range entries {
		points = append(points, e.Mountpoint)
	}
	want := []string{
		"/",
		"/data",
		"/dev/shm",
		"/etc/hostname",
		"/home/admin/des/AI00003952",
		"/mnt/my data",
		"/mnt/nas_browser_profile",
		"/mnt/platform/skills",
		"/opt/opensandbox",
		"/tmp",
	}
	if !slices.Equal(points, want) {
		t.Fatalf("kept mountpoints = %v, want %v", points, want)
	}

	// Stacked mounts: the top (last) entry is the visible one.
	for _, e := range entries {
		if e.Mountpoint == "/data" && e.Source != "/dev/vdc2" {
			t.Fatalf("stacked mount kept wrong entry: %+v", e)
		}
	}
}

func TestRealMountEntriesGarbage(t *testing.T) {
	for _, data := range []string{"", "\n", "not a mountinfo line", "1 2 3:4 / / - ext4"} {
		if got := realMountEntries([]byte(data)); len(got) != 0 {
			t.Fatalf("realMountEntries(%q) = %+v, want none", data, got)
		}
	}
}

func TestIsPseudoDevice(t *testing.T) {
	for _, name := range []string{"loop0", "ram1", "fd0", "sr0", "zram2", "dm-0", "md0", "md127"} {
		if !isPseudoDevice(name) {
			t.Fatalf("isPseudoDevice(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"vda", "nvme0n1", "sda1", "pmem0"} {
		if isPseudoDevice(name) {
			t.Fatalf("isPseudoDevice(%q) = true, want false", name)
		}
	}
}

func TestIsPartitionDevice(t *testing.T) {
	for _, name := range []string{"sda1", "vdb12", "xvdf3", "hda9", "nvme0n1p2", "mmcblk0p1"} {
		if !isPartitionDevice(name) {
			t.Fatalf("isPartitionDevice(%q) = false, want true", name)
		}
	}
	// Whole disks: a trailing digit on a digit-suffixed base is a namespace
	// number, not a partition suffix (nvme0n12 = controller 0, namespace 12).
	for _, name := range []string{"sda", "vda", "nvme0n1", "nvme0n12", "mmcblk0", "pmem0"} {
		if isPartitionDevice(name) {
			t.Fatalf("isPartitionDevice(%q) = true, want false", name)
		}
	}
}

func countAllDevices(string) bool { return true }

func TestParseV1BlkioFile(t *testing.T) {
	data := []byte("8:0 Read 1000\n8:0 Write 2000\n8:0 Discard 5\n254:1 read 7\n8:0 Sync 3\n8:0 Total 3005\nbad line\n")
	read, written := parseV1BlkioFile(data, countAllDevices)
	if read != 1007 || written != 2000 {
		t.Fatalf("parseV1BlkioFile = (%d, %d), want (1007, 2000)", read, written)
	}
}

func TestParseV2IOStat(t *testing.T) {
	data := []byte("8:0 rbytes=100 wbytes=200 rios=10 wios=20 dios=5\n254:1 rbytes=1000 wbytes=2000 rios=30 wios=40\n")
	io := parseV2IOStat(data, countAllDevices)

	if io.ReadBytes != 1100 || io.WrittenBytes != 2200 {
		t.Fatalf("bytes = (%d, %d), want (1100, 2200)", io.ReadBytes, io.WrittenBytes)
	}
	if io.ReadOps != 40 || io.WriteOps != 60 {
		t.Fatalf("ops = (%d, %d), want (40, 60)", io.ReadOps, io.WriteOps)
	}
}
