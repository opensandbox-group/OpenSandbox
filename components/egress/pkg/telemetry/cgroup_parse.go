// Copyright 2026 Alibaba Group Holding Ltd.
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
	"strconv"
	"strings"
)

// cgroupSingleValueBytes reads a cgroup file holding one integer: memory.current on v2,
// memory.usage_in_bytes on v1. Returns false for "max" and for anything unparseable, so a
// caller never mistakes an unset limit or a garbled read for a measurement.
func cgroupSingleValueBytes(data []byte) (int64, bool) {
	field := strings.TrimSpace(string(data))
	if field == "" || field == "max" {
		return 0, false
	}
	value, err := strconv.ParseInt(field, 10, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// cgroupCPUSecondsFromStat pulls usage_usec out of a cgroup v2 cpu.stat.
func cgroupCPUSecondsFromStat(data []byte) (float64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		field, ok := strings.CutPrefix(strings.TrimSpace(line), "usage_usec ")
		if !ok {
			continue
		}
		micros, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || micros < 0 {
			return 0, false
		}
		return float64(micros) / 1e6, true
	}
	return 0, false
}

// cgroupCPUSecondsFromNanos converts a cgroup v1 cpuacct.usage (nanoseconds) to seconds.
func cgroupCPUSecondsFromNanos(data []byte) (float64, bool) {
	nanos, ok := cgroupSingleValueBytes(data)
	if !ok {
		return 0, false
	}
	return float64(nanos) / 1e9, true
}
