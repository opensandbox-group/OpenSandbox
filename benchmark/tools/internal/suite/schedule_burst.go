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
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// ScheduleBurstLevelResult records per-concurrency-wave metrics for schedule-burst.
type ScheduleBurstLevelResult struct {
	Concurrency             int                           `json:"concurrency"`
	Attempts                int                           `json:"attempts"`
	Succeeded               int                           `json:"succeeded"`
	Failed                  int                           `json:"failed"`
	RunningAckCount         int                           `json:"running_ack_count"`
	PendingAckCount         int                           `json:"pending_ack_count"`
	PendingFallbackRate     float64                       `json:"pending_fallback_rate"`
	Rate429Count            int                           `json:"rate_429_count"`
	Rate429Ratio            float64                       `json:"rate_429_ratio"`
	DurationSeconds         float64                       `json:"duration_seconds"`
	SchedulingThroughputRPS float64                       `json:"scheduling_throughput_rps"`
	Latencies               map[string]report.LatencyDist `json:"latencies"`
	Errors                  map[string]int                `json:"errors,omitempty"`
}

type scheduleBurstSuite struct {
	cfg suiteConfig
}

// NewScheduleBurstSuite returns the Phase 1 schedule-burst benchmark suite.
func NewScheduleBurstSuite(opts ...SuiteOption) Suite {
	var cfg suiteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return &scheduleBurstSuite{cfg: cfg}
}

func (s *scheduleBurstSuite) Name() string { return "schedule-burst" }

func (s *scheduleBurstSuite) Description() string {
	return "Stepped concurrency waves of create -> ping -> first-exec -> delete"
}

