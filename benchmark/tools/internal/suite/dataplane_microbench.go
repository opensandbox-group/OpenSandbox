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

package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	defaultMicrobenchConcurrentSandboxes = 4
	defaultMicrobenchCPUSeconds          = 5
	defaultMicrobenchStorageFileMiB      = 64
	defaultMicrobenchNetworkRequests     = 50
	defaultMicrobenchNetworkConcurrency  = 4
	defaultMicrobenchSandboxTimeoutSec   = 600
)

// CPUMicrobenchResult captures per-sandbox CPU benchmark output.
type CPUMicrobenchResult struct {
	SandboxID    string  `json:"sandbox_id,omitempty"`
	Tool         string  `json:"tool"` // sysbench | python3-fallback
	EventsPerSec float64 `json:"events_per_sec"`
	OpsPerSec    float64 `json:"ops_per_sec,omitempty"`
	TotalEvents  float64 `json:"total_events,omitempty"`
	ElapsedMs    float64 `json:"elapsed_ms"`
}

// CPUConcurrentSummary aggregates multi-sandbox concurrent CPU runs vs single baseline.
type CPUConcurrentSummary struct {
	SandboxCount           int                   `json:"sandbox_count"`
	SucceededCount         int                   `json:"succeeded_count"`
	MeanEventsPerSec       float64               `json:"mean_events_per_sec"`
	MinEventsPerSec        float64               `json:"min_events_per_sec"`
	MaxEventsPerSec        float64               `json:"max_events_per_sec"`
	ScalingEfficiencyRatio float64               `json:"scaling_efficiency_ratio,omitempty"`
	PerSandbox             []CPUMicrobenchResult `json:"per_sandbox,omitempty"`
}

// StorageMicrobenchResult captures guest upper-layer storage microbenchmark metrics.
type StorageMicrobenchResult struct {
	Tool              string  `json:"tool"`                    // fio | python3-dd-fallback
	IOPath            string  `json:"io_path,omitempty"`       // direct_io | guest_page_cache
	StorageLayer      string  `json:"storage_layer,omitempty"` // upper_rw
	FileSizeMiB       int     `json:"file_size_mib"`
	RandRead4KIOPS    float64 `json:"rand_read_4k_iops"`
	RandWrite4KIOPS   float64 `json:"rand_write_4k_iops"`
	SeqRead1MColdMiBs float64 `json:"seq_read_1m_cold_mibs"`
	SeqRead1MHotMiBs  float64 `json:"seq_read_1m_hot_mibs"`
	SeqWrite1MMiBs    float64 `json:"seq_write_1m_mibs"`
	ElapsedMs         float64 `json:"elapsed_ms"`
}

// NetworkMicrobenchResult captures egress / fastlet-proxy network throughput and RTT metrics.
type NetworkMicrobenchResult struct {
	Tool           string    `json:"tool"` // iperf3 | http-probe
	Target         string    `json:"target"`
	Concurrency    int       `json:"concurrency"`
	Requests       int       `json:"requests,omitempty"`
	Succeeded      int       `json:"succeeded,omitempty"`
	Failed         int       `json:"failed,omitempty"`
	RPS            float64   `json:"rps,omitempty"`
	ThroughputMbps float64   `json:"throughput_mbps,omitempty"`
	RTTP50Ms       float64   `json:"rtt_p50_ms,omitempty"`
	RTTP95Ms       float64   `json:"rtt_p95_ms,omitempty"`
	RTTP99Ms       float64   `json:"rtt_p99_ms,omitempty"`
	RTTMeanMs      float64   `json:"rtt_mean_ms,omitempty"`
	RTTSamplesMs   []float64 `json:"-"`
	ElapsedMs      float64   `json:"elapsed_ms"`
}

// DataplaneMicrobenchSuite evaluates in-sandbox CPU, OverlayBD ublk storage,
// and egress/fastlet-proxy network data-plane overhead (OSEP-0034 Phase 2).
type DataplaneMicrobenchSuite struct {
	cfg suiteConfig
}

// NewDataplaneMicrobenchSuite constructs the dataplane-microbench suite.
func NewDataplaneMicrobenchSuite(opts ...SuiteOption) *DataplaneMicrobenchSuite {
	s := &DataplaneMicrobenchSuite{}
	for _, opt := range opts {
		opt(&s.cfg)
	}
	return s
}

