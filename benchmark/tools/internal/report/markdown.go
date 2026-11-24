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

// RenderSummaryMarkdown produces the human-readable summary.md accompanying report.json.
func RenderSummaryMarkdown(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# OpenSandbox Benchmark Summary (%s)\n\n", r.Provenance.ProfileName)
	fmt.Fprintf(&b, "- **Run ID**: `%s`\n", r.Provenance.RunID)
	fmt.Fprintf(&b, "- **Runtime**: `%s`\n", r.Provenance.Runtime)
	fmt.Fprintf(&b, "- **OpenSandbox**: `%s` (`%s`)\n", r.Provenance.OpenSandboxTag, r.Provenance.OpenSandboxCommit)
	fmt.Fprintf(&b, "- **fast-sandbox**: `%s` (`%s`)\n", r.Provenance.FastSandboxTag, r.Provenance.FastSandboxCommit)
	fmt.Fprintf(&b, "- **Timestamp (UTC)**: `%s`\n\n", r.Provenance.Timestamp.Format("2006-01-02T15:04:05Z"))

	suiteNames := make([]string, 0, len(r.Suites))
	for name := range r.Suites {
		suiteNames = append(suiteNames, name)
	}
	sort.Strings(suiteNames)

	b.WriteString("## Suite Results\n\n")
	if len(suiteNames) == 0 {
		b.WriteString("_No suites recorded._\n\n")
	} else {
		b.WriteString("| Suite | Status | Ops (OK/Total) | Throughput (RPS) | P50 / P95 / P99 (`t_first_ping` ms) |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, name := range suiteNames {
			s := r.Suites[name]
			lat := "-"
			if d, ok := s.Latencies["t_first_ping"]; ok && d.Count > 0 {
				lat = fmt.Sprintf("%.1f / %.1f / %.1f", d.P50Ms, d.P95Ms, d.P99Ms)
			}
			fmt.Fprintf(&b, "| `%s` | %s | %d / %d | %.2f | %s |\n",
				s.Name, s.Status, s.SucceededOps, s.TotalOps, s.ThroughputRPS, lat)
		}
		b.WriteString("\n")
	}

	for _, name := range suiteNames {
		s := r.Suites[name]
		if len(s.StageAttribution) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### P99 Bottleneck Attribution (`%s`)\n\n", name)
		b.WriteString("| Span | P50 Self (ms) | P99 Self (ms) | Critical Share (%) | Note |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, st := range s.StageAttribution {
			fmt.Fprintf(&b, "| `%s` | %.2f | %.2f | %.1f%% | %s |\n",
				st.SpanName, st.P50SelfMs, st.P99SelfMs, st.CriticalShare, st.Note)
		}
		b.WriteString("\n")
	}

	if r.LeakAudit != nil {
		status := "PASS"
		if !r.LeakAudit.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "## Leak & Stability Audit: %s\n\n", status)
		fmt.Fprintf(&b, "- Host OOM kills: `%d`, Pod cgroup OOM kills: `%d`\n", r.LeakAudit.HostOOMKills, r.LeakAudit.PodCgroupOOMKills)
		fmt.Fprintf(&b, "- Residual CRs (Sandbox / Snapshot): `%d / %d`\n", r.LeakAudit.ResidualSandboxCRs, r.LeakAudit.ResidualSnapshotCRs)
		fmt.Fprintf(&b, "- Stuck deleting slots: `%d`, Orphaned ublk devices: `%d`, Leaked Firecracker PIDs: `%d`\n",
			r.LeakAudit.StuckDeletingSlots, r.LeakAudit.OrphanedUblkDevices, r.LeakAudit.LeakedFirecrackerPIDs)
	}

	return b.String()
}
