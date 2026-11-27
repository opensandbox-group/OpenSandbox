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
	TargetCount   int     `json:"target_count"`
	LiveSandboxes int     `json:"live_sandboxes"`
	PingP99Ms     float64 `json:"ping_p99_ms"`
	PingHealthy   int     `json:"ping_healthy"`
	PingSampled   int     `json:"ping_sampled"`
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
		w      = 880.0
		h      = 360.0
		padL   = 68.0
		padR   = 74.0
		padT   = 44.0
		padB   = 52.0
		plotW  = w - padL - padR
		plotH  = h - padT - padB
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

	var svg strings.Builder
	svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="Concurrency Knee Chart">`, w, h))

	// Horizontal gridlines
	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4.0
		y := padT + plotH*(1-frac)
		svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.1f</text>`, padL-8, y+4, maxRPS*frac))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="start" class="axis-label axis-amber">%.0f ms</text>`, w-padR+8, y+4, maxP99*frac))
	}

	stepW := plotW / float64(len(levels))
	barW := math.Min(44.0, stepW*0.48)

	// Zero-error knee highlight band
	if kneeIdx >= 0 {
		kx := padL + stepW*(float64(kneeIdx)+0.5)
		svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#10b981" stroke-width="2" stroke-dasharray="5,4"/>`, kx, padT-8, kx, padT+plotH))
		k := levels[kneeIdx]
		label := fmt.Sprintf("Zero-Error Knee: c=%d (%.1f RPS)", k.Concurrency, k.RPS)
		bx := math.Min(w-padR-190, math.Max(padL+4, kx-95))
		svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="8" width="190" height="22" rx="6" fill="#065f46" opacity="0.92"/>`, bx))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="23" text-anchor="middle" fill="#ecfdf5" font-size="11" font-weight="600">%s</text>`, bx+95, html.EscapeString(label)))
	}

	var p99Pts []string
	for i, lvl := range levels {
		cx := padL + stepW*(float64(i)+0.5)
		barH := (lvl.RPS / maxRPS) * plotH
		by := padT + plotH - barH
		fill := "#3b82f6"
		if lvl.Failed > 0 {
			fill = "#64748b"
		}
		tip := fmt.Sprintf("c=%d | Throughput: %.2f RPS | t_api_ack P99: %.1f ms | Success: %.1f%% (%d/%d)",
			lvl.Concurrency, lvl.RPS, lvl.ackP99Ms(), lvl.successPct(), lvl.Succeeded, lvl.Attempts)
		svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="%s" opacity="0.82" class="hoverable" data-tip="%s"/>`,
			cx-barW/2, by, barW, math.Max(2, barH), fill, html.EscapeString(tip)))

		// Success % badge above bar
		succColor := "#10b981"
		if lvl.Failed > 0 {
			succColor = "#f43f5e"
		}
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" font-weight="600" fill="%s">%.0f%%</text>`,
			cx, math.Max(padT+12, by-6), succColor, lvl.successPct()))

		// X-axis label
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">c=%d</text>`, cx, padT+plotH+20, lvl.Concurrency))

		p99Y := padT + plotH*(1-(lvl.ackP99Ms()/maxP99))
		p99Pts = append(p99Pts, fmt.Sprintf("%.1f,%.1f", cx, p99Y))
	}

	// P99 polyline + dots
	if len(p99Pts) > 0 {
		svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#f59e0b" stroke-width="2.5"/>`, strings.Join(p99Pts, " ")))
		for i, lvl := range levels {
			cx := padL + stepW*(float64(i)+0.5)
			p99Y := padT + plotH*(1-(lvl.ackP99Ms()/maxP99))
			tip := fmt.Sprintf("c=%d | t_api_ack P99: %.1f ms | Success: %.1f%%", lvl.Concurrency, lvl.ackP99Ms(), lvl.successPct())
			svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4.5" fill="#f59e0b" stroke="#0f172a" stroke-width="1.5" class="hoverable" data-tip="%s"/>`, cx, p99Y, html.EscapeString(tip)))
		}
	}

	// Axis titles
	svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Concurrency Level</text>`, padL+plotW/2, h-10))
	svg.WriteString(fmt.Sprintf(`<text x="16" y="%.1f" text-anchor="middle" transform="rotate(-90 16 %.1f)" class="axis-title">Throughput (RPS)</text>`, padT+plotH/2, padT+plotH/2))
	svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" transform="rotate(90 %.1f %.1f)" class="axis-title axis-amber">t_api_ack P99 (ms)</text>`, w-14, padT+plotH/2, w-14, padT+plotH/2))
	svg.WriteString(`</svg>`)

	return fmt.Sprintf(`
<section class="card" id="section-concurrency-knee">
  <div class="card-head">
    <div>
      <h2>Concurrency Knee — Throughput, t_api_ack P99 &amp; Success Rate</h2>
      <p class="subtitle">schedule-burst concurrency sweep with automatic zero-error sustainable knee detection.</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch" style="background:#3b82f6"></i>Throughput (0%% err)</span>
      <span class="leg-item"><i class="swatch" style="background:#64748b"></i>Throughput (429/err)</span>
      <span class="leg-item"><i class="swatch" style="background:#f59e0b"></i>t_api_ack P99 (ms)</span>
    </div>
  </div>
  %s
</section>
`, svg.String())
}

