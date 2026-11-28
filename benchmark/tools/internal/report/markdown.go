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
	fmt.Fprintf(&b, "- **Timestamp (UTC)**: `%s`\n", r.Provenance.Timestamp.Format("2006-01-02T15:04:05Z"))
	if r.TelemetryStatus != "" {
		fmt.Fprintf(&b, "- **Telemetry Status**: `%s`\n", r.TelemetryStatus)
		for _, m := range r.TelemetryMissing {
			fmt.Fprintf(&b, "  - Missing: %s\n", m)
		}
	}
	b.WriteString("\n")

	suiteNames := make([]string, 0, len(r.Suites))
	for name := range r.Suites {
		suiteNames = append(suiteNames, name)
	}
	sort.Strings(suiteNames)

	b.WriteString("## Suite Results\n\n")
	if len(suiteNames) == 0 {
		b.WriteString("_No suites recorded._\n\n")
	} else {
		b.WriteString("| Suite | Status | Ops (OK/Total) | Throughput (RPS) | `t_api_ack` P50/P95/P99 (ms) | `t_first_ping` P50/P95/P99 (ms) | `t_first_exec` P50/P95/P99 (ms) |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, name := range suiteNames {
			s := r.Suites[name]
			fmt.Fprintf(&b, "| `%s` | %s | %d / %d | %.2f | %s | %s | %s |\n",
				s.Name, s.Status, s.SucceededOps, s.TotalOps, s.ThroughputRPS,
				formatLatencyTriple(s.Latencies, "t_api_ack"),
				formatLatencyTriple(s.Latencies, "t_first_ping"),
				formatLatencyTriple(s.Latencies, "t_first_exec"))
		}
		b.WriteString("\n")
	}

	renderOverlayBDMarkdown(&b, suiteNames, r.Suites)
	renderDensityRampMarkdown(&b, suiteNames, r.Suites)
	renderRootfsOnlyRestoreMarkdown(&b, suiteNames, r.Suites)
	renderErrorBreakdownMarkdown(&b, suiteNames, r.Suites)

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

	if r.LoadProfiles != nil {
		renderLoadProfilesMarkdown(&b, r.LoadProfiles)
	}

	if r.LeakAudit != nil {
		status := "PASS"
		if !r.LeakAudit.Passed {
			if r.LeakAudit.Incomplete {
				status = "FAIL (INCOMPLETE)"
			} else {
				status = "FAIL"
			}
		}
		fmt.Fprintf(&b, "## Leak & Stability Audit: %s\n\n", status)
		fmt.Fprintf(&b, "- Host OOM kills: `%d`, Pod cgroup OOM kills: `%d`\n", r.LeakAudit.HostOOMKills, r.LeakAudit.PodCgroupOOMKills)
		fmt.Fprintf(&b, "- Residual CRs (Sandbox / Snapshot): `%d / %d`\n", r.LeakAudit.ResidualSandboxCRs, r.LeakAudit.ResidualSnapshotCRs)
		if r.LeakAudit.CRConvergenceSeconds > 0 {
			fmt.Fprintf(&b, "- CR cleanup convergence: `%.2fs`\n", r.LeakAudit.CRConvergenceSeconds)
		}
		fmt.Fprintf(&b, "- Stuck deleting slots: `%d`, Orphaned ublk devices: `%d`, Leaked Firecracker PIDs: `%d`\n",
			r.LeakAudit.StuckDeletingSlots, r.LeakAudit.OrphanedUblkDevices, r.LeakAudit.LeakedFirecrackerPIDs)
		if r.LeakAudit.BaselineCleanSlots > 0 || len(r.LeakAudit.AdmissionSlotsByState) > 0 {
			fmt.Fprintf(&b, "- Admission slots conservation: baseline clean=`%d`, after clean=`%d`, lost=`%d`, non-clean residual=`%d`\n",
				r.LeakAudit.BaselineCleanSlots, r.LeakAudit.CleanSlotsAfter, r.LeakAudit.LostCleanSlots, r.LeakAudit.NonCleanResidualSlots)
		}
		if r.LeakAudit.StateRootFreeHeadroomG > 0 {
			fmt.Fprintf(&b, "- StateRoot free headroom: `%.2f GiB`\n", r.LeakAudit.StateRootFreeHeadroomG)
		}
		if len(r.LeakAudit.Checks) > 0 {
			checkNames := make([]string, 0, len(r.LeakAudit.Checks))
			for k := range r.LeakAudit.Checks {
				checkNames = append(checkNames, k)
			}
			sort.Strings(checkNames)
			parts := make([]string, 0, len(checkNames))
			for _, k := range checkNames {
				parts = append(parts, fmt.Sprintf("%s=%s", k, r.LeakAudit.Checks[k]))
			}
			fmt.Fprintf(&b, "- Checks: `%s`\n", strings.Join(parts, ", "))
		}
		for _, w := range r.LeakAudit.Warnings {
			fmt.Fprintf(&b, "- Warning: %s\n", w)
		}
		for _, f := range r.LeakAudit.Failures {
			fmt.Fprintf(&b, "- Failure: %s\n", f)
		}
	}

	return b.String()
}

