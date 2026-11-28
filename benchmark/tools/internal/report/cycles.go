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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CycleRunInput holds the outcome of a single cycle for multi-cycle aggregation.
type CycleRunInput struct {
	Cycle  int
	Report *Report
	Err    error
}

// CycleSummaryReport is the N-cycle aggregate report written when --cycles > 1.
type CycleSummaryReport struct {
	SchemaVersion int                            `json:"schema_version"`
	RunID         string                         `json:"run_id"`
	ProfileName   string                         `json:"profile_name"`
	Runtime       string                         `json:"runtime"`
	TotalCycles   int                            `json:"total_cycles"`
	PassedCycles  int                            `json:"passed_cycles"`
	FailedCycles  int                            `json:"failed_cycles"`
	PassRate      float64                        `json:"pass_rate"`
	Timestamp     time.Time                      `json:"timestamp"`
	Suites        map[string]SuiteCycleAggregate `json:"suites"`
	Cycles        []CycleEntry                   `json:"cycles"`
}

// SuiteCycleAggregate summarizes a single suite across N consecutive cycles.
type SuiteCycleAggregate struct {
	Name         string           `json:"name"`
	CyclesRun    int              `json:"cycles_run"`
	CyclesPassed int              `json:"cycles_passed"`
	PassRate     float64          `json:"pass_rate"`
	MeanRPS      float64          `json:"mean_rps"`
	MinRPS       float64          `json:"min_rps"`
	MaxRPS       float64          `json:"max_rps"`
	TAPIAck      *CycleLatencyAgg `json:"t_api_ack,omitempty"`
	TFirstPing   *CycleLatencyAgg `json:"t_first_ping,omitempty"`
}

