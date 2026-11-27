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
	"fmt"
	"sort"
	"strings"
)

// CompareThresholds controls regression detection for opensandbox-bench compare.
type CompareThresholds struct {
	MaxLatencyRegressionPct    float64 // default 15.0%
	MaxThroughputDropPct       float64 // default 10.0%
	MaxPerSandboxOverheadPct   float64 // default 15.0%
	FailOnLeakAuditFailure     bool    // default true
}

func DefaultCompareThresholds() CompareThresholds {
	return CompareThresholds{
		MaxLatencyRegressionPct:  15.0,
		MaxThroughputDropPct:     10.0,
		MaxPerSandboxOverheadPct: 15.0,
		FailOnLeakAuditFailure:   true,
	}
}

type DiffRow struct {
	Suite      string  `json:"suite"`
	Metric     string  `json:"metric"`
	Baseline   float64 `json:"baseline"`
	Candidate  float64 `json:"candidate"`
	DeltaPct   float64 `json:"delta_pct"`
	Regression bool    `json:"regression"`
}

type ComparisonResult struct {
	Rows            []DiffRow `json:"rows"`
	Regressions     []string  `json:"regressions"`
	HasRegression   bool      `json:"has_regression"`
	MarkdownSummary string    `json:"markdown_summary"`
}

// Compare diffs two benchmark reports against the supplied regression thresholds.
func Compare(base, cand *Report, th CompareThresholds) *ComparisonResult {
	res := &ComparisonResult{}

	suiteNames := make([]string, 0, len(base.Suites))
	for name := range base.Suites {
		if _, ok := cand.Suites[name]; ok {
			suiteNames = append(suiteNames, name)
		}
	}
	sort.Strings(suiteNames)

	for _, name := range suiteNames {
		bs := base.Suites[name]
		cs := cand.Suites[name]

		if bs.ThroughputRPS > 0 {
			delta := pctChange(bs.ThroughputRPS, cs.ThroughputRPS)
			reg := delta < -th.MaxThroughputDropPct
			res.Rows = append(res.Rows, DiffRow{
				Suite: name, Metric: "throughput_rps",
				Baseline: bs.ThroughputRPS, Candidate: cs.ThroughputRPS,
				DeltaPct: delta, Regression: reg,
			})
			if reg {
				res.Regressions = append(res.Regressions, fmt.Sprintf("%s throughput_rps dropped %.1f%% (%.2f -> %.2f)", name, -delta, bs.ThroughputRPS, cs.ThroughputRPS))
			}
		}

		latNames := make([]string, 0, len(bs.Latencies))
		for k := range bs.Latencies {
			if _, ok := cs.Latencies[k]; ok {
				latNames = append(latNames, k)
			}
		}
		sort.Strings(latNames)

		for _, lk := range latNames {
			bl := bs.Latencies[lk]
			cl := cs.Latencies[lk]
			for _, pair := range []struct {
				suffix string
				bVal   float64
				cVal   float64
				gated  bool
			}{
				{"p50_ms", bl.P50Ms, cl.P50Ms, false},
				{"p95_ms", bl.P95Ms, cl.P95Ms, true},
				{"p99_ms", bl.P99Ms, cl.P99Ms, true},
			} {
				if pair.bVal <= 0 {
					continue
				}
				delta := pctChange(pair.bVal, pair.cVal)
				reg := pair.gated && delta > th.MaxLatencyRegressionPct
				metric := fmt.Sprintf("%s.%s", lk, pair.suffix)
				res.Rows = append(res.Rows, DiffRow{
					Suite: name, Metric: metric,
					Baseline: pair.bVal, Candidate: pair.cVal,
					DeltaPct: delta, Regression: reg,
				})
				if reg {
					res.Regressions = append(res.Regressions, fmt.Sprintf("%s %s regressed +%.1f%% (%.2f -> %.2f)", name, metric, delta, pair.bVal, pair.cVal))
				}
			}
		}
	}

	if base.LoadProfiles != nil && cand.LoadProfiles != nil &&
		base.LoadProfiles.Fastlet != nil && cand.LoadProfiles.Fastlet != nil &&
		base.LoadProfiles.Fastlet.PerSandboxResidentMiB > 0 {
		bMem := base.LoadProfiles.Fastlet.PerSandboxResidentMiB
		cMem := cand.LoadProfiles.Fastlet.PerSandboxResidentMiB
		delta := pctChange(bMem, cMem)
		reg := delta > th.MaxPerSandboxOverheadPct
		res.Rows = append(res.Rows, DiffRow{
			Suite: "load_profiles", Metric: "fastlet.per_sandbox_resident_mib",
			Baseline: bMem, Candidate: cMem, DeltaPct: delta, Regression: reg,
		})
		if reg {
			res.Regressions = append(res.Regressions, fmt.Sprintf("fastlet per_sandbox_resident_mib regressed +%.1f%% (%.2f -> %.2f)", delta, bMem, cMem))
		}
	}

	if th.FailOnLeakAuditFailure && cand.LeakAudit != nil && !cand.LeakAudit.Passed {
		res.Regressions = append(res.Regressions, "candidate leak_audit failed")
	}

	res.HasRegression = len(res.Regressions) > 0
	res.MarkdownSummary = renderComparisonMarkdown(base, cand, res)
	return res
}

func pctChange(base, cand float64) float64 {
	if base == 0 {
		return 0
	}
	return ((cand - base) / base) * 100.0
}

func renderComparisonMarkdown(base, cand *Report, res *ComparisonResult) string {
	var b strings.Builder
	b.WriteString("# Benchmark Regression Comparison\n\n")
	fmt.Fprintf(&b, "- **Baseline**: `%s` (`%s` / fast-sandbox `%s`)\n",
		base.Provenance.OpenSandboxTag, base.Provenance.OpenSandboxCommit, base.Provenance.FastSandboxCommit)
	fmt.Fprintf(&b, "- **Candidate**: `%s` (`%s` / fast-sandbox `%s`)\n\n",
		cand.Provenance.OpenSandboxTag, cand.Provenance.OpenSandboxCommit, cand.Provenance.FastSandboxCommit)

	b.WriteString("| Suite | Metric | Baseline | Candidate | Delta | Status |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, row := range res.Rows {
		status := "OK"
		if row.Regression {
			status = "REGRESSION"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %.2f | %.2f | %+.1f%% | %s |\n",
			row.Suite, row.Metric, row.Baseline, row.Candidate, row.DeltaPct, status)
	}
	if len(res.Regressions) > 0 {
		b.WriteString("\n## Regressions Detected\n\n")
		for _, r := range res.Regressions {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	return b.String()
}