func (s *scheduleBurstSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

func (s *scheduleBurstSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	log := loggerFromRunContext(rc)
	hooks := resolveHooks(ctx, s.cfg)
	client := NewBenchClient(rc.SDKConfig, WithClientHooks(hooks), WithClientLogger(log))

	spec := &profile.ScheduleBurstSuiteSpec{
		Concurrencies:    []int{1, 4, 8},
		AttemptsPerLevel: 10,
		ReadinessPollMs:  10,
	}
	if rc.Profile != nil && rc.Profile.Suites.ScheduleBurst != nil {
		p := rc.Profile.Suites.ScheduleBurst
		if len(p.Concurrencies) > 0 {
			spec.Concurrencies = p.Concurrencies
		}
		if p.AttemptsPerLevel > 0 {
			spec.AttemptsPerLevel = p.AttemptsPerLevel
		}
		if p.ReadinessPollMs > 0 {
			spec.ReadinessPollMs = p.ReadinessPollMs
		}
	}

	pollInterval := time.Duration(spec.ReadinessPollMs) * time.Millisecond
	templateID, err := client.ResolveTemplateID(ctx, rc.Profile)
	if err != nil {
		return nil, nil, fmt.Errorf("schedule-burst: resolve template: %w", err)
	}
	baseImage := ""
	if rc.Profile != nil {
		baseImage = rc.Profile.Stack.Template.BaseImage
	}
	limits := templateResourceLimits(rc.Profile)

	// Warmup cycles (excluded from statistics).
	for i := 0; i < rc.WarmupRuns; i++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		wRes, wErr := client.CreateSandbox(ctx, CreateOptions{
			TemplateID:            templateID,
			Image:                 baseImage,
			ResourceLimits:        limits,
			ReadinessPollInterval: pollInterval,
			Metadata: map[string]string{
				"bench-suite":  s.Name(),
				"bench-run-id": rc.RunID,
				"bench-warmup": "true",
			},
		})
		if wRes != nil && wRes.SandboxID != "" {
			client.BestEffortDelete(wRes.SandboxID)
		}
		if wErr != nil {
			log.Warn("schedule-burst warmup attempt failed", "attempt", i+1, "error", wErr)
		}
	}

	if hooks.MetricsSnapshotter != nil {
		if err := hooks.MetricsSnapshotter.BeforeSuite(ctx, s.Name()); err != nil {
			log.Warn("metrics snapshotter BeforeSuite failed", "suite", s.Name(), "error", err)
		}
	}

	suiteStart := time.Now().UTC()

	var (
		allSamples          []OperationSample
		allAPIAckMs         []float64
		allEndpointResolveMs []float64
		allFirstPingMs      []float64
		allFirstExecMs      []float64
		totalOps            int
		succeededOps        int
		failedOps           int
		runningAckTotal     int
		pendingAckTotal     int
		count429Total       int
		peakLevelRPS        float64
		allErrors           = make(map[string]int)
		levelResults        = make([]ScheduleBurstLevelResult, 0, len(spec.Concurrencies))
	)

	for _, conc := range spec.Concurrencies {
		if ctx.Err() != nil {
			return nil, allSamples, ctx.Err()
		}
		if conc <= 0 {
			continue
		}

		levelRes, levelSamples, acks, resolves, pings, execs := s.runWave(
			ctx, client, rc, templateID, baseImage, limits, pollInterval, conc, spec.AttemptsPerLevel,
		)

		levelResults = append(levelResults, levelRes)
		allSamples = append(allSamples, levelSamples...)
		allAPIAckMs = append(allAPIAckMs, acks...)
		allEndpointResolveMs = append(allEndpointResolveMs, resolves...)
		allFirstPingMs = append(allFirstPingMs, pings...)
		allFirstExecMs = append(allFirstExecMs, execs...)

		totalOps += levelRes.Attempts
		succeededOps += levelRes.Succeeded
		failedOps += levelRes.Failed
		runningAckTotal += levelRes.RunningAckCount
		pendingAckTotal += levelRes.PendingAckCount
		count429Total += levelRes.Rate429Count
		if levelRes.SchedulingThroughputRPS > peakLevelRPS {
			peakLevelRPS = levelRes.SchedulingThroughputRPS
		}
		for k, v := range levelRes.Errors {
			allErrors[k] += v
		}
	}

	durationSec := time.Since(suiteStart).Seconds()
	var overallRPS float64
	if durationSec > 0 {
		overallRPS = float64(succeededOps) / durationSec
	}

	ackTotal := runningAckTotal + pendingAckTotal
	var pendingFallbackRate float64
	if ackTotal > 0 {
		pendingFallbackRate = float64(pendingAckTotal) / float64(ackTotal)
	}
	var rate429 float64
	if totalOps > 0 {
		rate429 = float64(count429Total) / float64(totalOps)
	}

	status := "passed"
	if totalOps == 0 || failedOps > 0 {
		status = "failed"
	}

	res := &report.SuiteResult{
		Name:               s.Name(),
		Status:             status,
		StartedAt:          suiteStart,
		DurationSeconds:    durationSec,
		ThroughputRPS:      overallRPS,
		TotalOps:           totalOps,
		SucceededOps:       succeededOps,
		FailedOps:          failedOps,
		PendingFallbackOps: pendingAckTotal,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":          report.ComputeLatencyDist(allAPIAckMs),
			"t_endpoint_resolve": report.ComputeLatencyDist(allEndpointResolveMs),
			"t_first_ping":       report.ComputeLatencyDist(allFirstPingMs),
			"t_first_exec":       report.ComputeLatencyDist(allFirstExecMs),
		},
		Extra: map[string]any{
			"warmup_runs":                    rc.WarmupRuns,
			"readiness_poll_ms":              spec.ReadinessPollMs,
			"running_ack_count":              runningAckTotal,
			"pending_ack_count":              pendingAckTotal,
			"create_pending_fallback_rate":   pendingFallbackRate,
			"client_429_count":               count429Total,
			"client_429_retry_after_rate":    rate429,
			"scheduling_throughput_rps":      overallRPS,
			"peak_scheduling_throughput_rps": peakLevelRPS,
			"levels":                         levelResults,
		},
	}
	if len(allErrors) > 0 {
		res.Errors = allErrors
	}

	if hooks.MetricsSnapshotter != nil {
		ov, prom, err := hooks.MetricsSnapshotter.AfterSuite(ctx, s.Name())
		if err != nil {
			log.Warn("metrics snapshotter AfterSuite failed", "suite", s.Name(), "error", err)
		} else {
			res.OverlayBDMetrics = ov
			res.PromMetricDeltas = prom
		}
	}

	return res, allSamples, nil
}

