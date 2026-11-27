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

package plot

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// Options configures HTML report generation for opensandbox-bench plot.
type Options struct {
	Title          string
	HostMemoryPath string
	HostMemory     *HostMemoryProfile
}

// HostMemoryProfile matches the host-memory-profile.json schema emitted alongside density-ramp runs.
type HostMemoryProfile struct {
	TargetNode            string               `json:"target_node"`
	FastletPod            string               `json:"fastlet_pod,omitempty"`
	RuntimePod            string               `json:"runtime_pod,omitempty"`
	SampleIntervalSeconds float64              `json:"sample_interval_seconds,omitempty"`
	TotalSamples          int                  `json:"total_samples,omitempty"`
	Baseline              *HostMemoryBaseline  `json:"baseline_0_sandboxes,omitempty"`
	StepSnapshots         []HostMemorySnapshot `json:"step_snapshots,omitempty"`
	PeakSnapshot          *HostMemorySnapshot  `json:"peak_snapshot,omitempty"`
	PostDrainSnapshot     *HostMemorySnapshot  `json:"post_drain_0_sandboxes,omitempty"`
}

type HostMemoryBaseline struct {
	MemTotalMiB          float64 `json:"mem_total_mib"`
	MemAvailableMiB      float64 `json:"mem_available_mib"`
	SlabMiB              float64 `json:"slab_mib"`
	FastletRSSMiB        float64 `json:"fastlet_rss_mib"`
	EgressRSSMiB         float64 `json:"egress_rss_mib"`
	OverlayBDUblkdRSSMiB float64 `json:"overlaybd_ublkd_rss_mib"`
	FastletProxyRSSMiB   float64 `json:"fastlet_proxy_rss_mib"`
}

type HostMemorySnapshot struct {
	Label                               string  `json:"label"`
	FirecrackerCount                    int     `json:"firecracker_count"`
	MemAvailableMiB                     float64 `json:"mem_available_mib"`
	HostMemAvailableDeltaMiB            float64 `json:"host_mem_available_delta_mib"`
	HostMemAvailableDeltaMiBPerSandbox  float64 `json:"host_mem_available_delta_mib_per_sandbox"`
	SlabDeltaMiB                        float64 `json:"slab_delta_mib"`
	FirecrackerRSSMiBPerSandbox         float64 `json:"firecracker_rss_mib_per_sandbox"`
	FirecrackerPSSMiBPerSandbox         float64 `json:"firecracker_pss_mib_per_sandbox"`
	FirecrackerPrivateDirtyMiBPerSbx    float64 `json:"firecracker_private_dirty_mib_per_sandbox"`
	FastletRSSMiB                       float64 `json:"fastlet_rss_mib"`
	EgressRSSMiB                        float64 `json:"egress_rss_mib"`
	OverlayBDUblkdRSSMiB                float64 `json:"overlaybd_ublkd_rss_mib"`
	FastletProxyRSSMiB                  float64 `json:"fastlet_proxy_rss_mib"`
}

// LoadHostMemoryProfile reads a host-memory-profile.json file from disk.
func LoadHostMemoryProfile(path string) (*HostMemoryProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read host memory profile %q: %w", path, err)
	}
	var p HostMemoryProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("unmarshal host memory profile %q: %w", path, err)
	}
	return &p, nil
}

// WriteHTML renders the report and writes a self-contained single-file HTML document to outputPath.
func WriteHTML(rep *report.Report, outputPath string, opts Options) error {
	content, err := RenderHTML(rep, opts)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(outputPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(outputPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write html report %q: %w", outputPath, err)
	}
	return nil
}

// RenderHTML builds a standalone, zero-external-CDN interactive HTML benchmark report.
func RenderHTML(rep *report.Report, opts Options) (string, error) {
	if rep == nil {
		return "", fmt.Errorf("report must not be nil")
	}
	hostMem := opts.HostMemory
	if hostMem == nil && strings.TrimSpace(opts.HostMemoryPath) != "" {
		loaded, err := LoadHostMemoryProfile(opts.HostMemoryPath)
		if err != nil {
			return "", err
		}
		hostMem = loaded
	}

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		if rep.Provenance.RunID != "" {
			title = fmt.Sprintf("OpenSandbox Benchmark Report — %s", rep.Provenance.RunID)
		} else {
			title = "OpenSandbox Benchmark Report"
		}
	}

	var sections strings.Builder

	// 1. Concurrency Knee Chart (schedule-burst)
	if sb, ok := rep.Suites["schedule-burst"]; ok {
		levels := extractBurstLevels(sb.Extra)
		if len(levels) > 0 {
			sections.WriteString(renderConcurrencyKneeSection(levels))
		}
	}

	// 2. Latency CDF Chart (t_api_ack & t_first_ping only, per OSEP-0034 / user directive)
	cdfSuiteName, ackDist, pingDist, hasCDF := selectCDFLatencies(rep)
	if hasCDF {
		sections.WriteString(renderLatencyCDFSection(cdfSuiteName, ackDist, pingDist))
	}

	// 3. Single-Host Density & Per-Sandbox Memory Step Chart (density-ramp + optional host-memory-profile)
	densitySuite, hasDensity := rep.Suites["density-ramp"]
	var densitySteps []densityStepData
	if hasDensity {
		densitySteps = extractDensitySteps(densitySuite.Extra)
	}
	if len(densitySteps) > 0 || (hostMem != nil && len(hostMem.StepSnapshots) > 0) {
		sections.WriteString(renderDensityMemorySection(densitySteps, hostMem))
	}

	// 4. Dataplane Microbench & Noisy-Neighbor Degradation
	if mb, ok := rep.Suites["dataplane-microbench"]; ok {
		sections.WriteString(renderDataplaneMicrobenchSection(mb))
	}
	if nn, ok := rep.Suites["noisy-neighbor"]; ok {
		sections.WriteString(renderNoisyNeighborSection(nn))
	}

	// 5. Suite Summary Table (always rendered)
	sections.WriteString(renderSuiteOverviewSection(rep))

	var doc strings.Builder
	doc.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	doc.WriteString("<meta charset=\"utf-8\">\n")
	doc.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	doc.WriteString(fmt.Sprintf("<title>%s</title>\n", html.EscapeString(title)))
	doc.WriteString("<style>\n")
	doc.WriteString(reportCSS)
	doc.WriteString("\n</style>\n</head>\n<body>\n")
	doc.WriteString("<div class=\"container\">\n")
	doc.WriteString(renderHeader(title, rep, hostMem))
	if envCard := renderEnvironmentSpecsCard(rep); envCard != "" {
		doc.WriteString(envCard)
	}
	doc.WriteString(sections.String())
	doc.WriteString(renderFooter(rep))
	doc.WriteString("</div>\n")
	doc.WriteString("<div id=\"tooltip\" class=\"tooltip\" role=\"status\" aria-live=\"polite\"></div>\n")
	doc.WriteString("<script>\n")
	doc.WriteString(reportJS)
	doc.WriteString("\n</script>\n</body>\n</html>\n")
	return doc.String(), nil
}

type burstLevelData struct {
	Concurrency int     `json:"concurrency"`
	Attempts    int     `json:"attempts"`
	Succeeded   int     `json:"succeeded"`
	Failed      int     `json:"failed"`
	RPS         float64 `json:"scheduling_throughput_rps"`
	Latencies   map[string]report.LatencyDist `json:"latencies"`
}

