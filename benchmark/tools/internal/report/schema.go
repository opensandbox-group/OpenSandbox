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
	"time"
)

const SchemaVersion = 2

// Report is the canonical schema_version=2 JSON output archived under benchmark/reports/.
type Report struct {
	SchemaVersion int                    `json:"schema_version"`
	Provenance    Provenance             `json:"provenance"`
	Environment   EnvironmentFingerprint `json:"environment"`
	Suites        map[string]SuiteResult `json:"suites"`
	LoadProfiles  *LoadProfiles          `json:"load_profiles,omitempty"`
	LeakAudit     *LeakAudit             `json:"leak_audit,omitempty"`
	Extensions    map[string]any         `json:"extensions,omitempty"`
}

// Provenance stamps exact OpenSandbox and fast-sandbox versions for release tracking.
type Provenance struct {
	RunID             string    `json:"run_id"`
	ProfileName       string    `json:"profile_name"`
	Runtime           string    `json:"runtime"`
	OpenSandboxTag    string    `json:"opensandbox_tag"`
	OpenSandboxCommit string    `json:"opensandbox_commit"`
	FastSandboxTag    string    `json:"fast_sandbox_tag"`
	FastSandboxCommit string    `json:"fast_sandbox_commit"`
	BenchGitCommit    string    `json:"bench_git_commit"`
	Timestamp         time.Time `json:"timestamp"`
}

type EnvironmentFingerprint struct {
	KubernetesVersion string            `json:"kubernetes_version,omitempty"`
	ClusterArch       string            `json:"cluster_arch,omitempty"`
	PoolMode          string            `json:"pool_mode,omitempty"`
	ReadinessPollMs   int               `json:"readiness_poll_ms,omitempty"`
	Nodes             []NodeInfo        `json:"nodes,omitempty"`
	ImageDigests      map[string]string `json:"image_digests,omitempty"`
	OverlayBD         map[string]any    `json:"overlaybd,omitempty"`
	RunnerPlacement   string            `json:"runner_placement,omitempty"`
}

type NodeInfo struct {
	Name              string `json:"name"`
	KernelVersion     string `json:"kernel_version,omitempty"`
	OSImage           string `json:"os_image,omitempty"`
	ContainerRuntime  string `json:"container_runtime,omitempty"`
	CPUIdentity       string `json:"cpu_identity,omitempty"`
	CPUCapacity       string `json:"cpu_capacity,omitempty"`
	MemoryCapacity    string `json:"memory_capacity,omitempty"`
	KVMReady          bool   `json:"kvm_ready"`
	UblkReady         bool   `json:"ublk_ready"`
	StateRootFS       string `json:"stateroot_fs,omitempty"`
	ReflinkSupported  bool   `json:"reflink_supported"`
	SpillDirAvailable bool   `json:"spill_dir_available"`
}

type SuiteResult struct {
	Name               string                 `json:"name"`
	Status             string                 `json:"status"` // passed | failed | skipped
	StartedAt          time.Time              `json:"started_at"`
	DurationSeconds    float64                `json:"duration_seconds"`
	ThroughputRPS      float64                `json:"throughput_rps,omitempty"`
	TotalOps           int                    `json:"total_ops"`
	SucceededOps       int                    `json:"succeeded_ops"`
	FailedOps          int                    `json:"failed_ops"`
	PendingFallbackOps int                    `json:"pending_fallback_ops,omitempty"`
	Errors             map[string]int         `json:"errors,omitempty"`
	Latencies          map[string]LatencyDist `json:"latencies,omitempty"` // e.g. t_api_ack, t_first_ping, t_first_exec
	OverlayBDMetrics   *OverlayBDMetrics      `json:"overlaybd_metrics,omitempty"`
	PromMetricDeltas   map[string]float64     `json:"prom_metric_deltas,omitempty"`
	StageAttribution   []StageAttribution     `json:"stage_attribution,omitempty"`
	TraceDrilldowns    []TraceDrilldown       `json:"trace_drilldowns,omitempty"`
	Extra              map[string]any         `json:"extra,omitempty"`
}

// LatencyDist uses nearest-rank percentiles matching OSEP-0034 §4.
type LatencyDist struct {
	Count  int     `json:"count"`
	MinMs  float64 `json:"min_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P90Ms  float64 `json:"p90_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
	MeanMs float64 `json:"mean_ms"`
}

type OverlayBDMetrics struct {
	UblkdPoolHitsDelta      int64   `json:"ublkd_pool_hits_delta"`
	UblkdPoolMissesDelta    int64   `json:"ublkd_pool_misses_delta"`
	UblkdPoolHitRate        float64 `json:"ublkd_pool_hit_rate"`
	UblkdPoolIdleMin        int     `json:"ublkd_pool_idle_min"`
	UblkdPoolIdleEnd        int     `json:"ublkd_pool_idle_end"`
	UblkdDaemonRestarts     int     `json:"ublkd_daemon_restarts"`
	SquashTriggeredCount    int     `json:"squash_triggered_count,omitempty"`
	UpperCopyMode           string  `json:"upper_copy_mode,omitempty"`
	StateRootDuBytesPerSbx  int64   `json:"stateroot_du_bytes_per_sandbox,omitempty"`
	StateRootFreeHeadroomGi float64 `json:"stateroot_free_headroom_gib,omitempty"`
}

