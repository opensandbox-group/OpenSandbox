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
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	defaultNoisyFleetSize        = 20
	defaultNoisyAggressorRatio   = 0.10
	defaultNoisyProbeRounds      = 20
	defaultNoisyStressDuration   = 30
	defaultNoisySandboxTimeout   = 600
	defaultNoisyStressWarmup     = 500 * time.Millisecond
	defaultNoisyCreateConcurrent = 16
)

// NoisyNeighborOption configures optional timings or hooks for NoisyNeighborSuite.
type NoisyNeighborOption func(*NoisyNeighborSuite)

// WithNoisyNeighborHooks attaches BenchHooks to NoisyNeighborSuite.
func WithNoisyNeighborHooks(hooks BenchHooks) NoisyNeighborOption {
	return func(s *NoisyNeighborSuite) {
		s.cfg.hooks = hooks
	}
}

// WithNoisyNeighborStressWarmup overrides the settle wait after launching aggressor stressors.
func WithNoisyNeighborStressWarmup(d time.Duration) NoisyNeighborOption {
	return func(s *NoisyNeighborSuite) {
		if d >= 0 {
			s.stressWarmup = d
		}
	}
}

// NoisyNeighborSuite quantifies multi-tenant isolation degradation (aligned with
// Firecracker NSDI '20 noisy-neighbor evaluation) by partitioning a live fleet
// into aggressors and victims and comparing victim_baseline_exec vs victim_noisy_exec.
type NoisyNeighborSuite struct {
	cfg          suiteConfig
	stressWarmup time.Duration
}

