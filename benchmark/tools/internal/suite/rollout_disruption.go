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
	"sort"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	rolloutMarkerPath          = "/var/tmp/bench-rollout-marker"
	maxRolloutErrorSamples     = 20
	defaultRolloutCreateTimout = 30 * time.Second
)

// RolloutPhaseResult captures black-box availability, outage duration, latency, and
// survivor continuity metrics for a single rollout-disruption phase.
type RolloutPhaseResult struct {
	Phase              string             `json:"phase"`
	Kind               string             `json:"kind"` // baseline | disruption | recovery
	Scenario           string             `json:"scenario,omitempty"`
	Target             string             `json:"target,omitempty"`
	TargetNode         string             `json:"target_node,omitempty"`
	AffectedPods       []string           `json:"affected_pods,omitempty"`
	RolloutDurationS   float64            `json:"rollout_duration_s"`
	RolloutCompleted   bool               `json:"rollout_completed"`
	TriggerDetail      string             `json:"trigger_detail,omitempty"`
	TriggerError       string             `json:"trigger_error,omitempty"`
	TotalCreates       int                `json:"total_creates"`
	SucceededCreates   int                `json:"succeeded_creates"`
	FailedCreates      int                `json:"failed_creates"`
	CreateSuccessPct   float64            `json:"create_success_pct"`
	MaxOutageWindowMs  float64            `json:"max_outage_window_ms"`
	APIAck             report.LatencyDist `json:"t_api_ack"`
	FirstPing          report.LatencyDist `json:"t_first_ping"`
	SurvivorTotal      int                `json:"survivor_total"`
	SurvivorAliveAfter int                `json:"survivor_alive_after"`
	SurvivorPingTotal  int                `json:"survivor_ping_total"`
	SurvivorPingOK     int                `json:"survivor_ping_ok"`
	SurvivorPingOKPct  float64            `json:"survivor_ping_ok_pct"`
	Errors             map[string]int     `json:"errors,omitempty"`
}

type rolloutCreateAttempt struct {
	StartTime time.Time
	EndTime   time.Time
	OK        bool
}

type rolloutSurvivor struct {
	mu     sync.Mutex
	ID     string
	Marker string
	Execd  *opensandbox.ExecdClient
	Alive  bool
}

type rolloutDisruptionSuite struct {
	cfg suiteConfig
}

// NewRolloutDisruptionSuite returns the rollout-disruption benchmark suite measuring
// availability, outage windows, and survivor continuity across SandboxPool upgrades,
// same-node multi-Fastlet co-location, and firecracker-runtime DaemonSet rollouts.
func NewRolloutDisruptionSuite(opts ...SuiteOption) Suite {
	var cfg suiteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return &rolloutDisruptionSuite{cfg: cfg}
}

func (s *rolloutDisruptionSuite) Name() string { return "rollout-disruption" }

func (s *rolloutDisruptionSuite) Description() string {
	return "Measures create outage windows and survivor sandbox continuity during Fastlet pool rollouts, multi-Fastlet co-location, and runtime DaemonSet upgrades"
}