type StageAttribution struct {
	SpanName      string  `json:"span_name"`
	P50SelfMs     float64 `json:"p50_self_ms"`
	P99SelfMs     float64 `json:"p99_self_ms"`
	CriticalShare float64 `json:"critical_share_pct"`
	Note          string  `json:"note,omitempty"`
}

type TraceDrilldown struct {
	Category     string             `json:"category"` // slow | median | error
	Operation    string             `json:"operation"`
	SandboxID    string             `json:"sandbox_id,omitempty"`
	TraceID      string             `json:"trace_id"`
	LatencyMs    float64            `json:"latency_ms"`
	Status       string             `json:"status"`
	TraceFile    string             `json:"trace_file,omitempty"`
	TopSpans     []StageAttribution `json:"top_spans,omitempty"`
	LinkedTraces []string           `json:"linked_traces,omitempty"`
}

type LoadProfiles struct {
	Host               *ComponentLoadSummary `json:"host,omitempty"`
	Fastlet            *ComponentLoadSummary `json:"fastlet,omitempty"`
	Egress             *ComponentLoadSummary `json:"egress,omitempty"`
	FastletProxy       *ComponentLoadSummary `json:"fastlet_proxy,omitempty"`
	FirecrackerRuntime *ComponentLoadSummary `json:"firecracker_runtime,omitempty"`
	OpenSandboxServer  *ComponentLoadSummary `json:"opensandbox_server,omitempty"`
	IngressGateway     *ComponentLoadSummary `json:"ingress_gateway,omitempty"`
	DensityFrontier    []DensityTierRow      `json:"density_frontier,omitempty"`
	// Reclaim is reserved for Phase 2 dynamic memory-reclaim / managed-balloon extensions.
	Reclaim map[string]any `json:"reclaim,omitempty"`
}

type StatSummary struct {
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

type ComponentLoadSummary struct {
	CPUCores              *StatSummary `json:"cpu_cores,omitempty"`
	RSSMiB                *StatSummary `json:"rss_mib,omitempty"`
	PerSandboxResidentMiB float64      `json:"per_sandbox_resident_mib,omitempty"`
	PerSandboxCPUMilli    float64      `json:"per_sandbox_cpu_milli,omitempty"`
	Metrics               map[string]any `json:"metrics,omitempty"`
}

type DensityTierRow struct {
	OversellRatio         float64 `json:"oversell_ratio"`
	ActiveSandboxes       int     `json:"active_sandboxes"`
	ResidentMiBPerSandbox float64 `json:"resident_mib_per_sandbox"`
	CPUPressureSomeAvg60  float64 `json:"cpu_psi_some_avg60"`
	MemPressureFullAvg60  float64 `json:"mem_psi_full_avg60"`
	MemAvailableMinMiB    float64 `json:"mem_available_min_mib"`
	AdmissionDenials      int     `json:"admission_denials"`
	OOMKills              int     `json:"oom_kills"`
	P99ExecLatencyMs      float64 `json:"p99_exec_latency_ms"`
	RecoveryTimeSeconds   float64 `json:"recovery_time_seconds,omitempty"`
}

type LeakAudit struct {
	Passed                 bool     `json:"passed"`
	HostOOMKills           int      `json:"host_oom_kills"`
	PodCgroupOOMKills      int      `json:"pod_cgroup_oom_kills"`
	ResidualSandboxCRs     int      `json:"residual_sandbox_crs"`
	ResidualSnapshotCRs    int      `json:"residual_snapshot_crs"`
	StuckDeletingSlots     int      `json:"stuck_deleting_slots"`
	OrphanedUblkDevices    int      `json:"orphaned_ublk_devices"`
	LeakedFirecrackerPIDs  int      `json:"leaked_firecracker_pids"`
	StateRootFreeHeadroomG float64  `json:"stateroot_free_headroom_gib"`
	Failures               []string `json:"failures,omitempty"`
}

// New initializes a schema_version=2 Report.
func New(prov Provenance) *Report {
	if prov.Timestamp.IsZero() {
		prov.Timestamp = time.Now().UTC()
	}
	return &Report{
		SchemaVersion: SchemaVersion,
		Provenance:    prov,
		Suites:        make(map[string]SuiteResult),
	}
}

// WriteBundle writes report.json and summary.md to outputDir.
func (r *Report) WriteBundle(outputDir string) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create report dir %q: %w", outputDir, err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report json: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(outputDir, "report.json"), raw, 0o644); err != nil {
		return fmt.Errorf("write report.json: %w", err)
	}
	md := RenderSummaryMarkdown(r)
	if err := os.WriteFile(filepath.Join(outputDir, "summary.md"), []byte(md), 0o644); err != nil {
		return fmt.Errorf("write summary.md: %w", err)
	}
	return nil
}

// Load reads a JSON report file from disk and verifies its schema_version.
func Load(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read report %q: %w", path, err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unmarshal report %q: %w", path, err)
	}
	if r.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported report schema_version %d in %q (expected %d)", r.SchemaVersion, path, SchemaVersion)
	}
	return &r, nil
}