func formatLatencyTriple(latencies map[string]LatencyDist, key string) string {
	if d, ok := latencies[key]; ok && d.Count > 0 {
		return fmt.Sprintf("%.1f / %.1f / %.1f", d.P50Ms, d.P95Ms, d.P99Ms)
	}
	return "-"
}

func renderOverlayBDMarkdown(b *strings.Builder, suiteNames []string, suites map[string]SuiteResult) {
	hasOBD := false
	for _, name := range suiteNames {
		if suites[name].OverlayBDMetrics != nil {
			hasOBD = true
			break
		}
	}
	if !hasOBD {
		return
	}
	const mib = 1024.0 * 1024.0
	b.WriteString("### OverlayBD & Storage Metrics\n\n")
	b.WriteString("| Suite | Pool Hit Rate | Pool Idle (Min/End) | Daemon Restarts | StateRoot Du/Sbx (MiB) | StateRoot Free (GiB) | Block Cache (MiB) | Blob Cache (MiB / Files) |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, name := range suiteNames {
		obd := suites[name].OverlayBDMetrics
		if obd == nil {
			continue
		}
		duPerSbx := "-"
		if obd.StateRootDuBytesPerSbx > 0 {
			duPerSbx = fmt.Sprintf("%.1f", float64(obd.StateRootDuBytesPerSbx)/mib)
		}
		fmt.Fprintf(b, "| `%s` | %.1f%% (%d/%d) | %d / %d | %d | %s | %.2f | %.1f | %.1f / %d |\n",
			name,
			obd.UblkdPoolHitRate*100.0, obd.UblkdPoolHitsDelta, obd.UblkdPoolHitsDelta+obd.UblkdPoolMissesDelta,
			obd.UblkdPoolIdleMin, obd.UblkdPoolIdleEnd,
			obd.UblkdDaemonRestarts,
			duPerSbx,
			obd.StateRootFreeHeadroomGi,
			float64(obd.OverlayBDBlockCacheBytes)/mib,
			float64(obd.FSBBlobCacheBytes)/mib, obd.FSBBlobCacheFileCount)
	}
	b.WriteString("\n")
}