func (s *DataplaneMicrobenchSuite) Name() string { return "dataplane-microbench" }

func (s *DataplaneMicrobenchSuite) Description() string {
	return "In-sandbox CPU (sysbench/python3), OverlayBD ublk storage (fio/python3+dd), and egress/proxy network (iperf3/HTTP RTT) data-plane microbenchmarks"
}

func (s *DataplaneMicrobenchSuite) SupportedRuntimes() []string { return nil }

func (s *DataplaneMicrobenchSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	startedAt := time.Now().UTC()
	hooks := resolveHooks(ctx, s.cfg)
	client := NewBenchClient(rc.SDKConfig, WithClientHooks(hooks), WithClientLogger(rc.Logger))

	spec := resolveDataplaneMicrobenchSpec(rc)
	pollInterval := time.Duration(spec.ReadinessPollMs) * time.Millisecond
	if pollInterval <= 0 {
		pollInterval = defaultReadinessPollInterval
	}

	templateID, err := client.ResolveTemplateID(ctx, rc.Profile)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve template id: %w", err)
	}
	baseImage := "python:3.12-bookworm"
	if rc.Profile != nil && rc.Profile.Stack.Template.BaseImage != "" {
		baseImage = rc.Profile.Stack.Template.BaseImage
	}

	if hooks.MetricsSnapshotter != nil {
		_ = hooks.MetricsSnapshotter.BeforeSuite(ctx, s.Name())
	}

	rec := newScaleRecorder()
	var (
		createdMu  sync.Mutex
		createdIDs []string
	)
	trackSandbox := func(id string) {
		if id == "" {
			return
		}
		createdMu.Lock()
		createdIDs = append(createdIDs, id)
		createdMu.Unlock()
	}
	defer func() {
		createdMu.Lock()
		ids := append([]string(nil), createdIDs...)
		createdMu.Unlock()
		deleteSandboxesBestEffort(client, ids, 16)
	}()

	createOpts := CreateOptions{
		TemplateID:            templateID,
		Image:                 baseImage,
		TimeoutSeconds:        spec.SandboxTimeoutSeconds,
		ReadinessPollInterval: pollInterval,
		SkipFirstExec:         true,
	}

	// 1. Launch single baseline sandbox for CPU baseline, Storage, and Network microbenchmarks.
	baseSbx, err := client.CreateSandbox(ctx, createOpts)
	if baseSbx != nil && baseSbx.SandboxID != "" {
		trackSandbox(baseSbx.SandboxID)
	}
	if err != nil {
		code := "create_error"
		if baseSbx != nil && baseSbx.ErrorCode != "" {
			code = baseSbx.ErrorCode
		}
		rec.recordOp(OperationSample{
			Operation: "dataplane_create_baseline",
			SandboxID: sandboxIDOrEmpty(baseSbx),
			Status:    "error",
			ErrorCode: code,
		})
		return buildDataplaneResult(s.Name(), startedAt, rec, nil, nil, nil, nil, hooks, ctx), rec.samples, nil
	}
	rec.addLatency("t_api_ack", baseSbx.APIAckMs)
	rec.addLatency("t_first_ping", baseSbx.FirstPingMs)
	rec.recordOp(OperationSample{
		Operation: "dataplane_create_baseline",
		SandboxID: baseSbx.SandboxID,
		LatencyMs: baseSbx.FirstPingMs,
		Status:    "ok",
		AckState:  baseSbx.AckState,
	})

	// 1a. CPU single-sandbox baseline.
	cpuCmd := buildCPUMicrobenchCommand(spec.CPUSeconds)
	cpuSingle, cpuErr := runCPUMicrobench(ctx, client, baseSbx.SandboxID, baseSbx.Execd, cpuCmd)
	if cpuErr != nil {
		rec.recordOp(OperationSample{
			Operation: "dataplane_cpu_single",
			SandboxID: baseSbx.SandboxID,
			Status:    "error",
			ErrorCode: scaleErrorCode(cpuErr),
		})
	} else {
		rec.addLatency("cpu_bench_ms", cpuSingle.ElapsedMs)
		rec.recordOp(OperationSample{
			Operation: "dataplane_cpu_single",
			SandboxID: baseSbx.SandboxID,
			LatencyMs: cpuSingle.ElapsedMs,
			Status:    "ok",
		})
	}

	// 2. Storage (OverlayBD ublk 4K rand R/W + 1M seq cold/hot R/W).
	storageCmd := buildStorageMicrobenchCommand(spec.StorageFileSizeMiB)
	storageRes, storageErr := runStorageMicrobench(ctx, client, baseSbx.SandboxID, baseSbx.Execd, storageCmd, spec.StorageFileSizeMiB)
	if storageErr != nil {
		rec.recordOp(OperationSample{
			Operation: "dataplane_storage",
			SandboxID: baseSbx.SandboxID,
			Status:    "error",
			ErrorCode: scaleErrorCode(storageErr),
		})
	} else {
		rec.addLatency("storage_bench_ms", storageRes.ElapsedMs)
		rec.recordOp(OperationSample{
			Operation: "dataplane_storage",
			SandboxID: baseSbx.SandboxID,
			LatencyMs: storageRes.ElapsedMs,
			Status:    "ok",
		})
	}

	// 3. Network (iperf3 if configured, else concurrent HTTP throughput & RTT; skip when networkTargetUrl="none").
	netTarget := strings.TrimSpace(spec.NetworkTargetURL)
	var netRes *NetworkMicrobenchResult
	if !strings.EqualFold(netTarget, "none") && !strings.EqualFold(netTarget, "disabled") {
		if netTarget == "" {
			netTarget = strings.TrimRight(rc.SDKConfig.GetBaseURL(), "/") + "/health"
		}
		netCmd := buildNetworkMicrobenchCommand(spec.Iperf3Server, netTarget, spec.NetworkRequests, spec.NetworkConcurrency, spec.CPUSeconds)
		var netErr error
		netRes, netErr = runNetworkMicrobench(ctx, client, baseSbx.SandboxID, baseSbx.Execd, netCmd, netTarget, spec.NetworkConcurrency)
		if netErr != nil {
			rec.recordOp(OperationSample{
				Operation: "dataplane_network",
				SandboxID: baseSbx.SandboxID,
				Status:    "error",
				ErrorCode: scaleErrorCode(netErr),
			})
		} else if netRes.Requests > 0 && netRes.Failed >= netRes.Requests {
			rec.recordOp(OperationSample{
				Operation: "dataplane_network",
				SandboxID: baseSbx.SandboxID,
				LatencyMs: netRes.ElapsedMs,
				Status:    "error",
				ErrorCode: "network_all_requests_failed",
			})
		} else {
			if len(netRes.RTTSamplesMs) > 0 {
				for _, rtt := range netRes.RTTSamplesMs {
					rec.addLatency("network_rtt_ms", rtt)
				}
			} else if netRes.RTTP50Ms > 0 {
				rec.addLatency("network_rtt_ms", netRes.RTTP50Ms)
			}
			rec.addLatency("network_bench_ms", netRes.ElapsedMs)
			rec.recordOp(OperationSample{
				Operation: "dataplane_network",
				SandboxID: baseSbx.SandboxID,
				LatencyMs: netRes.ElapsedMs,
				Status:    "ok",
			})
		}
	}

	// 4. Multi-sandbox concurrent CPU comparison (when ConcurrentSandboxes > 1).
	var cpuConcurrent *CPUConcurrentSummary
	if spec.ConcurrentSandboxes > 1 && ctx.Err() == nil {
		cpuConcurrent = runConcurrentCPUMicrobench(ctx, client, createOpts, cpuCmd, spec.ConcurrentSandboxes, cpuSingle, trackSandbox, rec)
	}

	res := buildDataplaneResult(s.Name(), startedAt, rec, cpuSingle, cpuConcurrent, storageRes, netRes, hooks, ctx)
	return res, rec.samples, nil
}