func renderLatencyCDFSection(suiteName string, ack, ping *report.LatencyDist) string {
	const (
		w     = 880.0
		h     = 340.0
		padL  = 64.0
		padR  = 32.0
		padT  = 28.0
		padB  = 48.0
		plotW = w - padL - padR
		plotH = h - padT - padB
	)

	maxMs := 1.0
	for _, d := range []*report.LatencyDist{ack, ping} {
		if d != nil {
			if d.MaxMs > maxMs {
				maxMs = d.MaxMs
			}
			if d.P99Ms > maxMs {
				maxMs = d.P99Ms
			}
		}
	}
	maxMs = niceCeiling(maxMs)

	var svg strings.Builder
	svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="Latency Quantile Distribution">`, w, h))

	// Y-axis reference lines at 0, 50, 90, 95, 99, 100%
	for _, pct := range []float64{0, 25, 50, 75, 90, 95, 99, 100} {
		y := padT + plotH*(1-pct/100.0)
		cls := "grid-line"
		if pct == 50 || pct == 95 || pct == 99 {
			cls = "grid-line-ref"
		}
		svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="%s"/>`, padL, y, w-padR, y, cls))
		if pct == 0 || pct == 50 || pct == 90 || pct == 99 || pct == 100 {
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">P%.0f</text>`, padL-8, y+4, pct))
		}
	}

	// X-axis ticks
	for i := 0; i <= 5; i++ {
		frac := float64(i) / 5.0
		x := padL + plotW*frac
		svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, x, padT, x, padT+plotH))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">%.0f ms</text>`, x, padT+plotH+18, maxMs*frac))
	}

	drawCurve := func(label string, d *report.LatencyDist, color string) {
		if d == nil || d.Count == 0 {
			return
		}
		type pt struct {
			ms  float64
			pct float64
			tag string
		}
		rawPts := []pt{
			{d.MinMs, 0, "Min"},
			{d.P50Ms, 50, "P50"},
			{d.P90Ms, 90, "P90"},
			{d.P95Ms, 95, "P95"},
			{d.P99Ms, 99, "P99"},
			{d.MaxMs, 100, "Max"},
		}
		// Ensure monotonic X
		for i := 1; i < len(rawPts); i++ {
			if rawPts[i].ms < rawPts[i-1].ms {
				rawPts[i].ms = rawPts[i-1].ms
			}
		}
		var path strings.Builder
		for i, p := range rawPts {
			x := padL + (p.ms/maxMs)*plotW
			y := padT + plotH*(1-p.pct/100.0)
			if i == 0 {
				path.WriteString(fmt.Sprintf("M %.1f %.1f", x, y))
			} else {
				prevX := padL + (rawPts[i-1].ms/maxMs)*plotW
				prevY := padT + plotH*(1-rawPts[i-1].pct/100.0)
				midX := (prevX + x) / 2.0
				path.WriteString(fmt.Sprintf(" C %.1f %.1f, %.1f %.1f, %.1f %.1f", midX, prevY, midX, y, x, y))
			}
		}
		svg.WriteString(fmt.Sprintf(`<path d="%s" fill="none" stroke="%s" stroke-width="2.6"/>`, path.String(), color))
		for _, p := range rawPts {
			x := padL + (p.ms/maxMs)*plotW
			y := padT + plotH*(1-p.pct/100.0)
			tip := fmt.Sprintf("%s %s: %.2f ms (n=%d)", label, p.tag, p.ms, d.Count)
			svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="#0f172a" stroke-width="1.5" class="hoverable" data-tip="%s"/>`, x, y, color, html.EscapeString(tip)))
		}
	}

	// Only render t_api_ack and t_first_ping (no t_first_exec).
	drawCurve("t_api_ack", ack, "#38bdf8")
	drawCurve("t_first_ping", ping, "#10b981")

	svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Latency (ms)</text>`, padL+plotW/2, h-10))
	svg.WriteString(fmt.Sprintf(`<text x="16" y="%.1f" text-anchor="middle" transform="rotate(-90 16 %.1f)" class="axis-title">Cumulative Percentile</text>`, padT+plotH/2, padT+plotH/2))
	svg.WriteString(`</svg>`)

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
      <h2>Latency Quantile Distribution (Stored Quantiles: Min / P50 / P90 / P95 / P99 / Max) — t_api_ack &amp; t_first_ping</h2>
      <p class="subtitle">Piecewise quantile distribution from <code>%s</code> using stored summary quantiles (Min / P50 / P90 / P95 / P99 / Max) for control-plane ACK and guest <code>/ping</code> readiness.</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch" style="background:#38bdf8"></i>t_api_ack</span>
      <span class="leg-item"><i class="swatch" style="background:#10b981"></i>t_first_ping</span>
    </div>
  </div>
  %s
  %s
