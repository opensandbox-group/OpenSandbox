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

// meminfoUsedBytesFromContent approximates used RAM from kB fields (MemTotal−MemAvailable preferred).
func meminfoUsedBytesFromContent(data []byte) int64 {
	var memTotal, memAvail, memFree int64
	var haveT, haveA, haveF bool
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			memTotal = meminfoFieldKB(line)
			haveT = memTotal > 0
		case strings.HasPrefix(line, "MemAvailable:"):
			memAvail = meminfoFieldKB(line)
			haveA = true
		case strings.HasPrefix(line, "MemFree:"):
			memFree = meminfoFieldKB(line)
			haveF = true
		}
	}
	if haveT && haveA && memTotal >= memAvail {
		return (memTotal - memAvail) * 1024
	}
	if haveT && haveF && memTotal >= memFree {
		return (memTotal - memFree) * 1024
	}
	return 0
}

func meminfoFieldKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}
