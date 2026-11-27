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
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	defaultDensityRampConcurrency    = 32
	defaultDensityRampMaxFailureRate = 0.05
	defaultDensityRampPingSampleSize = 20
	defaultDensityRampSandboxTimeout = 1800
)

// DensityRampStepResult records the outcome of a single live-retention step.
type DensityRampStepResult struct {
	TargetCount                int     `json:"target_count"`
	RequestedDelta             int     `json:"requested_delta"`
	BatchSucceeded             int     `json:"batch_succeeded"`
	BatchFailed                int     `json:"batch_failed"`
	BatchFailureRate           float64 `json:"batch_failure_rate"`
	LiveSandboxes              int     `json:"live_sandboxes"`
	PingSampled                int     `json:"ping_sampled"`
	PingHealthy                int     `json:"ping_healthy"`
	PingFailed                 int     `json:"ping_failed"`
	PingP99Ms                  float64 `json:"ping_p99_ms,omitempty"`
	DurationSeconds            float64 `json:"duration_seconds"`
	StoppedEarly               bool    `json:"stopped_early,omitempty"`
	DeltaMemAvailableMiBPerSbx float64 `json:"delta_mem_available_mib_per_sbx,omitempty"`
	FCPssMiBPerSbx             float64 `json:"fc_pss_mib_per_sbx,omitempty"`
	FCRssMiBPerSbx             float64 `json:"fc_rss_mib_per_sbx,omitempty"`
	DeltaSlabMiBPerSbx         float64 `json:"delta_slab_mib_per_sbx,omitempty"`
	DeltaSUnreclaimMiBPerSbx   float64 `json:"delta_sunreclaim_mib_per_sbx,omitempty"`
	FastletRSSMiB              float64 `json:"fastlet_rss_mib,omitempty"`
	EgressRSSMiB               float64 `json:"egress_rss_mib,omitempty"`
	UblkdRSSMiB                float64 `json:"ublkd_rss_mib,omitempty"`
	ProxyRSSMiB                float64 `json:"proxy_rss_mib,omitempty"`
	HostCPUUtilPct             float64 `json:"host_cpu_util_pct,omitempty"`
	HostLoad1m                 float64 `json:"host_load_1m,omitempty"`
	MemAvailableMiB            float64 `json:"mem_available_mib,omitempty"`
}

// DensityRampOption configures optional timings for DensityRampSuite.
type DensityRampOption func(*DensityRampSuite)