func (b burstLevelData) successPct() float64 {
	if b.Attempts <= 0 {
		return 0
	}
	return float64(b.Succeeded) * 100.0 / float64(b.Attempts)
}

func (b burstLevelData) ackP99Ms() float64 {
	if d, ok := b.Latencies["t_api_ack"]; ok {
		return d.P99Ms
	}
	return 0
}

// FindZeroErrorKnee returns the index of the highest-concurrency level with zero errors
// (or -1 if no level achieved 100% success).
func FindZeroErrorKnee(levels []burstLevelData) int {
	knee := -1
	for i, lvl := range levels {
		if lvl.Attempts > 0 && lvl.Failed == 0 {
			if knee == -1 || lvl.Concurrency > levels[knee].Concurrency {
				knee = i
			}
		}
	}
	return knee
}

func extractBurstLevels(extra map[string]any) []burstLevelData {
	if extra == nil {
		return nil
	}
	raw, ok := extra["levels"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var levels []burstLevelData
	if err := json.Unmarshal(b, &levels); err != nil {
		return nil
	}
	sort.Slice(levels, func(i, j int) bool { return levels[i].Concurrency < levels[j].Concurrency })
	return levels
}

type densityStepData struct {
	TargetCount                int     `json:"target_count"`
	LiveSandboxes              int     `json:"live_sandboxes"`
	PingP99Ms                  float64 `json:"ping_p99_ms"`
	PingHealthy                int     `json:"ping_healthy"`
	PingSampled                int     `json:"ping_sampled"`
	DeltaMemAvailableMiBPerSbx float64 `json:"delta_mem_available_mib_per_sbx"`
	FCPssMiBPerSbx             float64 `json:"fc_pss_mib_per_sbx"`
	FCRssMiBPerSbx             float64 `json:"fc_rss_mib_per_sbx"`
}

func extractDensitySteps(extra map[string]any) []densityStepData {
	if extra == nil {
		return nil
	}
	raw, ok := extra["steps"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var steps []densityStepData
	if err := json.Unmarshal(b, &steps); err != nil {
		return nil
	}
	return steps
}

func selectCDFLatencies(rep *report.Report) (string, *report.LatencyDist, *report.LatencyDist, bool) {
	// Prefer schedule-burst, then steady-churn, then density-ramp, then any other suite.
	preferred := []string{"schedule-burst", "steady-churn", "density-ramp", "pause-resume", "wake-storm"}
	seen := make(map[string]bool, len(preferred))
	check := func(name string) (string, *report.LatencyDist, *report.LatencyDist, bool) {
		sr, ok := rep.Suites[name]
		if !ok || len(sr.Latencies) == 0 {
			return "", nil, nil, false
		}
		var ackPtr, pingPtr *report.LatencyDist
		if d, ok := sr.Latencies["t_api_ack"]; ok && d.Count > 0 {
			cp := d
			ackPtr = &cp
		}
		if d, ok := sr.Latencies["t_first_ping"]; ok && d.Count > 0 {
			cp := d
			pingPtr = &cp
		}
		if ackPtr != nil || pingPtr != nil {
			return name, ackPtr, pingPtr, true
		}
		return "", nil, nil, false
	}
	for _, name := range preferred {
		seen[name] = true
		if s, a, p, ok := check(name); ok {
			return s, a, p, true
		}
	}
	names := make([]string, 0, len(rep.Suites))
	for k := range rep.Suites {
		if !seen[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if s, a, p, ok := check(name); ok {
			return s, a, p, true
		}
	}
	return "", nil, nil, false
}

func renderHeader(title string, rep *report.Report, hostMem *HostMemoryProfile) string {
	var b strings.Builder
	b.WriteString("<header class=\"hero\">\n")
	b.WriteString(fmt.Sprintf("<h1>%s</h1>\n", html.EscapeString(title)))
	b.WriteString("<div class=\"meta-row\">\n")
	if rep.Provenance.Runtime != "" {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">Runtime: <strong>%s</strong></span>\n", html.EscapeString(rep.Provenance.Runtime)))
	}
	if rep.Provenance.OpenSandboxTag != "" {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">OpenSandbox: <strong>%s</strong></span>\n", html.EscapeString(rep.Provenance.OpenSandboxTag)))
	}
	if rep.Environment.KubernetesVersion != "" {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">K8s: <strong>%s</strong></span>\n", html.EscapeString(rep.Environment.KubernetesVersion)))
	}
	if rep.Environment.PoolMode != "" {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">Pool: <strong>%s</strong></span>\n", html.EscapeString(rep.Environment.PoolMode)))
	}
	if !rep.Provenance.Timestamp.IsZero() {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">Generated: <strong>%s</strong></span>\n", rep.Provenance.Timestamp.UTC().Format("2006-01-02 15:04 UTC")))
	}
	b.WriteString("</div>\n")

	// Top KPI cards
	b.WriteString("<div class=\"kpi-grid\">\n")
	if sb, ok := rep.Suites["schedule-burst"]; ok {
		levels := extractBurstLevels(sb.Extra)
		if kneeIdx := FindZeroErrorKnee(levels); kneeIdx >= 0 {
			k := levels[kneeIdx]
			b.WriteString(renderKPICard("Zero-Error Knee", fmt.Sprintf("c=%d · %.1f RPS", k.Concurrency, k.RPS), fmt.Sprintf("t_api_ack P99 %.1f ms · 100%% ok", k.ackP99Ms()), "accent-blue"))
		}
		if d, ok := sb.Latencies["t_first_ping"]; ok && d.Count > 0 {
			b.WriteString(renderKPICard("First Ping P50 / P99", fmt.Sprintf("%.1f / %.1f ms", d.P50Ms, d.P99Ms), fmt.Sprintf("schedule-burst (n=%d)", d.Count), "accent-teal"))
		}
	}
	if hostMem != nil && hostMem.PeakSnapshot != nil {
		pk := hostMem.PeakSnapshot
		b.WriteString(renderKPICard("Per-Sandbox Memory", fmt.Sprintf("%.2f MiB/sbx", pk.HostMemAvailableDeltaMiBPerSandbox), fmt.Sprintf("PSS %.2f MiB · Peak %d VMs", pk.FirecrackerPSSMiBPerSandbox, pk.FirecrackerCount), "accent-purple"))
	} else if dr, ok := rep.Suites["density-ramp"]; ok {
		if peak, ok := dr.Extra["peak_active_sandboxes"]; ok {
			b.WriteString(renderKPICard("Peak Live Density", fmt.Sprintf("%v sandboxes", peak), fmt.Sprintf("density-ramp (%s)", dr.Status), "accent-purple"))
		}
	}
	if nn, ok := rep.Suites["noisy-neighbor"]; ok {
		if ratio := asFloat(nn.Extra["p99_degradation_ratio"]); ratio > 0 {
			b.WriteString(renderKPICard("Noisy P99 Degradation", fmt.Sprintf("%.2fx", ratio), fmt.Sprintf("P50 %.2fx · %d aggressors", asFloat(nn.Extra["p50_degradation_ratio"]), int(asFloat(nn.Extra["aggressor_count"]))), "accent-amber"))
		}
	}
	if rep.LeakAudit != nil {
		st := "PASS"
		cls := "accent-green"
		if !rep.LeakAudit.Passed {
			st = "FAIL"
			cls = "accent-red"
		}
		b.WriteString(renderKPICard("Leak Audit", st, fmt.Sprintf("OOM %d · Residual CR %d", rep.LeakAudit.HostOOMKills+rep.LeakAudit.PodCgroupOOMKills, rep.LeakAudit.ResidualSandboxCRs), cls))
	}
	b.WriteString("</div>\n</header>\n")
	return b.String()
}

