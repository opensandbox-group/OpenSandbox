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
	"sort"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const defaultMaxRootfsLayers = 24

// PauseResumeCaseResult records the outcome of one (mode, dirtyUpperMiB) matrix cell.
type PauseResumeCaseResult struct {
	Mode               string  `json:"mode"`
	DirtyUpperMiB      int     `json:"dirty_upper_mib"`
	SandboxID          string  `json:"sandbox_id,omitempty"`
	BeforeNode         string  `json:"before_node,omitempty"`
	AfterNode          string  `json:"after_node,omitempty"`
	CrossNodeVerified  bool    `json:"cross_node_verified,omitempty"`
	CacheEvicted       bool    `json:"cache_evicted,omitempty"`
	PauseAPIAckMs      float64 `json:"pause_api_ack_ms"`
	PauseTotalMs       float64 `json:"pause_total_ms"`
	ResumeAPIAckMs     float64 `json:"resume_api_ack_ms"`
	ResumeFirstPingMs  float64 `json:"resume_first_ping_ms"`
	ResumeFirstExecMs  float64 `json:"resume_first_exec_ms"`
	DiskMarkerVerified bool    `json:"disk_marker_verified"`
	MemProcessVerified bool    `json:"mem_process_verified"`
	Status             string  `json:"status"` // ok | error
	Error              string  `json:"error,omitempty"`
}

// ChainSquashRoundResult captures per-round latencies across multi-round pause/resume chains
// to expose the defaultMaxRootfsLayers=24 OverlayBD squash cliff (OSEP-0034 §3).
type ChainSquashRoundResult struct {
	Round             int     `json:"round"`
	PauseAPIAckMs     float64 `json:"pause_api_ack_ms"`
	PauseTotalMs      float64 `json:"pause_total_ms"`
	ResumeAPIAckMs    float64 `json:"resume_api_ack_ms"`
	ResumeFirstPingMs float64 `json:"resume_first_ping_ms"`
	ResumeFirstExecMs float64 `json:"resume_first_exec_ms"`
	StateVerified     bool    `json:"state_verified"`
	Status            string  `json:"status"` // ok | error
	Error             string  `json:"error,omitempty"`
}

// ChainSquashSummary summarizes multi-round layer-chain pause/resume behavior.
type ChainSquashSummary struct {
	RoundsConfigured       int                      `json:"rounds_configured"`
	RoundsCompleted        int                      `json:"rounds_completed"`
	DefaultMaxRootfsLayers int                      `json:"default_max_rootfs_layers"`
	MedianPauseMs          float64                  `json:"median_pause_ms"`
	MaxPauseMs             float64                  `json:"max_pause_ms"`
	MaxPauseRound          int                      `json:"max_pause_round"`
	SquashCliffRatio       float64                  `json:"squash_cliff_ratio,omitempty"`
	Rounds                 []ChainSquashRoundResult `json:"rounds"`
}

type pauseResumeSuite struct {
	cfg suiteConfig
}

// NewPauseResumeSuite returns the Phase 1 pause-resume benchmark suite.
func NewPauseResumeSuite(opts ...SuiteOption) Suite {
	var cfg suiteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return &pauseResumeSuite{cfg: cfg}
}

func (s *pauseResumeSuite) Name() string { return "pause-resume" }

func (s *pauseResumeSuite) Description() string {
	return "Full checkpoint pause/resume matrix (warm, cache-evicted, cross-node, dirty-upper, 24-layer squash)"
}

