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
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

type steadyChurnSuite struct {
	cfg suiteConfig
}

// NewSteadyChurnSuite returns the Phase 1 steady-churn benchmark suite.
func NewSteadyChurnSuite(opts ...SuiteOption) Suite {
	var cfg suiteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return &steadyChurnSuite{cfg: cfg}
}

func (s *steadyChurnSuite) Name() string { return "steady-churn" }

func (s *steadyChurnSuite) Description() string {
	return "Sustained target-RPS create -> short workload -> delete churn"
}

func (s *steadyChurnSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

func (s *steadyChurnSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	log := loggerFromRunContext(rc)
	hooks := resolveHooks(ctx, s.cfg)
	client := NewBenchClient(rc.SDKConfig, WithClientHooks(hooks), WithClientLogger(log))

	spec := &profile.SteadyChurnSuiteSpec{
		TargetRPS:       1.0,
		DurationSeconds: 10,
		ReadinessPollMs: 10,
	}
	if rc.Profile != nil && rc.Profile.Suites.SteadyChurn != nil {
		p := rc.Profile.Suites.SteadyChurn
		if p.TargetRPS > 0 {
			spec.TargetRPS = p.TargetRPS
		}
		if p.DurationSeconds > 0 {
			spec.DurationSeconds = p.DurationSeconds
		}
		if p.ReadinessPollMs > 0 {
			spec.ReadinessPollMs = p.ReadinessPollMs
		}
	}

	pollInterval := time.Duration(spec.ReadinessPollMs) * time.Millisecond
	templateID, err := client.ResolveTemplateID(ctx, rc.Profile)
	if err != nil {
		return nil, nil, fmt.Errorf("steady-churn: resolve template: %w", err)
	}
	baseImage := ""
	if rc.Profile != nil {
		baseImage = rc.Profile.Stack.Template.BaseImage
	}
	limits := templateResourceLimits(rc.Profile)

	// Warmup runs (excluded from stats).
	for i := 0; i < rc.WarmupRuns; i++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		wRes, wErr := client.CreateSandbox(ctx, CreateOptions{
			TemplateID:            templateID,
			Image:                 baseImage,
			ResourceLimits:        limits,
			ReadinessPollInterval: pollInterval,
			FirstExecCommand:      "echo churn-warmup-ok",
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
			log.Warn("steady-churn warmup attempt failed", "attempt", i+1, "error", wErr)
		}
	}

	if hooks.MetricsSnapshotter != nil {
		if err := hooks.MetricsSnapshotter.BeforeSuite(ctx, s.Name()); err != nil {
			log.Warn("metrics snapshotter BeforeSuite failed", "suite", s.Name(), "error", err)
		}
	}

	suiteStart := time.Now().UTC()
	targetDuration := time.Duration(spec.DurationSeconds) * time.Second
	deadline := suiteStart.Add(targetDuration)

	interval := time.Duration(float64(time.Second) / spec.TargetRPS)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	// Size worker pool to sustain TargetRPS even under moderate tail latency without unbounded goroutines.
	workerCount := int(math.Ceil(spec.TargetRPS * 10))
	if workerCount < 4 {
		workerCount = 4
	}
	if workerCount > 256 {
		workerCount = 256
	}

	jobs := make(chan int, workerCount)

	var (
		mu             sync.Mutex
		samples        []OperationSample
		apiAckMs       []float64
		firstPingMs    []float64
		firstExecMs    []float64
		deleteMs       []float64
		cycleTotalMs   []float64
		errCounts      = make(map[string]int)
		totalOps       int
		succeededOps   int
		failedOps      int
		pendingAckOps  int
		count429       int
	)

	var wg sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				opStart := time.Now()
				cRes, cErr := client.CreateSandbox(ctx, CreateOptions{
					TemplateID:            templateID,
					Image:                 baseImage,
					ResourceLimits:        limits,
					ReadinessPollInterval: pollInterval,
					FirstExecCommand:      "echo churn-ok && uname -s",
					Metadata: map[string]string{
						"bench-suite":  s.Name(),
						"bench-run-id": rc.RunID,
						"bench-seq":    fmt.Sprintf("%d", idx),
					},
				})

				var delErr error
				var delDurMs float64
				if cRes != nil && cRes.SandboxID != "" {
					delStart := time.Now()
					delCtx, delCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
					delErr = client.Delete(delCtx, cRes.SandboxID)
					delCancel()
					delDurMs = msSince(delStart)
				}
				totalDurMs := msSince(opStart)

				mu.Lock()
				totalOps++
				if cErr != nil {
					failedOps++
					errCode := "client_error"
					latMs := totalDurMs
					sbxID := ""
					ackState := ""
					if cRes != nil {
						if cRes.ErrorCode != "" {
							errCode = cRes.ErrorCode
						}
						if cRes.Is429 {
							count429++
						}
						if cRes.APIAckMs > 0 {
							latMs = cRes.APIAckMs
						}
						sbxID = cRes.SandboxID
						ackState = cRes.AckState
						if ackState == string(opensandbox.StatePending) {
							pendingAckOps++
						}
					}
					errCounts[errCode]++
					samples = append(samples, newOperationSample("churn_cycle", sbxID, latMs, ackState, cErr))
				} else if delErr != nil {
					failedOps++
					errCode, _, is429, _ := ClassifyError(delErr)
					if is429 {
						count429++
					}
					errCounts["delete_"+errCode]++
					if cRes.AckState == string(opensandbox.StatePending) {
						pendingAckOps++
					}
					apiAckMs = append(apiAckMs, cRes.APIAckMs)
					firstPingMs = append(firstPingMs, cRes.FirstPingMs)
					firstExecMs = append(firstExecMs, cRes.FirstExecMs)
					samples = append(samples, newOperationSample("churn_cycle", cRes.SandboxID, cRes.FirstPingMs, cRes.AckState, delErr))
				} else {
					succeededOps++
					if cRes.AckState == string(opensandbox.StatePending) {
						pendingAckOps++
					}
					apiAckMs = append(apiAckMs, cRes.APIAckMs)
					firstPingMs = append(firstPingMs, cRes.FirstPingMs)
					firstExecMs = append(firstExecMs, cRes.FirstExecMs)
					deleteMs = append(deleteMs, delDurMs)
					cycleTotalMs = append(cycleTotalMs, totalDurMs)
					samples = append(samples, newOperationSample("churn_cycle", cRes.SandboxID, cRes.FirstPingMs, cRes.AckState, nil))
				}
				mu.Unlock()
			}
		}()
	}

	// Rate-limited dispatch loop. Fire the first job immediately so short durations still run work.
	seq := 0
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	dispatch := func() bool {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		select {
		case jobs <- seq:
			seq++
			return true
		case <-ctx.Done():
			return false
		default:
			// All workers saturated; drop this tick rather than stalling the rate-limiter clock.
			return true
		}
	}

	if dispatch() {
		for {
			select {
			case <-ctx.Done():
				goto doneDispatch
			case <-ticker.C:
				if !dispatch() {
					goto doneDispatch
				}
			}
		}
	}