// NewNoisyNeighborSuite constructs the noisy-neighbor suite.
func NewNoisyNeighborSuite(opts ...NoisyNeighborOption) *NoisyNeighborSuite {
	s := &NoisyNeighborSuite{
		stressWarmup: defaultNoisyStressWarmup,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *NoisyNeighborSuite) Name() string { return "noisy-neighbor" }

func (s *NoisyNeighborSuite) Description() string {
	return "Multi-tenant noisy-neighbor isolation benchmark measuring victim P50/P95/P99 latency degradation under CPU/disk/network aggressor load"
}

func (s *NoisyNeighborSuite) SupportedRuntimes() []string { return nil }

type noisySandbox struct {
	id    string
	execd *opensandbox.ExecdClient
}

func (s *NoisyNeighborSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	startedAt := time.Now().UTC()
	hooks := resolveHooks(ctx, s.cfg)
	client := NewBenchClient(rc.SDKConfig, WithClientHooks(hooks), WithClientLogger(rc.Logger))

	spec := resolveNoisyNeighborSpec(rc)
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
		fleetMu sync.Mutex
		fleet   []noisySandbox
	)
	// Always destroy every sandbox created during the suite.
	defer func() {
		fleetMu.Lock()
		ids := make([]string, 0, len(fleet))
		for _, sbx := range fleet {
			ids = append(ids, sbx.id)
		}
		fleetMu.Unlock()
		deleteSandboxesBestEffort(client, ids, defaultNoisyCreateConcurrent)
	}()

	createOpts := CreateOptions{
		TemplateID:            templateID,
		Image:                 baseImage,
		TimeoutSeconds:        spec.SandboxTimeoutSeconds,
		ReadinessPollInterval: pollInterval,
		SkipFirstExec:         true,
	}

	// 1. Concurrently launch fleetSize sandboxes.
	sem := make(chan struct{}, defaultNoisyCreateConcurrent)
	var wg sync.WaitGroup
	for i := 0; i < spec.FleetSize; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			cr, cErr := client.CreateSandbox(ctx, createOpts)
			if cr != nil && cr.SandboxID != "" {
				fleetMu.Lock()
				fleet = append(fleet, noisySandbox{id: cr.SandboxID, execd: cr.Execd})
				fleetMu.Unlock()
			}
			if cErr != nil {
				code := scaleErrorCode(cErr)
				if cr != nil && cr.ErrorCode != "" {
					code = cr.ErrorCode
				}
				rec.recordOp(OperationSample{
					Operation: "noisy_neighbor_create",
					SandboxID: sandboxIDOrEmpty(cr),
					Status:    "error",
					ErrorCode: code,
				})
				return
			}
			rec.addLatency("t_api_ack", cr.APIAckMs)
			rec.addLatency("t_first_ping", cr.FirstPingMs)
			rec.recordOp(OperationSample{
				Operation: "noisy_neighbor_create",
				SandboxID: cr.SandboxID,
				LatencyMs: cr.FirstPingMs,
				Status:    "ok",
				AckState:  cr.AckState,
			})
		}()
	}
	wg.Wait()

	// Filter to sandboxes that reached ready execd.
	readyFleet := make([]noisySandbox, 0, len(fleet))
	for _, sbx := range fleet {
		if sbx.execd != nil {
			readyFleet = append(readyFleet, sbx)
		}
	}

	if len(readyFleet) < 2 {
		return buildNoisyNeighborResult(s.Name(), startedAt, spec, len(readyFleet), 0, 0, 0, 0, 0, 0, rec, hooks, ctx, "insufficient ready sandboxes (need at least 2)"), rec.samples, nil
	}

	// Partition into aggressors (at least 1, at most len-1) and victims.
	aggressorCount := int(math.Round(float64(len(readyFleet)) * spec.AggressorRatio))
	if aggressorCount < 1 {
		aggressorCount = 1
	}
	if aggressorCount >= len(readyFleet) {
		aggressorCount = len(readyFleet) - 1
	}
	aggressors := readyFleet[:aggressorCount]
	victims := readyFleet[aggressorCount:]

	// 2. Baseline phase: probe victims concurrently while aggressors are idle.
	baseTotal, baseFailed := runVictimProbePhase(ctx, client, rec, victims, spec.ProbeRounds, "victim_baseline_exec")

	// 3. Noisy phase: saturate aggressors in background, then run identical victim probes.
	targetURL := strings.TrimRight(rc.SDKConfig.GetBaseURL(), "/") + "/health"
	stressCmd := buildAggressorStressCommand(spec.StressModes, spec.StressDurationSeconds, targetURL)
	var stressWg sync.WaitGroup
	for _, agg := range aggressors {
		if ctx.Err() != nil {
			break
		}
		stressWg.Add(1)
		go func(target noisySandbox) {
			defer stressWg.Done()
			if _, err := client.ExecBackground(ctx, target.execd, stressCmd); err != nil {
				rec.recordOp(OperationSample{
					Operation: "noisy_aggressor_start",
					SandboxID: target.id,
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
				return
			}
			rec.recordOp(OperationSample{
				Operation: "noisy_aggressor_start",
				SandboxID: target.id,
				Status:    "ok",
			})
		}(agg)
	}
	stressWg.Wait()

	if s.stressWarmup > 0 && ctx.Err() == nil {
		_ = sleepWithContext(ctx, s.stressWarmup)
	}

	noisyTotal, noisyFailed := runVictimProbePhase(ctx, client, rec, victims, spec.ProbeRounds, "victim_noisy_exec")

	res := buildNoisyNeighborResult(
		s.Name(),
		startedAt,
		spec,
		len(readyFleet),
		len(aggressors),
		len(victims),
		baseTotal,
		baseFailed,
		noisyTotal,
		noisyFailed,
		rec,
		hooks,
		ctx,
		"",
	)
	return res, rec.samples, nil
}

func resolveNoisyNeighborSpec(rc RunContext) profile.NoisyNeighborSuiteSpec {
	spec := profile.NoisyNeighborSuiteSpec{
		FleetSize:             defaultNoisyFleetSize,
		AggressorRatio:        defaultNoisyAggressorRatio,
		ProbeRounds:           defaultNoisyProbeRounds,
		StressModes:           []string{"cpu", "disk", "network"},
		StressDurationSeconds: defaultNoisyStressDuration,
		ReadinessPollMs:       10,
		SandboxTimeoutSeconds: defaultNoisySandboxTimeout,
	}
	if rc.Profile != nil && rc.Profile.Suites.NoisyNeighbor != nil {
		in := rc.Profile.Suites.NoisyNeighbor
		if in.FleetSize > 0 {
			spec.FleetSize = in.FleetSize
		}
		if in.AggressorRatio > 0 && in.AggressorRatio < 1 {
			spec.AggressorRatio = in.AggressorRatio
		}
		if in.ProbeRounds > 0 {
			spec.ProbeRounds = in.ProbeRounds
		}
		if len(in.StressModes) > 0 {
			spec.StressModes = append([]string(nil), in.StressModes...)
		}
		if in.StressDurationSeconds > 0 {
			spec.StressDurationSeconds = in.StressDurationSeconds
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

func runVictimProbePhase(
	ctx context.Context,
	client *BenchClient,
	rec *scaleRecorder,
	victims []noisySandbox,
	rounds int,
	metricName string,
) (totalProbes, failedProbes int) {
	if len(victims) == 0 || rounds <= 0 {
		return 0, 0
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 32)
	)
	for round := 0; round < rounds; round++ {
		if ctx.Err() != nil {
			break
		}
		probeCmd := fmt.Sprintf("echo noisy-probe-%s-%d", metricName, round)
		for _, v := range victims {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(target noisySandbox, cmd string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				execRes, err := client.ExecCommand(ctx, target.execd, cmd)
				mu.Lock()
				totalProbes++
				mu.Unlock()
				if err != nil {
					mu.Lock()
					failedProbes++
					mu.Unlock()
					rec.recordOp(OperationSample{
						Operation: metricName,
						SandboxID: target.id,
						Status:    "error",
						ErrorCode: scaleErrorCode(err),
					})
					return
				}
				rec.addLatency(metricName, execRes.DurationMs)
				rec.recordOp(OperationSample{
					Operation: metricName,
					SandboxID: target.id,
					LatencyMs: execRes.DurationMs,
					Status:    "ok",
				})
			}(v, probeCmd)
		}
		wg.Wait()
	}
	return totalProbes, failedProbes
}

func buildAggressorStressCommand(modes []string, durationSec int, targetURL string) string {
	if durationSec <= 0 {
		durationSec = defaultNoisyStressDuration
	}
	var parts []string
	for _, m := range modes {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "cpu":
			parts = append(parts, fmt.Sprintf(`(end=$(($(date +%%s)+%d)); while [ "$(date +%%s)" -lt "$end" ]; do :; done) &`, durationSec))
		case "disk":
			parts = append(parts, fmt.Sprintf(`(end=$(($(date +%%s)+%d)); while [ "$(date +%%s)" -lt "$end" ]; do dd if=/dev/zero of=/tmp/noisy-stress.dat bs=1M count=8 conv=fdatasync 2>/dev/null || true; done; rm -f /tmp/noisy-stress.dat) &`, durationSec))
		case "network":
			parts = append(parts, fmt.Sprintf(`(end=$(($(date +%%s)+%d)); while [ "$(date +%%s)" -lt "$end" ]; do python3 -c "import urllib.request; urllib.request.urlopen(%q, timeout=2).read()" >/dev/null 2>&1 || true; done) &`, durationSec, targetURL))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf(`(end=$(($(date +%%s)+%d)); while [ "$(date +%%s)" -lt "$end" ]; do :; done) &`, durationSec))
	}
	return fmt.Sprintf("sh -c '%s wait'", strings.Join(parts, " "))
}

func buildNoisyNeighborResult(
	name string,
	startedAt time.Time,
	spec profile.NoisyNeighborSuiteSpec,
	liveCount, aggressorCount, victimCount int,
	baseTotal, baseFailed, noisyTotal, noisyFailed int,
	rec *scaleRecorder,
	hooks BenchHooks,
	ctx context.Context,
	failReason string,
) *report.SuiteResult {
	durationSec := time.Since(startedAt).Seconds()
	latencies := make(map[string]report.LatencyDist, len(rec.latencies))
	for k, vals := range rec.latencies {
		if len(vals) > 0 {
			latencies[k] = report.ComputeLatencyDist(vals)
		}
	}

	baseDist := latencies["victim_baseline_exec"]
	noisyDist := latencies["victim_noisy_exec"]

	var p50Ratio, p95Ratio, p99Ratio float64
	if baseDist.P50Ms > 0 && noisyDist.P50Ms > 0 {
		p50Ratio = noisyDist.P50Ms / baseDist.P50Ms
	}
	if baseDist.P95Ms > 0 && noisyDist.P95Ms > 0 {
		p95Ratio = noisyDist.P95Ms / baseDist.P95Ms
	}
	if baseDist.P99Ms > 0 && noisyDist.P99Ms > 0 {
		p99Ratio = noisyDist.P99Ms / baseDist.P99Ms
	}

	var baseFailRate, noisyFailRate float64
	if baseTotal > 0 {
		baseFailRate = float64(baseFailed) / float64(baseTotal)
	}
	if noisyTotal > 0 {
		noisyFailRate = float64(noisyFailed) / float64(noisyTotal)
	}

	status := "passed"
	if failReason != "" || rec.totalOps == 0 || ctx.Err() != nil || baseDist.Count == 0 || noisyDist.Count == 0 {
		status = "failed"
	}

	var throughput float64
	if durationSec > 0 && rec.succeededOps > 0 {
		throughput = float64(rec.succeededOps) / durationSec
	}

	extra := map[string]any{
		"fleet_size":              spec.FleetSize,
		"live_sandboxes":          liveCount,
		"aggressor_count":         aggressorCount,
		"victim_count":            victimCount,
		"aggressor_ratio":         spec.AggressorRatio,
		"probe_rounds":            spec.ProbeRounds,
		"stress_modes":            spec.StressModes,
		"stress_duration_seconds": spec.StressDurationSeconds,
		"baseline_p50_ms":         baseDist.P50Ms,
		"baseline_p95_ms":         baseDist.P95Ms,
		"baseline_p99_ms":         baseDist.P99Ms,
		"noisy_p50_ms":            noisyDist.P50Ms,
		"noisy_p95_ms":            noisyDist.P95Ms,
		"noisy_p99_ms":            noisyDist.P99Ms,
		"p50_degradation_ratio":   p50Ratio,
		"p95_degradation_ratio":   p95Ratio,
		"p99_degradation_ratio":   p99Ratio,
		"baseline_failure_rate":   baseFailRate,
		"noisy_failure_rate":      noisyFailRate,
		"failure_rate_delta":      noisyFailRate - baseFailRate,
	}
	if failReason != "" {
		extra["error"] = failReason
	}

	res := &report.SuiteResult{
		Name:               name,
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
	if hooks.MetricsSnapshotter != nil {
		if obd, prom, err := hooks.MetricsSnapshotter.AfterSuite(ctx, name); err == nil {
			res.OverlayBDMetrics = obd
			res.PromMetricDeltas = prom
		}
	}
	return res
}
