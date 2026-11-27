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
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// CompareOptions configures two-version comparison HTML generation.
type CompareOptions struct {
	Title                   string
	BaselineLabel           string
	CandidateLabel          string
	BaselineHostMemoryPath  string
	CandidateHostMemoryPath string
	BaselineHostMemory      *HostMemoryProfile
	CandidateHostMemory     *HostMemoryProfile
	Thresholds              report.CompareThresholds
}

// WriteCompareHTML renders a two-version comparison HTML report to outputPath.
func WriteCompareHTML(base, cand *report.Report, outputPath string, opts CompareOptions) error {
	content, err := RenderCompareHTML(base, cand, opts)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(outputPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(outputPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write compare html %q: %w", outputPath, err)
	}
	return nil
}

// RenderCompareHTML builds a standalone, zero-CDN light-theme comparison report
// overlaying baseline vs candidate benchmark metrics.
func RenderCompareHTML(base, cand *report.Report, opts CompareOptions) (string, error) {
	if base == nil || cand == nil {
		return "", fmt.Errorf("both baseline and candidate reports must be non-nil")
	}

	baseMem := opts.BaselineHostMemory
	if baseMem == nil && strings.TrimSpace(opts.BaselineHostMemoryPath) != "" {
		loaded, err := LoadHostMemoryProfile(opts.BaselineHostMemoryPath)
		if err != nil {
			return "", err
		}
		baseMem = loaded
	}
	candMem := opts.CandidateHostMemory
	if candMem == nil && strings.TrimSpace(opts.CandidateHostMemoryPath) != "" {
		loaded, err := LoadHostMemoryProfile(opts.CandidateHostMemoryPath)
		if err != nil {
			return "", err
		}
		candMem = loaded
	}

	baseLabel := strings.TrimSpace(opts.BaselineLabel)
	if baseLabel == "" {
		baseLabel = deriveVersionLabel(base, "Baseline")
	}
	candLabel := strings.TrimSpace(opts.CandidateLabel)
	if candLabel == "" {
		candLabel = deriveVersionLabel(cand, "Candidate")
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = fmt.Sprintf("OpenSandbox Benchmark Comparison — %s vs %s", baseLabel, candLabel)
	}

	th := opts.Thresholds
	if th.MaxLatencyRegressionPct == 0 && th.MaxThroughputDropPct == 0 && th.MaxPerSandboxOverheadPct == 0 {
		th = report.DefaultCompareThresholds()
	}
	cmpRes := report.Compare(base, cand, th)

	var sections strings.Builder

	// 1. Concurrency Knee Overlay (schedule-burst)
	baseLevels := extractBurstLevels(suiteExtra(base, "schedule-burst"))
	candLevels := extractBurstLevels(suiteExtra(cand, "schedule-burst"))
	if len(baseLevels) > 0 || len(candLevels) > 0 {
		sections.WriteString(renderCompareConcurrencySection(baseLabel, candLabel, baseLevels, candLevels))
	}

	// 2. Latency Quantile Distribution Overlay (t_api_ack & t_first_ping only)
	cdfSuite, bAck, bPing, cAck, cPing, hasCDF := selectCompareCDFLatencies(base, cand)
	if hasCDF {
		sections.WriteString(renderCompareLatencySection(cdfSuite, baseLabel, candLabel, bAck, bPing, cAck, cPing))
	}

	// 3. Density Ramp & Per-Sandbox Memory Comparison
	baseDensity := extractDensitySteps(suiteExtra(base, "density-ramp"))
	candDensity := extractDensitySteps(suiteExtra(cand, "density-ramp"))
	if len(baseDensity) > 0 || len(candDensity) > 0 || baseMem != nil || candMem != nil {
		sections.WriteString(renderCompareDensitySection(baseLabel, candLabel, baseDensity, candDensity, baseMem, candMem))
	}

	// 4. Dataplane Microbench & Noisy-Neighbor Comparison (when present)
	if _, okB := base.Suites["dataplane-microbench"]; okB || hasSuite(cand, "dataplane-microbench") {
		sections.WriteString(renderCompareDataplaneSection(baseLabel, candLabel, base.Suites["dataplane-microbench"], cand.Suites["dataplane-microbench"]))
	}
	if _, okB := base.Suites["noisy-neighbor"]; okB || hasSuite(cand, "noisy-neighbor") {
		sections.WriteString(renderCompareNoisySection(baseLabel, candLabel, base.Suites["noisy-neighbor"], cand.Suites["noisy-neighbor"]))
	}

	// 5. Regression Summary Table (reusing report.Compare)
	sections.WriteString(renderCompareRegressionSection(baseLabel, candLabel, cmpRes))

	var doc strings.Builder
	doc.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	doc.WriteString("<meta charset=\"utf-8\">\n")
	doc.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	doc.WriteString(fmt.Sprintf("<title>%s</title>\n", html.EscapeString(title)))
	doc.WriteString("<style>\n")
	doc.WriteString(reportCSS)
	doc.WriteString("\n</style>\n</head>\n<body>\n")
	doc.WriteString("<div class=\"container\">\n")
	doc.WriteString(renderCompareHeader(title, baseLabel, candLabel, base, cand, baseMem, candMem))
	envSource := cand
	if len(cand.Environment.Nodes) == 0 && len(base.Environment.Nodes) > 0 {
		envSource = base
	}
	if envCard := renderEnvironmentSpecsCard(envSource); envCard != "" {
		doc.WriteString(envCard)
	}
	doc.WriteString(sections.String())
	doc.WriteString(fmt.Sprintf(
		"<footer class=\"footer\">OpenSandbox Benchmark Comparison · Baseline: <code>%s</code> (%s) · Candidate: <code>%s</code> (%s)</footer>\n",
		html.EscapeString(baseLabel), html.EscapeString(shortSHA(base.Provenance.OpenSandboxCommit)),
		html.EscapeString(candLabel), html.EscapeString(shortSHA(cand.Provenance.OpenSandboxCommit)),
	))
	doc.WriteString("</div>\n")
	doc.WriteString("<div id=\"tooltip\" class=\"tooltip\" role=\"status\" aria-live=\"polite\"></div>\n")
	doc.WriteString("<script>\n")
	doc.WriteString(reportJS)
	doc.WriteString("\n</script>\n</body>\n</html>\n")
	return doc.String(), nil
}

func deriveVersionLabel(r *report.Report, fallback string) string {
	if r == nil {
		return fallback
	}
	tag := strings.TrimSpace(r.Provenance.OpenSandboxTag)
	sha := shortSHA(r.Provenance.OpenSandboxCommit)
	if tag != "" && sha != "n/a" {
		return fmt.Sprintf("%s (%s)", tag, sha)
	}
	if tag != "" {
		return tag
	}
	if sha != "n/a" {
		return sha
	}
	if strings.TrimSpace(r.Provenance.RunID) != "" {
		return r.Provenance.RunID
	}
	return fallback
}

func suiteExtra(r *report.Report, name string) map[string]any {
	if r == nil {
		return nil
	}
	if sr, ok := r.Suites[name]; ok {
		return sr.Extra
	}
	return nil
}

func hasSuite(r *report.Report, name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.Suites[name]
	return ok
}

func renderCompareHeader(title, baseLabel, candLabel string, base, cand *report.Report, baseMem, candMem *HostMemoryProfile) string {
	var b strings.Builder
	b.WriteString("<header class=\"hero\">\n")
	b.WriteString(fmt.Sprintf("<h1>%s</h1>\n", html.EscapeString(title)))
	b.WriteString("<div class=\"meta-row\">\n")
	b.WriteString(fmt.Sprintf("<span class=\"pill\">Baseline: <strong>%s</strong></span>\n", html.EscapeString(baseLabel)))
	b.WriteString(fmt.Sprintf("<span class=\"pill\">Candidate: <strong>%s</strong></span>\n", html.EscapeString(candLabel)))
	if cand.Provenance.Runtime != "" {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">Runtime: <strong>%s</strong></span>\n", html.EscapeString(cand.Provenance.Runtime)))
	}
	if cand.Environment.KubernetesVersion != "" {
		b.WriteString(fmt.Sprintf("<span class=\"pill\">K8s: <strong>%s</strong></span>\n", html.EscapeString(cand.Environment.KubernetesVersion)))
	}
	b.WriteString("</div>\n")

	b.WriteString("<div class=\"kpi-grid\">\n")

	// 1. Peak Throughput RPS
	bPeakRPS := peakThroughputRPS(base)
	cPeakRPS := peakThroughputRPS(cand)
	if bPeakRPS > 0 || cPeakRPS > 0 {
		deltaHTML, accent := formatDeltaHTML(bPeakRPS, cPeakRPS, true)
		b.WriteString(renderCompareKPICard("Peak Throughput", fmt.Sprintf("%.1f → %.1f RPS", bPeakRPS, cPeakRPS), deltaHTML, accent))
	}

	// 2. c=1 t_api_ack P50 & P99
	bAck1 := c1Latency(base, "t_api_ack")
	cAck1 := c1Latency(cand, "t_api_ack")
	if bAck1 != nil && cAck1 != nil {
		deltaHTML, accent := formatDeltaHTML(bAck1.P99Ms, cAck1.P99Ms, false)
		b.WriteString(renderCompareKPICard("c=1 t_api_ack P99", fmt.Sprintf("%.1f → %.1f ms", bAck1.P99Ms, cAck1.P99Ms),
			fmt.Sprintf("P50 %.1f → %.1f ms · %s", bAck1.P50Ms, cAck1.P50Ms, deltaHTML), accent))
	}

	// 3. c=1 t_first_ping P50 & P99
	bPing1 := c1Latency(base, "t_first_ping")
	cPing1 := c1Latency(cand, "t_first_ping")
	if bPing1 != nil && cPing1 != nil {
		deltaHTML, accent := formatDeltaHTML(bPing1.P99Ms, cPing1.P99Ms, false)
		b.WriteString(renderCompareKPICard("c=1 t_first_ping P99", fmt.Sprintf("%.1f → %.1f ms", bPing1.P99Ms, cPing1.P99Ms),
			fmt.Sprintf("P50 %.1f → %.1f ms · %s", bPing1.P50Ms, cPing1.P50Ms, deltaHTML), accent))
	}

	// 4. Overall Success Rate
	bSucc := overallSuccessPct(base)
	cSucc := overallSuccessPct(cand)
	if bSucc >= 0 && cSucc >= 0 {
		diff := cSucc - bSucc
		cls, accent := "delta-neutral", "accent-blue"
		if diff > 0.1 {
			cls, accent = "delta-good", "accent-green"
		} else if diff < -0.1 {
			cls, accent = "delta-bad", "accent-red"
		}
		b.WriteString(renderCompareKPICard("Overall Success Rate", fmt.Sprintf("%.1f%% → %.1f%%", bSucc, cSucc),
			fmt.Sprintf(`<span class="%s">%+.2f pp</span>`, cls, diff), accent))
	}

	// 5. Per-Sandbox Memory (ΔMemAvailable/sbx & FC PSS)
	bHostMem, bPSS := extractPeakPerSandboxMem(base, baseMem)
	cHostMem, cPSS := extractPeakPerSandboxMem(cand, candMem)
	if bHostMem > 0 || cHostMem > 0 {
		deltaHTML, accent := formatDeltaHTML(bHostMem, cHostMem, false)
		sub := deltaHTML
		if bPSS > 0 || cPSS > 0 {
			sub = fmt.Sprintf("PSS %.2f → %.2f MiB · %s", bPSS, cPSS, deltaHTML)
		}
		b.WriteString(renderCompareKPICard("Per-Sandbox Memory", fmt.Sprintf("%.2f → %.2f MiB", bHostMem, cHostMem), sub, accent))
	}

	// 6. LeakAudit status
	if base.LeakAudit != nil || cand.LeakAudit != nil {
		bSt := leakAuditLabel(base.LeakAudit)
		cSt := leakAuditLabel(cand.LeakAudit)
		accent := "accent-green"
		if cand.LeakAudit != nil && !cand.LeakAudit.Passed {
			accent = "accent-red"
		}
		b.WriteString(renderCompareKPICard("Leak Audit", fmt.Sprintf("%s → %s", bSt, cSt), "baseline vs candidate", accent))
	}

	b.WriteString("</div>\n</header>\n")
	return b.String()
}

