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
	"sync"
	"time"

	gopsdisk "github.com/shirou/gopsutil/disk"
	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/cgroupfs"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// Test-overridable inputs.
var (
	cgroupV1BlkioDir = "/sys/fs/cgroup/blkio"
	// sysDevBlockDir links each block device number (MAJ:MIN) to its
	// device; a variable so tests can fake it.
	sysDevBlockDir = "/sys/dev/block"
	// statfsTimeout bounds each per-mount statfs: a stale network mount
	// (NFS hard mount, hung FUSE daemon) can otherwise block the caller
	// for minutes. A variable so tests can shrink it.
	statfsTimeout = 2 * time.Second
	// statfsSyscall is the syscall behind statfsWithTimeout; a variable so
	// tests can simulate mounts that never return.
	statfsSyscall = unix.Statfs
)

type statfsResult struct {
	st  unix.Statfs_t
	err error
}

// statfsCall is the in-flight statfs of one mount point.
type statfsCall struct {
	ch chan statfsResult
	// hung marks a call that already timed out once: later collections
	// skip it without re-paying the timeout, until the kernel returns.
	hung bool
}

// statfsPending holds the in-flight statfs call per mount point. A call
// that hit the timeout keeps its entry: its goroutine is still blocked in
// the syscall, and the next collection reuses the same channel instead of
// stacking a new one, so a permanently hung mount leaks at most one
// goroutine for the whole process lifetime. Callers for one mount are
// serialized upstream (one scan per collection, shared between the usage
// gauges), so the channel's single result always reaches its only waiter.
var (
	statfsMu      sync.Mutex
	statfsPending = map[string]*statfsCall{}
)

// diskUsages snapshots every visible real mount. Per-mount failures are
// skipped best-effort: one broken mount must not hide the others.
func diskUsages() ([]DiskUsage, error) {
	data, err := os.ReadFile(procSelfMountinfo)
	if err != nil {
		return nil, err
	}
	var usages []DiskUsage
	for _, entry := range realMountEntries(data) {
		st, ok := statfsWithTimeout(entry.Mountpoint)
		if !ok {
			continue
		}
		// Block counts are in fragment units, as df reads them. f_bsize is
		// only the preferred I/O size and can differ, e.g. on FUSE mounts.
		total := int64(st.Blocks) * int64(st.Frsize)
		free := int64(st.Bfree) * int64(st.Frsize)
		avail := int64(st.Bavail) * int64(st.Frsize)
		usages = append(usages, DiskUsage{
			Mountpoint: entry.Mountpoint,
			FSType:     entry.FSType,
			Source:     entry.Source,
			TotalBytes: total,
			UsedBytes:  max(total-free, 0),
			AvailBytes: max(avail, 0),
		})
	}
	return usages, nil
}

// statfsWithTimeout runs statfs off-thread so a hung mount cannot stall the
// metrics pipeline. The syscall itself is not interruptible: on timeout the
// goroutine stays blocked until the kernel returns, its pending entry makes
// later collections reuse it instead of spawning fresh calls, and those
// collections skip the mount without re-paying the full timeout.
func statfsWithTimeout(path string) (unix.Statfs_t, bool) {
	statfsMu.Lock()
	call, pending := statfsPending[path]
	if !pending {
		call = &statfsCall{ch: make(chan statfsResult, 1)}
		statfsPending[path] = call
		statfsMu.Unlock()
		go func() {
			var st unix.Statfs_t
			err := statfsSyscall(path, &st)
			call.ch <- statfsResult{st, err}
		}()
	} else if call.hung {
		// A previous collection already waited the full timeout on this
		// call. Peek without blocking: if the kernel finally returned,
		// consume the result and resume normal scanning; otherwise skip
		// this mount for now at O(1) cost.
		statfsMu.Unlock()
		select {
		case r := <-call.ch:
			statfsMu.Lock()
			delete(statfsPending, path)
			statfsMu.Unlock()
			return r.st, r.err == nil
		default:
			return unix.Statfs_t{}, false
		}
	} else {
		statfsMu.Unlock()
	}
	select {
	case r := <-call.ch:
		statfsMu.Lock()
		delete(statfsPending, path)
		statfsMu.Unlock()
		if r.err != nil {
			return unix.Statfs_t{}, false
		}
		return r.st, true
	case <-time.After(statfsTimeout):
		statfsMu.Lock()
		call.hung = true
		statfsMu.Unlock()
		log.Warn("resources: statfs %s timed out, skipping mount until it returns", path)
		// Keep the pending entry: the blocked syscall owns its goroutine.
		return unix.Statfs_t{}, false
	}
}