func (s *scheduleBurstSuite) runWave(
	ctx context.Context,
	client *BenchClient,
	rc RunContext,
	templateID, baseImage string,
	limits opensandbox.ResourceLimits,
	pollInterval time.Duration,
	concurrency, attempts int,
) (ScheduleBurstLevelResult, []OperationSample, []float64, []float64, []float64, []float64) {
	waveStart := time.Now()
	jobs := make(chan int, attempts)
	for i := 0; i < attempts; i++ {
		jobs <- i
	}
	close(jobs)

	workers := concurrency
	if workers > attempts {
		workers = attempts
	}

	var (
		mu                sync.Mutex
		samples           = make([]OperationSample, 0, attempts)
		apiAckMs          = make([]float64, 0, attempts)
		endpointResolveMs = make([]float64, 0, attempts)
		firstPingMs       = make([]float64, 0, attempts)
		firstExecMs       = make([]float64, 0, attempts)
		errCounts         = make(map[string]int)
		succeeded         int
		failed            int
		runningAck        int
		pendingAck        int
		rate429Count      int
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				cRes, err := client.CreateSandbox(ctx, CreateOptions{
					TemplateID:            templateID,
					Image:                 baseImage,
					ResourceLimits:        limits,
					ReadinessPollInterval: pollInterval,
					Metadata: map[string]string{
						"bench-suite":       s.Name(),
						"bench-run-id":      rc.RunID,
						"bench-concurrency": fmt.Sprintf("%d", concurrency),
						"bench-attempt":     fmt.Sprintf("%d", idx),
					},
				})
				if cRes != nil && cRes.SandboxID != "" {
					client.BestEffortDelete(cRes.SandboxID)
				}

				mu.Lock()
				if err != nil {
					failed++
					errCode := "client_error"
					latMs := 0.0
					sbxID := ""
					ackState := ""
					if cRes != nil {
						if cRes.ErrorCode != "" {
							errCode = cRes.ErrorCode
						}
						if cRes.Is429 {
							rate429Count++
						}
						latMs = cRes.APIAckMs
						sbxID = cRes.SandboxID
						ackState = cRes.AckState
						if ackState == string(opensandbox.StateRunning) {
							runningAck++
						} else if ackState == string(opensandbox.StatePending) {
							pendingAck++
						}
					}
					errCounts[errCode]++
					samples = append(samples, newOperationSample(
						fmt.Sprintf("create_c%d", concurrency), sbxID, latMs, ackState, err,
					))
				} else {
					succeeded++
					if cRes.AckState == string(opensandbox.StateRunning) {
						runningAck++
					} else {
						pendingAck++
					}
					apiAckMs = append(apiAckMs, cRes.APIAckMs)
					if cRes.EndpointResolveMs > 0 {
						endpointResolveMs = append(endpointResolveMs, cRes.EndpointResolveMs)
					}
					firstPingMs = append(firstPingMs, cRes.FirstPingMs)
					firstExecMs = append(firstExecMs, cRes.FirstExecMs)
					samples = append(samples, newOperationSample(
						fmt.Sprintf("create_c%d", concurrency), cRes.SandboxID, cRes.FirstPingMs, cRes.AckState, nil,
					))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	durSec := time.Since(waveStart).Seconds()
	var rps float64
	if durSec > 0 {
		rps = float64(succeeded) / durSec
	}
	ackTotal := runningAck + pendingAck
	var pendingRate float64
	if ackTotal > 0 {
		pendingRate = float64(pendingAck) / float64(ackTotal)
	}
	var rate429Ratio float64
	if attempts > 0 {
		rate429Ratio = float64(rate429Count) / float64(attempts)
	}

	levelRes := ScheduleBurstLevelResult{
		Concurrency:             concurrency,
		Attempts:                succeeded + failed,
		Succeeded:               succeeded,
		Failed:                  failed,
		RunningAckCount:         runningAck,
		PendingAckCount:         pendingAck,
		PendingFallbackRate:     pendingRate,
		Rate429Count:            rate429Count,
		Rate429Ratio:            rate429Ratio,
		DurationSeconds:         durSec,
		SchedulingThroughputRPS: rps,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":          report.ComputeLatencyDist(apiAckMs),
			"t_endpoint_resolve": report.ComputeLatencyDist(endpointResolveMs),
			"t_first_ping":       report.ComputeLatencyDist(firstPingMs),
			"t_first_exec":       report.ComputeLatencyDist(firstExecMs),
		},
	}
	if len(errCounts) > 0 {
		levelRes.Errors = errCounts
	}
	return levelRes, samples, apiAckMs, endpointResolveMs, firstPingMs, firstExecMs
}