// WithDensityRampPollInterval overrides readiness polling cadence.
func WithDensityRampPollInterval(d time.Duration) DensityRampOption {
	return func(s *DensityRampSuite) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// DensityRampSuite performs a live concurrent retention ramp across configured
// sandbox count steps, keeping created sandboxes alive across steps, sampling
// /ping health at each step, stopping early safely when batch failure rate
// exceeds the threshold, and always cleaning up all live sandboxes before return.
type DensityRampSuite struct {
	pollInterval time.Duration
}

// NewDensityRampSuite constructs the density-ramp suite.
func NewDensityRampSuite(opts ...DensityRampOption) *DensityRampSuite {
	s := &DensityRampSuite{
		pollInterval: scaleDefaultPollInterval,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *DensityRampSuite) Name() string { return "density-ramp" }

func (s *DensityRampSuite) Description() string {
	return "Live concurrent sandbox retention ramp with step health sampling, safety stop, and guaranteed cleanup"
}

func (s *DensityRampSuite) SupportedRuntimes() []string {
	return nil
}

type densityActiveSandbox struct {
	id        string
	epURL     string
	epHeaders map[string]string
}

func (s *DensityRampSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	startedAt := time.Now().UTC()

	spec := &profile.DensityRampSuiteSpec{
		Steps:                  []int{100, 200, 400, 600, 800, 1000},
		BatchCreateConcurrency: defaultDensityRampConcurrency,
		MaxBatchFailureRate:    defaultDensityRampMaxFailureRate,
		PingSampleSize:         defaultDensityRampPingSampleSize,
		ReadinessPollMs:        10,
		SandboxTimeoutSeconds:  defaultDensityRampSandboxTimeout,
	}
	if rc.Profile != nil && rc.Profile.Suites.DensityRamp != nil {
		spec = rc.Profile.Suites.DensityRamp
	}

	steps := spec.Steps
	if len(steps) == 0 {
		steps = []int{100, 200, 400, 600, 800, 1000}
	}
	batchConcurrency := spec.BatchCreateConcurrency
	if batchConcurrency <= 0 {
		batchConcurrency = defaultDensityRampConcurrency
	}
	maxFailureRate := spec.MaxBatchFailureRate
	if maxFailureRate <= 0 {
		maxFailureRate = defaultDensityRampMaxFailureRate
	}
	pingSampleSize := spec.PingSampleSize
	if pingSampleSize <= 0 {
		pingSampleSize = defaultDensityRampPingSampleSize
	}
	sandboxTimeout := spec.SandboxTimeoutSeconds
	if sandboxTimeout <= 0 {
		sandboxTimeout = defaultDensityRampSandboxTimeout
	}
	pollInterval := s.pollInterval
	if spec.ReadinessPollMs > 0 {
		pollInterval = time.Duration(spec.ReadinessPollMs) * time.Millisecond
	}

	hooks := BenchHooksFromContext(ctx)
	client := newScaleClient(rc)
	rec := newScaleRecorder()

	var baselineSnap *StepTelemetrySnapshot
	if hooks.StepTelemetrySampler != nil {
		if snap, err := hooks.StepTelemetrySampler.Snapshot(ctx); err == nil && snap != nil {
			baselineSnap = snap
		}
	}

	var (
		activeMu sync.Mutex
		active   []densityActiveSandbox
	)
	// Always clean up every live sandbox created during the ramp before returning
	// (and before post-run LeakAudit executes).
	defer func() {
		activeMu.Lock()
		toDelete := append([]densityActiveSandbox(nil), active...)
		activeMu.Unlock()
		s.deleteAllBestEffort(client, toDelete, batchConcurrency)
		if hooks.ActiveSandboxReporter != nil {
			hooks.ActiveSandboxReporter(0)
		}
	}()

	stepResults := make([]DensityRampStepResult, 0, len(steps))
	var (
		peakActive        int
		earlyStopped      bool
		stopReason        string
		pingSampledTotal  int
		pingFailuresTotal int
	)

	for _, stepTarget := range steps {
		if err := ctx.Err(); err != nil {
			earlyStopped = true
			stopReason = fmt.Sprintf("context ended before step %d: %v", stepTarget, err)
			break
		}

		stepStart := time.Now()
		activeMu.Lock()
		currentLive := len(active)
		activeMu.Unlock()

		delta := stepTarget - currentLive
		if delta < 0 {
			delta = 0
		}

		var batchSucceeded, batchFailed atomic.Int64
		if delta > 0 {
			sem := make(chan struct{}, batchConcurrency)
			var wg sync.WaitGroup
			opName := fmt.Sprintf("density_ramp_create_%d", stepTarget)

			for i := 0; i < delta; i++ {
				if ctx.Err() != nil {
					batchFailed.Add(1)
					rec.recordOp(OperationSample{
						Operation: opName,
						Status:    "error",
						ErrorCode: scaleErrorCode(ctx.Err()),
					})
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					if ctx.Err() != nil {
						batchFailed.Add(1)
						rec.recordOp(OperationSample{
							Operation: opName,
							Status:    "error",
							ErrorCode: scaleErrorCode(ctx.Err()),
						})
						return
					}

					req := scaleBuildBaseCreateRequest(rc.Profile, sandboxTimeout, nil)
					t0 := time.Now()
					// Use a bounded independent context for CreateSandbox so mid-POST cancellation
					// still returns the created sandbox ID for guaranteed deletion.
					createCtx, createCancel := context.WithTimeout(context.Background(), scaleDefaultReadyTimeout)
					info, err := client.lifecycle.CreateSandbox(createCtx, req)
					createCancel()
					ackMs := scaleElapsedMs(t0)
					if err != nil {
						batchFailed.Add(1)
						rec.recordOp(OperationSample{
							Operation:  opName,
							LatencyMs:  ackMs,
							Status:     "error",
							ErrorCode:  scaleErrorCode(err),
							StderrHead: scaleErrorStderrHead(err, 200),
						})
						return
					}

					readyCtx, cancel := context.WithTimeout(ctx, scaleDefaultReadyTimeout)
					epURL, epHeaders, resolveAt, err := client.waitSandboxRunningAndPingTimed(readyCtx, info.ID, pollInterval)
					cancel()
					pingMs := scaleElapsedMs(t0)
					if err != nil {
						client.deleteSandboxBestEffort(info.ID)
						batchFailed.Add(1)
						rec.recordOp(OperationSample{
							Operation:  opName,
							SandboxID:  info.ID,
							LatencyMs:  pingMs,
							Status:     "error",
							AckState:   string(info.Status.State),
							ErrorCode:  scaleErrorCode(err),
							StderrHead: scaleErrorStderrHead(err, 200),
						})
						return
					}

					rec.addLatency("t_api_ack", ackMs)
					if !resolveAt.IsZero() {
						rec.addLatency("t_endpoint_resolve", float64(resolveAt.Sub(t0).Microseconds())/1000.0)
					}
					rec.addLatency("t_first_ping", pingMs)
					rec.recordOp(OperationSample{
						Operation: opName,
						SandboxID: info.ID,
						LatencyMs: pingMs,
						Status:    "ok",
						AckState:  string(info.Status.State),
					})

					activeMu.Lock()
					active = append(active, densityActiveSandbox{
						id:        info.ID,
						epURL:     epURL,
						epHeaders: epHeaders,
					})
					activeMu.Unlock()
					batchSucceeded.Add(1)
				}()
			}
			wg.Wait()
		}

		activeMu.Lock()
		liveSnapshot := append([]densityActiveSandbox(nil), active...)
		activeMu.Unlock()
		if len(liveSnapshot) > peakActive {
			peakActive = len(liveSnapshot)
		}
		if hooks.ActiveSandboxReporter != nil {
			hooks.ActiveSandboxReporter(len(liveSnapshot))
		}

		// Sample /ping health across the live fleet at this step.
		pingHealthy, pingFailed, pingLatencies := s.sampleLivePings(ctx, client, rec, liveSnapshot, pingSampleSize, stepTarget)
		pingSampled := pingHealthy + pingFailed
		pingSampledTotal += pingSampled
		pingFailuresTotal += pingFailed

		var pingP99 float64
		if len(pingLatencies) > 0 {
			pingP99 = report.ComputeLatencyDist(pingLatencies).P99Ms
		}

		succ := int(batchSucceeded.Load())
		fail := int(batchFailed.Load())
		var failRate float64
		if delta > 0 {
			failRate = float64(fail) / float64(delta)
		}

		stepRes := DensityRampStepResult{
			TargetCount:      stepTarget,
			RequestedDelta:   delta,
			BatchSucceeded:   succ,
			BatchFailed:      fail,
			BatchFailureRate: failRate,
			LiveSandboxes:    len(liveSnapshot),
			PingSampled:      pingSampled,
			PingHealthy:      pingHealthy,
			PingFailed:       pingFailed,
			PingP99Ms:        pingP99,
			DurationSeconds:  time.Since(stepStart).Seconds(),
		}

		if hooks.StepTelemetrySampler != nil {
			if stepSnap, err := hooks.StepTelemetrySampler.Snapshot(ctx); err == nil && stepSnap != nil {
				stepRes.FCPssMiBPerSbx = stepSnap.FCPssAvgMiB
				stepRes.FCRssMiBPerSbx = stepSnap.FCRssAvgMiB
				stepRes.FastletRSSMiB = stepSnap.FastletRSSMiB
				stepRes.EgressRSSMiB = stepSnap.EgressRSSMiB
				stepRes.UblkdRSSMiB = stepSnap.UblkdRSSMiB
				stepRes.ProxyRSSMiB = stepSnap.ProxyRSSMiB
				stepRes.HostCPUUtilPct = stepSnap.CPUUtilPct
				stepRes.HostLoad1m = stepSnap.Load1m
				stepRes.MemAvailableMiB = stepSnap.MemAvailableMiB
				if baselineSnap != nil && len(liveSnapshot) > 0 {
					liveF := float64(len(liveSnapshot))
					stepRes.DeltaMemAvailableMiBPerSbx = math.Max(0, baselineSnap.MemAvailableMiB-stepSnap.MemAvailableMiB) / liveF
					stepRes.DeltaSlabMiBPerSbx = math.Max(0, stepSnap.SlabMiB-baselineSnap.SlabMiB) / liveF
					stepRes.DeltaSUnreclaimMiBPerSbx = math.Max(0, stepSnap.SUnreclaimMiB-baselineSnap.SUnreclaimMiB) / liveF
				}
			}
		}

		if delta > 0 && failRate > maxFailureRate {
			stepRes.StoppedEarly = true
			earlyStopped = true
			stopReason = fmt.Sprintf("step %d batch failure rate %.2f%% exceeded threshold %.2f%% (%d/%d failed)",
				stepTarget, failRate*100.0, maxFailureRate*100.0, fail, delta)
			stepResults = append(stepResults, stepRes)
			break
		}

		stepResults = append(stepResults, stepRes)
	}

	durationSec := time.Since(startedAt).Seconds()
	latencies := make(map[string]report.LatencyDist, len(rec.latencies))
	for k, vals := range rec.latencies {
		if len(vals) > 0 {
			latencies[k] = report.ComputeLatencyDist(vals)
		}
	}

	status := "passed"
	if rec.totalOps == 0 || earlyStopped || ctx.Err() != nil {
		status = "failed"
	} else if pingSampledTotal > 0 && float64(pingFailuresTotal)/float64(pingSampledTotal) > maxFailureRate {
		status = "failed"
	}

	var throughput float64
	if durationSec > 0 && rec.succeededOps > 0 {
		throughput = float64(rec.succeededOps) / durationSec
	}

	extra := map[string]any{
		"steps":                    stepResults,
		"peak_active_sandboxes":    peakActive,
		"early_stopped":            earlyStopped,
		"max_batch_failure_rate":   maxFailureRate,
		"batch_create_concurrency": batchConcurrency,
	}
	if stopReason != "" {
		extra["stop_reason"] = stopReason
	}
	if baselineSnap != nil {
		extra["baseline_telemetry"] = baselineSnap
	}

	res := &report.SuiteResult{
		Name:               s.Name(),
		Status:             status,
		StartedAt:          startedAt,
		DurationSeconds:    durationSec,
		ThroughputRPS:      throughput,
		TotalOps:           rec.totalOps,
		SucceededOps:       rec.succeededOps,
		FailedOps:          rec.failedOps,
		PendingFallbackOps: rec.pendingFallback,
		Errors:             rec.errors,
		Latencies:          latencies,
		Extra:              extra,
	}
	if len(rec.errorsByOp) > 0 {
		res.ErrorsByOperation = rec.errorsByOp
	}
	if len(rec.errorSamples) > 0 {
		res.ErrorSamples = rec.errorSamples
	}
	return res, rec.samples, nil
}

func (s *DensityRampSuite) sampleLivePings(
	ctx context.Context,
	client *scaleClient,
	rec *scaleRecorder,
	live []densityActiveSandbox,
	sampleSize int,
	stepTarget int,
) (healthy, failed int, latencies []float64) {
	if len(live) == 0 || sampleSize <= 0 {
		return 0, 0, nil
	}
	targets := pickEvenlySpaced(live, sampleSize)
	opName := fmt.Sprintf("density_ramp_ping_%d", stepTarget)

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 16)
	)
	for _, sbx := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(target densityActiveSandbox) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pingURL := strings.TrimRight(target.epURL, "/") + "/ping"
			statusCode, _, elapsedMs, err := client.probeURL(ctx, pingURL, target.epHeaders)
			if err == nil && statusCode == http.StatusOK {
				rec.addLatency("density_ping", elapsedMs)
				rec.recordOp(OperationSample{
					Operation: opName,
					SandboxID: target.id,
					LatencyMs: elapsedMs,
					Status:    "ok",
				})
				mu.Lock()
				healthy++
				latencies = append(latencies, elapsedMs)
				mu.Unlock()
			} else {
				code := scaleErrorCode(err)
				if err == nil {
					code = fmt.Sprintf("http_%d", statusCode)
				}
				rec.recordOp(OperationSample{
					Operation: opName,
					SandboxID: target.id,
					LatencyMs: elapsedMs,
					Status:    "error",
					ErrorCode: code,
				})
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}(sbx)
	}
	wg.Wait()
	return healthy, failed, latencies
}

func pickEvenlySpaced(live []densityActiveSandbox, sampleSize int) []densityActiveSandbox {
	if len(live) <= sampleSize {
		return live
	}
	out := make([]densityActiveSandbox, 0, sampleSize)
	step := float64(len(live)) / float64(sampleSize)
	for i := 0; i < sampleSize; i++ {
		idx := int(float64(i) * step)
		if idx >= len(live) {
			idx = len(live) - 1
		}
		out = append(out, live[idx])
	}
	return out
}

func (s *DensityRampSuite) deleteAllBestEffort(client *scaleClient, sandboxes []densityActiveSandbox, concurrency int) {
	if len(sandboxes) == 0 {
		return
	}
	if concurrency <= 0 {
		concurrency = defaultDensityRampConcurrency
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, sbx := range sandboxes {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			client.deleteSandboxBestEffort(id)
		}(sbx.id)
	}
	wg.Wait()
}