func resolveDataplaneMicrobenchSpec(rc RunContext) profile.DataplaneMicrobenchSuiteSpec {
	spec := profile.DataplaneMicrobenchSuiteSpec{
		ConcurrentSandboxes:   defaultMicrobenchConcurrentSandboxes,
		CPUSeconds:            defaultMicrobenchCPUSeconds,
		StorageFileSizeMiB:    defaultMicrobenchStorageFileMiB,
		NetworkRequests:       defaultMicrobenchNetworkRequests,
		NetworkConcurrency:    defaultMicrobenchNetworkConcurrency,
		ReadinessPollMs:       10,
		SandboxTimeoutSeconds: defaultMicrobenchSandboxTimeoutSec,
	}
	if rc.Profile != nil && rc.Profile.Suites.DataplaneMicrobench != nil {
		in := rc.Profile.Suites.DataplaneMicrobench
		if in.ConcurrentSandboxes > 0 {
			spec.ConcurrentSandboxes = in.ConcurrentSandboxes
		}
		if in.CPUSeconds > 0 {
			spec.CPUSeconds = in.CPUSeconds
		}
		if in.StorageFileSizeMiB > 0 {
			spec.StorageFileSizeMiB = in.StorageFileSizeMiB
		}
		if in.NetworkTargetURL != "" {
			spec.NetworkTargetURL = in.NetworkTargetURL
		}
		if in.Iperf3Server != "" {
			spec.Iperf3Server = in.Iperf3Server
		}
		if in.NetworkRequests > 0 {
			spec.NetworkRequests = in.NetworkRequests
		}
		if in.NetworkConcurrency > 0 {
			spec.NetworkConcurrency = in.NetworkConcurrency
		}
		if in.ReadinessPollMs > 0 {
			spec.ReadinessPollMs = in.ReadinessPollMs
		}
		if in.SandboxTimeoutSeconds > 0 {
			spec.SandboxTimeoutSeconds = in.SandboxTimeoutSeconds
		}
	}
	return spec
}