func renderCompareKPICard(label, value, subHTML, accent string) string {
	return fmt.Sprintf(
		"<div class=\"kpi-card %s\"><div class=\"kpi-label\">%s</div><div class=\"kpi-value\">%s</div><div class=\"kpi-sub\">%s</div></div>\n",
		accent, html.EscapeString(label), html.EscapeString(value), subHTML,
	)
}

func formatDeltaHTML(baseVal, candVal float64, higherIsBetter bool) (string, string) {
	if baseVal <= 0 {
		return `<span class="delta-neutral">n/a</span>`, "accent-blue"
	}
	pct := ((candVal - baseVal) / baseVal) * 100.0
	ratio := candVal / baseVal
	improved := (higherIsBetter && pct > 1.0) || (!higherIsBetter && pct < -1.0)
	regressed := (higherIsBetter && pct < -1.0) || (!higherIsBetter && pct > 1.0)
	cls, accent := "delta-neutral", "accent-blue"
	if improved {
		cls, accent = "delta-good", "accent-green"
	} else if regressed {
		cls, accent = "delta-bad", "accent-red"
	}
	return fmt.Sprintf(`<span class="%s">%+.1f%% (%.2fx)</span>`, cls, pct, ratio), accent
}

func peakThroughputRPS(r *report.Report) float64 {
	if r == nil {
		return 0
	}
	var peak float64
	if sb, ok := r.Suites["schedule-burst"]; ok {
		for _, lvl := range extractBurstLevels(sb.Extra) {
			if lvl.RPS > peak {
				peak = lvl.RPS
			}
		}
		if sb.ThroughputRPS > peak {
			peak = sb.ThroughputRPS
		}
	}
	if peak == 0 {
		for _, sr := range r.Suites {
			if sr.ThroughputRPS > peak {
				peak = sr.ThroughputRPS
			}
		}
	}
	return peak
}