doneDispatch:
	close(jobs)
	wg.Wait()

	durationSec := time.Since(suiteStart).Seconds()
	var achievedRPS float64
	if durationSec > 0 {
		achievedRPS = float64(succeededOps) / durationSec
	}
	var rate429 float64
	if totalOps > 0 {
		rate429 = float64(count429) / float64(totalOps)
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
		ThroughputRPS:      achievedRPS,
		TotalOps:           totalOps,
		SucceededOps:       succeededOps,
		FailedOps:          failedOps,
		PendingFallbackOps: pendingAckOps,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":     report.ComputeLatencyDist(apiAckMs),
			"t_first_ping":  report.ComputeLatencyDist(firstPingMs),
			"t_first_exec":  report.ComputeLatencyDist(firstExecMs),
			"t_delete":      report.ComputeLatencyDist(deleteMs),
			"t_cycle_total": report.ComputeLatencyDist(cycleTotalMs),
		},
		Extra: map[string]any{
			"target_rps":                  spec.TargetRPS,
			"achieved_rps":                achievedRPS,
			"duration_seconds_target":     spec.DurationSeconds,
			"readiness_poll_ms":           spec.ReadinessPollMs,
			"worker_pool_size":            workerCount,
			"client_429_count":            count429,
			"client_429_retry_after_rate": rate429,
		},
	}
	if len(errCounts) > 0 {
		res.Errors = errCounts
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

	return res, samples, nil
}