func runConcurrentCPUMicrobench(
	ctx context.Context,
	client *BenchClient,
	createOpts CreateOptions,
	cpuCmd string,
	count int,
	singleBaseline *CPUMicrobenchResult,
	trackSandbox func(string),
	rec *scaleRecorder,
) *CPUConcurrentSummary {
	type liveSbx struct {
		id    string
		execd *opensandbox.ExecdClient
	}
	var (
		liveMu sync.Mutex
		live   []liveSbx
		wg     sync.WaitGroup
	)
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cr, err := client.CreateSandbox(ctx, createOpts)
			if cr != nil && cr.SandboxID != "" {
				trackSandbox(cr.SandboxID)
			}
			if err != nil {
				code := scaleErrorCode(err)
				if cr != nil && cr.ErrorCode != "" {
					code = cr.ErrorCode
				}
				rec.recordOp(OperationSample{
					Operation: "dataplane_create_concurrent",
					SandboxID: sandboxIDOrEmpty(cr),
					Status:    "error",
					ErrorCode: code,
				})
				return
			}
			rec.addLatency("t_api_ack", cr.APIAckMs)
			rec.addLatency("t_first_ping", cr.FirstPingMs)
			rec.recordOp(OperationSample{
				Operation: "dataplane_create_concurrent",
				SandboxID: cr.SandboxID,
				LatencyMs: cr.FirstPingMs,
				Status:    "ok",
				AckState:  cr.AckState,
			})
			liveMu.Lock()
			live = append(live, liveSbx{id: cr.SandboxID, execd: cr.Execd})
			liveMu.Unlock()
		}()
	}
	wg.Wait()

	summary := &CPUConcurrentSummary{
		SandboxCount: count,
		PerSandbox:   make([]CPUMicrobenchResult, 0, len(live)),
	}
	var resMu sync.Mutex
	for _, sbx := range live {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(target liveSbx) {
			defer wg.Done()
			cpuRes, err := runCPUMicrobench(ctx, client, target.id, target.execd, cpuCmd)
			if err != nil {
				rec.recordOp(OperationSample{
					Operation: "dataplane_cpu_concurrent",
					SandboxID: target.id,
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
				return
			}
			rec.addLatency("cpu_bench_concurrent_ms", cpuRes.ElapsedMs)
			rec.recordOp(OperationSample{
				Operation: "dataplane_cpu_concurrent",
				SandboxID: target.id,
				LatencyMs: cpuRes.ElapsedMs,
				Status:    "ok",
			})
			resMu.Lock()
			summary.PerSandbox = append(summary.PerSandbox, *cpuRes)
			resMu.Unlock()
		}(sbx)
	}
	wg.Wait()

	summary.SucceededCount = len(summary.PerSandbox)
	if len(summary.PerSandbox) > 0 {
		var sum float64
		minV := math.MaxFloat64
		maxV := 0.0
		for _, item := range summary.PerSandbox {
			v := item.EventsPerSec
			if v == 0 {
				v = item.OpsPerSec
			}
			sum += v
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		summary.MeanEventsPerSec = sum / float64(len(summary.PerSandbox))
		summary.MinEventsPerSec = minV
		summary.MaxEventsPerSec = maxV
		if singleBaseline != nil {
			baseRate := singleBaseline.EventsPerSec
			if baseRate == 0 {
				baseRate = singleBaseline.OpsPerSec
			}
			if baseRate > 0 {
				summary.ScalingEfficiencyRatio = summary.MeanEventsPerSec / baseRate
			}
		}
	}
	return summary
}

func runCPUMicrobench(ctx context.Context, client *BenchClient, sandboxID string, execd *opensandbox.ExecdClient, cmd string) (*CPUMicrobenchResult, error) {
	execRes, err := client.ExecCommand(ctx, execd, cmd)
	if err != nil {
		return nil, err
	}
	line := extractLastJSONLine(execRes.Stdout)
	var out CPUMicrobenchResult
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		return nil, fmt.Errorf("parse cpu microbench output %q: %w", execRes.Stdout, err)
	}
	out.SandboxID = sandboxID
	if out.ElapsedMs <= 0 {
		out.ElapsedMs = execRes.DurationMs
	}
	if out.EventsPerSec == 0 && out.OpsPerSec > 0 {
		out.EventsPerSec = out.OpsPerSec
	}
	return &out, nil
}

func runStorageMicrobench(ctx context.Context, client *BenchClient, sandboxID string, execd *opensandbox.ExecdClient, cmd string, fileSizeMiB int) (*StorageMicrobenchResult, error) {
	execRes, err := client.ExecCommand(ctx, execd, cmd)
	if err != nil {
		return nil, err
	}
	line := extractLastJSONLine(execRes.Stdout)
	var out StorageMicrobenchResult
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		return nil, fmt.Errorf("parse storage microbench output %q: %w", execRes.Stdout, err)
	}
	if out.FileSizeMiB <= 0 {
		out.FileSizeMiB = fileSizeMiB
	}
	if out.ElapsedMs <= 0 {
		out.ElapsedMs = execRes.DurationMs
	}
	_ = sandboxID
	return &out, nil
}