func c1Latency(r *report.Report, metric string) *report.LatencyDist {
	if r == nil {
		return nil
	}
	if sb, ok := r.Suites["schedule-burst"]; ok {
		for _, lvl := range extractBurstLevels(sb.Extra) {
			if lvl.Concurrency == 1 {
				if d, ok := lvl.Latencies[metric]; ok && d.Count > 0 {
					cp := d
					return &cp
				}
			}
		}
		if d, ok := sb.Latencies[metric]; ok && d.Count > 0 {
			cp := d
			return &cp
		}
	}
	return nil
}

func overallSuccessPct(r *report.Report) float64 {
	if r == nil || len(r.Suites) == 0 {
		return -1
	}
	var total, succ int
	for _, sr := range r.Suites {
		total += sr.TotalOps
		succ += sr.SucceededOps
	}
	if total == 0 {
		return -1
	}
	return float64(succ) * 100.0 / float64(total)
}

func extractPeakPerSandboxMem(r *report.Report, hm *HostMemoryProfile) (hostDelta, pss float64) {
	if hm != nil && hm.PeakSnapshot != nil {
		return hm.PeakSnapshot.HostMemAvailableDeltaMiBPerSandbox, hm.PeakSnapshot.FirecrackerPSSMiBPerSandbox
	}
	steps := extractDensitySteps(suiteExtra(r, "density-ramp"))
	for _, st := range steps {
		if st.DeltaMemAvailableMiBPerSbx > 0 {
			hostDelta = st.DeltaMemAvailableMiBPerSbx
		}
		if st.FCPssMiBPerSbx > 0 {
			pss = st.FCPssMiBPerSbx
		}
	}
	return hostDelta, pss
}