func renderKPICard(label, value, sub, cls string) string {
	return fmt.Sprintf(
		"<div class=\"kpi-card %s\"><div class=\"kpi-label\">%s</div><div class=\"kpi-value\">%s</div><div class=\"kpi-sub\">%s</div></div>\n",
		cls, html.EscapeString(label), html.EscapeString(value), html.EscapeString(sub),
	)
}

func renderConcurrencyKneeSection(levels []burstLevelData) string {
	const (
		w     = 440.0
		h     = 270.0
		padL  = 58.0
		padR  = 18.0
		padT  = 46.0
		padB  = 44.0
		plotW = w - padL - padR
		plotH = h - padT - padB
	)

	maxRPS := 1.0
	maxP99 := 1.0
	for _, lvl := range levels {
		if lvl.RPS > maxRPS {
			maxRPS = lvl.RPS
		}
		if p := lvl.ackP99Ms(); p > maxP99 {
			maxP99 = p
		}
	}
	maxRPS = niceCeiling(maxRPS)
	maxP99 = niceCeiling(maxP99)
	kneeIdx := FindZeroErrorKnee(levels)

	stepW := plotW / float64(maxInt(1, len(levels)))
	barW := math.Min(34.0, stepW*0.52)

	// Left sub-chart: Throughput (RPS) + Zero-Error Knee
	var leftSVG strings.Builder
	leftSVG.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="Throughput by Concurrency">`, w, h))
	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4.0
		y := padT + plotH*(1-frac)
		leftSVG.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
		leftSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.1f</text>`, padL-8, y+4, maxRPS*frac))
	}
	if kneeIdx >= 0 {
		kx := padL + stepW*(float64(kneeIdx)+0.5)
		leftSVG.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#16a34a" stroke-width="1.6" stroke-dasharray="5,4"/>`, kx, padT-12, kx, padT+plotH))
		k := levels[kneeIdx]
		label := fmt.Sprintf("Zero-Error Knee: c=%d (%.1f RPS)", k.Concurrency, k.RPS)
		bx := math.Min(w-padR-196, math.Max(padL, kx-98))
		leftSVG.WriteString(fmt.Sprintf(`<rect x="%.1f" y="6" width="196" height="22" rx="4" fill="#f0fdf4" stroke="#86efac" stroke-width="1"/>`, bx))
		leftSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="21" text-anchor="middle" fill="#15803d" font-size="10.5" font-weight="600">%s</text>`, bx+98, html.EscapeString(label)))
	}
	for i, lvl := range levels {
		cx := padL + stepW*(float64(i)+0.5)
		barH := (lvl.RPS / maxRPS) * plotH
		by := padT + plotH - barH
		fill := "#2563eb"
		if lvl.Failed > 0 {
			fill = "#94a3b8"
		}
		tip := fmt.Sprintf("c=%d | Throughput: %.2f RPS | Success: %.1f%% (%d/%d)",
			lvl.Concurrency, lvl.RPS, lvl.successPct(), lvl.Succeeded, lvl.Attempts)
		leftSVG.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="3" fill="%s" opacity="0.88" class="hoverable" data-tip="%s"/>`,
			cx-barW/2, by, barW, math.Max(2, barH), fill, html.EscapeString(tip)))
		succColor := "#16a34a"
		if lvl.Failed > 0 {
			succColor = "#dc2626"
		}
		leftSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" font-weight="600" fill="%s">%.0f%%</text>`,
			cx, math.Max(padT-2, by-5), succColor, lvl.successPct()))
		leftSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">c=%d</text>`, cx, padT+plotH+18, lvl.Concurrency))
	}
	leftSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Concurrency</text>`, padL+plotW/2, h-8))
	leftSVG.WriteString(fmt.Sprintf(`<text x="14" y="%.1f" text-anchor="middle" transform="rotate(-90 14 %.1f)" class="axis-title">Throughput (RPS)</text>`, padT+plotH/2, padT+plotH/2))
	leftSVG.WriteString(`</svg>`)

	// Right sub-chart: t_api_ack P99 Latency (ms)
	var rightSVG strings.Builder
	rightSVG.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="t_api_ack P99 by Concurrency">`, w, h))
	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4.0
		y := padT + plotH*(1-frac)
		rightSVG.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
		rightSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.0f</text>`, padL-8, y+4, maxP99*frac))
	}
	var p99Pts []string
	for i, lvl := range levels {
		cx := padL + stepW*(float64(i)+0.5)
		p99Y := padT + plotH*(1-(lvl.ackP99Ms()/maxP99))
		p99Pts = append(p99Pts, fmt.Sprintf("%.1f,%.1f", cx, p99Y))
		rightSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">c=%d</text>`, cx, padT+plotH+18, lvl.Concurrency))
	}
	if len(p99Pts) > 0 {
		rightSVG.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#d97706" stroke-width="2.4"/>`, strings.Join(p99Pts, " ")))
		for i, lvl := range levels {
			cx := padL + stepW*(float64(i)+0.5)
			p99Y := padT + plotH*(1-(lvl.ackP99Ms()/maxP99))
			tip := fmt.Sprintf("c=%d | t_api_ack P99: %.1f ms | Success: %.1f%%", lvl.Concurrency, lvl.ackP99Ms(), lvl.successPct())
			rightSVG.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4" fill="#d97706" stroke="#ffffff" stroke-width="1.5" class="hoverable" data-tip="%s"/>`, cx, p99Y, html.EscapeString(tip)))
		}
	}
	rightSVG.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Concurrency</text>`, padL+plotW/2, h-8))
	rightSVG.WriteString(fmt.Sprintf(`<text x="14" y="%.1f" text-anchor="middle" transform="rotate(-90 14 %.1f)" class="axis-title axis-amber">t_api_ack P99 (ms)</text>`, padT+plotH/2, padT+plotH/2))
	rightSVG.WriteString(`</svg>`)

	return fmt.Sprintf(`
<section class="card" id="section-concurrency-knee">
  <div class="card-head">
    <div>
      <h2>Concurrency Knee — Throughput &amp; t_api_ack P99</h2>
      <p class="subtitle">schedule-burst concurrency sweep split into single-axis panels with automatic zero-error sustainable knee detection.</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch" style="background:#2563eb"></i>Throughput (0%% err)</span>
      <span class="leg-item"><i class="swatch" style="background:#94a3b8"></i>Throughput (429/err)</span>
      <span class="leg-item"><i class="swatch" style="background:#d97706"></i>t_api_ack P99 (ms)</span>
    </div>
  </div>
  <div class="two-col">
    <div><h3>Throughput (RPS) &amp; Success %%</h3>%s</div>
    <div><h3>t_api_ack P99 Latency (ms)</h3>%s</div>
  </div>