func (s *pauseResumeSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

func (s *pauseResumeSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	log := loggerFromRunContext(rc)
	hooks := resolveHooks(ctx, s.cfg)
	client := NewBenchClient(rc.SDKConfig, WithClientHooks(hooks), WithClientLogger(log))

	spec := &profile.PauseResumeSuiteSpec{
		Modes:                        []string{"warm"},
		DirtyUpperMiB:                []int{0},
		ChainSquashRounds:            0,
		ChainSquashTemplateMemoryMiB: 512,
	}
	if rc.Profile != nil && rc.Profile.Suites.PauseResume != nil {
		p := rc.Profile.Suites.PauseResume
		if len(p.Modes) > 0 {
			spec.Modes = p.Modes
		}
		if len(p.DirtyUpperMiB) > 0 {
			spec.DirtyUpperMiB = p.DirtyUpperMiB
		}
		if p.ChainSquashRounds >= 0 {
			spec.ChainSquashRounds = p.ChainSquashRounds
		}
		if p.ChainSquashTemplateMemoryMiB > 0 {
			spec.ChainSquashTemplateMemoryMiB = p.ChainSquashTemplateMemoryMiB
		}
	}

	templateID, err := client.ResolveTemplateID(ctx, rc.Profile)
	if err != nil {
		return nil, nil, fmt.Errorf("pause-resume: resolve template: %w", err)
	}
	baseImage := ""
	if rc.Profile != nil {
		baseImage = rc.Profile.Stack.Template.BaseImage
	}
	limits := templateResourceLimits(rc.Profile)
	pollInterval := defaultReadinessPollInterval

	if hooks.MetricsSnapshotter != nil {
		if err := hooks.MetricsSnapshotter.BeforeSuite(ctx, s.Name()); err != nil {
			log.Warn("metrics snapshotter BeforeSuite failed", "suite", s.Name(), "error", err)
		}
	}

	suiteStart := time.Now().UTC()

	var (
		samples           []OperationSample
		pauseAPIAckMs     []float64
		pauseTotalMs      []float64
		resumeAPIAckMs    []float64
		resumeFirstPingMs []float64
		resumeFirstExecMs []float64
		errCounts         = make(map[string]int)
		totalOps          int
		succeededOps      int
		failedOps         int
		matrixResults     = make([]PauseResumeCaseResult, 0, len(spec.Modes)*len(spec.DirtyUpperMiB))
	)

	// Part 1: (mode x dirtyUpperMiB) matrix.
	for _, mode := range spec.Modes {
		for _, dirtyMiB := range spec.DirtyUpperMiB {
			if ctx.Err() != nil {
				return nil, samples, ctx.Err()
			}
			caseRes, caseSamples, caseErr := s.runMatrixCase(
				ctx, client, hooks, rc, templateID, baseImage, limits, pollInterval, mode, dirtyMiB,
			)
			matrixResults = append(matrixResults, caseRes)
			samples = append(samples, caseSamples...)
			totalOps++
			if caseErr != nil {
				failedOps++
				errCode, _, _, _ := ClassifyError(caseErr)
				errCounts[errCode]++
			} else {
				succeededOps++
				pauseAPIAckMs = append(pauseAPIAckMs, caseRes.PauseAPIAckMs)
				pauseTotalMs = append(pauseTotalMs, caseRes.PauseTotalMs)
				resumeAPIAckMs = append(resumeAPIAckMs, caseRes.ResumeAPIAckMs)
				resumeFirstPingMs = append(resumeFirstPingMs, caseRes.ResumeFirstPingMs)
				resumeFirstExecMs = append(resumeFirstExecMs, caseRes.ResumeFirstExecMs)
			}
		}
	}

	// Part 2: Multi-round long-chain pause/resume to expose defaultMaxRootfsLayers=24 squash cliff.
	var chainSummary *ChainSquashSummary
	if spec.ChainSquashRounds > 0 {
		summary, chainSamples, chainErrs := s.runChainSquash(
			ctx, client, rc, templateID, baseImage, limits, pollInterval,
			spec.ChainSquashRounds, spec.ChainSquashTemplateMemoryMiB,
		)
		chainSummary = summary
		samples = append(samples, chainSamples...)
		for _, r := range summary.Rounds {
			totalOps++
			if r.Status == "ok" {
				succeededOps++
				pauseAPIAckMs = append(pauseAPIAckMs, r.PauseAPIAckMs)
				pauseTotalMs = append(pauseTotalMs, r.PauseTotalMs)
				resumeAPIAckMs = append(resumeAPIAckMs, r.ResumeAPIAckMs)
				resumeFirstPingMs = append(resumeFirstPingMs, r.ResumeFirstPingMs)
				resumeFirstExecMs = append(resumeFirstExecMs, r.ResumeFirstExecMs)
			} else {
				failedOps++
			}
		}
		for k, v := range chainErrs {
			errCounts[k] += v
		}
	}

	durationSec := time.Since(suiteStart).Seconds()
	var rps float64
	if durationSec > 0 {
		rps = float64(succeededOps) / durationSec
	}

	status := "passed"
	if failedOps > 0 || (totalOps > 0 && succeededOps == 0) {
		status = "failed"
	}

	extra := map[string]any{
		"modes":           spec.Modes,
		"dirty_upper_mib": spec.DirtyUpperMiB,
		"matrix":          matrixResults,
	}
	if chainSummary != nil {
		extra["chain_squash"] = chainSummary
	}

	res := &report.SuiteResult{
		Name:            s.Name(),
		Status:          status,
		StartedAt:       suiteStart,
		DurationSeconds: durationSec,
		ThroughputRPS:   rps,
		TotalOps:        totalOps,
		SucceededOps:    succeededOps,
		FailedOps:       failedOps,
		Latencies: map[string]report.LatencyDist{
			"t_api_ack":       report.ComputeLatencyDist(resumeAPIAckMs),
			"t_first_ping":    report.ComputeLatencyDist(resumeFirstPingMs),
			"t_first_exec":    report.ComputeLatencyDist(resumeFirstExecMs),
			"t_pause_api_ack": report.ComputeLatencyDist(pauseAPIAckMs),
			"t_pause_total":   report.ComputeLatencyDist(pauseTotalMs),
		},
		Extra: extra,
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

func (s *pauseResumeSuite) runMatrixCase(
	ctx context.Context,
	client *BenchClient,
	hooks BenchHooks,
	rc RunContext,
	templateID, baseImage string,
	limits opensandbox.ResourceLimits,
	pollInterval time.Duration,
	mode string,
	dirtyMiB int,
) (PauseResumeCaseResult, []OperationSample, error) {
	caseRes := PauseResumeCaseResult{
		Mode:          mode,
		DirtyUpperMiB: dirtyMiB,
		Status:        "error",
	}
	var samples []OperationSample

	cRes, err := client.CreateSandbox(ctx, CreateOptions{
		TemplateID:            templateID,
		Image:                 baseImage,
		ResourceLimits:        limits,
		ReadinessPollInterval: pollInterval,
		SkipFirstExec:         true,
		Metadata: map[string]string{
			"bench-suite":     s.Name(),
			"bench-run-id":    rc.RunID,
			"bench-mode":      mode,
			"bench-dirty-mib": fmt.Sprintf("%d", dirtyMiB),
		},
	})
	if cRes != nil && cRes.SandboxID != "" {
		caseRes.SandboxID = cRes.SandboxID
		defer client.BestEffortDelete(cRes.SandboxID)
	}
	if err != nil {
		caseRes.Error = err.Error()
		samples = append(samples, newOperationSample("pause_resume_create_"+mode, caseRes.SandboxID, 0, "", err))
		return caseRes, samples, err
	}

	beforeNode, _ := client.GetSandboxNode(ctx, cRes.SandboxID)
	caseRes.BeforeNode = beforeNode

	// Write disk marker, dirty upper payload, and start a live in-memory marker process.
	marker := fmt.Sprintf("osb-pr-%s-%d-%d", mode, dirtyMiB, time.Now().UnixNano())
	if err := preparePauseState(ctx, client, cRes.Execd, marker, dirtyMiB); err != nil {
		caseRes.Error = fmt.Sprintf("prepare state: %v", err)
		samples = append(samples, newOperationSample("pause_resume_prepare_"+mode, cRes.SandboxID, 0, "", err))
		return caseRes, samples, err
	}

	// Pause sandbox (Full checkpoint).
	pRes, err := client.Pause(ctx, cRes.SandboxID, pollInterval, defaultPauseTimeout)
	if pRes != nil {
		caseRes.PauseAPIAckMs = pRes.APIAckMs
		caseRes.PauseTotalMs = pRes.TotalMs
	}
	samples = append(samples, newOperationSample("pause_"+mode, cRes.SandboxID, caseRes.PauseTotalMs, "", err))
	if err != nil {
		caseRes.Error = fmt.Sprintf("pause: %v", err)
		return caseRes, samples, err
	}

	// Mode-specific pre-resume action.
	switch mode {
	case "cache-evicted":
		if hooks.CacheEvictor != nil {
			if err := hooks.CacheEvictor.EvictOverlayBDCaches(ctx, beforeNode); err != nil {
				caseRes.Error = fmt.Sprintf("evict caches: %v", err)
				return caseRes, samples, err
			}
			caseRes.CacheEvicted = true
		}
	case "cross-node":
		if hooks.NodeIsolator != nil && beforeNode != "" {
			restore, err := hooks.NodeIsolator.IsolateNodeFastlets(ctx, beforeNode)
			if err != nil {
				caseRes.Error = fmt.Sprintf("isolate source node %s: %v", beforeNode, err)
				return caseRes, samples, err
			}
			if restore != nil {
				defer func() {
					rCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_ = restore(rCtx)
				}()
			}
		}
	}

	// Resume sandbox.
	rStart := time.Now()
	rRes, err := client.Resume(ctx, cRes.SandboxID, pollInterval, defaultReadyTimeout)
	if rRes != nil {
		caseRes.ResumeAPIAckMs = rRes.APIAckMs
		caseRes.ResumeFirstPingMs = rRes.FirstPingMs
	}
	if err != nil {
		caseRes.Error = fmt.Sprintf("resume: %v", err)
		samples = append(samples, newOperationSample("resume_"+mode, cRes.SandboxID, caseRes.ResumeFirstPingMs, "", err))
		return caseRes, samples, err
	}

	// Verify node placement for cross-node mode.
	afterNode, _ := client.GetSandboxNode(ctx, cRes.SandboxID)
	caseRes.AfterNode = afterNode
	if mode == "cross-node" {
		if beforeNode == "" || afterNode == "" || beforeNode == afterNode {
			err := fmt.Errorf("cross-node placement assertion failed: before_node=%q after_node=%q", beforeNode, afterNode)
			caseRes.Error = err.Error()
			samples = append(samples, newOperationSample("resume_"+mode, cRes.SandboxID, caseRes.ResumeFirstPingMs, "", err))
			return caseRes, samples, err
		}
		caseRes.CrossNodeVerified = true
	}

	// Verify disk marker integrity and live in-memory process continuity.
	diskOK, memOK, verifyErr := verifyRestoredState(ctx, client, rRes.Execd, marker)
	caseRes.ResumeFirstExecMs = msSince(rStart)
	caseRes.DiskMarkerVerified = diskOK
	caseRes.MemProcessVerified = memOK
	if verifyErr != nil {
		caseRes.Error = verifyErr.Error()
		samples = append(samples, newOperationSample("resume_"+mode, cRes.SandboxID, caseRes.ResumeFirstExecMs, "", verifyErr))
		return caseRes, samples, verifyErr
	}

	caseRes.Status = "ok"
	samples = append(samples, newOperationSample("resume_"+mode, cRes.SandboxID, caseRes.ResumeFirstPingMs, "", nil))
	return caseRes, samples, nil
}

func (s *pauseResumeSuite) runChainSquash(
	ctx context.Context,
	client *BenchClient,
	rc RunContext,
	templateID, baseImage string,
	limits opensandbox.ResourceLimits,
	pollInterval time.Duration,
	rounds int,
	chainMemoryMiB int,
) (*ChainSquashSummary, []OperationSample, map[string]int) {
	summary := &ChainSquashSummary{
		RoundsConfigured:       rounds,
		DefaultMaxRootfsLayers: defaultMaxRootfsLayers,
		Rounds:                 make([]ChainSquashRoundResult, 0, rounds),
	}
	var samples []OperationSample
	errCounts := make(map[string]int)

	chainLimits := make(opensandbox.ResourceLimits, len(limits))
	for k, v := range limits {
		chainLimits[k] = v
	}
	if chainMemoryMiB > 0 {
		chainLimits["memory"] = fmt.Sprintf("%dMi", chainMemoryMiB)
	}

	cRes, err := client.CreateSandbox(ctx, CreateOptions{
		TemplateID:            templateID,
		Image:                 baseImage,
		ResourceLimits:        chainLimits,
		ReadinessPollInterval: pollInterval,
		SkipFirstExec:         true,
		Metadata: map[string]string{
			"bench-suite":  s.Name(),
			"bench-run-id": rc.RunID,
			"bench-phase":  "chain-squash",
		},
	})
	if cRes != nil && cRes.SandboxID != "" {
		defer client.BestEffortDelete(cRes.SandboxID)
	}
	if err != nil {
		errCode, _, _, _ := ClassifyError(err)
		errCounts[errCode]++
		summary.Rounds = append(summary.Rounds, ChainSquashRoundResult{
			Round:  1,
			Status: "error",
			Error:  fmt.Sprintf("create chain sandbox: %v", err),
		})
		samples = append(samples, newOperationSample("chain_create", "", 0, "", err))
		return summary, samples, errCounts
	}

	baseMarker := fmt.Sprintf("osb-chain-%d", time.Now().UnixNano())
	if err := preparePauseState(ctx, client, cRes.Execd, baseMarker, 0); err != nil {
		errCode, _, _, _ := ClassifyError(err)
		errCounts[errCode]++
		summary.Rounds = append(summary.Rounds, ChainSquashRoundResult{
			Round:  1,
			Status: "error",
			Error:  fmt.Sprintf("prepare chain state: %v", err),
		})
		return summary, samples, errCounts
	}

	execd := cRes.Execd
	var pauseTotals []float64

	for round := 1; round <= rounds; round++ {
		if ctx.Err() != nil {
			break
		}
		rr := ChainSquashRoundResult{
			Round:  round,
			Status: "error",
		}

		// Touch a new upper-layer file per round so every pause adds an OverlayBD rootfs layer.
		roundMarker := fmt.Sprintf("%s-r%d", baseMarker, round)
		touchCmd := fmt.Sprintf("printf '%%s' '%s' > /tmp/bench-round-%d.dat && sync", roundMarker, round)
		if _, err := client.ExecCommand(ctx, execd, touchCmd); err != nil {
			rr.Error = fmt.Sprintf("write round layer: %v", err)
			errCode, _, _, _ := ClassifyError(err)
			errCounts[errCode]++
			summary.Rounds = append(summary.Rounds, rr)
			break
		}

		pRes, err := client.Pause(ctx, cRes.SandboxID, pollInterval, defaultPauseTimeout)
		if pRes != nil {
			rr.PauseAPIAckMs = pRes.APIAckMs
			rr.PauseTotalMs = pRes.TotalMs
		}
		samples = append(samples, newOperationSample(fmt.Sprintf("chain_pause_r%d", round), cRes.SandboxID, rr.PauseTotalMs, "", err))
		if err != nil {
			rr.Error = fmt.Sprintf("pause round %d: %v", round, err)
			errCode, _, _, _ := ClassifyError(err)
			errCounts[errCode]++
			summary.Rounds = append(summary.Rounds, rr)
			break
		}

		rStart := time.Now()
		rRes, err := client.Resume(ctx, cRes.SandboxID, pollInterval, defaultReadyTimeout)
		if rRes != nil {
			rr.ResumeAPIAckMs = rRes.APIAckMs
			rr.ResumeFirstPingMs = rRes.FirstPingMs
			execd = rRes.Execd
		}
		if err != nil {
			rr.Error = fmt.Sprintf("resume round %d: %v", round, err)
			errCode, _, _, _ := ClassifyError(err)
			errCounts[errCode]++
			samples = append(samples, newOperationSample(fmt.Sprintf("chain_resume_r%d", round), cRes.SandboxID, rr.ResumeFirstPingMs, "", err))
			summary.Rounds = append(summary.Rounds, rr)
			break
		}

		diskOK, memOK, vErr := verifyRestoredState(ctx, client, execd, baseMarker)
		rr.ResumeFirstExecMs = msSince(rStart)
		rr.StateVerified = diskOK && memOK && vErr == nil
		if vErr != nil {
			rr.Error = fmt.Sprintf("verify round %d: %v", round, vErr)
			errCode, _, _, _ := ClassifyError(vErr)
			errCounts[errCode]++
			samples = append(samples, newOperationSample(fmt.Sprintf("chain_resume_r%d", round), cRes.SandboxID, rr.ResumeFirstExecMs, "", vErr))
			summary.Rounds = append(summary.Rounds, rr)
			break
		}

		rr.Status = "ok"
		summary.RoundsCompleted++
		pauseTotals = append(pauseTotals, rr.PauseTotalMs)
		if rr.PauseTotalMs > summary.MaxPauseMs {
			summary.MaxPauseMs = rr.PauseTotalMs
			summary.MaxPauseRound = round
		}
		samples = append(samples, newOperationSample(fmt.Sprintf("chain_resume_r%d", round), cRes.SandboxID, rr.ResumeFirstPingMs, "", nil))
		summary.Rounds = append(summary.Rounds, rr)
	}

	if len(pauseTotals) > 0 {
		sorted := make([]float64, len(pauseTotals))
		copy(sorted, pauseTotals)
		sort.Float64s(sorted)
		summary.MedianPauseMs = sorted[len(sorted)/2]
		if summary.MedianPauseMs > 0 {
			summary.SquashCliffRatio = summary.MaxPauseMs / summary.MedianPauseMs
		}
	}

	return summary, samples, errCounts
}

func preparePauseState(ctx context.Context, client *BenchClient, execd *opensandbox.ExecdClient, marker string, dirtyMiB int) error {
	var cmd string
	if dirtyMiB > 0 {
		cmd = fmt.Sprintf(
			"printf '%%s' '%s' > /tmp/bench-disk-marker && dd if=/dev/zero bs=1M count=%d 2>/dev/null | tr '\\0' 'X' > /tmp/bench-dirty.bin && sync",
			marker, dirtyMiB,
		)
	} else {
		cmd = fmt.Sprintf("printf '%%s' '%s' > /tmp/bench-disk-marker && sync", marker)
	}
	if _, err := client.ExecCommand(ctx, execd, cmd); err != nil {
		return fmt.Errorf("write disk marker: %w", err)
	}

	// Start a live in-memory marker process whose PID and environment survive Full pause/resume.
	bgCmd := fmt.Sprintf("sh -c 'echo $$ > /tmp/bench-mem.pid; export BENCH_MEM_MARKER=%s; exec sleep 3600'", marker)
	if _, err := client.ExecBackground(ctx, execd, bgCmd); err != nil {
		return fmt.Errorf("start memory marker process: %w", err)
	}
	return nil
}

func verifyRestoredState(ctx context.Context, client *BenchClient, execd *opensandbox.ExecdClient, marker string) (diskOK, memOK bool, err error) {
	verifyCmd := fmt.Sprintf(
		"DISK=$(cat /tmp/bench-disk-marker 2>/dev/null); "+
			"PID=$(cat /tmp/bench-mem.pid 2>/dev/null); "+
			"MEM_ALIVE=no; "+
			"if [ -n \"$PID\" ] && kill -0 \"$PID\" 2>/dev/null; then "+
			"  if tr '\\0' '\\n' < /proc/\"$PID\"/environ 2>/dev/null | grep -qx 'BENCH_MEM_MARKER=%s'; then MEM_ALIVE=yes; fi; "+
			"fi; "+
			"echo \"DISK=$DISK MEM=$MEM_ALIVE\"",
		marker,
	)
	res, execErr := client.ExecCommand(ctx, execd, verifyCmd)
	if execErr != nil {
		return false, false, fmt.Errorf("verify restored state exec: %w", execErr)
	}

	out := strings.TrimSpace(res.Stdout)
	diskOK = strings.Contains(out, "DISK="+marker)
	memOK = strings.Contains(out, "MEM=yes")
	if !diskOK || !memOK {
		return diskOK, memOK, fmt.Errorf("state continuity verification failed (disk_ok=%v mem_ok=%v output=%q)", diskOK, memOK, out)
	}
	return true, true, nil
}