func renderLoadProfilesMarkdown(b *strings.Builder, lp *LoadProfiles) {
	components := []struct {
		name string
		data *ComponentLoadSummary
	}{
		{"host", lp.Host},
		{"fastlet", lp.Fastlet},
		{"egress", lp.Egress},
		{"fastlet_proxy", lp.FastletProxy},
		{"firecracker_runtime", lp.FirecrackerRuntime},
		{"opensandbox_server", lp.OpenSandboxServer},
		{"ingress_gateway", lp.IngressGateway},
	}

	hasComp := false
	for _, c := range components {
		if c.data != nil {
			hasComp = true
			break
		}
	}

	if !hasComp && len(lp.DensityFrontier) == 0 {
		return
	}

	b.WriteString("## Load Profiles\n\n")
	if hasComp {
		b.WriteString("| Component | CPU Cores (Avg / P95 / Max) | RSS MiB (Avg / P95 / Max) | Per-Sandbox RSS (MiB) |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, c := range components {
			if c.data == nil {
				continue
			}
			cpu := "-"
			if c.data.CPUCores != nil {
				cpu = fmt.Sprintf("%.2f / %.2f / %.2f", c.data.CPUCores.Avg, c.data.CPUCores.P95, c.data.CPUCores.Max)
			}
			rss := "-"
			if c.data.RSSMiB != nil {
				rss = fmt.Sprintf("%.1f / %.1f / %.1f", c.data.RSSMiB.Avg, c.data.RSSMiB.P95, c.data.RSSMiB.Max)
			}
			perSbx := "-"
			if c.data.PerSandboxResidentMiB > 0 {
				perSbx = fmt.Sprintf("%.2f", c.data.PerSandboxResidentMiB)
			}
			fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", c.name, cpu, rss, perSbx)
		}
		b.WriteString("\n")
	}

	if len(lp.DensityFrontier) > 0 {
		b.WriteString("### Density Frontier\n\n")
		b.WriteString("| Oversell | Active Sandboxes | Resident ΔMemAvail (MiB/Sbx) | FC PSS/RSS (MiB/Sbx) | CPU PSI (some avg60) | Mem PSI (full avg60) | MemAvail Min (MiB) | Denials | OOM | P99 Exec (ms) | Recovery (s) |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, row := range lp.DensityFrontier {
			fcMem := "-"
			if row.FCPssMiBPerSandbox > 0 || row.FCRssMiBPerSandbox > 0 {
				fcMem = fmt.Sprintf("%.1f / %.1f", row.FCPssMiBPerSandbox, row.FCRssMiBPerSandbox)
			}
			fmt.Fprintf(b, "| %.2fx | %d | %.1f | %s | %.2f | %.2f | %.0f | %d | %d | %.1f | %.1f |\n",
				row.OversellRatio, row.ActiveSandboxes, row.ResidentMiBPerSandbox, fcMem,
				row.CPUPressureSomeAvg60, row.MemPressureFullAvg60, row.MemAvailableMinMiB,
				row.AdmissionDenials, row.OOMKills, row.P99ExecLatencyMs, row.RecoveryTimeSeconds)
		}
		b.WriteString("\n")
	}
}

type densityRampStepView struct {
	TargetCount                int     `json:"target_count"`
	LiveSandboxes              int     `json:"live_sandboxes"`
	BatchSucceeded             int     `json:"batch_succeeded"`
	BatchFailed                int     `json:"batch_failed"`
	PingHealthy                int     `json:"ping_healthy"`
	PingSampled                int     `json:"ping_sampled"`
	PingP99Ms                  float64 `json:"ping_p99_ms"`
	DeltaMemAvailableMiBPerSbx float64 `json:"delta_mem_available_mib_per_sbx"`
	FCPssMiBPerSbx             float64 `json:"fc_pss_mib_per_sbx"`
	FCRssMiBPerSbx             float64 `json:"fc_rss_mib_per_sbx"`
	DeltaSlabMiBPerSbx         float64 `json:"delta_slab_mib_per_sbx"`
	FastletRSSMiB              float64 `json:"fastlet_rss_mib"`
	EgressRSSMiB               float64 `json:"egress_rss_mib"`
	UblkdRSSMiB                float64 `json:"ublkd_rss_mib"`
	HostCPUUtilPct             float64 `json:"host_cpu_util_pct"`
	HostLoad1m                 float64 `json:"host_load_1m"`
}

