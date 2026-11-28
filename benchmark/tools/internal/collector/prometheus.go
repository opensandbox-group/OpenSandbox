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

package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const maxConcurrentScrapes = 16

// Component identifiers used when scraping Prometheus /metrics across the stack.
const (
	ComponentFSBController      = "fsb-controller"
	ComponentFSBFastlet         = "fsb-fastlet"
	ComponentFSBFastletProxy    = "fsb-fastlet-proxy"
	ComponentEgress             = "egress"
	ComponentOpenSandboxServer  = "opensandbox-server"
	ComponentIngressGateway     = "ingress-gateway"
	ComponentFirecrackerRuntime = "firecracker-runtime"
	ComponentOverlayBDUblkd     = "overlaybd-ublkd"
	ComponentOTelCollector      = "otel-collector"
)

// MetricType mirrors Prometheus exposition metric types.
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
	MetricTypeSummary   MetricType = "summary"
	MetricTypeUntyped   MetricType = "untyped"
)

// Sample represents a single Prometheus metric line with its parsed labels and value.
type Sample struct {
	Name      string
	Labels    map[string]string
	Value     float64
	Timestamp int64
}

// MetricFamily groups samples sharing a base metric family name with optional HELP/TYPE metadata.
type MetricFamily struct {
	Name    string
	Help    string
	Type    MetricType
	Samples []Sample
}

// MetricSet holds parsed Prometheus metrics indexed by sample metric name and family metadata.
type MetricSet struct {
	Families map[string]*MetricFamily
	Samples  map[string][]Sample
}

// NewMetricSet initializes an empty MetricSet.
func NewMetricSet() *MetricSet {
	return &MetricSet{
		Families: make(map[string]*MetricFamily),
		Samples:  make(map[string][]Sample),
	}
}

// Merge appends all samples and family metadata from other into m.
func (m *MetricSet) Merge(other *MetricSet) {
	if other == nil {
		return
	}
	for name, fam := range other.Families {
		existing, ok := m.Families[name]
		if !ok {
			cp := &MetricFamily{
				Name:    fam.Name,
				Help:    fam.Help,
				Type:    fam.Type,
				Samples: append([]Sample(nil), fam.Samples...),
			}
			m.Families[name] = cp
		} else {
			if existing.Type == "" || existing.Type == MetricTypeUntyped {
				existing.Type = fam.Type
			}
			if existing.Help == "" {
				existing.Help = fam.Help
			}
			existing.Samples = append(existing.Samples, fam.Samples...)
		}
	}
	for name, samples := range other.Samples {
		m.Samples[name] = append(m.Samples[name], samples...)
	}
}

// Sum returns the sum of all samples for metricName matching all key/value pairs in labelFilter.
func (m *MetricSet) Sum(metricName string, labelFilter map[string]string) float64 {
	if m == nil {
		return 0
	}
	var sum float64
	for _, s := range m.Samples[metricName] {
		if matchesLabels(s.Labels, labelFilter) && !math.IsNaN(s.Value) && !math.IsInf(s.Value, 0) {
			sum += s.Value
		}
	}
	return sum
}

// Max returns the maximum sample value for metricName matching labelFilter, and whether any matched.
func (m *MetricSet) Max(metricName string, labelFilter map[string]string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	found := false
	var maxVal float64
	for _, s := range m.Samples[metricName] {
		if !matchesLabels(s.Labels, labelFilter) || math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		if !found || s.Value > maxVal {
			maxVal = s.Value
			found = true
		}
	}
	return maxVal, found
}

// SumByLabel aggregates sample values for metricName grouped by the value of labelKey.
func (m *MetricSet) SumByLabel(metricName, labelKey string) map[string]float64 {
	out := make(map[string]float64)
	if m == nil {
		return out
	}
	for _, s := range m.Samples[metricName] {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		v := s.Labels[labelKey]
		out[v] += s.Value
	}
	return out
}

func matchesLabels(labels, filter map[string]string) bool {
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ParsePrometheusText parses Prometheus text exposition format (0.0.4 / OpenMetrics subset)
// using only Go's standard library.
func ParsePrometheusText(r io.Reader) (*MetricSet, error) {
	ms := NewMetricSet()
	scanner := bufio.NewScanner(r)
	// Increase buffer for long label lines (e.g., high-cardinality histograms).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			parseCommentLine(ms, line)
			continue
		}
		sample, err := parseSampleLine(line)
		if err != nil {
			return nil, fmt.Errorf("prometheus parse error at line %d: %w", lineNum, err)
		}
		ms.Samples[sample.Name] = append(ms.Samples[sample.Name], sample)
		famName := familyNameForSample(ms, sample.Name)
		fam, ok := ms.Families[famName]
		if !ok {
			fam = &MetricFamily{Name: famName, Type: MetricTypeUntyped}
			ms.Families[famName] = fam
		}
		fam.Samples = append(fam.Samples, sample)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read prometheus text: %w", err)
	}
	return ms, nil
}