</section>
`, html.EscapeString(suiteName), svg.String(), tbl.String())
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
		w     = 410.0
		rowH  = 42.0
		padL  = 155.0
		padR  = 74.0
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
		bw := math.Max(2, (it.Value/maxV)*plotW)
		tip := fmt.Sprintf("%s: %.2f %s", it.Label, it.Value, it.Unit)
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%s</text>`, padL-10, y+22, html.EscapeString(it.Label)))
		svg.WriteString(fmt.Sprintf(`<rect x="%.1f" y="%.1f" width="%.1f" height="22" rx="4" fill="%s" class="hoverable" data-tip="%s"/>`, padL, y+7, bw, it.Color, html.EscapeString(tip)))
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis-label">%.1f %s</text>`, padL+bw+6, y+22, it.Value, html.EscapeString(it.Unit)))
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
		`<footer class="footer">OpenSandbox Benchmark Harness (OSEP-0034) · Run ID: <code>%s</code> · Profile: <code>%s</code> · OpenSandbox Commit: <code>%s</code></footer>`,
		html.EscapeString(rep.Provenance.RunID),
		html.EscapeString(rep.Provenance.ProfileName),
		html.EscapeString(shortSHA(rep.Provenance.OpenSandboxCommit)),
	)
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
  --bg: #0b1120;
  --surface: #111827;
  --surface-2: #1e293b;
  --border: #1f2937;
  --text: #f8fafc;
  --muted: #94a3b8;
  --grid: #1e293b;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Inter, Helvetica, Arial, sans-serif;
  background: var(--bg);
  color: var(--text);
  line-height: 1.5;
}
.container { max-width: 1020px; margin: 0 auto; padding: 28px 20px 48px; }
.hero { margin-bottom: 24px; }
.hero h1 { font-size: 1.75rem; margin: 0 0 10px; letter-spacing: -0.02em; }
.meta-row { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 18px; }
.pill { background: var(--surface); border: 1px solid var(--border); padding: 4px 10px; border-radius: 999px; font-size: 0.8rem; color: var(--muted); }
.pill strong { color: var(--text); }
.kpi-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 12px; }
.kpi-card { background: var(--surface); border: 1px solid var(--border); border-top: 3px solid #3b82f6; border-radius: 10px; padding: 14px 16px; }
.accent-blue { border-top-color: #3b82f6; }
.accent-teal { border-top-color: #10b981; }
.accent-purple { border-top-color: #a855f7; }
.accent-amber { border-top-color: #f59e0b; }
.accent-green { border-top-color: #22c55e; }
.accent-red { border-top-color: #f43f5e; }
.kpi-label { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em; color: var(--muted); }
.kpi-value { font-size: 1.3rem; font-weight: 700; margin: 4px 0 2px; }
.kpi-sub { font-size: 0.78rem; color: var(--muted); }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 12px; padding: 20px; margin-bottom: 20px; }
.card-head { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: flex-start; gap: 12px; margin-bottom: 14px; }
.card h2 { margin: 0; font-size: 1.12rem; }
.card h3 { margin: 0 0 8px; font-size: 0.95rem; color: #cbd5e1; }
.subtitle { margin: 4px 0 0; font-size: 0.84rem; color: var(--muted); }
.legend { display: flex; flex-wrap: wrap; gap: 12px; font-size: 0.78rem; color: var(--muted); }
.leg-item { display: inline-flex; align-items: center; gap: 6px; }
.swatch { display: inline-block; width: 10px; height: 10px; border-radius: 3px; }
.chart-svg { width: 100%; height: auto; display: block; }
.grid-line { stroke: var(--grid); stroke-width: 1; }
.grid-line-ref { stroke: #334155; stroke-width: 1; stroke-dasharray: 4,3; }
.axis-label { fill: #94a3b8; font-size: 11px; }
.axis-title { fill: #cbd5e1; font-size: 11.5px; font-weight: 600; }
.axis-amber { fill: #fbbf24; }
.axis-purple { fill: #c084fc; }
.hoverable { cursor: pointer; transition: opacity 0.12s ease; }
.hoverable:hover { opacity: 1; }
.two-col { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 16px; }
.stat-strip { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; margin-top: 14px; padding-top: 14px; border-top: 1px solid var(--border); }
.stat-strip span { display: block; font-size: 0.74rem; color: var(--muted); }
.stat-strip strong { font-size: 0.98rem; color: var(--text); }
.mini-table { width: 100%; border-collapse: collapse; margin-top: 14px; font-size: 0.84rem; }
.mini-table th, .mini-table td { text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--border); }
.mini-table th { color: var(--muted); font-weight: 600; font-size: 0.76rem; text-transform: uppercase; letter-spacing: 0.04em; }
.badge { padding: 2px 8px; border-radius: 999px; font-size: 0.74rem; font-weight: 600; }
.badge-pass { background: #064e3b; color: #6ee7b7; }
.badge-fail { background: #4c0519; color: #fda4af; }
code { background: var(--surface-2); padding: 1px 5px; border-radius: 4px; font-size: 0.82em; }
.muted { color: var(--muted); text-align: center; }
.footer { text-align: center; font-size: 0.78rem; color: var(--muted); margin-top: 24px; }
.tooltip {
  position: fixed;
  pointer-events: none;
  background: #020617;
  color: #f8fafc;
  border: 1px solid #334155;
  padding: 6px 10px;
  border-radius: 6px;
  font-size: 0.78rem;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.45);
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