func runNetworkMicrobench(ctx context.Context, client *BenchClient, sandboxID string, execd *opensandbox.ExecdClient, cmd, target string, concurrency int) (*NetworkMicrobenchResult, error) {
	execRes, err := client.ExecCommand(ctx, execd, cmd)
	if err != nil {
		return nil, err
	}
	line := extractLastJSONLine(execRes.Stdout)
	var raw struct {
		NetworkMicrobenchResult
		RTTSamplesMs []float64 `json:"rtt_samples_ms,omitempty"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, fmt.Errorf("parse network microbench output %q: %w", execRes.Stdout, err)
	}
	out := raw.NetworkMicrobenchResult
	out.RTTSamplesMs = raw.RTTSamplesMs
	if out.Target == "" {
		out.Target = target
	}
	if out.Concurrency <= 0 {
		out.Concurrency = concurrency
	}
	if out.ElapsedMs <= 0 {
		out.ElapsedMs = execRes.DurationMs
	}
	if len(out.RTTSamplesMs) > 0 && out.RTTP50Ms == 0 {
		dist := report.ComputeLatencyDist(out.RTTSamplesMs)
		out.RTTP50Ms = dist.P50Ms
		out.RTTP95Ms = dist.P95Ms
		out.RTTP99Ms = dist.P99Ms
		out.RTTMeanMs = dist.MeanMs
	}
	_ = sandboxID
	return &out, nil
}

func buildDataplaneResult(
	name string,
	startedAt time.Time,
	rec *scaleRecorder,
	cpuSingle *CPUMicrobenchResult,
	cpuConcurrent *CPUConcurrentSummary,
	storageRes *StorageMicrobenchResult,
	netRes *NetworkMicrobenchResult,
	hooks BenchHooks,
	ctx context.Context,
) *report.SuiteResult {
	durationSec := time.Since(startedAt).Seconds()
	latencies := make(map[string]report.LatencyDist, len(rec.latencies))
	for k, vals := range rec.latencies {
		if len(vals) > 0 {
			latencies[k] = report.ComputeLatencyDist(vals)
		}
	}

	status := "passed"
	if rec.totalOps == 0 || rec.failedOps > 0 || ctx.Err() != nil {
		status = "failed"
	}

	extra := map[string]any{
		"measurement_scope": "guest_absolute",
	}
	cpuExtra := make(map[string]any)
	if cpuSingle != nil {
		cpuExtra["single"] = cpuSingle
	}
	if cpuConcurrent != nil {
		cpuExtra["concurrent"] = cpuConcurrent
	}
	if len(cpuExtra) > 0 {
		extra["cpu"] = cpuExtra
	}
	if storageRes != nil {
		extra["storage"] = storageRes
	}
	if netRes != nil {
		extra["network"] = netRes
	}

	res := &report.SuiteResult{
		Name:               name,
		Status:             status,
		StartedAt:          startedAt,
		DurationSeconds:    durationSec,
		TotalOps:           rec.totalOps,
		SucceededOps:       rec.succeededOps,
		FailedOps:          rec.failedOps,
		PendingFallbackOps: rec.pendingFallback,
		Errors:             rec.errors,
		Latencies:          latencies,
		Extra:              extra,
	}

	if hooks.MetricsSnapshotter != nil {
		if obd, prom, err := hooks.MetricsSnapshotter.AfterSuite(ctx, name); err == nil {
			res.OverlayBDMetrics = obd
			res.PromMetricDeltas = prom
		}
	}
	return res
}

func buildCPUMicrobenchCommand(seconds int) string {
	if seconds <= 0 {
		seconds = defaultMicrobenchCPUSeconds
	}
	return fmt.Sprintf(`sh -c '
if command -v sysbench >/dev/null 2>&1; then
  OUT=$(sysbench cpu --cpu-max-prime=20000 --time=%d run 2>&1)
  EPS=$(printf "%%s\n" "$OUT" | awk "/events per second:/ {print \$4; exit}")
  TOT=$(printf "%%s\n" "$OUT" | awk "/total number of events:/ {print \$5; exit}")
  SEC=$(printf "%%s\n" "$OUT" | awk "/total time:/ {gsub(/s/,\"\",\$3); print \$3; exit}")
  python3 -c "import json; eps=float(\"${EPS:-0}\"); tot=float(\"${TOT:-0}\"); sec=float(\"${SEC:-0}\"); print(json.dumps({\"tool\":\"sysbench\",\"events_per_sec\":eps,\"total_events\":tot,\"elapsed_ms\":sec*1000.0}))"
else
  python3 -c "
import json, math, time
t0 = time.perf_counter()
iters = 0
deadline = t0 + %d
while time.perf_counter() < deadline:
    n = 1000
    c = 0
    for x in range(2, n):
        r = int(math.isqrt(x))
        ok = True
        for d in range(2, r + 1):
            if x %% d == 0:
                ok = False
                break
        if ok:
            c += 1
    iters += 1
elapsed = max(time.perf_counter() - t0, 1e-6)
ops = iters / elapsed
print(json.dumps({\"tool\":\"python3-fallback\",\"events_per_sec\":ops,\"ops_per_sec\":ops,\"total_events\":iters,\"elapsed_ms\":elapsed*1000.0}))
"
fi
'`, seconds, seconds)
}

func buildStorageMicrobenchCommand(fileSizeMiB int) string {
	if fileSizeMiB <= 0 {
		fileSizeMiB = defaultMicrobenchStorageFileMiB
	}
	return fmt.Sprintf(`sh -c '
TESTFILE=/tmp/osb-microbench-io.dat
rm -f "$TESTFILE"
if command -v fio >/dev/null 2>&1; then
  python3 -c "
import json, subprocess, time, os
t0 = time.perf_counter()
fn = \"$TESTFILE\"
sz = \"%dM\"
def run_fio(args):
    out = subprocess.check_output([\"fio\", \"--name=bench\", \"--filename=\" + fn, \"--size=\" + sz, \"--ioengine=psync\", \"--output-format=json\"] + args, text=True)
    return json.loads(out)[\"jobs\"][0]
sw = run_fio([\"--rw=write\", \"--bs=1m\", \"--direct=1\"])
sr_cold = run_fio([\"--rw=read\", \"--bs=1m\", \"--direct=1\"])
sr_hot = run_fio([\"--rw=read\", \"--bs=1m\", \"--direct=0\"])
rw = run_fio([\"--rw=randwrite\", \"--bs=4k\", \"--direct=1\", \"--runtime=3\", \"--time_based\"])
rr = run_fio([\"--rw=randread\", \"--bs=4k\", \"--direct=1\", \"--runtime=3\", \"--time_based\"])
try: os.remove(fn)
except OSError: pass
print(json.dumps({
    \"tool\": \"fio\",
    \"io_path\": \"direct_io\",
    \"storage_layer\": \"upper_rw\",
    \"file_size_mib\": %d,
    \"rand_read_4k_iops\": rr[\"read\"][\"iops\"],
    \"rand_write_4k_iops\": rw[\"write\"][\"iops\"],
    \"seq_read_1m_cold_mibs\": sr_cold[\"read\"][\"bw_bytes\"] / (1024*1024),
    \"seq_read_1m_hot_mibs\": sr_hot[\"read\"][\"bw_bytes\"] / (1024*1024),
    \"seq_write_1m_mibs\": sw[\"write\"][\"bw_bytes\"] / (1024*1024),
    \"elapsed_ms\": (time.perf_counter() - t0) * 1000.0,
}))
"
else
  python3 -c "
import json, mmap, os, random, time
t0 = time.perf_counter()
fn = \"$TESTFILE\"
sz_mib = %d
o_direct = getattr(os, \"O_DIRECT\", 0)
io_path = \"guest_page_cache\"
def aligned_buf(size, fill):
    m = mmap.mmap(-1, size)
    m.write(fill * size)
    return m
def open_maybe_direct(path, flags, mode=0o600):
    global io_path
    if o_direct:
        try:
            fd = os.open(path, flags | o_direct, mode)
            io_path = \"direct_io\"
            return fd, True
        except OSError:
            pass
    return os.open(path, flags, mode), False
buf_1m = aligned_buf(1024 * 1024, b\"x\")
t_sw = time.perf_counter()
fd, is_dir = open_maybe_direct(fn, os.O_CREAT | os.O_TRUNC | os.O_WRONLY)
for _ in range(sz_mib):
    os.write(fd, buf_1m)
if not is_dir:
    os.fdatasync(fd)
os.close(fd)
seq_w = sz_mib / max(time.perf_counter() - t_sw, 1e-6)
fd, is_dir = open_maybe_direct(fn, os.O_RDONLY)
if not is_dir and hasattr(os, \"posix_fadvise\") and hasattr(os, \"POSIX_FADV_DONTNEED\"):
    try: os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
    except OSError: pass
t_sc = time.perf_counter()
if is_dir:
    for _ in range(sz_mib):
        os.read(fd, 1024 * 1024)
else:
    while os.read(fd, 1024 * 1024): pass
seq_rc = sz_mib / max(time.perf_counter() - t_sc, 1e-6)
os.close(fd)
fd = os.open(fn, os.O_RDONLY)
t_sh = time.perf_counter()
while os.read(fd, 1024 * 1024): pass
seq_rh = sz_mib / max(time.perf_counter() - t_sh, 1e-6)
os.close(fd)
pages = max(1, (sz_mib * 1024) // 4)
fd, is_dir = open_maybe_direct(fn, os.O_RDWR)
buf4k = aligned_buf(4096, b\"y\")
ops = 2000
t_rw = time.perf_counter()
for _ in range(ops):
    os.pwrite(fd, buf4k, random.randrange(pages) * 4096)
    if not is_dir:
        os.fdatasync(fd)
rand_w_iops = ops / max(time.perf_counter() - t_rw, 1e-6)
if not is_dir and hasattr(os, \"posix_fadvise\") and hasattr(os, \"POSIX_FADV_DONTNEED\"):
    try: os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
    except OSError: pass
t_rr = time.perf_counter()
for _ in range(ops):
    os.pread(fd, 4096, random.randrange(pages) * 4096)
rand_r_iops = ops / max(time.perf_counter() - t_rr, 1e-6)
os.close(fd)
try: os.remove(fn)
except OSError: pass
print(json.dumps({
    \"tool\": \"python3-dd-fallback\",
    \"io_path\": io_path,
    \"storage_layer\": \"upper_rw\",
    \"file_size_mib\": sz_mib,
    \"rand_read_4k_iops\": rand_r_iops,
    \"rand_write_4k_iops\": rand_w_iops,
    \"seq_read_1m_cold_mibs\": seq_rc,
    \"seq_read_1m_hot_mibs\": seq_rh,
    \"seq_write_1m_mibs\": seq_w,
    \"elapsed_ms\": (time.perf_counter() - t0) * 1000.0,
}))
"
fi
'`, fileSizeMiB, fileSizeMiB, fileSizeMiB)
}

func buildNetworkMicrobenchCommand(iperf3Server, targetURL string, requests, concurrency, durationSec int) string {
	if requests <= 0 {
		requests = defaultMicrobenchNetworkRequests
	}
	if concurrency <= 0 {
		concurrency = defaultMicrobenchNetworkConcurrency
	}
	if durationSec <= 0 {
		durationSec = defaultMicrobenchCPUSeconds
	}
	if strings.TrimSpace(iperf3Server) != "" {
		return fmt.Sprintf(`sh -c '
if command -v iperf3 >/dev/null 2>&1; then
  python3 -c "
import json, subprocess, time
t0 = time.perf_counter()
raw = subprocess.check_output([\"iperf3\", \"-c\", %q, \"-t\", \"%d\", \"-P\", \"%d\", \"-J\"], text=True)
data = json.loads(raw)
end = data.get(\"end\", {})
bps = end.get(\"sum_received\", end.get(\"sum_sent\", {})).get(\"bits_per_second\", 0.0)
print(json.dumps({\"tool\":\"iperf3\",\"target\":%q,\"concurrency\":%d,\"throughput_mbps\":bps/1e6,\"elapsed_ms\":(time.perf_counter()-t0)*1000.0}))
"
  exit 0
fi
'`, iperf3Server, durationSec, concurrency, iperf3Server, concurrency)
	}
	return fmt.Sprintf(`python3 -c '
import concurrent.futures, json, time, urllib.request
url = %q
total = %d
conc = %d
t0 = time.perf_counter()
rtts = []
total_bytes = 0
ok = 0
fail = 0
def one(_):
    s = time.perf_counter()
    with urllib.request.urlopen(url, timeout=5) as resp:
        data = resp.read()
    return (time.perf_counter() - s) * 1000.0, len(data)
with concurrent.futures.ThreadPoolExecutor(max_workers=conc) as ex:
    futs = [ex.submit(one, i) for i in range(total)]
    for f in futs:
        try:
            ms, nb = f.result()
            rtts.append(ms)
            total_bytes += nb
            ok += 1
        except Exception:
            fail += 1
elapsed = max(time.perf_counter() - t0, 1e-6)
rtts.sort()
def pct(p):
    if not rtts: return 0.0
    idx = min(len(rtts)-1, max(0, int((p/100.0)*len(rtts) - 0.0001)))
    return rtts[idx]
print(json.dumps({
    "tool": "http-probe",
    "target": url,
    "concurrency": conc,
    "requests": total,
    "succeeded": ok,
    "failed": fail,
    "rps": ok / elapsed,
    "throughput_mbps": (total_bytes * 8.0) / (elapsed * 1e6),
    "rtt_p50_ms": pct(50),
    "rtt_p95_ms": pct(95),
    "rtt_p99_ms": pct(99),
    "rtt_mean_ms": (sum(rtts)/len(rtts)) if rtts else 0.0,
    "rtt_samples_ms": rtts,
    "elapsed_ms": elapsed * 1000.0,
}))
'`, targetURL, requests, concurrency)
}

func extractLastJSONLine(stdout string) string {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			return trimmed
		}
	}
	return strings.TrimSpace(stdout)
}

func sandboxIDOrEmpty(cr *CreateResult) string {
	if cr == nil {
		return ""
	}
	return cr.SandboxID
}

func deleteSandboxesBestEffort(client *BenchClient, ids []string, concurrency int) {
	if len(ids) == 0 {
		return
	}
	if concurrency <= 0 {
		concurrency = 16
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		if id == "" {
			continue
		}
		wg.Add(1)
		go func(sbxID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			client.BestEffortDelete(sbxID)
		}(id)
	}
	wg.Wait()
}