func parseCommentLine(ms *MetricSet, line string) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	if strings.HasPrefix(rest, "HELP ") {
		parts := strings.SplitN(strings.TrimSpace(rest[5:]), " ", 2)
		if len(parts) == 0 || parts[0] == "" {
			return
		}
		fam := ensureFamily(ms, parts[0])
		if len(parts) == 2 {
			fam.Help = parts[1]
		}
		return
	}
	if strings.HasPrefix(rest, "TYPE ") {
		parts := strings.Fields(rest[5:])
		if len(parts) >= 2 {
			fam := ensureFamily(ms, parts[0])
			fam.Type = MetricType(strings.ToLower(parts[1]))
		}
	}
}

func ensureFamily(ms *MetricSet, name string) *MetricFamily {
	fam, ok := ms.Families[name]
	if !ok {
		fam = &MetricFamily{Name: name, Type: MetricTypeUntyped}
		ms.Families[name] = fam
	}
	return fam
}

func familyNameForSample(ms *MetricSet, sampleName string) string {
	if _, ok := ms.Families[sampleName]; ok {
		return sampleName
	}
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if strings.HasSuffix(sampleName, suffix) {
			base := strings.TrimSuffix(sampleName, suffix)
			if _, ok := ms.Families[base]; ok {
				return base
			}
		}
	}
	return sampleName
}

func parseSampleLine(line string) (Sample, error) {
	var s Sample
	i := 0
	n := len(line)
	for i < n && line[i] != '{' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	if i == 0 {
		return s, fmt.Errorf("missing metric name in %q", line)
	}
	s.Name = line[:i]

	if i < n && line[i] == '{' {
		labels, nextIdx, err := parseLabels(line, i)
		if err != nil {
			return s, err
		}
		s.Labels = labels
		i = nextIdx
	}

	rest := strings.TrimSpace(line[i:])
	if rest == "" {
		return s, fmt.Errorf("missing value for metric %q", s.Name)
	}
	fields := strings.Fields(rest)
	val, err := parsePromFloat(fields[0])
	if err != nil {
		return s, fmt.Errorf("invalid value %q for metric %q: %w", fields[0], s.Name, err)
	}
	s.Value = val
	if len(fields) >= 2 {
		if ts, tsErr := strconv.ParseInt(fields[1], 10, 64); tsErr == nil {
			s.Timestamp = ts
		}
	}
	return s, nil
}

func parseLabels(line string, start int) (map[string]string, int, error) {
	labels := make(map[string]string)
	i := start + 1 // skip '{'
	n := len(line)
	for i < n {
		for i < n && (line[i] == ' ' || line[i] == '\t' || line[i] == ',') {
			i++
		}
		if i < n && line[i] == '}' {
			return labels, i + 1, nil
		}
		keyStart := i
		for i < n && line[i] != '=' && line[i] != '}' {
			i++
		}
		key := strings.TrimSpace(line[keyStart:i])
		if key == "" || i >= n || line[i] != '=' {
			return nil, 0, fmt.Errorf("malformed label in %q", line)
		}
		i++ // skip '='
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= n || line[i] != '"' {
			return nil, 0, fmt.Errorf("expected quoted label value for %q in %q", key, line)
		}
		i++ // skip opening '"'
		var valBuf strings.Builder
		closed := false
		for i < n {
			ch := line[i]
			if ch == '\\' && i+1 < n {
				i++
				switch line[i] {
				case 'n':
					valBuf.WriteByte('\n')
				case '"':
					valBuf.WriteByte('"')
				case '\\':
					valBuf.WriteByte('\\')
				default:
					valBuf.WriteByte('\\')
					valBuf.WriteByte(line[i])
				}
				i++
				continue
			}
			if ch == '"' {
				closed = true
				i++
				break
			}
			valBuf.WriteByte(ch)
			i++
		}
		if !closed {
			return nil, 0, fmt.Errorf("unterminated label value for %q", key)
		}
		labels[key] = valBuf.String()
	}
	return nil, 0, fmt.Errorf("unterminated label set in %q", line)
}