</section>
`, leftSVG.String(), rightSVG.String())
}

func renderLatencyCDFSection(suiteName string, ack, ping *report.LatencyDist) string {
	renderQuantilePanel := func(label string, d *report.LatencyDist, color string) string {
		if d == nil || d.Count == 0 {
			return ""
		}
		const (
			w     = 440.0
			h     = 255.0
			padL  = 58.0
			padR  = 18.0
			padT  = 24.0
			padB  = 44.0
			plotW = w - padL - padR
			plotH = h - padT - padB
		)
		type qpt struct {
			tag string
			ms  float64
		}
		pts := []qpt{
			{"Min", d.MinMs},
			{"P50", d.P50Ms},
			{"P90", d.P90Ms},
			{"P95", d.P95Ms},
			{"P99", d.P99Ms},
			{"Max", d.MaxMs},
		}
		maxMs := niceCeiling(math.Max(1.0, math.Max(d.MaxMs, d.P99Ms)))

		var svg strings.Builder
		svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="%s Stored Quantiles">`, w, h, html.EscapeString(label)))
		for i := 0; i <= 4; i++ {
			frac := float64(i) / 4.0
			y := padT + plotH*(1-frac)
			svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.0f</text>`, padL-8, y+4, maxMs*frac))
		}
		stepW := plotW / float64(len(pts))
		var poly []string
		for i, p := range pts {
			cx := padL + stepW*(float64(i)+0.5)
			cy := padT + plotH*(1-p.ms/maxMs)
			poly = append(poly, fmt.Sprintf("%.1f,%.1f", cx, cy))
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">%s</text>`, cx, padT+plotH+18, p.tag))
		}
		svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="%s" stroke-width="2.4"/>`, strings.Join(poly, " "), color))
		for i, p := range pts {
			cx := padL + stepW*(float64(i)+0.5)
			cy := padT + plotH*(1-p.ms/maxMs)
			tip := fmt.Sprintf("%s %s: %.2f ms (n=%d)", label, p.tag, p.ms, d.Count)
			svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="#ffffff" stroke-width="1.5" class="hoverable" data-tip="%s"/>`, cx, cy, color, html.EscapeString(tip)))
		}
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Stored Quantile</text>`, padL+plotW/2, h-8))
		svg.WriteString(fmt.Sprintf(`<text x="14" y="%.1f" text-anchor="middle" transform="rotate(-90 14 %.1f)" class="axis-title">Latency (ms)</text>`, padT+plotH/2, padT+plotH/2))
		svg.WriteString(`</svg>`)
		return fmt.Sprintf(`<div><h3><code>%s</code> (ms, n=%d)</h3>%s</div>`, html.EscapeString(label), d.Count, svg.String())
	}

	var panels strings.Builder
	panels.WriteString(`<div class="two-col">`)
	panels.WriteString(renderQuantilePanel("t_api_ack", ack, "#2563eb"))
	panels.WriteString(renderQuantilePanel("t_first_ping", ping, "#0d9488"))
	panels.WriteString(`</div>`)

	var tbl strings.Builder
	tbl.WriteString(`<table class="mini-table"><thead><tr><th>Metric</th><th>N</th><th>Min</th><th>P50</th><th>P90</th><th>P95</th><th>P99</th><th>Max</th></tr></thead><tbody>`)
	appendRow := func(label string, d *report.LatencyDist) {
		if d == nil || d.Count == 0 {
			return
		}
		tbl.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td>%d</td><td>%.1f ms</td><td><strong>%.1f ms</strong></td><td>%.1f ms</td><td>%.1f ms</td><td><strong>%.1f ms</strong></td><td>%.1f ms</td></tr>`,
			html.EscapeString(label), d.Count, d.MinMs, d.P50Ms, d.P90Ms, d.P95Ms, d.P99Ms, d.MaxMs))
	}
	appendRow("t_api_ack", ack)
	appendRow("t_first_ping", ping)
	tbl.WriteString(`</tbody></table>`)

	return fmt.Sprintf(`
<section class="card" id="section-latency-cdf">
  <div class="card-head">
    <div>
      <h2>Latency Quantile Distribution (Stored Quantiles: Min / P50 / P90 / P95 / P99 / Max)</h2>
      <p class="subtitle">Side-by-side stored summary quantiles from <code>%s</code> for control-plane ACK (<code>t_api_ack</code>) and guest readiness (<code>t_first_ping</code>).</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch" style="background:#2563eb"></i>t_api_ack</span>
      <span class="leg-item"><i class="swatch" style="background:#0d9488"></i>t_first_ping</span>
    </div>
  </div>
  %s
  %s
</section>
`, html.EscapeString(suiteName), panels.String(), tbl.String())
}