func renderDensityRampMarkdown(b *strings.Builder, suiteNames []string, suites map[string]SuiteResult) {
	for _, name := range suiteNames {
		s := suites[name]
		if s.Extra == nil {
			continue
		}
		rawSteps, ok := s.Extra["steps"]
		if !ok || rawSteps == nil {
			continue
		}
		encoded, err := json.Marshal(rawSteps)
		if err != nil {
			continue
		}
		var steps []densityRampStepView
		if err := json.Unmarshal(encoded, &steps); err != nil || len(steps) == 0 {
			continue
		}
		fmt.Fprintf(b, "### Density Ramp Step Telemetry (`%s`)\n\n", name)
		if scope, ok := s.Extra["placement_scope"].(string); ok && scope != "" {
			if scope == "pool_level" {
				fmt.Fprintf(b, "- Placement scope: `%s` (multi-node/multi-fastlet pool capacity; not a single-fastlet ceiling)\n\n", scope)
			} else {
				fmt.Fprintf(b, "- Placement scope: `%s`\n\n", scope)
			}
		}
		b.WriteString("| Target | Live | Ping (OK/Sampled, P99 ms) | ΔMemAvail/Sbx (MiB) | FC PSS/RSS (MiB/Sbx) | ΔSlab/Sbx (MiB) | Daemons RSS fastlet/egress/ublkd (MiB) | Host CPU% / Load1m |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|\n")
		for _, st := range steps {
			fmt.Fprintf(b, "| %d | %d | %d/%d (%.1f) | %.2f | %.2f / %.2f | %.2f | %.1f / %.1f / %.1f | %.1f%% / %.2f |\n",
				st.TargetCount, st.LiveSandboxes,
				st.PingHealthy, st.PingSampled, st.PingP99Ms,
				st.DeltaMemAvailableMiBPerSbx,
				st.FCPssMiBPerSbx, st.FCRssMiBPerSbx,
				st.DeltaSlabMiBPerSbx,
				st.FastletRSSMiB, st.EgressRSSMiB, st.UblkdRSSMiB,
				st.HostCPUUtilPct, st.HostLoad1m)
		}
		b.WriteString("\n")
	}
	for _, name := range suiteNames {
		s := suites[name]
		if s.Extra == nil {
			continue
		}
		if scope, ok := s.Extra["measurement_scope"].(string); ok && scope != "" {
			fmt.Fprintf(b, "### Dataplane Microbench Scope (`%s`)\n\n- Measurement scope: `%s` (in-sandbox absolute compute, cold/hot OverlayBD storage, and network baseline; not a host bare-metal relative overhead ratio).\n\n", name, scope)
		}
	}
}

type rootfsDirtyTierView struct {
	DirtyUpperMiB       int     `json:"dirty_upper_mib"`
	SnapshotID          string  `json:"snapshot_id"`
	SourceNode          string  `json:"source_node"`
	SnapshotCreateMs    float64 `json:"snapshot_create_ms"`
	SourcePauseStallMs  float64 `json:"source_pause_stall_ms"`
	PostCaptureSourceOK *bool   `json:"post_capture_source_ok"`
	WarmFirstPingMs     float64 `json:"warm_first_ping_ms"`
	WarmBootGapMs       float64 `json:"warm_boot_gap_ms"`
	WarmMarkerVerified  bool    `json:"warm_marker_verified"`
	ColdCacheEvicted    *bool   `json:"cold_cache_evicted"`
	ColdFirstPingMs     float64 `json:"cold_first_ping_ms"`
	ColdBootGapMs       float64 `json:"cold_boot_gap_ms"`
	ColdMarkerVerified  bool    `json:"cold_marker_verified"`
}

type rootfsFanoutView struct {
	Concurrency  int         `json:"concurrency"`
	Succeeded    int         `json:"succeeded"`
	Total        int         `json:"total"`
	FirstPing    LatencyDist `json:"t_first_ping"`
	GuestBootGap LatencyDist `json:"guest_boot_gap"`
}

type rootfsCrossNodeView struct {
	BeforeNode     string  `json:"before_node"`
	AfterNode      string  `json:"after_node"`
	CrossNodeOK    bool    `json:"cross_node_ok"`
	FirstPingMs    float64 `json:"first_ping_ms"`
	BootGapMs      float64 `json:"boot_gap_ms"`
	MarkerVerified bool    `json:"marker_verified"`
	Error          string  `json:"error"`
}

type rootfsOverrideView struct {
	VCPUs              int      `json:"vcpus"`
	MemoryMiB          int      `json:"memory_mib"`
	ObservedVCPUs      int      `json:"observed_vcpus"`
	ObservedMemoryMiB  float64  `json:"observed_memory_mib"`
	Entrypoint         []string `json:"entrypoint"`
	EntrypointVerified *bool    `json:"entrypoint_verified"`
	FirstPingMs        float64  `json:"first_ping_ms"`
	BootGapMs          float64  `json:"boot_gap_ms"`
	MarkerVerified     bool     `json:"marker_verified"`
	OverrideVerified   bool     `json:"override_verified"`
	OK                 bool     `json:"ok"`
	Error              string   `json:"error"`
}