func leakAuditLabel(la *report.LeakAudit) string {
	if la == nil {
		return "N/A"
	}
	if la.Passed {
		return "PASS"
	}
	return "FAIL"
}

func renderCompareConcurrencySection(baseLabel, candLabel string, baseLevels, candLevels []burstLevelData) string {
	const (
		w     = 440.0
		h     = 265.0
		padL  = 58.0
		padR  = 18.0
		padT  = 24.0
		padB  = 44.0
		plotW = w - padL - padR
		plotH = h - padT - padB
	)

	concSet := make(map[int]struct{})
	bMap := make(map[int]burstLevelData, len(baseLevels))
	cMap := make(map[int]burstLevelData, len(candLevels))
	maxRPS, maxP99 := 1.0, 1.0
	for _, l := range baseLevels {
		concSet[l.Concurrency] = struct{}{}
		bMap[l.Concurrency] = l
		if l.RPS > maxRPS {
			maxRPS = l.RPS
		}
		if p := l.ackP99Ms(); p > maxP99 {
			maxP99 = p
		}
	}
	for _, l := range candLevels {
		concSet[l.Concurrency] = struct{}{}
		cMap[l.Concurrency] = l
		if l.RPS > maxRPS {
			maxRPS = l.RPS
		}
		if p := l.ackP99Ms(); p > maxP99 {
			maxP99 = p
		}
	}
	concs := make([]int, 0, len(concSet))
	for c := range concSet {
		concs = append(concs, c)
	}
	sort.Ints(concs)
	maxRPS = niceCeiling(maxRPS)
	maxP99 = niceCeiling(maxP99)
	stepW := plotW / float64(maxInt(1, len(concs)))

	buildPanel := func(yLabel, color, unit string, maxV float64, valFn func(burstLevelData) float64, axisCls string) string {
		var svg strings.Builder
		svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="%s">`, w, h, html.EscapeString(yLabel)))
		for i := 0; i <= 4; i++ {
			frac := float64(i) / 4.0
			y := padT + plotH*(1-frac)
			svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
			if maxV >= 100 {
				svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.0f</text>`, padL-8, y+4, maxV*frac))
			} else {
				svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.1f</text>`, padL-8, y+4, maxV*frac))
			}
		}
		var bPts, cPts []string
		for i, c := range concs {
			cx := padL + stepW*(float64(i)+0.5)
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">c=%d</text>`, cx, padT+plotH+18, c))
			if bl, ok := bMap[c]; ok {
				bPts = append(bPts, fmt.Sprintf("%.1f,%.1f", cx, padT+plotH*(1-valFn(bl)/maxV)))
			}
			if cl, ok := cMap[c]; ok {
				cPts = append(cPts, fmt.Sprintf("%.1f,%.1f", cx, padT+plotH*(1-valFn(cl)/maxV)))
			}
		}
		if len(bPts) > 0 {
			svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-dasharray="6,4" opacity="0.72"/>`, strings.Join(bPts, " "), color))
		}
		if len(cPts) > 0 {
			svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="%s" stroke-width="2.5"/>`, strings.Join(cPts, " "), color))
		}
		for i, c := range concs {
			cx := padL + stepW*(float64(i)+0.5)
			bl, hasB := bMap[c]
			cl, hasC := cMap[c]
			if hasB {
				bv := valFn(bl)
				tip := fmt.Sprintf("c=%d [%s] | %.2f %s | Success: %.1f%%", c, baseLabel, bv, unit, bl.successPct())
				svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="3.8" fill="#ffffff" stroke="%s" stroke-width="2" class="hoverable" data-tip="%s"/>`, cx, padT+plotH*(1-bv/maxV), color, html.EscapeString(tip)))
			}
			if hasC {
				cv := valFn(cl)
				tip := fmt.Sprintf("c=%d [%s] | %.2f %s | Success: %.1f%%", c, candLabel, cv, unit, cl.successPct())
				if hasB && valFn(bl) > 0 {
					tip += fmt.Sprintf(" | Δ: %+.1f%%", ((cv-valFn(bl))/valFn(bl))*100)
				}
				svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="4.2" fill="%s" stroke="#ffffff" stroke-width="1.5" class="hoverable" data-tip="%s"/>`, cx, padT+plotH*(1-cv/maxV), color, html.EscapeString(tip)))
			}
		}
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Concurrency</text>`, padL+plotW/2, h-8))
		svg.WriteString(fmt.Sprintf(`<text x="14" y="%.1f" text-anchor="middle" transform="rotate(-90 14 %.1f)" class="axis-title %s">%s</text>`, padT+plotH/2, padT+plotH/2, axisCls, html.EscapeString(yLabel)))
		svg.WriteString(`</svg>`)
		return svg.String()
	}

	leftPanel := buildPanel("Throughput (RPS)", "#2563eb", "RPS", maxRPS, func(l burstLevelData) float64 { return l.RPS }, "")
	rightPanel := buildPanel("t_api_ack P99 (ms)", "#d97706", "ms", maxP99, func(l burstLevelData) float64 { return l.ackP99Ms() }, "axis-amber")

	return fmt.Sprintf(`
