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

package report

import (
	"math"
	"sort"
)

// ComputeLatencyDist calculates nearest-rank percentiles (P50, P90, P95, P99),
// Min, Max, and Mean from millisecond latency samples.
func ComputeLatencyDist(samplesMs []float64) LatencyDist {
	n := len(samplesMs)
	if n == 0 {
		return LatencyDist{}
	}
	sorted := make([]float64, n)
	copy(sorted, samplesMs)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}

	return LatencyDist{
		Count:  n,
		MinMs:  sorted[0],
		P50Ms:  nearestRank(sorted, 50),
		P90Ms:  nearestRank(sorted, 90),
		P95Ms:  nearestRank(sorted, 95),
		P99Ms:  nearestRank(sorted, 99),
		MaxMs:  sorted[n-1],
		MeanMs: sum / float64(n),
	}
}

func nearestRank(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil((p / 100.0) * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