func decodeExtraSlice[T any](extra map[string]any, key string) []T {
	raw, ok := extra[key]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []T
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

func decodeExtraObject[T any](extra map[string]any, key string) *T {
	raw, ok := extra[key]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return &out
}

func renderRootfsOnlyRestoreMarkdown(b *strings.Builder, suiteNames []string, suites map[string]SuiteResult) {
	for _, name := range suiteNames {
		s := suites[name]
		if s.Extra == nil {
			continue
		}
		tiers := decodeExtraSlice[rootfsDirtyTierView](s.Extra, "dirty_upper_tiers")
		fanout := decodeExtraSlice[rootfsFanoutView](s.Extra, "fanout")
		crossNode := decodeExtraObject[rootfsCrossNodeView](s.Extra, "cross_node")
		overrides := decodeExtraSlice[rootfsOverrideView](s.Extra, "resource_overrides")
		if len(tiers) == 0 && len(fanout) == 0 && crossNode == nil && len(overrides) == 0 {
			continue
		}

		fmt.Fprintf(b, "### RootfsOnly Snapshot & Cold-Boot Restore (`%s`)\n\n", name)

		if len(tiers) > 0 {
			b.WriteString("| Dirty Upper (MiB) | Snapshot Create (ms) | Source Stall (ms) | Post-Capture OK | Warm First Ping / Boot Gap (ms) | Cold First Ping / Boot Gap (ms) | Cold Cache Evicted | Markers (Warm/Cold) |\n")
			b.WriteString("|---:|---:|---:|---|---:|---:|---|---|\n")
			for _, t := range tiers {
				postOK := "-"
				if t.PostCaptureSourceOK != nil {
					postOK = fmt.Sprintf("%t", *t.PostCaptureSourceOK)
				}
				evicted := "-"
				if t.ColdCacheEvicted != nil {
					evicted = fmt.Sprintf("%t", *t.ColdCacheEvicted)
				}
				fmt.Fprintf(b, "| %d | %.1f | %.1f | %s | %.1f / %.1f | %.1f / %.1f | %s | %t / %t |\n",
					t.DirtyUpperMiB, t.SnapshotCreateMs, t.SourcePauseStallMs, postOK,
					t.WarmFirstPingMs, t.WarmBootGapMs,
					t.ColdFirstPingMs, t.ColdBootGapMs,
					evicted, t.WarmMarkerVerified, t.ColdMarkerVerified)
			}
			b.WriteString("\n")
		}

		if len(fanout) > 0 {
			b.WriteString("#### Concurrent Fan-Out\n\n")
			b.WriteString("| Concurrency | OK / Total | `t_first_ping` P50/P95/P99 (ms) | `guest_boot_gap` P50/P95/P99 (ms) |\n")
			b.WriteString("|---:|---:|---|---|\n")
			for _, f := range fanout {
				pingStr := "-"
				if f.FirstPing.Count > 0 {
					pingStr = fmt.Sprintf("%.1f / %.1f / %.1f", f.FirstPing.P50Ms, f.FirstPing.P95Ms, f.FirstPing.P99Ms)
				}
				gapStr := "-"
				if f.GuestBootGap.Count > 0 {
					gapStr = fmt.Sprintf("%.1f / %.1f / %.1f", f.GuestBootGap.P50Ms, f.GuestBootGap.P95Ms, f.GuestBootGap.P99Ms)
				}
				fmt.Fprintf(b, "| %d | %d / %d | %s | %s |\n", f.Concurrency, f.Succeeded, f.Total, pingStr, gapStr)
			}
			b.WriteString("\n")
		}

		if crossNode != nil {
			b.WriteString("#### Cross-Node Cold Boot\n\n")
			status := "FAIL"
			if crossNode.CrossNodeOK {
				status = "PASS"
			}
			fmt.Fprintf(b, "- Status: **%s** (`%s` -> `%s`), First Ping: `%.1f ms` (Boot Gap: `%.1f ms`), Marker Verified: `%t`\n",
				status, crossNode.BeforeNode, crossNode.AfterNode, crossNode.FirstPingMs, crossNode.BootGapMs, crossNode.MarkerVerified)
			if crossNode.Error != "" {
				fmt.Fprintf(b, "- Error: %s\n", sanitizeMarkdownCell(crossNode.Error, 200))
			}
			b.WriteString("\n")
		}

		if len(overrides) > 0 {
			b.WriteString("#### Resource & Entrypoint Overrides\n\n")
			b.WriteString("| Target (vCPU / MiB) | Observed (vCPU / MiB) | Entrypoint | First Ping / Boot Gap (ms) | Override Verified | Status |\n")
			b.WriteString("|---|---|---|---:|---|---|\n")
			for _, ov := range overrides {
				ep := "-"
				if len(ov.Entrypoint) > 0 {
					ep = fmt.Sprintf("`%s`", sanitizeMarkdownCell(strings.Join(ov.Entrypoint, " "), 200))
					if ov.EntrypointVerified != nil {
						ep += fmt.Sprintf(" (ok=%t)", *ov.EntrypointVerified)
					}
				}
				st := "PASS"
				if !ov.OK {
					st = "FAIL"
					if ov.Error != "" {
						st += ": " + sanitizeMarkdownCell(ov.Error, 200)
					}
				}
				fmt.Fprintf(b, "| %d / %d | %d / %.1f | %s | %.1f / %.1f | %t | %s |\n",
					ov.VCPUs, ov.MemoryMiB, ov.ObservedVCPUs, ov.ObservedMemoryMiB,
					ep, ov.FirstPingMs, ov.BootGapMs, ov.OverrideVerified, st)
			}
			b.WriteString("\n")
		}
	}
}

// sanitizeMarkdownCell flattens multiline/whitespace runs, escapes pipes, replaces
// backticks, and caps length so raw error strings or entrypoint args cannot corrupt
// Markdown tables or bullet lines.
func sanitizeMarkdownCell(s string, maxLen int) string {
	collapsed := strings.Join(strings.Fields(s), " ")
	collapsed = strings.ReplaceAll(collapsed, "|", `\|`)
	collapsed = strings.ReplaceAll(collapsed, "`", "'")
	if maxLen > 0 && len(collapsed) > maxLen {
		return collapsed[:maxLen] + "..."
	}
	return collapsed
}

func renderErrorBreakdownMarkdown(b *strings.Builder, suiteNames []string, suites map[string]SuiteResult) {
	hasErrors := false
	for _, name := range suiteNames {
		s := suites[name]
		if len(s.ErrorsByOperation) > 0 || len(s.ErrorSamples) > 0 {
			hasErrors = true
			break
		}
	}
	if !hasErrors {
		return
	}

	b.WriteString("### Error Breakdown\n\n")
	b.WriteString("| Suite | Operation | Error Code | Count |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, name := range suiteNames {
		s := suites[name]
		ops := make([]string, 0, len(s.ErrorsByOperation))
		for op := range s.ErrorsByOperation {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			codes := make([]string, 0, len(s.ErrorsByOperation[op]))
			for code := range s.ErrorsByOperation[op] {
				codes = append(codes, code)
			}
			sort.Strings(codes)
			for _, code := range codes {
				fmt.Fprintf(b, "| `%s` | `%s` | `%s` | %d |\n", name, op, code, s.ErrorsByOperation[op][code])
			}
		}
	}
	b.WriteString("\n")

	for _, name := range suiteNames {
		s := suites[name]
		if len(s.ErrorSamples) == 0 {
			continue
		}
		fmt.Fprintf(b, "#### Error Samples (`%s`)\n\n", name)
		for _, es := range s.ErrorSamples {
			if es.StderrHead != "" {
				fmt.Fprintf(b, "- `%s` (`%s`, sandbox=`%s`): %s\n", es.Operation, es.Code, es.SandboxID, es.StderrHead)
			} else {
				fmt.Fprintf(b, "- `%s` (`%s`, sandbox=`%s`)\n", es.Operation, es.Code, es.SandboxID)
			}
		}
		b.WriteString("\n")
	}
}
