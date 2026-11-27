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

package telemetry

import (
	"fmt"
	"sync"
	"time"

	gopsnet "github.com/shirou/gopsutil/net"
	"github.com/shirou/gopsutil/process"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/resources"
)

var systemProcessCount = func() (int64, error) {
	pids, err := process.Pids()
	return int64(len(pids)), err
}

var systemCPUUsagePercent = func() (float64, error) {
	return resources.CPUPercent(0)
}

var systemMemoryStats = resources.MemoryStats

var systemNetworkIOBytes = func() (inBytes int64, outBytes int64, err error) {
	counters, err := gopsnet.IOCounters(false)
	if err != nil {
		return 0, 0, err
	}
	if len(counters) == 0 {
		return 0, 0, fmt.Errorf("network IO counters unavailable")
	}
	return int64(counters[0].BytesRecv), int64(counters[0].BytesSent), nil
}

// systemDiskUsages returns per-mount usage snapshots; nil when the platform
// cannot provide them (the gauge must not report zeros instead). One scan
// is shared between the usage and total gauges: their observable-gauge
// callbacks run back to back within a collection, and a scan with many
// mounts can take seconds.
var diskUsageMemo struct {
	sync.Mutex
	at     time.Time
	usages []resources.DiskUsage
	have   bool
}

// diskUsageMemoTTL only dedupes the callbacks of a single collection; it is
// far shorter than any export period.
var diskUsageMemoTTL = 500 * time.Millisecond

var systemDiskUsages = func() []resources.DiskUsage {
	diskUsageMemo.Lock()
	defer diskUsageMemo.Unlock()
	if diskUsageMemo.have && time.Since(diskUsageMemo.at) < diskUsageMemoTTL {
		return diskUsageMemo.usages
	}
	// A platform error (non-Linux) yields nil and is memoized the same way,
	// so both gauges of one collection stay consistent instead of retrying.
	usages, _ := resources.DiskUsages()
	// Stamp after the scan: a slow scan (statfs timeouts on network mounts)
	// must not eat into the memo TTL, or the second gauge callback of the
	// same collection would rescan.
	diskUsageMemo.have, diskUsageMemo.at, diskUsageMemo.usages = true, time.Now(), usages
	return usages
}

// diskIORebaser rebases cumulative IO counters at binding handoffs: a pooled
// execd reassigned by POST /internal/init keeps the same process and the
// same underlying counters, and the totals accumulated under the previous
// binding must not be attributed to the new sandbox identity. Observed
// values are therefore cumulative since the current binding's first sample.
type diskIORebaser struct {
	mu       sync.Mutex
	gen      uint64
	haveGen  bool
	baseline resources.DiskIO
}

func (r *diskIORebaser) rebase(gen uint64, io resources.DiskIO) resources.DiskIO {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.haveGen || gen != r.gen {
		r.haveGen, r.gen, r.baseline = true, gen, io
	}
	return resources.DiskIO{
		ReadBytes:    max(io.ReadBytes-r.baseline.ReadBytes, 0),
		WrittenBytes: max(io.WrittenBytes-r.baseline.WrittenBytes, 0),
		ReadOps:      max(io.ReadOps-r.baseline.ReadOps, 0),
		WriteOps:     max(io.WriteOps-r.baseline.WriteOps, 0),
	}
}

var systemDiskIORebaser diskIORebaser

var systemDiskIO = func() (resources.DiskIO, bool) {
	io, err := resources.DiskIOStats()
	if err != nil {
		return resources.DiskIO{}, false
	}
	var gen uint64
	if b := binding.Current(); b != nil {
		gen = b.Generation
	}
	return systemDiskIORebaser.rebase(gen, io), true
}