// diskIOReader reads one cumulative DiskIO snapshot from a fixed source.
type diskIOReader func() (DiskIO, error)

// diskIOSource selects the IO source once per process, mirroring the
// CPU/memory backend pattern: the sandbox cgroup's IO accounting when
// visible, /proc/diskstats otherwise.
var diskIOSource = sync.OnceValue(newDiskIOSource)

func diskIO() (DiskIO, error) {
	return diskIOSource()()
}

// newDiskIOSource picks the IO accounting source: cgroup v2 io.stat, then
// cgroup v1 blkio throttle files, then /proc/diskstats.
func newDiskIOSource() diskIOReader {
	data, err := os.ReadFile(procSelfCgroupFile)
	if err == nil {
		entries := cgroupfs.ParseEntries(data)
		if rel := cgroupfs.V2RelPath(entries); rel != "" {
			if mount, mountErr := cgroupV2Mount(); mountErr == nil {
				path := filepath.Join(mount, filepath.FromSlash(rel), "io.stat")
				if reader := v2IOReader(path); reader != nil {
					log.Info("resources: reading sandbox disk IO from cgroup v2 io.stat %s", path)
					return reader
				}
			}
		}
		if rel := cgroupfs.V1RelPath(entries, "blkio"); rel != "" {
			if dir, mountErr := cgroupV1MountDir([]string{cgroupV1BlkioDir}, "blkio"); mountErr == nil {
				// Unreadable mountinfo keeps the plain mount point + cgroup path join.
				mountinfo, _ := os.ReadFile(procSelfMountinfo)
				base := cgroupfs.Dir(mountinfo, dir, rel)
				reader := v1IOReader(
					filepath.Join(base, "blkio.throttle.io_service_bytes"),
					filepath.Join(base, "blkio.throttle.io_serviced"),
				)
				if reader != nil {
					log.Info("resources: reading sandbox disk IO from cgroup v1 blkio %s", base)
					return reader
				}
			}
		}
	}
	// Fallback: /proc/diskstats through gopsutil. In a microVM guest /proc
	// already describes the whole sandbox machine; in a plain container it
	// is host-wide, the same caveat as the CPU/memory host fallback.
	log.Warn("resources: sandbox cgroup IO accounting unavailable, using /proc/diskstats for disk IO")
	return func() (DiskIO, error) {
		counters, err := gopsdisk.IOCounters()
		if err != nil {
			return DiskIO{}, err
		}
		var totals DiskIO
		for name, counter := range counters {
			// Skip pseudo devices and partitions: the kernel charges
			// partition I/O to the whole device too, and summing both
			// layers double-counts.
			if !countedDevice(name) {
				continue
			}
			totals.ReadOps += int64(counter.ReadCount)
			totals.WriteOps += int64(counter.WriteCount)
			totals.ReadBytes += int64(counter.ReadBytes)
			totals.WrittenBytes += int64(counter.WriteBytes)
		}
		return totals, nil
	}
}

// countedDeviceNumber applies countedDevice to a cgroup IO line's MAJ:MIN.
// The cgroup charges a bio on every layer it is submitted through, so a
// device-mapper or loop device and the disk under it both carry the same
// I/O. A device sysfs cannot name is counted.
func countedDeviceNumber(majMin string) bool {
	target, err := os.Readlink(filepath.Join(sysDevBlockDir, majMin))
	if err != nil {
		return true
	}
	return countedDevice(filepath.Base(target))
}

// v2IOReader returns a reader over a cgroup v2 io.stat file, or nil when
// the file is missing.
func v2IOReader(path string) diskIOReader {
	if !fileExists(path) {
		return nil
	}
	return func() (DiskIO, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return DiskIO{}, err
		}
		return parseV2IOStat(data, countedDeviceNumber), nil
	}
}

// v1IOReader returns a reader over the cgroup v1 blkio throttle files, or
// nil when either file is missing.
func v1IOReader(bytesPath, opsPath string) diskIOReader {
	if !fileExists(bytesPath) || !fileExists(opsPath) {
		return nil
	}
	return func() (DiskIO, error) {
		bytesData, err := os.ReadFile(bytesPath)
		if err != nil {
			return DiskIO{}, err
		}
		opsData, err := os.ReadFile(opsPath)
		if err != nil {
			return DiskIO{}, err
		}
		readBytes, writtenBytes := parseV1BlkioFile(bytesData, countedDeviceNumber)
		readOps, writeOps := parseV1BlkioFile(opsData, countedDeviceNumber)
		return DiskIO{
			ReadBytes:    readBytes,
			WrittenBytes: writtenBytes,
			ReadOps:      readOps,
			WriteOps:     writeOps,
		}, nil
	}
}