func renderDensityMemorySection(steps []densityStepData, hostMem *HostMemoryProfile) string {
	const (
		w     = 880.0
		h     = 340.0
		padL  = 64.0
		padR  = 74.0
		padT  = 34.0
		padB  = 50.0
		plotW = w - padL - padR
		plotH = h - padT - padB
	)

	type point struct {
		sandboxes    int
		pingP99Ms    float64
		hasPing      bool
		hostMemPerVM float64
		pssPerVM     float64
		hasMem       bool
	}
	var pts []point
	if len(steps) > 0 {
		for _, st := range steps {
			p := point{
				sandboxes: st.LiveSandboxes,
				pingP99Ms: st.PingP99Ms,
				hasPing:   st.PingP99Ms > 0,
			}
			if st.DeltaMemAvailableMiBPerSbx > 0 || st.FCPssMiBPerSbx > 0 {
				p.hostMemPerVM = st.DeltaMemAvailableMiBPerSbx
				p.pssPerVM = st.FCPssMiBPerSbx
				p.hasMem = true
			}
			if hostMem != nil {
				if snap := nearestMemorySnapshot(hostMem.StepSnapshots, st.LiveSandboxes); snap != nil {
					p.hostMemPerVM = snap.HostMemAvailableDeltaMiBPerSandbox
					p.pssPerVM = snap.FirecrackerPSSMiBPerSandbox
					p.hasMem = true
				}
			}
			pts = append(pts, p)
		}
	} else if hostMem != nil {
		for _, snap := range hostMem.StepSnapshots {
			pts = append(pts, point{
				sandboxes:    snap.FirecrackerCount,
				hostMemPerVM: snap.HostMemAvailableDeltaMiBPerSandbox,
				pssPerVM:     snap.FirecrackerPSSMiBPerSandbox,
				hasMem:       true,
			})
		}
	}

	maxPing := 1.0
	maxMem := 1.0
	hasAnyPing := false
	hasAnyMem := false
	for _, p := range pts {
		if p.hasPing {
			hasAnyPing = true
			if p.pingP99Ms > maxPing {
				maxPing = p.pingP99Ms
			}
		}
		if p.hasMem {
			hasAnyMem = true
			if p.hostMemPerVM > maxMem {
				maxMem = p.hostMemPerVM
			}
			if p.pssPerVM > maxMem {
				maxMem = p.pssPerVM
			}
		}
	}
	maxPing = niceCeiling(maxPing)
	maxMem = niceCeiling(maxMem)

	var svg strings.Builder
	svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="Density and Memory Step Chart">`, w, h))

	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4.0
		y := padT + plotH*(1-frac)
		svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
		if hasAnyPing {
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.1f ms</text>`, padL-8, y+4, maxPing*frac))
		}
		if hasAnyMem {
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="start" class="axis-label axis-purple">%.1f MiB</text>`, w-padR+8, y+4, maxMem*frac))
		}
	}

	stepW := plotW / float64(maxInt(1, len(pts)))
	barW := math.Min(42.0, stepW*0.42)

	var hostMemPts, pssPts []string
	for i, p := range pts {
		cx := padL + stepW*(float64(i)+0.5)
		if p.hasPing {
			bh := (p.pingP99Ms / maxPing) * plotH
			by := padT + plotH - bh
			tip := fmt.Sprintf("%d live sandboxes | /ping P99: %.2f ms", p.sandboxes, p.pingP99Ms)
			if p.hasMem {
				tip += fmt.Sprintf(" | Host ΔMem: %.2f MiB/sbx | FC PSS: %.2f MiB/sbx", p.hostMemPerVM, p.pssPerVM)
			}
			svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="#0ea5e9" opacity="0.78" class="hoverable" data-tip="%s"/>`,
				cx-barW/2, by, barW, math.Max(2, bh), html.EscapeString(tip)))
		}
		if p.hasMem {
			hy := padT + plotH*(1-(p.hostMemPerVM/maxMem))
			py := padT + plotH*(1-(p.pssPerVM/maxMem))
			hostMemPts = append(hostMemPts, fmt.Sprintf("%.1f,%.1f", cx, hy))
			pssPts = append(pssPts, fmt.Sprintf("%.1f,%.1f", cx, py))
		}
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">%d</text>`, cx, padT+plotH+19, p.sandboxes))
	}

	if len(hostMemPts) > 0 {
		svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#a855f7" stroke-width="2.5"/>`, strings.Join(hostMemPts, " ")))
		svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#10b981" stroke-width="2.2" stroke-dasharray="6,3"/>`, strings.Join(pssPts, " ")))
		for i, p := range pts {
			if !p.hasMem {
				continue
			}
			cx := padL + stepW*(float64(i)+0.5)
			hy := padT + plotH*(1-(p.hostMemPerVM/maxMem))
			py := padT + plotH*(1-(p.pssPerVM/maxMem))
			svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4" fill="#a855f7" class="hoverable" data-tip="%d VMs | Host ΔMemAvailable: %.2f MiB/sbx"/>`, cx, hy, p.sandboxes, p.hostMemPerVM))
			svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4" fill="#10b981" class="hoverable" data-tip="%d VMs | Firecracker PSS: %.2f MiB/sbx"/>`, cx, py, p.sandboxes, p.pssPerVM))
		}
	}

	svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Live Sandboxes (Single Host)</text>`, padL+plotW/2, h-10))
	if hasAnyPing {
		svg.WriteString(fmt.Sprintf(`<text x="16" y="%.1f" text-anchor="middle" transform="rotate(-90 16 %.1f)" class="axis-title">/ping P99 (ms)</text>`, padT+plotH/2, padT+plotH/2))
	}
	if hasAnyMem {
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" transform="rotate(90 %.1f %.1f)" class="axis-title axis-purple">Memory / Sandbox (MiB)</text>`, w-14, padT+plotH/2, w-14, padT+plotH/2))
	}
	svg.WriteString(`</svg>`)

	var extraCards strings.Builder
	if hostMem != nil && hostMem.PeakSnapshot != nil {
		pk := hostMem.PeakSnapshot
		extraCards.WriteString("<div class=\"stat-strip\">")
		extraCards.WriteString(fmt.Sprintf("<div><span>Target Node</span><strong>%s</strong></div>", html.EscapeString(hostMem.TargetNode)))
		extraCards.WriteString(fmt.Sprintf("<div><span>Peak VMs</span><strong>%d</strong></div>", pk.FirecrackerCount))
		extraCards.WriteString(fmt.Sprintf("<div><span>Host ΔMemAvailable / sbx</span><strong>%.2f MiB</strong></div>", pk.HostMemAvailableDeltaMiBPerSandbox))
		extraCards.WriteString(fmt.Sprintf("<div><span>Firecracker PSS / sbx</span><strong>%.2f MiB</strong></div>", pk.FirecrackerPSSMiBPerSandbox))
		extraCards.WriteString(fmt.Sprintf("<div><span>Firecracker RSS / sbx</span><strong>%.2f MiB</strong></div>", pk.FirecrackerRSSMiBPerSandbox))
		extraCards.WriteString("</div>")
	}

	return fmt.Sprintf(`
<section class="card" id="section-density-memory">
  <div class="card-head">
    <div>
      <h2>Single-Host Density &amp; Per-Sandbox Memory Step Chart</h2>
      <p class="subtitle">Live sandbox retention step progression vs guest <code>/ping</code> P99 and per-sandbox host memory footprint.</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch" style="background:#0ea5e9"></i>/ping P99 (ms)</span>
      <span class="leg-item"><i class="swatch" style="background:#a855f7"></i>Host ΔMemAvailable/sbx</span>
      <span class="leg-item"><i class="swatch" style="background:#10b981"></i>Firecracker PSS/sbx</span>
    </div>
  </div>
  %s
  %s
</section>
`, svg.String(), extraCards.String())
}

func nearestMemorySnapshot(snaps []HostMemorySnapshot, count int) *HostMemorySnapshot {
	if len(snaps) == 0 {
		return nil
	}
	best := &snaps[0]
	bestDiff := absInt(snaps[0].FirecrackerCount - count)
	for i := 1; i < len(snaps); i++ {
		d := absInt(snaps[i].FirecrackerCount - count)
		if d < bestDiff {
			bestDiff = d
			best = &snaps[i]
		}
	}
	return best
}

func renderDataplaneMicrobenchSection(sr report.SuiteResult) string {
	cpuMap := asMap(sr.Extra["cpu"])
	singleCPU := asMap(cpuMap["single"])
	concCPU := asMap(cpuMap["concurrent"])
	storage := asMap(sr.Extra["storage"])
	netMap := asMap(sr.Extra["network"])

	var bars []barItem
	if singleEPS := asFloat(singleCPU["events_per_sec"]); singleEPS > 0 {
		bars = append(bars, barItem{Label: "CPU Single (" + asString(singleCPU["tool"]) + ")", Value: singleEPS, Unit: "ops/s", Color: "#38bdf8"})
	}
	if concEPS := asFloat(concCPU["mean_events_per_sec"]); concEPS > 0 {
		bars = append(bars, barItem{Label: fmt.Sprintf("CPU %dx Conc Mean", int(asFloat(concCPU["sandbox_count"]))), Value: concEPS, Unit: "ops/s", Color: "#818cf8"})
	}

	var storageBars []barItem
	if v := asFloat(storage["seq_read_1m_cold_mibs"]); v > 0 {
		storageBars = append(storageBars, barItem{Label: "1M Seq Read (Cold)", Value: v, Unit: "MiB/s", Color: "#0ea5e9"})
	}
	if v := asFloat(storage["seq_read_1m_hot_mibs"]); v > 0 {
		storageBars = append(storageBars, barItem{Label: "1M Seq Read (Hot)", Value: v, Unit: "MiB/s", Color: "#10b981"})
	}
	if v := asFloat(storage["seq_write_1m_mibs"]); v > 0 {
		storageBars = append(storageBars, barItem{Label: "1M Seq Write", Value: v, Unit: "MiB/s", Color: "#f59e0b"})
	}

	var body strings.Builder
	body.WriteString("<div class=\"two-col\">\n")
	if len(bars) > 0 {
		body.WriteString("<div><h3>CPU Microbench</h3>" + renderHorizontalBarSVG(bars) + "</div>\n")
	}
	if len(storageBars) > 0 {
		body.WriteString(fmt.Sprintf("<div><h3>Storage Throughput (%s)</h3>%s</div>\n", html.EscapeString(asString(storage["tool"])), renderHorizontalBarSVG(storageBars)))
	}
	body.WriteString("</div>\n")

	body.WriteString("<div class=\"stat-strip\">")
	if v := asFloat(concCPU["scaling_efficiency_ratio"]); v > 0 {
		body.WriteString(fmt.Sprintf("<div><span>CPU Scaling Efficiency</span><strong>%.2fx</strong></div>", v))
	}
	if v := asFloat(storage["rand_read_4k_iops"]); v > 0 {
		body.WriteString(fmt.Sprintf("<div><span>4K Rand Read</span><strong>%.0f IOPS</strong></div>", v))
	}
	if v := asFloat(storage["rand_write_4k_iops"]); v > 0 {
		body.WriteString(fmt.Sprintf("<div><span>4K Rand Write</span><strong>%.0f IOPS</strong></div>", v))
	}
	if tool := asString(netMap["tool"]); tool != "" {
		if mbps := asFloat(netMap["throughput_mbps"]); mbps > 0 {
			body.WriteString(fmt.Sprintf("<div><span>Network (%s)</span><strong>%.1f Mbps</strong></div>", html.EscapeString(tool), mbps))
		}
		if p99 := asFloat(netMap["rtt_p99_ms"]); p99 > 0 {
			body.WriteString(fmt.Sprintf("<div><span>Network RTT P50 / P99</span><strong>%.2f / %.2f ms</strong></div>", asFloat(netMap["rtt_p50_ms"]), p99))
		}
	}
	body.WriteString("</div>")

	return fmt.Sprintf(`
<section class="card" id="section-dataplane-microbench">
  <div class="card-head">
    <div>
      <h2>Dataplane Microbench — CPU, Storage (OverlayBD ublk) &amp; Network</h2>
      <p class="subtitle"><code>measurement_scope: guest_absolute</code> — in-sandbox absolute compute, cold vs hot OverlayBD block I/O, and egress/proxy network baseline (no host bare-metal control group).</p>
    </div>
  </div>
  %s
</section>
`, body.String())
}