// CycleLatencyAgg holds aggregated P50/P95/P99 latencies across cycles.
type CycleLatencyAgg struct {
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

// CycleEntry records per-cycle pass/fail status and key metrics.
type CycleEntry struct {
	Cycle           int                        `json:"cycle"`
	RunID           string                     `json:"run_id"`
	Passed          bool                       `json:"passed"`
	LeakAuditPassed *bool                      `json:"leak_audit_passed,omitempty"`
	Error           string                     `json:"error,omitempty"`
	Suites          map[string]CycleSuiteBrief `json:"suites"`
}

// CycleSuiteBrief captures a compact per-cycle snapshot of one suite.
type CycleSuiteBrief struct {
	Status          string  `json:"status"`
	SucceededOps    int     `json:"succeeded_ops"`
	TotalOps        int     `json:"total_ops"`
	ThroughputRPS   float64 `json:"throughput_rps"`
	TAPIAckP50Ms    float64 `json:"t_api_ack_p50_ms,omitempty"`
	TAPIAckP95Ms    float64 `json:"t_api_ack_p95_ms,omitempty"`
	TAPIAckP99Ms    float64 `json:"t_api_ack_p99_ms,omitempty"`
	TFirstPingP50Ms float64 `json:"t_first_ping_p50_ms,omitempty"`
	TFirstPingP95Ms float64 `json:"t_first_ping_p95_ms,omitempty"`
	TFirstPingP99Ms float64 `json:"t_first_ping_p99_ms,omitempty"`
}

type suiteAccum struct {
	cyclesRun    int
	cyclesPassed int
	rpsSum       float64
	minRPS       float64
	maxRPS       float64
	ackCount     int
	ackP50Sum    float64
	ackP95Sum    float64
	ackP99Sum    float64
	pingCount    int
	pingP50Sum   float64
	pingP95Sum   float64
	pingP99Sum   float64
}

// AggregateCycles computes an N-cycle aggregate summary from individual cycle reports.
func AggregateCycles(runID, profileName, runtimeName string, inputs []CycleRunInput) *CycleSummaryReport {
	out := &CycleSummaryReport{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		ProfileName:   profileName,
		Runtime:       runtimeName,
		TotalCycles:   len(inputs),
		Timestamp:     time.Now().UTC(),
		Suites:        make(map[string]SuiteCycleAggregate),
		Cycles:        make([]CycleEntry, 0, len(inputs)),
	}

	accums := make(map[string]*suiteAccum)

	for i, in := range inputs {
		cycleNum := in.Cycle
		if cycleNum <= 0 {
			cycleNum = i + 1
		}
		entry := CycleEntry{
			Cycle:  cycleNum,
			Passed: in.Err == nil,
			Suites: make(map[string]CycleSuiteBrief),
		}
		if in.Err != nil {
			entry.Error = in.Err.Error()
		}

		if in.Report != nil {
			entry.RunID = in.Report.Provenance.RunID
			if out.ProfileName == "" {
				out.ProfileName = in.Report.Provenance.ProfileName
			}
			if out.Runtime == "" {
				out.Runtime = in.Report.Provenance.Runtime
			}
			if in.Report.LeakAudit != nil {
				p := in.Report.LeakAudit.Passed
				entry.LeakAuditPassed = &p
				if !p {
					entry.Passed = false
				}
			}
			for name, sr := range in.Report.Suites {
				if sr.Status != "passed" {
					entry.Passed = false
				}
				brief := CycleSuiteBrief{
					Status:        sr.Status,
					SucceededOps:  sr.SucceededOps,
					TotalOps:      sr.TotalOps,
					ThroughputRPS: sr.ThroughputRPS,
				}
				acc := accums[name]
				if acc == nil {
					acc = &suiteAccum{}
					accums[name] = acc
				}
				if acc.cyclesRun == 0 || sr.ThroughputRPS < acc.minRPS {
					acc.minRPS = sr.ThroughputRPS
				}
				if acc.cyclesRun == 0 || sr.ThroughputRPS > acc.maxRPS {
					acc.maxRPS = sr.ThroughputRPS
				}
				acc.cyclesRun++
				acc.rpsSum += sr.ThroughputRPS
				if sr.Status == "passed" {
					acc.cyclesPassed++
				}

				if d, ok := sr.Latencies["t_api_ack"]; ok && d.Count > 0 {
					brief.TAPIAckP50Ms = d.P50Ms
					brief.TAPIAckP95Ms = d.P95Ms
					brief.TAPIAckP99Ms = d.P99Ms
					acc.ackCount++
					acc.ackP50Sum += d.P50Ms
					acc.ackP95Sum += d.P95Ms
					acc.ackP99Sum += d.P99Ms
				}
				if d, ok := sr.Latencies["t_first_ping"]; ok && d.Count > 0 {
					brief.TFirstPingP50Ms = d.P50Ms
					brief.TFirstPingP95Ms = d.P95Ms
					brief.TFirstPingP99Ms = d.P99Ms
					acc.pingCount++
					acc.pingP50Sum += d.P50Ms
					acc.pingP95Sum += d.P95Ms
					acc.pingP99Sum += d.P99Ms
				}
				entry.Suites[name] = brief
			}
		}

		if entry.Passed {
			out.PassedCycles++
		} else {
			out.FailedCycles++
		}
		out.Cycles = append(out.Cycles, entry)
	}

	if out.TotalCycles > 0 {
		out.PassRate = float64(out.PassedCycles) / float64(out.TotalCycles)
	}

	for name, acc := range accums {
		agg := SuiteCycleAggregate{
			Name:         name,
			CyclesRun:    acc.cyclesRun,
			CyclesPassed: acc.cyclesPassed,
			MinRPS:       acc.minRPS,
			MaxRPS:       acc.maxRPS,
		}
		if acc.cyclesRun > 0 {
			agg.PassRate = float64(acc.cyclesPassed) / float64(acc.cyclesRun)
			agg.MeanRPS = acc.rpsSum / float64(acc.cyclesRun)
		}
		if acc.ackCount > 0 {
			n := float64(acc.ackCount)
			agg.TAPIAck = &CycleLatencyAgg{
				P50Ms: acc.ackP50Sum / n,
				P95Ms: acc.ackP95Sum / n,
				P99Ms: acc.ackP99Sum / n,
			}
		}
		if acc.pingCount > 0 {
			n := float64(acc.pingCount)
			agg.TFirstPing = &CycleLatencyAgg{
				P50Ms: acc.pingP50Sum / n,
				P95Ms: acc.pingP95Sum / n,
				P99Ms: acc.pingP99Sum / n,
			}
		}
		out.Suites[name] = agg
	}

	return out
}

// WriteBundle writes cycles-report.json and summary.md to outputDir.
func (c *CycleSummaryReport) WriteBundle(outputDir string) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create cycles report dir %q: %w", outputDir, err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cycles report json: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(outputDir, "cycles-report.json"), raw, 0o644); err != nil {
		return fmt.Errorf("write cycles-report.json: %w", err)
	}
	md := RenderCyclesSummaryMarkdown(c)
	if err := os.WriteFile(filepath.Join(outputDir, "summary.md"), []byte(md), 0o644); err != nil {
		return fmt.Errorf("write cycles summary.md: %w", err)
	}
	return nil
}

