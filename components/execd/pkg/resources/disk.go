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
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DiskUsage is a per-mount filesystem usage snapshot.
type DiskUsage struct {
	// Mountpoint is the visible mount path.
	Mountpoint string
	// FSType is the filesystem type (e.g. ext4, virtiofs, nfs4, fuse.ossfs2).
	FSType string
	// Source is the mount source: a device path or remote target.
	Source string
	// TotalBytes is the total data-block capacity as reported by statfs.
	TotalBytes int64
	// UsedBytes is TotalBytes minus free bytes.
	UsedBytes int64
	// AvailBytes is the capacity available to unprivileged processes.
	AvailBytes int64
}

// DiskIO contains source-lifetime counters, not execd-relative totals; consumers derive rates from deltas.
type DiskIO struct {
	// ReadBytes / WrittenBytes are cumulative bytes transferred.
	ReadBytes    int64
	WrittenBytes int64
	// ReadOps / WriteOps are cumulative completed I/O operations.
	ReadOps  int64
	WriteOps int64
}

// DiskUsages reports a usage snapshot for every visible real filesystem
// mount: pseudo trees (proc/sys/dev plumbing) are excluded, stacked mounts
// are resolved to the visible one, and mounts whose statfs fails or times
// out (e.g. a stale NFS export) are skipped instead of reported as zeros.
func DiskUsages() ([]DiskUsage, error) {
	return diskUsages()
}

// DiskIOStats reports cumulative sandbox disk I/O counters. The sandbox
// cgroup's IO accounting is preferred; /proc/diskstats is the fallback.
func DiskIOStats() (DiskIO, error) {
	return diskIO()
}

// mountEntry is one parsed /proc/self/mountinfo line.
type mountEntry struct {
	Mountpoint string
	FSType     string
	Source     string
}

// parseMountinfo parses mount points, filesystem types, and sources out of
// /proc/self/mountinfo. Malformed lines are skipped and the kernel's octal
// escapes (\040, \011, \012, \134) are decoded.
func parseMountinfo(data []byte) []mountEntry {
	var entries []mountEntry
	for _, line := range strings.Split(string(data), "\n") {
		left, right, found := strings.Cut(strings.TrimSpace(line), " - ")
		if !found {
			continue
		}
		fields := strings.Fields(left)
		opts := strings.SplitN(right, " ", 3)
		// left: id parent major:minor root mountpoint mount-options [optional...]
		// right: fstype source super-options
		if len(fields) < 6 || len(opts) < 2 {
			continue
		}
		entries = append(entries, mountEntry{
			Mountpoint: unescapeMountPath(fields[4]),
			FSType:     opts[0],
			Source:     unescapeMountPath(opts[1]),
		})
	}
	return entries
}

// unescapeMountPath decodes the kernel's octal escapes in mountinfo fields.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// pseudoFS lists filesystem types that carry kernel or plumbing state
// rather than sandbox data.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"mqueue": true, "cgroup": true, "cgroup2": true, "securityfs": true,
	"debugfs": true, "tracefs": true, "configfs": true, "pstore": true,
	"bpf": true, "autofs": true, "hugetlbfs": true, "ramfs": true,
	"binfmt_misc": true, "fusectl": true, "nsfs": true, "rpc_pipefs": true,
}

// realMountEntries filters mountinfo down to filesystems that hold sandbox
// data. A mount point is reported once: the last mountinfo entry for it is
// the one stacked on top and the only one a sandbox process can see.
func realMountEntries(data []byte) []mountEntry {
	kept := make(map[string]mountEntry, 32)
	for _, entry := range parseMountinfo(data) {
		if pseudoFS[entry.FSType] || !realMountpoint(entry.FSType, entry.Mountpoint) {
			continue
		}
		kept[entry.Mountpoint] = entry
	}
	points := slices.Sorted(maps.Keys(kept))
	out := make([]mountEntry, 0, len(points))
	for _, point := range points {
		out = append(out, kept[point])
	}
	return out
}

// realMountpoint decides whether a mount path holds sandbox data. Kernel
// trees are skipped regardless of filesystem type; tmpfs counts as data
// everywhere except the per-boot plumbing directories, with /dev/shm as the
// classic data exception.
func realMountpoint(fstype, mountpoint string) bool {
	if underAny(mountpoint, "/proc", "/sys") {
		return false
	}
	if fstype != "tmpfs" {
		return true
	}
	if mountpoint == "/dev/shm" {
		return true
	}
	return !underAny(mountpoint, "/dev", "/run")
}

// underAny reports whether path is prefix itself or lies beneath it.
func underAny(path string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// parseDiskstats is delegated to gopsutil's disk.IOCounters, which owns the
// /proc/diskstats format parsing; this package only applies the sandbox
// device-filtering policy on top.

// isPseudoDevice reports whether a diskstats device name is kernel plumbing
// or a virtual stacking layer whose throughput is already charged elsewhere
// (device-mapper and soft-raid I/O also lands on their underlying devices),
// so summing both levels would double-count.
func isPseudoDevice(name string) bool {
	for _, prefix := range []string{"loop", "ram", "fd", "sr", "zram", "dm", "md"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// diskstatsPartition matches partition device names: sda1, vdb12, xvdz3 on
// whole-disk lettering, and explicit pN suffixes on digit-suffixed bases
// (nvme0n1p2, mmcblk0p1). A plain trailing digit is NOT enough for the
// digit-suffixed bases: nvme0n12 is a whole disk (namespace 12), not a
// partition. The kernel charges partition I/O to the whole device as well,
// so an aggregate must not sum both layers.
var diskstatsPartition = regexp.MustCompile(`^(?:(?:sd|hd|vd|xvd)[a-z]+\d+|nvme\d+n\d+p\d+|mmcblk\d+p\d+)$`)

// isPartitionDevice reports whether a diskstats device name is a partition
// of a whole disk.
func isPartitionDevice(name string) bool {
	return diskstatsPartition.MatchString(name)
}

// countedDevice reports whether a block device's I/O belongs in the sandbox
// aggregate: pseudo and stacked devices and partitions are left out, since
// the same I/O is also charged to the disk underneath them.
func countedDevice(name string) bool {
	return !isPseudoDevice(name) && !isPartitionDevice(name)
}

// parseV1BlkioFile sums the Read and Write values of a cgroup v1 blkio
// throttle file: lines of "major:minor <op> <count>". Other operations
// (Discard, Sync, Async, Total) are ignored, and so are devices for which
// counted returns false.
func parseV1BlkioFile(data []byte, counted func(majMin string) bool) (read, written int64) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !counted(fields[0]) {
			continue
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || count < 0 {
			continue
		}
		switch strings.ToLower(fields[1]) {
		case "read":
			read += count
		case "write":
			written += count
		}
	}
	return read, written
}

// parseV2IOStat sums rbytes/wbytes/rios/wios across the per-device lines of
// a cgroup v2 io.stat file, skipping devices for which counted returns false.
func parseV2IOStat(data []byte, counted func(majMin string) bool) DiskIO {
	var totals DiskIO
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !counted(fields[0]) {
			continue
		}
		for _, kv := range fields[1:] {
			key, value, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			count, err := strconv.ParseInt(value, 10, 64)
			if err != nil || count < 0 {
				continue
			}
			switch key {
			case "rbytes":
				totals.ReadBytes += count
			case "wbytes":
				totals.WrittenBytes += count
			case "rios":
				totals.ReadOps += count
			case "wios":
				totals.WriteOps += count
			}
		}
	}
	return totals
}
