// Copyright 2025 Alibaba Group Holding Ltd.
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

package model

import "time"

type Metrics struct {
	CpuCount    float64 `json:"cpu_count"`
	CpuUsedPct  float64 `json:"cpu_used_pct"`
	MemTotalMiB float64 `json:"mem_total_mib"`
	MemUsedMiB  float64 `json:"mem_used_mib"`
	Timestamp   int64   `json:"timestamp"`
}

func NewMetrics() *Metrics {
	return &Metrics{
		CpuCount:    0,
		CpuUsedPct:  0,
		MemTotalMiB: 0,
		MemUsedMiB:  0,
		Timestamp:   time.Now().UnixMilli(),
	}
}