func (s *rolloutDisruptionSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

func (s *rolloutDisruptionSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	log := loggerFromRunContext(rc)
	hooks := resolveHooks(ctx, s.cfg)
	client := NewBenchClient(rc.SDKConfig, WithClientHooks(hooks), WithClientLogger(log))

	spec := defaultRolloutDisruptionSpec()
	if rc.Profile != nil && rc.Profile.Suites.RolloutDisruption != nil {
		mergeRolloutDisruptionSpec(spec, rc.Profile.Suites.RolloutDisruption)
	}

	suiteStart := time.Now().UTC()
	if hooks.RolloutTrigger == nil {
		return &report.SuiteResult{
			Name:            s.Name(),
			Status:          "failed",
			StartedAt:       suiteStart,
			DurationSeconds: time.Since(suiteStart).Seconds(),
			Errors:          map[string]int{"rollout_hook_unavailable": 1},
			ErrorsByOperation: map[string]map[string]int{
				"preflight": {"rollout_hook_unavailable": 1},
			},
			ErrorSamples: []report.ErrorSample{
				{
					Operation:  "preflight",
					Code:       "rollout_hook_unavailable",
					StderrHead: "cluster RolloutTrigger hook is required for rollout-disruption suite",
				},
			},
			Extra: map[string]any{
				"error":     "cluster RolloutTrigger hook unavailable",
				"scenarios": spec.Scenarios,
			},
		}, nil, nil
	}

	pollInterval := time.Duration(spec.ReadinessPollMs) * time.Millisecond
	templateID, err := client.ResolveTemplateID(ctx, rc.Profile)
	if err != nil {
		return nil, nil, fmt.Errorf("rollout-disruption: resolve template: %w", err)
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
			ReadyTimeout:          defaultRolloutCreateTimout,
			SkipFirstExec:         true,
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
			log.Warn("rollout-disruption warmup attempt failed", "attempt", i+1, "error", wErr)
		}
	}

	if hooks.MetricsSnapshotter != nil {
		if err := hooks.MetricsSnapshotter.BeforeSuite(ctx, s.Name()); err != nil {
			log.Warn("metrics snapshotter BeforeSuite failed", "suite", s.Name(), "error", err)
		}
	}

	var (
		allCreatedSurvivors   []string
		survivors             []*rolloutSurvivor
		survivorSeq           int
		survivorSetupFailures int
		samples               []OperationSample
		allAPIAck           []float64
		allFirstPing        []float64
		baseAPIAck          []float64
		baseFirstPing       []float64
		disruptAPIAck       []float64
		disruptFirstPing    []float64
		recoverAPIAck       []float64
		recoverFirstPing    []float64
		errCounts           = make(map[string]int)
		errByOp             = make(map[string]map[string]int)
		errSamples          []report.ErrorSample
		totalOps            int
		succeededOps        int
		failedOps           int
		pendingAckOps       int
	)

	defer func() {
		seen := make(map[string]struct{}, len(allCreatedSurvivors))
		for _, id := range allCreatedSurvivors {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			client.BestEffortDelete(id)
		}
	}()

	recordError := func(op, code, sbxID, detail string) {
		if code == "" {
			code = "client_error"
		}
		errCounts[code]++
		if errByOp[op] == nil {
			errByOp[op] = make(map[string]int)
		}
		errByOp[op][code]++
		if len(errSamples) < maxRolloutErrorSamples {
			errSamples = append(errSamples, report.ErrorSample{
				Operation:  op,
				SandboxID:  sbxID,
				Code:       code,
				StderrHead: truncateString(detail, 240),
			})
		}
	}

	// Ensure a full cohort of live survivor sandboxes before baseline and before each disruption scenario.
	replenishSurvivors := func(phaseLabel string) {
		var alive []*rolloutSurvivor
		for _, sv := range survivors {
			if sv.Alive {
				alive = append(alive, sv)
			} else if sv.ID != "" {
				client.BestEffortDelete(sv.ID)
			}
		}
		survivors = alive

		for len(survivors) < spec.SurvivorCount {
			if ctx.Err() != nil {
				return
			}
			survivorSeq++
			marker := fmt.Sprintf("rollout-survivor-%s-%d", rc.RunID, survivorSeq)
			cRes, cErr := client.CreateSandbox(ctx, CreateOptions{
				TemplateID:            templateID,
				Image:                 baseImage,
				ResourceLimits:        limits,
				ReadinessPollInterval: pollInterval,
				ReadyTimeout:          defaultReadyTimeout,
				FirstExecCommand:      fmt.Sprintf("printf '%%s' %s > %s && cat %s", shellSingleQuote(marker), rolloutMarkerPath, rolloutMarkerPath),
				Metadata: map[string]string{
					"bench-suite":  s.Name(),
					"bench-run-id": rc.RunID,
					"bench-role":   "rollout-survivor",
					"bench-phase":  phaseLabel,
				},
			})
			if cRes != nil && cRes.SandboxID != "" {
				allCreatedSurvivors = append(allCreatedSurvivors, cRes.SandboxID)
			}
			if cErr != nil || cRes == nil || cRes.Execd == nil {
				survivorSetupFailures++
				code := "survivor_create_error"
				sbxID := ""
				if cRes != nil {
					if cRes.ErrorCode != "" {
						code = cRes.ErrorCode
					}
					sbxID = cRes.SandboxID
				}
				detail := ""
				if cErr != nil {
					detail = cErr.Error()
				}
				recordError("survivor_setup_"+phaseLabel, code, sbxID, detail)
				log.Warn("failed to provision rollout survivor sandbox", "phase", phaseLabel, "error", cErr)
				break
			}
			survivors = append(survivors, &rolloutSurvivor{
				ID:     cRes.SandboxID,
				Marker: marker,
				Execd:  cRes.Execd,
				Alive:  true,
			})
		}
	}

	runPhase := func(
		phaseName, kind, scenario string,
		runWindow func(phaseCtx context.Context) (*RolloutTriggerResult, error),
	) RolloutPhaseResult {
		phaseStart := time.Now()
		phaseCtx, phaseCancel := context.WithCancel(ctx)
		defer phaseCancel()

		interval := time.Duration(float64(time.Second) / spec.TargetRPS)
		if interval < 10*time.Millisecond {
			interval = 10 * time.Millisecond
		}
		workerCount := int(math.Ceil(spec.TargetRPS * 6))
		if workerCount < 2 {
			workerCount = 2
		}
		if workerCount > 64 {
			workerCount = 64
		}

		jobs := make(chan int, workerCount)
		var (
			pMu        sync.Mutex
			attempts   []rolloutCreateAttempt
			pAPIAck    []float64
			pFirstPing []float64
			pSamples   []OperationSample
			pErrCounts = make(map[string]int)
			pTotal     int
			pSucceeded int
			pFailed    int
			pPending   int
			pPingTotal int
			pPingOK    int
		)

		var churnWG sync.WaitGroup
		for w := 0; w < workerCount; w++ {
			churnWG.Add(1)
			go func() {
				defer churnWG.Done()
				for seq := range jobs {
					if phaseCtx.Err() != nil {
						return
					}
					opStart := time.Now()
					createCtx, createCancel := context.WithTimeout(ctx, defaultRolloutCreateTimout)
					cRes, cErr := client.CreateSandbox(createCtx, CreateOptions{
						TemplateID:            templateID,
						Image:                 baseImage,
						ResourceLimits:        limits,
						ReadinessPollInterval: pollInterval,
						ReadyTimeout:          defaultRolloutCreateTimout,
						SkipFirstExec:         true,
						Metadata: map[string]string{
							"bench-suite":  s.Name(),
							"bench-run-id": rc.RunID,
							"bench-phase":  phaseName,
							"bench-seq":    fmt.Sprintf("%d", seq),
						},
					})
					createCancel()
					opEnd := time.Now()

					if cRes != nil && cRes.SandboxID != "" {
						client.BestEffortDelete(cRes.SandboxID)
					}

					pMu.Lock()
					pTotal++
					if cErr != nil {
						pFailed++
						attempts = append(attempts, rolloutCreateAttempt{StartTime: opStart, EndTime: opEnd, OK: false})
						errCode := "client_error"
						sbxID := ""
						ackState := ""
						latMs := float64(opEnd.Sub(opStart).Microseconds()) / 1000.0
						if cRes != nil {
							if cRes.ErrorCode != "" {
								errCode = cRes.ErrorCode
							}
							if cRes.APIAckMs > 0 {
								latMs = cRes.APIAckMs
							}
							sbxID = cRes.SandboxID
							ackState = cRes.AckState
							if ackState == string(opensandbox.StatePending) {
								pPending++
							}
						}
						pErrCounts[errCode]++
						recordError("create_"+phaseName, errCode, sbxID, cErr.Error())
						pSamples = append(pSamples, newOperationSample("rollout_create_"+phaseName, sbxID, latMs, ackState, cErr))
					} else {
						pSucceeded++
						attempts = append(attempts, rolloutCreateAttempt{StartTime: opStart, EndTime: opEnd, OK: true})
						if cRes.AckState == string(opensandbox.StatePending) {
							pPending++
						}
						pAPIAck = append(pAPIAck, cRes.APIAckMs)
						pFirstPing = append(pFirstPing, cRes.FirstPingMs)
						pSamples = append(pSamples, newOperationSample("rollout_create_"+phaseName, cRes.SandboxID, cRes.FirstPingMs, cRes.AckState, nil))
					}
					pMu.Unlock()
				}
			}()
		}

		// Background rate-limited churn dispatcher.
		var dispatchWG sync.WaitGroup
		dispatchWG.Add(1)
		go func() {
			defer dispatchWG.Done()
			defer close(jobs)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			seq := 0
			sendJob := func() bool {
				if phaseCtx.Err() != nil {
					return false
				}
				select {
				case jobs <- seq:
					seq++
					return true
				case <-phaseCtx.Done():
					return false
				default:
					return true
				}
			}

			if !sendJob() {
				return
			}
			for {
				select {
				case <-phaseCtx.Done():
					return
				case <-ticker.C:
					if !sendJob() {
						return
					}
				}
			}
		}()

		// Background survivor /ping probe loop.
		var probeWG sync.WaitGroup
		activeCohort := append([]*rolloutSurvivor(nil), survivors...)
		if len(activeCohort) > 0 {
			probeInterval := time.Duration(spec.SurvivorProbeIntervalMs) * time.Millisecond
			if probeInterval < 50*time.Millisecond {
				probeInterval = 50 * time.Millisecond
			}
			probeWG.Add(1)
			go func() {
				defer probeWG.Done()
				ticker := time.NewTicker(probeInterval)
				defer ticker.Stop()

				probeOnce := func() {
					for _, sv := range activeCohort {
						if phaseCtx.Err() != nil {
							return
						}
						pCtx, pCancel := context.WithTimeout(phaseCtx, 3*time.Second)
						sv.mu.Lock()
						execd := sv.Execd
						sv.mu.Unlock()
						err := error(nil)
						if execd != nil {
							err = execd.Ping(pCtx)
						} else {
							err = fmt.Errorf("execd client nil")
						}
						if err != nil && phaseCtx.Err() == nil {
							// Re-resolve endpoint once in case ingress/fastlet route changed during rollout.
							if fresh, rErr := client.ResolveExecd(pCtx, sv.ID); rErr == nil && fresh != nil {
								if retryErr := fresh.Ping(pCtx); retryErr == nil {
									sv.mu.Lock()
									sv.Execd = fresh
									sv.mu.Unlock()
									err = nil
								}
							}
						}
						pCancel()

						pMu.Lock()
						pPingTotal++
						if err == nil {
							pPingOK++
						}
						pMu.Unlock()
					}
				}

				probeOnce()
				for {
					select {
					case <-phaseCtx.Done():
						return
					case <-ticker.C:
						probeOnce()
					}
				}
			}()
		}

		// Execute the phase window (sleep for baseline/recovery, or TriggerRollout for disruption).
		trigRes, trigErr := runWindow(phaseCtx)
		// Cancel phaseCtx before recording phaseEnd so buffered churn jobs stop immediately.
		phaseCancel()
		phaseEnd := time.Now()

		// Drain dispatcher, survivor probe loop, and any already-in-flight churn workers cleanly.
		dispatchWG.Wait()
		probeWG.Wait()
		churnWG.Wait()

		// Post-phase survivor liveness & filesystem marker integrity verification.
		aliveAfter := 0
		for _, sv := range activeCohort {
			if ctx.Err() != nil {
				break
			}
			vCtx, vCancel := context.WithTimeout(ctx, 10*time.Second)
			sv.mu.Lock()
			execd := sv.Execd
			sv.mu.Unlock()

			execRes, execErr := client.ExecCommandWithRetry(vCtx, execd, "cat "+rolloutMarkerPath, 2, 150*time.Millisecond)
			if execErr != nil {
				if fresh, rErr := client.ResolveExecd(vCtx, sv.ID); rErr == nil && fresh != nil {
					execRes, execErr = client.ExecCommandWithRetry(vCtx, fresh, "cat "+rolloutMarkerPath, 2, 150*time.Millisecond)
					if execErr == nil {
						sv.mu.Lock()
						sv.Execd = fresh
						sv.mu.Unlock()
					}
				}
			}
			vCancel()

			if execErr == nil && execRes != nil && strings.TrimSpace(execRes.Stdout) == sv.Marker {
				aliveAfter++
				sv.Alive = true
			} else {
				sv.Alive = false
				detail := "marker mismatch"
				code := "survivor_marker_mismatch"
				if execErr != nil {
					code, _, _, _ = ClassifyError(execErr)
					if code == "" {
						code = "survivor_lost"
					}
					detail = execErr.Error()
				}
				recordError("survivor_verify_"+phaseName, code, sv.ID, detail)
			}
		}

		pMu.Lock()
		totalOps += pTotal
		succeededOps += pSucceeded
		failedOps += pFailed
		pendingAckOps += pPending
		samples = append(samples, pSamples...)
		allAPIAck = append(allAPIAck, pAPIAck...)
		allFirstPing = append(allFirstPing, pFirstPing...)
		switch kind {
		case "baseline":
			baseAPIAck = append(baseAPIAck, pAPIAck...)
			baseFirstPing = append(baseFirstPing, pFirstPing...)
		case "disruption":
			disruptAPIAck = append(disruptAPIAck, pAPIAck...)
			disruptFirstPing = append(disruptFirstPing, pFirstPing...)
		case "recovery":
			recoverAPIAck = append(recoverAPIAck, pAPIAck...)
			recoverFirstPing = append(recoverFirstPing, pFirstPing...)
		}

		createSuccPct := 0.0
		if pTotal > 0 {
			createSuccPct = float64(pSucceeded) * 100.0 / float64(pTotal)
		}
		pingOKPct := 100.0
		if pPingTotal > 0 {
			pingOKPct = float64(pPingOK) * 100.0 / float64(pPingTotal)
		} else if len(activeCohort) == 0 {
			pingOKPct = 0.0
		}

		pr := RolloutPhaseResult{
			Phase:              phaseName,
			Kind:               kind,
			Scenario:           scenario,
			RolloutDurationS:   phaseEnd.Sub(phaseStart).Seconds(),
			RolloutCompleted:   trigErr == nil,
			TotalCreates:       pTotal,
			SucceededCreates:   pSucceeded,
			FailedCreates:      pFailed,
			CreateSuccessPct:   createSuccPct,
			MaxOutageWindowMs:  computeMaxOutageWindowMs(attempts, phaseEnd),
			APIAck:             report.ComputeLatencyDist(pAPIAck),
			FirstPing:          report.ComputeLatencyDist(pFirstPing),
			SurvivorTotal:      len(activeCohort),
			SurvivorAliveAfter: aliveAfter,
			SurvivorPingTotal:  pPingTotal,
			SurvivorPingOK:     pPingOK,
			SurvivorPingOKPct:  pingOKPct,
		}
		if len(pErrCounts) > 0 {
			pr.Errors = pErrCounts
		}
		pMu.Unlock()

		if trigRes != nil {
			pr.Target = trigRes.Target
			pr.TargetNode = trigRes.TargetNode
			pr.AffectedPods = trigRes.AffectedPods
			if trigRes.DurationSeconds > 0 {
				pr.RolloutDurationS = trigRes.DurationSeconds
			}
			pr.RolloutCompleted = trigRes.RolloutCompleted && trigErr == nil
			pr.TriggerDetail = trigRes.Detail
		}
		if trigErr != nil {
			pr.RolloutCompleted = false
			pr.TriggerError = trigErr.Error()
			code, _, _, _ := ClassifyError(trigErr)
			if code == "" {
				code = "rollout_trigger_error"
			}
			recordError("trigger_"+phaseName, code, pr.Target, trigErr.Error())
		}

		return pr
	}

	var phases []RolloutPhaseResult

	// 1. Baseline phase (steady create/delete churn + survivor probing without disruption).
	replenishSurvivors("baseline")
	if spec.BaselineSeconds > 0 && ctx.Err() == nil {
		baseDur := time.Duration(spec.BaselineSeconds) * time.Second
		phases = append(phases, runPhase("baseline", "baseline", "", func(pCtx context.Context) (*RolloutTriggerResult, error) {
			if err := sleepWithContext(pCtx, baseDur); err != nil {
				return nil, err
			}
			return &RolloutTriggerResult{
				DurationSeconds:  baseDur.Seconds(),
				RolloutCompleted: true,
				Detail:           fmt.Sprintf("steady baseline window (%s)", baseDur),
			}, nil
		}))
	}

	// 2. Disruption scenarios.
	for _, scenario := range spec.Scenarios {
		if ctx.Err() != nil {
			break
		}
		// Replenish any survivors lost during an earlier scenario so every scenario is evaluated independently.
		replenishSurvivors(scenario)

		sc := scenario
		trigOpts := RolloutTriggerOptions{
			PoolName:       spec.PoolName,
			DaemonSetName:  spec.DaemonSetName,
			Timeout:        time.Duration(spec.RolloutTimeoutSeconds) * time.Second,
			ColocationHold: time.Duration(spec.ColocationHoldSeconds) * time.Second,
		}
		phases = append(phases, runPhase(sc, "disruption", sc, func(pCtx context.Context) (*RolloutTriggerResult, error) {
			return hooks.RolloutTrigger.TriggerRollout(pCtx, sc, trigOpts)
		}))
	}

	// 3. Post-rollout steady-state recovery phase.
	if spec.PostRolloutSeconds > 0 && ctx.Err() == nil {
		replenishSurvivors("post_rollout")
		postDur := time.Duration(spec.PostRolloutSeconds) * time.Second
		phases = append(phases, runPhase("post_rollout", "recovery", "", func(pCtx context.Context) (*RolloutTriggerResult, error) {
			if err := sleepWithContext(pCtx, postDur); err != nil {
				return nil, err
			}
			return &RolloutTriggerResult{
				DurationSeconds:  postDur.Seconds(),
				RolloutCompleted: true,
				Detail:           fmt.Sprintf("post-rollout recovery window (%s)", postDur),
			}, nil
		}))
	}

	durationSec := time.Since(suiteStart).Seconds()
	achievedRPS := 0.0
	if durationSec > 0 {
		achievedRPS = float64(succeededOps) / durationSec
	}

	worstOutageMs := 0.0
	minCreateSuccessPct := 100.0
	hasDisruptionPhase := false
	totalSurvivorDrops := 0
	allTriggersOK := true
	postRolloutSuccessPct := 0.0

	for _, ph := range phases {
		if !ph.RolloutCompleted || ph.TriggerError != "" {
			allTriggersOK = false
		}
		if ph.Kind == "disruption" {
			hasDisruptionPhase = true
			if ph.MaxOutageWindowMs > worstOutageMs {
				worstOutageMs = ph.MaxOutageWindowMs
			}
			if ph.TotalCreates > 0 && ph.CreateSuccessPct < minCreateSuccessPct {
				minCreateSuccessPct = ph.CreateSuccessPct
			}
			if ph.SurvivorTotal > ph.SurvivorAliveAfter {
				totalSurvivorDrops += ph.SurvivorTotal - ph.SurvivorAliveAfter
			}
		}
		if ph.Kind == "recovery" {
			postRolloutSuccessPct = ph.CreateSuccessPct
		}
	}
	if !hasDisruptionPhase {
		minCreateSuccessPct = 0.0
	}

	status := "passed"
	if totalOps == 0 || failedOps > 0 || totalSurvivorDrops > 0 || survivorSetupFailures > 0 || !allTriggersOK || ctx.Err() != nil {
		status = "failed"
	}

	baseAckDist := report.ComputeLatencyDist(baseAPIAck)
	basePingDist := report.ComputeLatencyDist(baseFirstPing)

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
			"t_api_ack":               report.ComputeLatencyDist(allAPIAck),
			"t_first_ping":            report.ComputeLatencyDist(allFirstPing),
			"baseline_t_api_ack":      baseAckDist,
			"baseline_t_first_ping":   basePingDist,
			"disruption_t_api_ack":    report.ComputeLatencyDist(disruptAPIAck),
			"disruption_t_first_ping": report.ComputeLatencyDist(disruptFirstPing),
			"recovery_t_api_ack":      report.ComputeLatencyDist(recoverAPIAck),
			"recovery_t_first_ping":   report.ComputeLatencyDist(recoverFirstPing),
		},
		Extra: map[string]any{
			"scenarios":                       spec.Scenarios,
			"target_rps":                      spec.TargetRPS,
			"survivor_count":                  spec.SurvivorCount,
			"pool_name":                       spec.PoolName,
			"daemonset_name":                  spec.DaemonSetName,
			"phases":                          phases,
			"worst_max_outage_window_ms":      worstOutageMs,
			"min_create_success_pct":          minCreateSuccessPct,
			"total_survivor_drops":            totalSurvivorDrops,
			"survivor_setup_failures":         survivorSetupFailures,
			"baseline_api_ack_p99_ms":         baseAckDist.P99Ms,
			"baseline_first_ping_p99_ms":      basePingDist.P99Ms,
			"post_rollout_create_success_pct": postRolloutSuccessPct,
		},
	}
	if len(errCounts) > 0 {
		res.Errors = errCounts
	}
	if len(errByOp) > 0 {
		res.ErrorsByOperation = errByOp
	}
	if len(errSamples) > 0 {
		res.ErrorSamples = errSamples
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

// computeMaxOutageWindowMs calculates the longest contiguous wall-clock failure streak
// (in milliseconds) across create attempts. Recovery boundaries are derived from the
// earliest completed successful outcome at or after the outage began so slow, still-in-flight
// failures cannot inflate the outage past a proven recovery.
func computeMaxOutageWindowMs(attempts []rolloutCreateAttempt, phaseEnd time.Time) float64 {
	if len(attempts) == 0 {
		return 0
	}
	ordered := make([]rolloutCreateAttempt, len(attempts))
	copy(ordered, attempts)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].StartTime.Equal(ordered[j].StartTime) {
			return ordered[i].StartTime.Before(ordered[j].StartTime)
		}
		if !ordered[i].EndTime.Equal(ordered[j].EndTime) {
			return ordered[i].EndTime.Before(ordered[j].EndTime)
		}
		return !ordered[i].OK && ordered[j].OK
	})

	// minOKEnd[i] stores the earliest completion time among all successful attempts at index >= i.
	minOKEnd := make([]time.Time, len(ordered))
	var earliest time.Time
	for i := len(ordered) - 1; i >= 0; i-- {
		if ordered[i].OK {
			if earliest.IsZero() || ordered[i].EndTime.Before(earliest) {
				earliest = ordered[i].EndTime
			}
		}
		minOKEnd[i] = earliest
	}

	var (
		inOutage    bool
		streakStart time.Time
		lastFailEnd time.Time
		maxOutageMs float64
	)

	recordStreak := func(endT time.Time) {
		if !inOutage {
			return
		}
		if endT.Before(streakStart) {
			endT = streakStart
		}
		durMs := float64(endT.Sub(streakStart).Microseconds()) / 1000.0
		if durMs > maxOutageMs {
			maxOutageMs = durMs
		}
		inOutage = false
	}

	for i, a := range ordered {
		if !a.OK {
			if !inOutage {
				inOutage = true
				streakStart = a.StartTime
				lastFailEnd = a.EndTime
			} else if a.EndTime.After(lastFailEnd) {
				lastFailEnd = a.EndTime
			}
		} else if inOutage {
			recoveryEnd := minOKEnd[i]
			if recoveryEnd.IsZero() {
				recoveryEnd = a.EndTime
			}
			recordStreak(recoveryEnd)
		}
	}
	if inOutage {
		endT := phaseEnd
		if endT.IsZero() || endT.Before(streakStart) {
			endT = lastFailEnd
		}
		recordStreak(endT)
	}
	return maxOutageMs
}