func parsePromFloat(raw string) (float64, error) {
	switch raw {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN", "nan":
		return math.NaN(), nil
	default:
		return strconv.ParseFloat(raw, 64)
	}
}

// ScrapeTarget identifies one discovered pod /metrics HTTP endpoint.
type ScrapeTarget struct {
	Component string
	Namespace string
	PodName   string
	NodeName  string
	URL       string
}

// PromSnapshot holds all metrics scraped across discovered cluster components at a point in time.
type PromSnapshot struct {
	Timestamp   time.Time
	ByComponent map[string]*MetricSet
	ByPod       map[string]*MetricSet // key: "<namespace>/<pod>"
	Merged      *MetricSet
	Targets     []ScrapeTarget
	Warnings    []string
}

// PrometheusScraper discovers and scrapes pod /metrics endpoints across OpenSandbox and fast-sandbox.
type PrometheusScraper struct {
	cfg     Config
	clients *Clients
}

// NewPrometheusScraper creates a PrometheusScraper using the provided config and clients.
func NewPrometheusScraper(cfg Config, clients *Clients) *PrometheusScraper {
	cfg.applyDefaults()
	return &PrometheusScraper{cfg: cfg, clients: clients}
}

// DiscoverTargets lists pods in systemNamespace, resourceNamespace, and obsNamespace
// and resolves their Prometheus /metrics endpoints.
func (s *PrometheusScraper) DiscoverTargets(ctx context.Context) ([]ScrapeTarget, []string) {
	var targets []ScrapeTarget
	var warnings []string

	if s.clients == nil || s.clients.Clientset == nil {
		return nil, []string{"prometheus discovery skipped: kubernetes clientset not configured"}
	}

	namespaces := uniqueNonEmpty(s.cfg.SystemNamespace, s.cfg.ResourceNamespace, s.cfg.ObsNamespace)
	for _, ns := range namespaces {
		podList, err := s.clients.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("list pods in %s: %v", ns, err))
			continue
		}
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
				continue
			}
			targets = append(targets, resolvePodScrapeTargets(pod)...)
		}
	}
	return targets, warnings
}

func resolvePodScrapeTargets(pod *corev1.Pod) []ScrapeTarget {
	path := pod.Annotations["prometheus.io/path"]
	if path == "" {
		path = "/metrics"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	annotPort := parsePortString(pod.Annotations["prometheus.io/port"])

	var targets []ScrapeTarget
	seen := make(map[string]struct{})
	addTarget := func(component string, port int) {
		if port <= 0 {
			return
		}
		key := fmt.Sprintf("%s:%d", component, port)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, ScrapeTarget{
			Component: component,
			Namespace: pod.Namespace,
			PodName:   pod.Name,
			NodeName:  pod.Spec.NodeName,
			URL:       fmt.Sprintf("http://%s:%d%s", pod.Status.PodIP, port, path),
		})
	}

	// Fastlet pods host fsb-fastlet (serves /metrics on FASTLET_CONTROL_PORT=:5758)
	// and optionally fsb-fastlet-proxy (:9093). Egress is OTLP-only (exported via OTel Collector).
	if isFastletPod(pod) {
		fastletPort := resolveContainerMetricsPort(pod, []string{"fastlet", "fsb-fastlet"}, annotPort, 5758)
		addTarget(ComponentFSBFastlet, fastletPort)
		if hasContainerMatching(pod, "fastlet-proxy", "proxy") {
			if p := resolveContainerMetricsPort(pod, []string{"fastlet-proxy", "fsb-fastlet-proxy", "proxy"}, 0, 9093); p > 0 && p != fastletPort {
				addTarget(ComponentFSBFastletProxy, p)
			}
		}
		return targets
	}

	switch {
	case podMatchesKeywords(pod, "opensandbox-bench-otel-collector", "otel-collector"):
		addTarget(ComponentOTelCollector, resolveContainerMetricsPort(pod, []string{"otel-collector", "collector"}, annotPort, 8889))
	case podMatchesKeywords(pod, "fast-sandbox-controller", "fsb-controller") || pod.Labels["app.kubernetes.io/component"] == "controller":
		addTarget(ComponentFSBController, resolveContainerMetricsPort(pod, []string{"manager", "controller"}, annotPort, 9091))
	case podMatchesKeywords(pod, "firecracker-runtime"):
		addTarget(ComponentFirecrackerRuntime, resolveContainerMetricsPort(pod, []string{"janitor", "firecracker-agent"}, annotPort, 9092))
		if hasContainerMatching(pod, "overlaybd-ublkd", "ublkd") {
			if p := resolveContainerMetricsPort(pod, []string{"overlaybd-ublkd", "ublkd"}, 0, 9863); p > 0 {
				addTarget(ComponentOverlayBDUblkd, p)
			}
		}
	case podMatchesKeywords(pod, "fastlet-proxy"):
		addTarget(ComponentFSBFastletProxy, resolveContainerMetricsPort(pod, []string{"fastlet-proxy", "proxy"}, annotPort, 9093))
	}
	return targets
}