<section class="card" id="section-compare-concurrency">
  <div class="card-head">
    <div>
      <h2>Concurrency Sweep Comparison — Throughput &amp; t_api_ack P99</h2>
      <p class="subtitle">Dashed = <strong>%s</strong> (Baseline); solid = <strong>%s</strong> (Candidate).</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch-dash" style="border-color:#2563eb"></i>Baseline</span>
      <span class="leg-item"><i class="swatch-line" style="border-color:#2563eb"></i>Candidate</span>
    </div>
  </div>
  <div class="two-col">
    <div><h3>Throughput (RPS)</h3>%s</div>
    <div><h3>t_api_ack P99 Latency (ms)</h3>%s</div>
  </div>
</section>
`, html.EscapeString(baseLabel), html.EscapeString(candLabel), leftPanel, rightPanel)
}

func selectCompareCDFLatencies(base, cand *report.Report) (string, *report.LatencyDist, *report.LatencyDist, *report.LatencyDist, *report.LatencyDist, bool) {
	preferred := []string{"schedule-burst", "steady-churn", "density-ramp", "pause-resume", "wake-storm"}
	for _, name := range preferred {
		bs, okB := base.Suites[name]
		cs, okC := cand.Suites[name]
		if !okB && !okC {
			continue
		}
		var bAck, bPing, cAck, cPing *report.LatencyDist
		if okB {
			if d, ok := bs.Latencies["t_api_ack"]; ok && d.Count > 0 {
				cp := d
				bAck = &cp
			}
			if d, ok := bs.Latencies["t_first_ping"]; ok && d.Count > 0 {
				cp := d
				bPing = &cp
			}
		}
		if okC {
			if d, ok := cs.Latencies["t_api_ack"]; ok && d.Count > 0 {
				cp := d
				cAck = &cp
			}
			if d, ok := cs.Latencies["t_first_ping"]; ok && d.Count > 0 {
				cp := d
				cPing = &cp
			}
		}
		if bAck != nil || bPing != nil || cAck != nil || cPing != nil {
			return name, bAck, bPing, cAck, cPing, true
		}
	}
	return "", nil, nil, nil, nil, false
}

func renderCompareLatencySection(suiteName, baseLabel, candLabel string, bAck, bPing, cAck, cPing *report.LatencyDist) string {
	renderMetricPanel := func(metricName string, bd, cd *report.LatencyDist, color string) string {
		if bd == nil && cd == nil {
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
		tags := []string{"Min", "P50", "P90", "P95", "P99", "Max"}
		vals := func(d *report.LatencyDist) []float64 {
			if d == nil || d.Count == 0 {
				return nil
			}
			return []float64{d.MinMs, d.P50Ms, d.P90Ms, d.P95Ms, d.P99Ms, d.MaxMs}
		}
		bVals := vals(bd)
		cVals := vals(cd)
		maxMs := 1.0
		for _, v := range append(append([]float64(nil), bVals...), cVals...) {
			if v > maxMs {
				maxMs = v
			}
		}
		maxMs = niceCeiling(maxMs)
		stepW := plotW / float64(len(tags))

		var svg strings.Builder
		svg.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %.0f %.0f" class="chart-svg" role="img" aria-label="%s Stored Quantiles">`, w, h, html.EscapeString(metricName)))
		for i := 0; i <= 4; i++ {
			frac := float64(i) / 4.0
			y := padT + plotH*(1-frac)
			svg.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid-line"/>`, padL, y, w-padR, y))
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="end" class="axis-label">%.0f</text>`, padL-8, y+4, maxMs*frac))
		}
		for i, tag := range tags {
			cx := padL + stepW*(float64(i)+0.5)
			svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-label">%s</text>`, cx, padT+plotH+18, tag))
		}
		drawSeries := func(seriesLabel string, vs []float64, dashed bool) {
			if len(vs) == 0 {
				return
			}
			var poly []string
			for i, v := range vs {
				cx := padL + stepW*(float64(i)+0.5)
				cy := padT + plotH*(1-v/maxMs)
				poly = append(poly, fmt.Sprintf("%.1f,%.1f", cx, cy))
			}
			dashAttr := ""
			sw := "2.4"
			if dashed {
				dashAttr = ` stroke-dasharray="6,4" opacity="0.72"`
				sw = "2.0"
			}
			svg.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="%s" stroke-width="%s"%s/>`, strings.Join(poly, " "), color, sw, dashAttr))
			for i, v := range vs {
				cx := padL + stepW*(float64(i)+0.5)
				cy := padT + plotH*(1-v/maxMs)
				fill := color
				if dashed {
					fill = "#ffffff"
				}
				tip := fmt.Sprintf("%s [%s] %s: %.2f ms", metricName, seriesLabel, tags[i], v)
				if !dashed && len(bVals) == len(vs) && bVals[i] > 0 {
					tip += fmt.Sprintf(" (Δ %+.1f%%)", ((v-bVals[i])/bVals[i])*100)
				}
				svg.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="3.8" fill="%s" stroke="%s" stroke-width="1.6" class="hoverable" data-tip="%s"/>`, cx, cy, fill, color, html.EscapeString(tip)))
			}
		}
		drawSeries(baseLabel, bVals, true)
		drawSeries(candLabel, cVals, false)
		svg.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" text-anchor="middle" class="axis-title">Stored Quantile</text>`, padL+plotW/2, h-8))
		svg.WriteString(fmt.Sprintf(`<text x="14" y="%.1f" text-anchor="middle" transform="rotate(-90 14 %.1f)" class="axis-title">Latency (ms)</text>`, padT+plotH/2, padT+plotH/2))
		svg.WriteString(`</svg>`)
		return fmt.Sprintf(`<div><h3><code>%s</code> (ms)</h3>%s</div>`, html.EscapeString(metricName), svg.String())
	}

	var panels strings.Builder
	panels.WriteString(`<div class="two-col">`)
	panels.WriteString(renderMetricPanel("t_api_ack", bAck, cAck, "#2563eb"))
	panels.WriteString(renderMetricPanel("t_first_ping", bPing, cPing, "#0d9488"))
	panels.WriteString(`</div>`)

	var tbl strings.Builder
	tbl.WriteString(`<table class="mini-table"><thead><tr><th>Metric</th><th>P50 (Base → Cand)</th><th>ΔP50</th><th>P95 (Base → Cand)</th><th>P99 (Base → Cand)</th><th>ΔP99</th></tr></thead><tbody>`)
	appendCompareRow := func(metric string, bd, cd *report.LatencyDist) {
		if bd == nil || cd == nil {
			return
		}
		d50, _ := formatDeltaHTML(bd.P50Ms, cd.P50Ms, false)
		d99, _ := formatDeltaHTML(bd.P99Ms, cd.P99Ms, false)
		tbl.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td>%.1f → %.1f ms</td><td>%s</td><td>%.1f → %.1f ms</td><td><strong>%.1f → %.1f ms</strong></td><td>%s</td></tr>`,
			html.EscapeString(metric), bd.P50Ms, cd.P50Ms, d50, bd.P95Ms, cd.P95Ms, bd.P99Ms, cd.P99Ms, d99))
	}
	appendCompareRow("t_api_ack", bAck, cAck)
	appendCompareRow("t_first_ping", bPing, cPing)
	tbl.WriteString(`</tbody></table>`)

	return fmt.Sprintf(`
<section class="card" id="section-compare-latency">
  <div class="card-head">
    <div>
      <h2>Latency Quantile Distribution (Stored Quantiles: Min / P50 / P90 / P95 / P99 / Max)</h2>
      <p class="subtitle">Side-by-side <code>%s</code> comparison — dashed = <strong>%s</strong>, solid = <strong>%s</strong> (focused on <code>t_api_ack</code> &amp; <code>t_first_ping</code>).</p>
    </div>
    <div class="legend">
      <span class="leg-item"><i class="swatch-dash" style="border-color:#2563eb"></i>Base t_api_ack</span>
      <span class="leg-item"><i class="swatch-line" style="border-color:#2563eb"></i>Cand t_api_ack</span>
      <span class="leg-item"><i class="swatch-dash" style="border-color:#0d9488"></i>Base t_first_ping</span>
      <span class="leg-item"><i class="swatch-line" style="border-color:#0d9488"></i>Cand t_first_ping</span>
    </div>
  </div>
  %s
  %s
</section>
`, html.EscapeString(suiteName), html.EscapeString(baseLabel), html.EscapeString(candLabel), panels.String(), tbl.String())
}

func renderCompareDensitySection(baseLabel, candLabel string, baseSteps, candSteps []densityStepData, baseMem, candMem *HostMemoryProfile) string {
	type stepPair struct {
		sandboxes int
		bPing     float64
		cPing     float64
		bMem      float64
		cMem      float64
		bPSS      float64
		cPSS      float64
	}
	byCount := make(map[int]*stepPair)
	get := func(n int) *stepPair {
		if p, ok := byCount[n]; ok {
			return p
		}
		p := &stepPair{sandboxes: n}
		byCount[n] = p
		return p
	}
	for _, st := range baseSteps {
		p := get(st.LiveSandboxes)
		p.bPing = st.PingP99Ms
		p.bMem = st.DeltaMemAvailableMiBPerSbx
		p.bPSS = st.FCPssMiBPerSbx
		if baseMem != nil {
			if snap := nearestMemorySnapshot(baseMem.StepSnapshots, st.LiveSandboxes); snap != nil {
				p.bMem = snap.HostMemAvailableDeltaMiBPerSandbox
				p.bPSS = snap.FirecrackerPSSMiBPerSandbox
			}
		}
	}
	for _, st := range candSteps {
		p := get(st.LiveSandboxes)
		p.cPing = st.PingP99Ms
		p.cMem = st.DeltaMemAvailableMiBPerSbx
		p.cPSS = st.FCPssMiBPerSbx
		if candMem != nil {
			if snap := nearestMemorySnapshot(candMem.StepSnapshots, st.LiveSandboxes); snap != nil {
				p.cMem = snap.HostMemAvailableDeltaMiBPerSandbox
				p.cPSS = snap.FirecrackerPSSMiBPerSandbox
			}
		}
	}
	if len(byCount) == 0 {
		if baseMem != nil {
			for _, s := range baseMem.StepSnapshots {
				p := get(s.FirecrackerCount)
				p.bMem = s.HostMemAvailableDeltaMiBPerSandbox
				p.bPSS = s.FirecrackerPSSMiBPerSandbox
			}
		}
		if candMem != nil {
			for _, s := range candMem.StepSnapshots {
				p := get(s.FirecrackerCount)
				p.cMem = s.HostMemAvailableDeltaMiBPerSandbox
				p.cPSS = s.FirecrackerPSSMiBPerSandbox
			}
		}
	}
	keys := make([]int, 0, len(byCount))
	for k := range byCount {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var rows strings.Builder
	for _, k := range keys {
		p := byCount[k]
		pingCell := fmt.Sprintf("%.2f → %.2f ms", p.bPing, p.cPing)
		if p.bPing == 0 && p.cPing == 0 {
			pingCell = "-"
		}
		memCell := fmt.Sprintf("%.2f → %.2f MiB", p.bMem, p.cMem)
		if p.bMem == 0 && p.cMem == 0 {
			memCell = "-"
		}
		pssCell := fmt.Sprintf("%.2f → %.2f MiB", p.bPSS, p.cPSS)
		if p.bPSS == 0 && p.cPSS == 0 {
			pssCell = "-"
		}
		deltaMem := "-"
		if p.bMem > 0 && p.cMem > 0 {
			deltaMem, _ = formatDeltaHTML(p.bMem, p.cMem, false)
		}
		rows.WriteString(fmt.Sprintf(`<tr><td><strong>%d</strong></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			k, pingCell, memCell, deltaMem, pssCell))
	}

	return fmt.Sprintf(`
<section class="card" id="section-compare-density">
  <div class="card-head">
    <div>
      <h2>Density Ramp &amp; Per-Sandbox Memory Comparison</h2>
      <p class="subtitle">Step-by-step live sandbox <code>/ping</code> P99 and per-sandbox memory (<strong>%s</strong> → <strong>%s</strong>).</p>
    </div>
  </div>
  <table class="mini-table">
    <thead><tr><th>Live Sandboxes</th><th>/ping P99 (Base → Cand)</th><th>Host ΔMemAvailable/sbx</th><th>ΔMemory</th><th>Firecracker PSS/sbx</th></tr></thead>
    <tbody>%s</tbody>
  </table>
</section>
`, html.EscapeString(baseLabel), html.EscapeString(candLabel), rows.String())
}