// shellSingleQuote wraps s in POSIX single quotes so characters like $(), backticks,
// and variable expansions are treated literally by sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func defaultRolloutDisruptionSpec() *profile.RolloutDisruptionSuiteSpec {
	return &profile.RolloutDisruptionSuiteSpec{
		Scenarios:               []string{"pool_rollout", "multi_fastlet_colocation", "daemonset_rollout"},
		TargetRPS:               2.0,
		BaselineSeconds:         10,
		PostRolloutSeconds:      10,
		RolloutTimeoutSeconds:   180,
		ColocationHoldSeconds:   10,
		SurvivorCount:           4,
		SurvivorProbeIntervalMs: 500,
		PoolName:                "opensandbox-bench-pool",
		DaemonSetName:           "firecracker-runtime",
		ReadinessPollMs:         10,
	}
}

func mergeRolloutDisruptionSpec(dst, src *profile.RolloutDisruptionSuiteSpec) {
	if len(src.Scenarios) > 0 {
		dst.Scenarios = append([]string(nil), src.Scenarios...)
	}
	if src.TargetRPS > 0 {
		dst.TargetRPS = src.TargetRPS
	}
	if src.BaselineSeconds > 0 {
		dst.BaselineSeconds = src.BaselineSeconds
	}
	if src.PostRolloutSeconds > 0 {
		dst.PostRolloutSeconds = src.PostRolloutSeconds
	}
	if src.RolloutTimeoutSeconds > 0 {
		dst.RolloutTimeoutSeconds = src.RolloutTimeoutSeconds
	}
	if src.ColocationHoldSeconds > 0 {
		dst.ColocationHoldSeconds = src.ColocationHoldSeconds
	}
	if src.SurvivorCount > 0 {
		dst.SurvivorCount = src.SurvivorCount
	}
	if src.SurvivorProbeIntervalMs > 0 {
		dst.SurvivorProbeIntervalMs = src.SurvivorProbeIntervalMs
	}
	if strings.TrimSpace(src.PoolName) != "" {
		dst.PoolName = strings.TrimSpace(src.PoolName)
	}
	if strings.TrimSpace(src.DaemonSetName) != "" {
		dst.DaemonSetName = strings.TrimSpace(src.DaemonSetName)
	}
	if src.ReadinessPollMs > 0 {
		dst.ReadinessPollMs = src.ReadinessPollMs
	}
}

func truncateString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen || maxLen <= 0 {
		return s
	}
	return s[:maxLen] + "..."
}