func isFastletPod(pod *corev1.Pod) bool {
	if pod.Labels["app.kubernetes.io/component"] == "fastlet" ||
		pod.Labels["component"] == "fastlet" ||
		pod.Labels["fast-sandbox.io/pool"] != "" ||
		pod.Labels["sandbox.fast.io/pool"] != "" {
		return true
	}
	return hasContainerMatching(pod, "fastlet") && !podMatchesKeywords(pod, "controller", "firecracker-runtime")
}

func hasContainerMatching(pod *corev1.Pod, keywords ...string) bool {
	for _, c := range pod.Spec.Containers {
		cn := strings.ToLower(c.Name)
		for _, kw := range keywords {
			if strings.Contains(cn, kw) {
				return true
			}
		}
	}
	return false
}

func resolveContainerMetricsPort(pod *corev1.Pod, containerHints []string, annotPort, fallbackPort int) int {
	for _, c := range pod.Spec.Containers {
		cn := strings.ToLower(c.Name)
		matched := len(containerHints) == 0
		for _, h := range containerHints {
			if strings.Contains(cn, h) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, p := range c.Ports {
			pn := strings.ToLower(p.Name)
			if pn == "metrics" || pn == "http-metrics" || strings.Contains(pn, "metric") {
				return int(p.ContainerPort)
			}
		}
	}
	if annotPort > 0 {
		return annotPort
	}
	return fallbackPort
}

func parsePortString(s string) int {
	if s == "" {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return p
}

// ScrapeURL fetches and parses a single Prometheus /metrics HTTP endpoint.
func (s *PrometheusScraper) ScrapeURL(ctx context.Context, rawURL string) (*MetricSet, error) {
	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.ScrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build scrape request %q: %w", rawURL, err)
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4, */*;q=0.1")

	resp, err := s.clients.httpClient(s.cfg.ScrapeTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected HTTP status %d", rawURL, resp.StatusCode)
	}
	return ParsePrometheusText(resp.Body)
}

// Snapshot discovers all target pods and scrapes their /metrics endpoints with graceful degradation.
func (s *PrometheusScraper) Snapshot(ctx context.Context) (*PromSnapshot, error) {
	targets, warnings := s.DiscoverTargets(ctx)
	snap := s.ScrapeTargets(ctx, targets)
	snap.Warnings = append(warnings, snap.Warnings...)
	return snap, nil
}

// ScrapeTargets scrapes an explicit list of targets concurrently (bounded by maxConcurrentScrapes)
// and aggregates them into a PromSnapshot.
func (s *PrometheusScraper) ScrapeTargets(ctx context.Context, targets []ScrapeTarget) *PromSnapshot {
	snap := &PromSnapshot{
		Timestamp:   time.Now().UTC(),
		ByComponent: make(map[string]*MetricSet),
		ByPod:       make(map[string]*MetricSet),
		Merged:      NewMetricSet(),
		Targets:     append([]ScrapeTarget(nil), targets...),
	}
	if len(targets) == 0 {
		return snap
	}

	type scrapeResult struct {
		target ScrapeTarget
		ms     *MetricSet
		err    error
	}
	results := make([]scrapeResult, len(targets))
	sem := make(chan struct{}, maxConcurrentScrapes)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, tgt ScrapeTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ms, err := s.ScrapeURL(ctx, tgt.URL)
			results[idx] = scrapeResult{target: tgt, ms: ms, err: err}
		}(i, t)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			msg := fmt.Sprintf("scrape %s (%s/%s @ %s): %v", r.target.Component, r.target.Namespace, r.target.PodName, r.target.URL, r.err)
			snap.Warnings = append(snap.Warnings, msg)
			s.clients.logger().Warn("prometheus scrape degraded", "component", r.target.Component, "pod", r.target.PodName, "error", r.err)
			continue
		}
		if snap.ByComponent[r.target.Component] == nil {
			snap.ByComponent[r.target.Component] = NewMetricSet()
		}
		snap.ByComponent[r.target.Component].Merge(r.ms)
		podKey := r.target.Namespace + "/" + r.target.PodName
		if snap.ByPod[podKey] == nil {
			snap.ByPod[podKey] = NewMetricSet()
		}
		snap.ByPod[podKey].Merge(r.ms)
		snap.Merged.Merge(r.ms)
	}
	return snap
}

// ComputeDeltas computes OSEP-0034 §4.2, §4.3, §4.4 metric deltas between before and after snapshots.
// Counters are diffed (clamped to >= 0 on counter reset), gauges report end/max state,
// and histograms report count/sum deltas plus delta-estimated average and P95/P99 latencies.
func ComputeDeltas(before, after *PromSnapshot) map[string]float64 {
	deltas := make(map[string]float64)
	if after == nil || after.Merged == nil {
		return deltas
	}
	var beforeMS *MetricSet
	if before != nil {
		beforeMS = before.Merged
	}
	afterMS := after.Merged

	// 1. OSEP-0034 §4.3 scheduling & admission counters with label breakdowns.
	addCounterWithLabelBreakdown(deltas, beforeMS, afterMS, "fast_sandbox_image_affinity_result_total", "result")
	addCounterWithLabelBreakdown(deltas, beforeMS, afterMS, "fast_sandbox_topk_retry_total", "result")
	addCounterWithMultiLabelBreakdown(deltas, beforeMS, afterMS, "fast_sandbox_fastlet_admission_total", []string{"operation", "result", "reason"})

	// 2. OSEP-0034 §4.3 placement histograms & admission gauges.
	addHistogramDelta(deltas, beforeMS, afterMS, "fast_sandbox_registry_heartbeat_age_seconds")
	addHistogramDelta(deltas, beforeMS, afterMS, "fast_sandbox_registry_candidate_count")
	for state, val := range afterMS.SumByLabel("fast_sandbox_fastlet_admission_slots", "state") {
		if state != "" {
			deltas[fmt.Sprintf("fast_sandbox_fastlet_admission_slots{state=%q}", state)] = val
		}
	}
	addGaugeMultiLabel(deltas, afterMS, "fast_sandbox_fastlet_resource_admission_state", []string{"state", "reason"})

	// 3. OSEP-0034 §4.1 & §4.4 latency histograms.
	addHistogramDelta(deltas, beforeMS, afterMS, "fast_sandbox_fastlet_proxy_upstream_latency_seconds")
	addHistogramDeltaByLabel(deltas, beforeMS, afterMS, "fast_sandbox_create_stage_latency_seconds", "stage")

	// 4. OSEP-0010 OTLP metrics exported via OTel Collector Prometheus exporter (:8889/metrics).
	addHistogramDelta(deltas, beforeMS, afterMS, "opensandbox_sandbox_create_duration_milliseconds")
	addHistogramDelta(deltas, beforeMS, afterMS, "opensandbox_sandbox_create_duration")
	addHistogramDelta(deltas, beforeMS, afterMS, "egress_dns_query_duration_seconds")
	addCounterWithLabelBreakdown(deltas, beforeMS, afterMS, "ingress_wake_flights_total", "outcome")
	addCounterWithLabelBreakdown(deltas, beforeMS, afterMS, "ingress_wake_flights", "outcome")
	addCounterWithLabelBreakdown(deltas, beforeMS, afterMS, "egress_dns_query_failed_total", "reason")
	if v := counterDelta(beforeMS.Sum("egress_policy_denied_total", nil), afterMS.Sum("egress_policy_denied_total", nil)); len(afterMS.Samples["egress_policy_denied_total"]) > 0 {
		deltas["egress_policy_denied_total"] = v
	}

	return deltas
}

// BuildStageAttribution maps deterministic client-side create latencies
// (t_api_ack, t_endpoint_resolve, t_first_ping) and Prometheus histogram deltas
// (opensandbox_sandbox_create_duration_milliseconds, fast_sandbox_create_stage_latency_seconds{stage=...},
// fast_sandbox_fastlet_proxy_upstream_latency_seconds) into report.StageAttribution entries.
func BuildStageAttribution(deltas map[string]float64, latencies map[string]report.LatencyDist) []report.StageAttribution {
	var stages []report.StageAttribution

	var refTotalP99 float64
	if fp, ok := latencies["t_first_ping"]; ok && fp.Count > 0 && fp.P99Ms > 0 {
		refTotalP99 = fp.P99Ms
	} else if ack, ok := latencies["t_api_ack"]; ok && ack.Count > 0 && ack.P99Ms > 0 {
		refTotalP99 = ack.P99Ms
	}

	share := func(p99Ms float64) float64 {
		if refTotalP99 <= 0 || p99Ms <= 0 {
			return 0
		}
		pct := (p99Ms / refTotalP99) * 100.0
		if pct > 100.0 {
			pct = 100.0
		}
		return math.Round(pct*10) / 10
	}

	// 1. Client-side create breakdown (t_api_ack, t_endpoint_resolve, t_first_ping).
	ack, hasAck := latencies["t_api_ack"]
	res, hasRes := latencies["t_endpoint_resolve"]
	fp, hasFP := latencies["t_first_ping"]

	if hasAck && ack.Count > 0 {
		stages = append(stages, report.StageAttribution{
			SpanName:      "client.api_ack",
			P50SelfMs:     ack.P50Ms,
			P99SelfMs:     ack.P99Ms,
			CriticalShare: share(ack.P99Ms),
			Note:          "client POST /v1/sandboxes ack",
		})
	}
	if hasRes && res.Count > 0 {
		p50 := res.P50Ms
		p99 := res.P99Ms
		if hasAck && ack.Count > 0 {
			p50 = math.Max(0, res.P50Ms-ack.P50Ms)
			p99 = math.Max(0, res.P99Ms-ack.P99Ms)
		}
		stages = append(stages, report.StageAttribution{
			SpanName:      "client.endpoint_resolve",
			P50SelfMs:     p50,
			P99SelfMs:     p99,
			CriticalShare: share(p99),
			Note:          "delta after t_api_ack until endpoint resolved",
		})
	}
	if hasFP && fp.Count > 0 {
		var baseP50, baseP99 float64
		note := "delta until guest /ping 200 OK"
		if hasRes && res.Count > 0 {
			baseP50, baseP99 = res.P50Ms, res.P99Ms
			note = "delta after t_endpoint_resolve until guest /ping 200 OK"
		} else if hasAck && ack.Count > 0 {
			baseP50, baseP99 = ack.P50Ms, ack.P99Ms
			note = "delta after t_api_ack until guest /ping 200 OK"
		}
		p50 := math.Max(0, fp.P50Ms-baseP50)
		p99 := math.Max(0, fp.P99Ms-baseP99)
		stages = append(stages, report.StageAttribution{
			SpanName:      "client.first_ping_wait",
			P50SelfMs:     p50,
			P99SelfMs:     p99,
			CriticalShare: share(p99),
			Note:          note,
		})
	}

	if len(deltas) == 0 {
		return stages
	}

	// 2. OpenSandbox Server sandbox create duration histogram (already in ms).
	for _, base := range []string{"opensandbox_sandbox_create_duration_milliseconds", "opensandbox_sandbox_create_duration"} {
		p50, ok50 := deltas[base+"_p50"]
		p99, ok99 := deltas[base+"_p99"]
		if ok50 || ok99 {
			if refTotalP99 <= 0 && p99 > 0 {
				refTotalP99 = p99
			}
			stages = append(stages, report.StageAttribution{
				SpanName:      "server.sandbox_create",
				P50SelfMs:     p50,
				P99SelfMs:     p99,
				CriticalShare: share(p99),
				Note:          "Prometheus " + base,
			})
			break
		}
	}

	// 3. fast-sandbox create stage latency histograms (seconds -> ms), grouped by stage label.
	const stagePrefix = "fast_sandbox_create_stage_latency_seconds_"
	stageNames := make(map[string]struct{})
	for k := range deltas {
		if !strings.HasPrefix(k, stagePrefix) {
			continue
		}
		idx := strings.Index(k, `{stage="`)
		if idx < 0 {
			continue
		}
		rest := k[idx+len(`{stage="`):]
		end := strings.Index(rest, `"}`)
		if end <= 0 {
			continue
		}
		stageNames[rest[:end]] = struct{}{}
	}
	if len(stageNames) > 0 {
		type stageEntry struct {
			name  string
			p50Ms float64
			p99Ms float64
		}
		entries := make([]stageEntry, 0, len(stageNames))
		for st := range stageNames {
			suffix := fmt.Sprintf("{stage=%q}", st)
			p50Sec := deltas["fast_sandbox_create_stage_latency_seconds_p50"+suffix]
			p99Sec := deltas["fast_sandbox_create_stage_latency_seconds_p99"+suffix]
			entries = append(entries, stageEntry{
				name:  st,
				p50Ms: p50Sec * 1000.0,
				p99Ms: p99Sec * 1000.0,
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].p99Ms != entries[j].p99Ms {
				return entries[i].p99Ms > entries[j].p99Ms
			}
			return entries[i].name < entries[j].name
		})
		if refTotalP99 <= 0 {
			for _, e := range entries {
				refTotalP99 += e.p99Ms
			}
		}
		for _, e := range entries {
			stages = append(stages, report.StageAttribution{
				SpanName:      "fast_sandbox.create." + e.name,
				P50SelfMs:     e.p50Ms,
				P99SelfMs:     e.p99Ms,
				CriticalShare: share(e.p99Ms),
				Note:          "Prometheus fast_sandbox_create_stage_latency_seconds",
			})
		}
	}

	// 4. fastlet-proxy upstream latency histogram (seconds -> ms).
	if p50, ok50 := deltas["fast_sandbox_fastlet_proxy_upstream_latency_seconds_p50"]; ok50 || deltas["fast_sandbox_fastlet_proxy_upstream_latency_seconds_p99"] > 0 {
		p99 := deltas["fast_sandbox_fastlet_proxy_upstream_latency_seconds_p99"]
		stages = append(stages, report.StageAttribution{
			SpanName:      "fastlet_proxy.upstream",
			P50SelfMs:     p50 * 1000.0,
			P99SelfMs:     p99 * 1000.0,
			CriticalShare: share(p99 * 1000.0),
			Note:          "Prometheus fast_sandbox_fastlet_proxy_upstream_latency_seconds",
		})
	}

	return stages
}

func counterDelta(before, after float64) float64 {
	d := after - before
	if d < 0 {
		// Pod restart / counter reset: fall back to after value.
		return after
	}
	return d
}

func addCounterWithLabelBreakdown(out map[string]float64, before, after *MetricSet, metricName, labelKey string) {
	afterByLabel := after.SumByLabel(metricName, labelKey)
	if len(afterByLabel) == 0 {
		return
	}
	beforeByLabel := before.SumByLabel(metricName, labelKey)
	var total float64
	for lv, av := range afterByLabel {
		d := counterDelta(beforeByLabel[lv], av)
		total += d
		if lv != "" {
			out[fmt.Sprintf("%s{%s=%q}", metricName, labelKey, lv)] = d
		}
	}
	out[metricName] = total
}

func addCounterWithMultiLabelBreakdown(out map[string]float64, before, after *MetricSet, metricName string, keys []string) {
	afterMap := sumByLabelTuple(after, metricName, keys)
	if len(afterMap) == 0 {
		return
	}
	beforeMap := sumByLabelTuple(before, metricName, keys)
	var total float64
	for tuple, av := range afterMap {
		d := counterDelta(beforeMap[tuple], av)
		total += d
		if tuple != "" {
			out[fmt.Sprintf("%s{%s}", metricName, tuple)] = d
		}
	}
	out[metricName] = total
}

func addGaugeMultiLabel(out map[string]float64, after *MetricSet, metricName string, keys []string) {
	afterMap := sumByLabelTuple(after, metricName, keys)
	for tuple, av := range afterMap {
		if tuple != "" {
			out[fmt.Sprintf("%s{%s}", metricName, tuple)] = av
		} else {
			out[metricName] = av
		}
	}
}

func sumByLabelTuple(ms *MetricSet, metricName string, keys []string) map[string]float64 {
	out := make(map[string]float64)
	if ms == nil {
		return out
	}
	for _, s := range ms.Samples[metricName] {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		var parts []string
		for _, k := range keys {
			if v, ok := s.Labels[k]; ok && v != "" {
				parts = append(parts, fmt.Sprintf("%s=%q", k, v))
			}
		}
		out[strings.Join(parts, ",")] += s.Value
	}
	return out
}

func addHistogramDelta(out map[string]float64, before, after *MetricSet, baseName string) {
	addHistogramDeltaFiltered(out, before, after, baseName, nil, "")
}

// addHistogramDeltaByLabel computes per-label-value histogram deltas when labelKey is present
// (e.g. stage on fast_sandbox_create_stage_latency_seconds), falling back to an unlabeled
// aggregate if no samples carry labelKey.
func addHistogramDeltaByLabel(out map[string]float64, before, after *MetricSet, baseName, labelKey string) {
	if after == nil {
		return
	}
	seen := make(map[string]struct{})
	for _, sampleName := range []string{baseName + "_count", baseName + "_bucket"} {
		for _, s := range after.Samples[sampleName] {
			if lv := s.Labels[labelKey]; lv != "" {
				seen[lv] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		addHistogramDeltaFiltered(out, before, after, baseName, nil, "")
		return
	}
	vals := make([]string, 0, len(seen))
	for lv := range seen {
		vals = append(vals, lv)
	}
	sort.Strings(vals)
	for _, lv := range vals {
		filter := map[string]string{labelKey: lv}
		suffix := fmt.Sprintf("{%s=%q}", labelKey, lv)
		addHistogramDeltaFiltered(out, before, after, baseName, filter, suffix)
	}
}

func addHistogramDeltaFiltered(out map[string]float64, before, after *MetricSet, baseName string, filter map[string]string, keySuffix string) {
	countName := baseName + "_count"
	sumName := baseName + "_sum"
	bucketName := baseName + "_bucket"

	afterCount := after.Sum(countName, filter)
	beforeCount := before.Sum(countName, filter)
	countDelta := counterDelta(beforeCount, afterCount)

	afterSum := after.Sum(sumName, filter)
	beforeSum := before.Sum(sumName, filter)
	sumDelta := counterDelta(beforeSum, afterSum)

	afterBuckets := aggregateHistogramBuckets(after, bucketName, filter)
	if countDelta == 0 && len(afterBuckets) == 0 && !hasMatchingSample(after, countName, filter) {
		return
	}
	out[countName+keySuffix] = countDelta
	out[sumName+keySuffix] = sumDelta
	if countDelta > 0 {
		out[baseName+"_avg"+keySuffix] = sumDelta / countDelta
	}

	if len(afterBuckets) == 0 {
		return
	}
	beforeBuckets := aggregateHistogramBuckets(before, bucketName, filter)
	var buckets []histBucket
	for le, ac := range afterBuckets {
		bc := beforeBuckets[le]
		buckets = append(buckets, histBucket{le: le, count: counterDelta(bc, ac)})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].le < buckets[j].le })
	// Enforce monotonicity after counter-reset clamping.
	for i := 1; i < len(buckets); i++ {
		if buckets[i].count < buckets[i-1].count {
			buckets[i].count = buckets[i-1].count
		}
	}
	total := buckets[len(buckets)-1].count
	if total <= 0 {
		return
	}
	for _, q := range []struct {
		name string
		p    float64
	}{
		{"_p50", 0.50},
		{"_p95", 0.95},
		{"_p99", 0.99},
	} {
		out[baseName+q.name+keySuffix] = estimateHistogramQuantile(buckets, total, q.p)
	}
}

func hasMatchingSample(ms *MetricSet, metricName string, filter map[string]string) bool {
	if ms == nil {
		return false
	}
	for _, s := range ms.Samples[metricName] {
		if matchesLabels(s.Labels, filter) {
			return true
		}
	}
	return false
}

func aggregateHistogramBuckets(ms *MetricSet, bucketName string, filter map[string]string) map[float64]float64 {
	out := make(map[float64]float64)
	if ms == nil {
		return out
	}
	for _, s := range ms.Samples[bucketName] {
		if !matchesLabels(s.Labels, filter) {
			continue
		}
		leStr, ok := s.Labels["le"]
		if !ok {
			continue
		}
		le, err := parsePromFloat(leStr)
		if err != nil || math.IsNaN(le) {
			continue
		}
		if !math.IsNaN(s.Value) && !math.IsInf(s.Value, 0) {
			out[le] += s.Value
		}
	}
	return out
}

type histBucket struct {
	le    float64
	count float64
}

func estimateHistogramQuantile(buckets []histBucket, total, q float64) float64 {
	rank := q * total
	var prevLE, prevCount float64
	for _, b := range buckets {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				return prevLE
			}
			if b.count == prevCount {
				return b.le
			}
			frac := (rank - prevCount) / (b.count - prevCount)
			return prevLE + (b.le-prevLE)*frac
		}
		if !math.IsInf(b.le, 1) {
			prevLE = b.le
		}
		prevCount = b.count
	}
	return prevLE
}

func uniqueNonEmpty(items ...string) []string {
	seen := make(map[string]struct{}, len(items))
	var out []string
	for _, s := range items {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