func renderNoisyNeighborSection(sr report.SuiteResult) string {
	baseP50 := asFloat(sr.Extra["baseline_p50_ms"])
	baseP95 := asFloat(sr.Extra["baseline_p95_ms"])
	baseP99 := asFloat(sr.Extra["baseline_p99_ms"])
	noisyP50 := asFloat(sr.Extra["noisy_p50_ms"])
	noisyP95 := asFloat(sr.Extra["noisy_p95_ms"])
	noisyP99 := asFloat(sr.Extra["noisy_p99_ms"])
	if d, ok := sr.Latencies["victim_baseline_exec"]; ok && baseP99 == 0 {
		baseP50, baseP95, baseP99 = d.P50Ms, d.P95Ms, d.P99Ms
	}
	if d, ok := sr.Latencies["victim_noisy_exec"]; ok && noisyP99 == 0 {
		noisyP50, noisyP95, noisyP99 = d.P50Ms, d.P95Ms, d.P99Ms
	}

	p50Ratio := asFloat(sr.Extra["p50_degradation_ratio"])
	p99Ratio := asFloat(sr.Extra["p99_degradation_ratio"])
	failDelta := asFloat(sr.Extra["failure_rate_delta"]) * 100.0
	aggressors := int(asFloat(sr.Extra["aggressor_count"]))
	victims := int(asFloat(sr.Extra["victim_count"]))

	groups := []groupedBar{
		{Group: "P50", Baseline: baseP50, Noisy: noisyP50},
		{Group: "P95", Baseline: baseP95, Noisy: noisyP95},
		{Group: "P99", Baseline: baseP99, Noisy: noisyP99},
	}

	cards := fmt.Sprintf(`
<div class="stat-strip">
  <div><span>P99 Degradation Ratio</span><strong>%.2fx</strong></div>
  <div><span>P50 Degradation Ratio</span><strong>%.2fx</strong></div>
  <div><span>Failure Rate Delta</span><strong>%+.2f%%</strong></div>
  <div><span>Fleet Split</span><strong>%d aggressors / %d victims</strong></div>
</div>`, p99Ratio, p50Ratio, failDelta, aggressors, victims)

	return fmt.Sprintf(`
<section class="card" id="section-noisy-neighbor">
  <div class="card-head">
    <div>
      <h2>Noisy-Neighbor Isolation — Victim Baseline vs Under Aggressor Load</h2>
      <p class="subtitle">Multi-tenant interference comparison measuring victim exec P50/P95/P99 latency degradation.</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch" style="background:#10b981"></i>Baseline (Idle Aggressors)</span>
      <span class="leg-item"><i class="swatch" style="background:#f43f5e"></i>Noisy (Active Aggressors)</span>
    </div>
  </div>
  %s
  %s
</section>
`, renderGroupedBarSVG(groups), cards)
}

type barItem struct {
	Label string
	Value float64
	Unit  string
	Color string
}

func renderHorizontalBarSVG(items []barItem) string {
	const (
		w     = 440.0
		rowH  = 40.0
		padL  = 150.0
		padR  = 92.0
		padT  = 10.0
		plotW = w - padL - padR
	)
	h := padT + float64(len(items))*rowH + 8
	maxV := 1.0
	for _, it := range items {
		if it.Value > maxV {
			maxV = it.Value
		}
	}
	var svg strings.Builder
	svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg">`, w, h))
	for i, it := range items {
		y := padT + float64(i)*rowH
		bw := math.Max(4, (it.Value/maxV)*plotW)
		tip := fmt.Sprintf("%s: %.2f %s", it.Label, it.Value, it.Unit)
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%s</text>`, padL-10, y+22, html.EscapeString(it.Label)))
		svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="20" rx="3" fill="%s" class="hoverable" data-tip="%s"/>`, padL, y+8, bw, it.Color, html.EscapeString(tip)))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.1f %s</text>`, w-4, y+22, it.Value, html.EscapeString(it.Unit)))
	}
	svg.WriteString(`</svg>`)
	return svg.String()
}

type groupedBar struct {
	Group    string
	Baseline float64
	Noisy    float64
}

func renderGroupedBarSVG(groups []groupedBar) string {
	const (
		w     = 880.0
		h     = 260.0
		padL  = 64.0
		padR  = 32.0
		padT  = 24.0
		padB  = 42.0
		plotW = w - padL - padR
		plotH = h - padT - padB
	)
	maxV := 1.0
	for _, g := range groups {
		if g.Baseline > maxV {
			maxV = g.Baseline
		}
		if g.Noisy > maxV {
			maxV = g.Noisy
		}
	}
	maxV = niceCeiling(maxV)

	var svg strings.Builder
	svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg">`, w, h))
	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4.0
		y := padT + plotH*(1-frac)
		svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.1f ms</text>`, padL-8, y+4, maxV*frac))
	}

	gw := plotW / float64(len(groups))
	bw := math.Min(48.0, gw*0.26)
	for i, g := range groups {
		cx := padL + gw*(float64(i)+0.5)
		bh1 := (g.Baseline / maxV) * plotH
		bh2 := (g.Noisy / maxV) * plotH
		y1 := padT + plotH - bh1
		y2 := padT + plotH - bh2
		svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="#10b981" class="hoverable" data-tip="%s Baseline: %.2f ms"/>`, cx-bw-4, y1, bw, math.Max(2, bh1), g.Group, g.Baseline))
		svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="#f43f5e" class="hoverable" data-tip="%s Noisy: %.2f ms"/>`, cx+4, y2, bw, math.Max(2, bh2), g.Group, g.Noisy))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">%s</text>`, cx, padT+plotH+20, html.EscapeString(g.Group)))
	}
	svg.WriteString(`</svg>`)
	return svg.String()
}

