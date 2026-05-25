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

// procStatCPUJiffies parses the aggregate "cpu" line; idle counts idle+iowait (fields 4–5).
func procStatCPUJiffies(line string) (total, idle uint64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var nums []uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		nums = append(nums, v)
	}
	var sum uint64
	for _, v := range nums {
		sum += v
	}
	idle = nums[3]
	if len(nums) > 4 {
		idle += nums[4]
	}
	return sum, idle, true
}