// RenderCyclesSummaryMarkdown renders a human-readable N-cycle aggregate summary.
func RenderCyclesSummaryMarkdown(c *CycleSummaryReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# OpenSandbox Multi-Cycle Benchmark Summary (%s)\n\n", c.ProfileName)
	fmt.Fprintf(&b, "- **Run ID**: `%s`\n", c.RunID)
	fmt.Fprintf(&b, "- **Runtime**: `%s`\n", c.Runtime)
	fmt.Fprintf(&b, "- **Cycles Passed**: `%d / %d` (`%.1f%%`)\n", c.PassedCycles, c.TotalCycles, c.PassRate*100.0)
	fmt.Fprintf(&b, "- **Timestamp (UTC)**: `%s`\n\n", c.Timestamp.Format("2006-01-02T15:04:05Z"))

	suiteNames := make([]string, 0, len(c.Suites))
	for name := range c.Suites {
		suiteNames = append(suiteNames, name)
	}
	sort.Strings(suiteNames)

	b.WriteString("## Aggregate Suite Summary\n\n")
	if len(suiteNames) == 0 {
		b.WriteString("_No suites recorded._\n\n")
	} else {
		b.WriteString("| Suite | Pass Rate | RPS (Mean / Min / Max) | `t_api_ack` P50/P95/P99 (ms) | `t_first_ping` P50/P95/P99 (ms) |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, name := range suiteNames {
			s := c.Suites[name]
			fmt.Fprintf(&b, "| `%s` | %d/%d (%.1f%%) | %.2f / %.2f / %.2f | %s | %s |\n",
				s.Name, s.CyclesPassed, s.CyclesRun, s.PassRate*100.0,
				s.MeanRPS, s.MinRPS, s.MaxRPS,
				formatCycleLatency(s.TAPIAck),
				formatCycleLatency(s.TFirstPing))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Per-Cycle Breakdown\n\n")
	b.WriteString("| Cycle | Run ID | Status | Leak Audit | Suites (RPS / `t_api_ack` P99 / `t_first_ping` P99) | Error |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, cyc := range c.Cycles {
		status := "PASS"
		if !cyc.Passed {
			status = "FAIL"
		}
		leakStr := "-"
		if cyc.LeakAuditPassed != nil {
			if *cyc.LeakAuditPassed {
				leakStr = "PASS"
			} else {
				leakStr = "FAIL"
			}
		}
		parts := make([]string, 0, len(cyc.Suites))
		cSuiteNames := make([]string, 0, len(cyc.Suites))
		for sn := range cyc.Suites {
			cSuiteNames = append(cSuiteNames, sn)
		}
		sort.Strings(cSuiteNames)
		for _, sn := range cSuiteNames {
			sb := cyc.Suites[sn]
			parts = append(parts, fmt.Sprintf("`%s`: %s (%.2f rps, ack=%.1fms, ping=%.1fms)",
				sn, sb.Status, sb.ThroughputRPS, sb.TAPIAckP99Ms, sb.TFirstPingP99Ms))
		}
		suitesCol := strings.Join(parts, "; ")
		if suitesCol == "" {
			suitesCol = "-"
		}
		errCol := cyc.Error
		if errCol == "" {
			errCol = "-"
		}
		fmt.Fprintf(&b, "| %d | `%s` | %s | %s | %s | %s |\n",
			cyc.Cycle, cyc.RunID, status, leakStr, suitesCol, errCol)
	}
	b.WriteString("\n")
	return b.String()
}

func formatCycleLatency(l *CycleLatencyAgg) string {
	if l == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f / %.1f / %.1f", l.P50Ms, l.P95Ms, l.P99Ms)
}