func renderSuiteOverviewSection(rep *report.Report) string {
	names := make([]string, 0, len(rep.Suites))
	for k := range rep.Suites {
		names = append(names, k)
	}
	sort.Strings(names)

	var rows strings.Builder
	for _, name := range names {
		sr := rep.Suites[name]
		badge := "badge-pass"
		if sr.Status != "passed" {
			badge = "badge-fail"
		}
		ack := "-"
		if d, ok := sr.Latencies["t_api_ack"]; ok && d.Count > 0 {
			ack = fmt.Sprintf("%.1f / %.1f ms", d.P50Ms, d.P99Ms)
		}
		ping := "-"
		if d, ok := sr.Latencies["t_first_ping"]; ok && d.Count > 0 {
			ping = fmt.Sprintf("%.1f / %.1f ms", d.P50Ms, d.P99Ms)
		}
		rows.WriteString(fmt.Sprintf(
			`<tr><td><code>%s</code></td><td><span class="badge %s">%s</span></td><td>%d / %d</td><td>%.2f RPS</td><td>%s</td><td>%s</td><td>%.1fs</td></tr>`,
			html.EscapeString(name), badge, html.EscapeString(sr.Status), sr.SucceededOps, sr.TotalOps, sr.ThroughputRPS, ack, ping, sr.DurationSeconds,
		))
	}
	if len(names) == 0 {
		rows.WriteString(`<tr><td colspan="7" class="muted">No suites recorded in this report.</td></tr>`)
	}

	return fmt.Sprintf(`
<section class="card" id="section-suite-overview">
  <div class="card-head">
    <div>
      <h2>Suite Summary</h2>
      <p class="subtitle">Aggregated results across all executed suites in schema_version=%d.</p>
    </div>
  </div>
  <table class="mini-table">
    <thead><tr><th>Suite</th><th>Status</th><th>Succeeded / Total</th><th>Throughput</th><th>t_api_ack (P50/P99)</th><th>t_first_ping (P50/P99)</th><th>Duration</th></tr></thead>
    <tbody>%s</tbody>
  </table>
</section>
`, rep.SchemaVersion, rows.String())
}

func renderFooter(rep *report.Report) string {
	return fmt.Sprintf(
		`<footer class="footer">OpenSandbox Benchmark Report · Run ID: <code>%s</code> · Profile: <code>%s</code> · OpenSandbox Commit: <code>%s</code></footer>`,
		html.EscapeString(rep.Provenance.RunID),
		html.EscapeString(rep.Provenance.ProfileName),
		html.EscapeString(shortSHA(rep.Provenance.OpenSandboxCommit)),
	)
}