func renderCompareDataplaneSection(baseLabel, candLabel string, baseSR, candSR report.SuiteResult) string {
	bCPU := asMap(asMap(baseSR.Extra["cpu"])["single"])
	cCPU := asMap(asMap(candSR.Extra["cpu"])["single"])
	bSto := asMap(baseSR.Extra["storage"])
	cSto := asMap(candSR.Extra["storage"])
	bNet := asMap(baseSR.Extra["network"])
	cNet := asMap(candSR.Extra["network"])

	var rows strings.Builder
	addRow := func(label string, bv, cv float64, unit string, higherIsBetter bool) {
		if bv == 0 && cv == 0 {
			return
		}
		d, _ := formatDeltaHTML(bv, cv, higherIsBetter)
		rows.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%.2f %s</td><td>%.2f %s</td><td>%s</td></tr>`,
			html.EscapeString(label), bv, unit, cv, unit, d))
	}
	addRow("CPU Single-Sandbox Rate", asFloat(bCPU["events_per_sec"]), asFloat(cCPU["events_per_sec"]), "ops/s", true)
	addRow("Storage 4K Rand Read", asFloat(bSto["rand_read_4k_iops"]), asFloat(cSto["rand_read_4k_iops"]), "IOPS", true)
	addRow("Storage 4K Rand Write", asFloat(bSto["rand_write_4k_iops"]), asFloat(cSto["rand_write_4k_iops"]), "IOPS", true)
	addRow("Storage 1M Seq Read (Cold)", asFloat(bSto["seq_read_1m_cold_mibs"]), asFloat(cSto["seq_read_1m_cold_mibs"]), "MiB/s", true)
	addRow("Storage 1M Seq Read (Hot)", asFloat(bSto["seq_read_1m_hot_mibs"]), asFloat(cSto["seq_read_1m_hot_mibs"]), "MiB/s", true)
	addRow("Storage 1M Seq Write", asFloat(bSto["seq_write_1m_mibs"]), asFloat(cSto["seq_write_1m_mibs"]), "MiB/s", true)
	addRow("Network Throughput", asFloat(bNet["throughput_mbps"]), asFloat(cNet["throughput_mbps"]), "Mbps", true)
	addRow("Network RTT P99", asFloat(bNet["rtt_p99_ms"]), asFloat(cNet["rtt_p99_ms"]), "ms", false)

	return fmt.Sprintf(`
<section class="card" id="section-compare-dataplane">
  <div class="card-head">
    <div>
      <h2>Dataplane Microbench Comparison (Guest-Absolute)</h2>
      <p class="subtitle">CPU, OverlayBD ublk storage, and network comparison (<strong>%s</strong> vs <strong>%s</strong>).</p>
    </div>
  </div>
  <table class="mini-table">
    <thead><tr><th>Metric</th><th>Baseline</th><th>Candidate</th><th>Delta</th></tr></thead>
    <tbody>%s</tbody>
  </table>
</section>
`, html.EscapeString(baseLabel), html.EscapeString(candLabel), rows.String())
}

func renderCompareNoisySection(baseLabel, candLabel string, baseSR, candSR report.SuiteResult) string {
	var rows strings.Builder
	addRow := func(label string, bv, cv float64, unit string) {
		if bv == 0 && cv == 0 {
			return
		}
		d, _ := formatDeltaHTML(bv, cv, false)
		rows.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%.2f%s</td><td>%.2f%s</td><td>%s</td></tr>`,
			html.EscapeString(label), bv, unit, cv, unit, d))
	}
	addRow("P50 Degradation Ratio", asFloat(baseSR.Extra["p50_degradation_ratio"]), asFloat(candSR.Extra["p50_degradation_ratio"]), "x")
	addRow("P99 Degradation Ratio", asFloat(baseSR.Extra["p99_degradation_ratio"]), asFloat(candSR.Extra["p99_degradation_ratio"]), "x")
	addRow("Same-Node P99 Degradation Ratio", asFloat(baseSR.Extra["same_node_p99_degradation_ratio"]), asFloat(candSR.Extra["same_node_p99_degradation_ratio"]), "x")
	addRow("Victim Noisy P99", asFloat(baseSR.Extra["noisy_p99_ms"]), asFloat(candSR.Extra["noisy_p99_ms"]), " ms")

	return fmt.Sprintf(`
<section class="card" id="section-compare-noisy">
  <div class="card-head">
    <div>
      <h2>Noisy-Neighbor Isolation Comparison</h2>
      <p class="subtitle">Multi-tenant degradation ratios (<strong>%s</strong> vs <strong>%s</strong>; lower is better).</p>
    </div>
  </div>
  <table class="mini-table">
    <thead><tr><th>Metric</th><th>Baseline</th><th>Candidate</th><th>Delta</th></tr></thead>
    <tbody>%s</tbody>
  </table>
</section>
`, html.EscapeString(baseLabel), html.EscapeString(candLabel), rows.String())
}

func renderCompareRegressionSection(baseLabel, candLabel string, cmp *report.ComparisonResult) string {
	var rows strings.Builder
	shown := 0
	for _, r := range cmp.Rows {
		if strings.HasPrefix(r.Metric, "t_first_exec") || strings.Contains(r.Metric, "first_exec_cold_read_ms") {
			continue
		}
		shown++
		badge := `<span class="badge badge-pass">OK</span>`
		if r.Regression {
			badge = `<span class="badge badge-fail">REGRESSION</span>`
		}
		deltaCls := "delta-neutral"
		if r.Regression {
			deltaCls = "delta-bad"
		} else if (r.Metric == "throughput_rps" && r.DeltaPct > 1.0) || (r.Metric != "throughput_rps" && r.DeltaPct < -1.0) {
			deltaCls = "delta-good"
		}
		rows.WriteString(fmt.Sprintf(
			`<tr><td><code>%s</code></td><td><code>%s</code></td><td>%.2f</td><td>%.2f</td><td><span class="%s">%+.1f%%</span></td><td>%s</td></tr>`,
			html.EscapeString(r.Suite), html.EscapeString(r.Metric), r.Baseline, r.Candidate, deltaCls, r.DeltaPct, badge,
		))
	}
	if shown == 0 {
		rows.WriteString(`<tr><td colspan="6" class="muted">No overlapping numeric metrics found between baseline and candidate reports.</td></tr>`)
	}

	statusBadge := `<span class="badge badge-pass">NO REGRESSIONS</span>`
	if cmp.HasRegression {
		statusBadge = fmt.Sprintf(`<span class="badge badge-fail">%d REGRESSION(S)</span>`, len(cmp.Regressions))
	}

	return fmt.Sprintf(`
<section class="card" id="section-compare-regression">
  <div class="card-head">
    <div>
      <h2>Regression &amp; Delta Summary</h2>
      <p class="subtitle">Automated threshold comparison (<strong>%s</strong> → <strong>%s</strong>) via <code>report.Compare</code>.</p>
    </div>
    <div>%s</div>
  </div>
  <table class="mini-table">
    <thead><tr><th>Suite</th><th>Metric</th><th>Baseline</th><th>Candidate</th><th>Delta</th><th>Status</th></tr></thead>
    <tbody>%s</tbody>
  </table>
</section>
`, html.EscapeString(baseLabel), html.EscapeString(candLabel), statusBadge, rows.String())
}