func renderEnvironmentSpecsCard(rep *report.Report) string {
	if rep == nil {
		return ""
	}
	env := rep.Environment
	prov := rep.Provenance

	var hostRows []string
	if len(env.Nodes) > 0 {
		n0 := env.Nodes[0]
		nodeSummary := fmt.Sprintf("%d node(s)", len(env.Nodes))
		if n0.CPUCapacity != "" || n0.MemoryCapacity != "" {
			specParts := make([]string, 0, 2)
			if n0.CPUCapacity != "" {
				specParts = append(specParts, n0.CPUCapacity+" vCPU")
			}
			if n0.MemoryCapacity != "" {
				specParts = append(specParts, formatMemoryCapacity(n0.MemoryCapacity))
			}
			nodeSummary += " (" + strings.Join(specParts, " / ") + " each)"
		}
		hostRows = append(hostRows, fmt.Sprintf("<li><span>Nodes</span><strong>%s</strong></li>", html.EscapeString(nodeSummary)))
		if n0.OSImage != "" {
			hostRows = append(hostRows, fmt.Sprintf("<li><span>OS</span><strong>%s</strong></li>", html.EscapeString(n0.OSImage)))
		}
		if n0.KernelVersion != "" {
			hostRows = append(hostRows, fmt.Sprintf("<li><span>Kernel</span><strong>%s</strong></li>", html.EscapeString(n0.KernelVersion)))
		}
		if n0.ContainerRuntime != "" {
			hostRows = append(hostRows, fmt.Sprintf("<li><span>Container Runtime</span><strong>%s</strong></li>", html.EscapeString(n0.ContainerRuntime)))
		}
	}
	if env.KubernetesVersion != "" {
		k8s := env.KubernetesVersion
		if env.ClusterArch != "" {
			k8s += " (" + env.ClusterArch + ")"
		}
		hostRows = append(hostRows, fmt.Sprintf("<li><span>Kubernetes</span><strong>%s</strong></li>", html.EscapeString(k8s)))
	}

	var compRows []string
	if prov.Runtime != "" {
		rt := prov.Runtime
		if env.PoolMode != "" {
			rt += " · " + env.PoolMode
		}
		compRows = append(compRows, fmt.Sprintf("<li><span>Runtime &amp; Pool</span><strong>%s</strong></li>", html.EscapeString(rt)))
	}
	if prov.OpenSandboxTag != "" || prov.OpenSandboxCommit != "" {
		osb := strings.TrimSpace(prov.OpenSandboxTag)
		if sha := shortSHA(prov.OpenSandboxCommit); sha != "n/a" {
			if osb != "" {
				osb += " (" + sha + ")"
			} else {
				osb = sha
			}
		}
		compRows = append(compRows, fmt.Sprintf("<li><span>OpenSandbox</span><strong>%s</strong></li>", html.EscapeString(osb)))
	}
	if prov.FastSandboxTag != "" || prov.FastSandboxCommit != "" {
		fsb := strings.TrimSpace(prov.FastSandboxTag)
		if sha := shortSHA(prov.FastSandboxCommit); sha != "n/a" {
			if fsb != "" {
				fsb += " (" + sha + ")"
			} else {
				fsb = sha
			}
		}
		compRows = append(compRows, fmt.Sprintf("<li><span>fast-sandbox</span><strong>%s</strong></li>", html.EscapeString(fsb)))
	}
	if len(env.OverlayBD) > 0 {
		keys := make([]string, 0, len(env.OverlayBD))
		for k := range env.OverlayBD {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, env.OverlayBD[k]))
		}
		compRows = append(compRows, fmt.Sprintf("<li><span>OverlayBD</span><strong>%s</strong></li>", html.EscapeString(strings.Join(parts, ", "))))
	}
	if len(env.ImageDigests) > 0 {
		keys := make([]string, 0, len(env.ImageDigests))
		for k := range env.ImageDigests {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			val := env.ImageDigests[k]
			if len(val) > 48 {
				val = val[:45] + "..."
			}
			parts = append(parts, fmt.Sprintf("%s: %s", k, val))
		}
		compRows = append(compRows, fmt.Sprintf("<li><span>Components</span><strong>%s</strong></li>", html.EscapeString(strings.Join(parts, " · "))))
	}

	if len(hostRows) == 0 && len(compRows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<section class="card spec-card" id="section-env-specs">`)
	b.WriteString(`<div class="card-head"><div><h2>Environment &amp; Component Specs</h2></div></div>`)
	b.WriteString(`<div class="two-col">`)
	if len(hostRows) > 0 {
		b.WriteString(`<div><h3>Host &amp; Cluster</h3><ul class="spec-list">` + strings.Join(hostRows, "") + `</ul></div>`)
	}
	if len(compRows) > 0 {
		b.WriteString(`<div><h3>Control Plane &amp; Runtime</h3><ul class="spec-list">` + strings.Join(compRows, "") + `</ul></div>`)
	}
	b.WriteString(`</div></section>`)
	return b.String()
}

func formatMemoryCapacity(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasSuffix(s, "Ki") {
		var ki float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "Ki"), "%f", &ki); err == nil && ki > 0 {
			return fmt.Sprintf("%.0f GiB RAM", ki/(1024*1024))
		}
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	if s == "" {
		return "n/a"
	}
	return s
}

func niceCeiling(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	n := v / exp
	switch {
	case n <= 1.2:
		return 1.2 * exp
	case n <= 2.0:
		return 2.0 * exp
	case n <= 5.0:
		return 5.0 * exp
	default:
		return 10.0 * exp
	}
}

func asMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		return m
	}
	return nil
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const reportCSS = `
:root {
  --bg: #ffffff;
  --surface: #ffffff;
  --surface-2: #f8fafc;
  --border: #e2e8f0;
  --text: #0f172a;
  --muted: #64748b;
  --subtle: #475569;
  --grid: #f1f5f9;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Inter, Helvetica, Arial, sans-serif;
  background: var(--bg);
  color: var(--text);
  line-height: 1.6;
  -webkit-font-smoothing: antialiased;
}
.container { max-width: 1000px; margin: 0 auto; padding: 40px 24px 64px; }
.hero { margin-bottom: 32px; padding-bottom: 24px; border-bottom: 1px solid var(--border); }
.hero h1 { font-size: 1.7rem; font-weight: 700; margin: 0 0 12px; letter-spacing: -0.02em; color: var(--text); }
.meta-row { display: flex; flex-wrap: wrap; gap: 16px; margin-bottom: 22px; font-size: 0.84rem; color: var(--muted); }
.pill { display: inline-flex; align-items: center; gap: 4px; }
.pill strong { color: var(--subtle); font-weight: 600; }
.kpi-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); gap: 14px; }
.kpi-card { background: var(--surface-2); border: 1px solid var(--border); border-radius: 8px; padding: 14px 16px; }
.accent-blue { border-left: 3px solid #2563eb; }
.accent-teal { border-left: 3px solid #0d9488; }
.accent-purple { border-left: 3px solid #7c3aed; }
.accent-amber { border-left: 3px solid #d97706; }
.accent-green { border-left: 3px solid #16a34a; }
.accent-red { border-left: 3px solid #dc2626; }
.kpi-label { font-size: 0.74rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--muted); font-weight: 600; }
.kpi-value { font-size: 1.22rem; font-weight: 700; margin: 4px 0 2px; color: var(--text); }
.kpi-sub { font-size: 0.78rem; color: var(--muted); }
.delta-good { color: #16a34a; font-weight: 600; }
.delta-bad { color: #dc2626; font-weight: 600; }
.delta-neutral { color: var(--muted); font-weight: 600; }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 24px; margin-bottom: 24px; }
.card-head { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: flex-start; gap: 12px; margin-bottom: 18px; }
.card h2 { margin: 0; font-size: 1.1rem; font-weight: 650; color: var(--text); }
.card h3 { margin: 0 0 10px; font-size: 0.94rem; font-weight: 600; color: var(--subtle); }
.subtitle { margin: 4px 0 0; font-size: 0.84rem; color: var(--muted); }
.legend { display: flex; flex-wrap: wrap; gap: 14px; font-size: 0.79rem; color: var(--subtle); }
.leg-item { display: inline-flex; align-items: center; gap: 6px; }
.swatch { display: inline-block; width: 10px; height: 10px; border-radius: 2px; }
.swatch-dash { display: inline-block; width: 16px; height: 0; border-top: 2px dashed; }
.swatch-line { display: inline-block; width: 16px; height: 0; border-top: 2.5px solid; }
.chart-svg { width: 100%; height: auto; display: block; }
.grid-line { stroke: var(--grid); stroke-width: 1; }
.grid-line-ref { stroke: #cbd5e1; stroke-width: 1; stroke-dasharray: 4,3; }
.axis-label { fill: #64748b; font-size: 11px; }
.axis-title { fill: #475569; font-size: 11.5px; font-weight: 600; }
.axis-amber { fill: #b45309; }
.axis-purple { fill: #7c3aed; }
.hoverable { cursor: pointer; transition: opacity 0.12s ease; }
.hoverable:hover { opacity: 1; }
.two-col { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 20px; }
.stat-strip { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; margin-top: 16px; padding-top: 16px; border-top: 1px solid var(--border); }
.stat-strip span { display: block; font-size: 0.74rem; color: var(--muted); }
.stat-strip strong { font-size: 0.96rem; color: var(--text); }
.spec-card { padding: 18px 24px; }
.spec-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 6px; font-size: 0.83rem; }
.spec-list li { display: flex; justify-content: space-between; gap: 12px; padding-bottom: 5px; border-bottom: 1px solid var(--grid); }
.spec-list li:last-child { border-bottom: none; padding-bottom: 0; }
.spec-list span { color: var(--muted); flex-shrink: 0; }
.spec-list strong { color: var(--text); font-weight: 500; text-align: right; word-break: break-word; }
.mini-table { width: 100%; border-collapse: collapse; margin-top: 16px; font-size: 0.84rem; }
.mini-table th, .mini-table td { text-align: left; padding: 9px 10px; border-bottom: 1px solid var(--border); }
.mini-table th { color: var(--muted); font-weight: 600; font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.04em; background: var(--surface-2); }
.badge { padding: 2px 8px; border-radius: 999px; font-size: 0.73rem; font-weight: 600; }
.badge-pass { background: #dcfce7; color: #166534; }
.badge-fail { background: #fee2e2; color: #991b1b; }
code { background: var(--surface-2); border: 1px solid var(--border); padding: 1px 5px; border-radius: 4px; font-size: 0.82em; color: var(--text); }
.muted { color: var(--muted); text-align: center; }
.footer { text-align: center; font-size: 0.79rem; color: var(--muted); margin-top: 32px; padding-top: 18px; border-top: 1px solid var(--border); }
.tooltip {
  position: fixed;
  pointer-events: none;
  background: #0f172a;
  color: #ffffff;
  padding: 7px 11px;
  border-radius: 6px;
  font-size: 0.78rem;
  box-shadow: 0 4px 14px rgba(15, 23, 42, 0.18);
  opacity: 0;
  transition: opacity 0.1s ease;
  z-index: 100;
}
`

const reportJS = `
(function () {
  var tip = document.getElementById('tooltip');
  if (!tip) return;
  document.addEventListener('mousemove', function (e) {
    var el = e.target.closest ? e.target.closest('[data-tip]') : null;
    if (!el) {
      tip.style.opacity = '0';
      return;
    }
    tip.textContent = el.getAttribute('data-tip');
    tip.style.left = Math.min(window.innerWidth - 260, e.clientX + 14) + 'px';
    tip.style.top = Math.max(8, e.clientY - 34) + 'px';
    tip.style.opacity = '1';
  });
})();
`
