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
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// SoakLoadSample captures a point-in-time host/fastlet/egress/proxy snapshot
// collected during high-density-soak tiers (OSEP-0034 §4.5).
type SoakLoadSample struct {
	ResidentMiBPerSandbox float64
	CPUPressureSomeAvg60  float64
	MemPressureFullAvg60  float64
	MemAvailableMiB       float64
	OOMKills              int
	DeletingSlots         int
	StateRootFreeGiB      float64
	LoadProfiles          *report.LoadProfiles
}

// SoakLoadSampler is an optional hook that samples host, fastlet, egress, and
// fastlet-proxy load telemetry during each soak tier and drain phase.
type SoakLoadSampler interface {
	Sample(ctx context.Context) (*SoakLoadSample, error)
}

// SoakLoadSamplerFunc adapts a function to SoakLoadSampler.
type SoakLoadSamplerFunc func(ctx context.Context) (*SoakLoadSample, error)

func (f SoakLoadSamplerFunc) Sample(ctx context.Context) (*SoakLoadSample, error) {
	return f(ctx)
}

// HighDensitySoakOption configures optional hooks and timings for HighDensitySoakSuite.
type HighDensitySoakOption func(*HighDensitySoakSuite)

// WithSoakLoadSampler attaches a host/fastlet/egress/fastlet-proxy load sampler.
func WithSoakLoadSampler(sampler SoakLoadSampler) HighDensitySoakOption {
	return func(s *HighDensitySoakSuite) {
		s.sampler = sampler
	}
}

// WithSoakNodeMemoryMiB provides total allocatable node memory (MiB) used to
// translate OversellTiers into target sandbox counts.
func WithSoakNodeMemoryMiB(mib int) HighDensitySoakOption {
	return func(s *HighDensitySoakSuite) {
		if mib > 0 {
			s.nodeMemoryMiB = mib
		}
	}
}

// WithSoakEgressProbeURL overrides the URL targeted by the in-sandbox outbound
// egress HTTP/TLS probe.
func WithSoakEgressProbeURL(targetURL string) HighDensitySoakOption {
	return func(s *HighDensitySoakSuite) {
		if strings.TrimSpace(targetURL) != "" {
			s.egressProbeURL = strings.TrimSpace(targetURL)
		}
	}
}

// WithSoakPollInterval overrides readiness and recovery polling cadence.
func WithSoakPollInterval(d time.Duration) HighDensitySoakOption {
	return func(s *HighDensitySoakSuite) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// WithSoakTickInterval overrides the periodic workload tick cadence within a tier.
func WithSoakTickInterval(d time.Duration) HighDensitySoakOption {
	return func(s *HighDensitySoakSuite) {
		if d > 0 {
			s.tickInterval = d
		}
	}
}

// HighDensitySoakSuite steps through memory oversell tiers, sustains periodic
// in-sandbox exec, outbound egress probes, ingress proxy requests, and
// background churn while sampling component load, then measures post-tier drain
// and recovery time (OSEP-0034 §3, §4.5, §4.6).
type HighDensitySoakSuite struct {
	sampler        SoakLoadSampler
	nodeMemoryMiB  int
	egressProbeURL string
	pollInterval   time.Duration
	tickInterval   time.Duration
}

// NewHighDensitySoakSuite constructs the Phase 1 high-density-soak suite.
func NewHighDensitySoakSuite(opts ...HighDensitySoakOption) *HighDensitySoakSuite {
	s := &HighDensitySoakSuite{
		pollInterval: scaleDefaultPollInterval,
		tickInterval: 1 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *HighDensitySoakSuite) Name() string { return "high-density-soak" }

func (s *HighDensitySoakSuite) Description() string {
	return "Multi-tier density oversell soak with host/fastlet/egress/proxy load sampling and drain recovery"
}

func (s *HighDensitySoakSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

type soakActiveSandbox struct {
	id        string
	epURL     string
	epHeaders map[string]string
}

func (s *HighDensitySoakSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	startedAt := time.Now().UTC()
	spec := &profile.HighDensitySoakSuiteSpec{
		OversellTiers:          []float64{0.5, 1.0},
		DurationSecondsPerTier: 5,
		MeasureRecovery:        true,
		SamplerIntervalSeconds: 2,
	}
	if rc.Profile != nil && rc.Profile.Suites.HighDensitySoak != nil {
		spec = rc.Profile.Suites.HighDensitySoak
	}
	tiers := spec.OversellTiers
	if len(tiers) == 0 {
		tiers = []float64{0.5, 1.0}
	}
	durationPerTier := time.Duration(spec.DurationSecondsPerTier) * time.Second
	if durationPerTier <= 0 {
		durationPerTier = 2 * time.Second
	}
	samplerInterval := time.Duration(spec.SamplerIntervalSeconds) * time.Second
	if samplerInterval <= 0 {
		samplerInterval = 2 * time.Second
	}

	hooks := BenchHooksFromContext(ctx)
	sampler := s.sampler
	if sampler == nil {
		sampler = hooks.SoakLoadSampler
	}

	templateMemMiB := 2048
	if rc.Profile != nil && rc.Profile.Stack.Template.MemoryMiB > 0 {
		templateMemMiB = rc.Profile.Stack.Template.MemoryMiB
	}
	nodeMemMiB := s.nodeMemoryMiB
	if nodeMemMiB <= 0 {
		nodeMemMiB = hooks.SoakNodeMemoryMiB
	}
	if nodeMemMiB <= 0 {
		// Sensible fallback when node memory is unavailable (e.g., offline tests).
		nodeMemMiB = templateMemMiB * 4
	}

	client := newScaleClient(rc)
	rec := newScaleRecorder()

	var active []soakActiveSandbox
	defer func() {
		s.deleteAllBestEffort(client, active)
	}()

	densityFrontier := make([]report.DensityTierRow, 0, len(tiers))
	recoveryByTier := make(map[string]float64, len(tiers))
	var latestLoadProfiles *report.LoadProfiles
	var maxRecoverySec float64

	for _, tier := range tiers {
		if err := ctx.Err(); err != nil {
			break
		}

		targetCount := int(math.Round(tier * float64(nodeMemMiB) / float64(templateMemMiB)))
		if targetCount < 1 {
			targetCount = 1
		}

		// 1. Scale up active sandboxes to targetCount for this tier.
		admissionDenials := 0
		if len(active) < targetCount {
			needed := targetCount - len(active)
			created, denials := s.scaleUp(ctx, client, rc.Profile, rec, needed, tier)
			active = append(active, created...)
			admissionDenials += denials
		}

		// 2. Start optional periodic load sampler for this tier.
		var sampleMu sync.Mutex
		var tierSamples []*SoakLoadSample
		samplerCtx, samplerCancel := context.WithCancel(ctx)
		var samplerWG sync.WaitGroup
		if sampler != nil {
			samplerWG.Add(1)
			go func() {
				defer samplerWG.Done()
				// Take an immediate sample and then sample on ticker.
				if sm, err := sampler.Sample(samplerCtx); err == nil && sm != nil {
					sampleMu.Lock()
					tierSamples = append(tierSamples, sm)
					sampleMu.Unlock()
				}
				tk := time.NewTicker(samplerInterval)
				defer tk.Stop()
				for {
					select {
					case <-samplerCtx.Done():
						return
					case <-tk.C:
						if sm, err := sampler.Sample(samplerCtx); err == nil && sm != nil {
							sampleMu.Lock()
							tierSamples = append(tierSamples, sm)
							sampleMu.Unlock()
						}
					}
				}
			}()
		}

		// 3. Run soak workload for durationPerTier: exec + egress probe + ingress proxy + churn.
		egressURL := s.resolveEgressProbeURL(spec, rc)
		tierExecLatencies := s.runTierWorkload(ctx, client, rc.Profile, rec, active, durationPerTier, tier, egressURL)

		samplerCancel()
		samplerWG.Wait()

		// 4. Aggregate load samples for this tier.
		row := report.DensityTierRow{
			OversellRatio:    tier,
			ActiveSandboxes:  len(active),
			AdmissionDenials: admissionDenials,
		}
		if len(tierExecLatencies) > 0 {
			row.P99ExecLatencyMs = report.ComputeLatencyDist(tierExecLatencies).P99Ms
		}
		sampleMu.Lock()
		for i, sm := range tierSamples {
			if sm.ResidentMiBPerSandbox > row.ResidentMiBPerSandbox {
				row.ResidentMiBPerSandbox = sm.ResidentMiBPerSandbox
			}
			if sm.CPUPressureSomeAvg60 > row.CPUPressureSomeAvg60 {
				row.CPUPressureSomeAvg60 = sm.CPUPressureSomeAvg60
			}
			if sm.MemPressureFullAvg60 > row.MemPressureFullAvg60 {
				row.MemPressureFullAvg60 = sm.MemPressureFullAvg60
			}
			if i == 0 || sm.MemAvailableMiB < row.MemAvailableMinMiB {
				row.MemAvailableMinMiB = sm.MemAvailableMiB
			}
			if sm.OOMKills > row.OOMKills {
				row.OOMKills = sm.OOMKills
			}
			if sm.LoadProfiles != nil {
				latestLoadProfiles = sm.LoadProfiles
			}
		}
		sampleMu.Unlock()

		// 5. Measure post-tier drain & recovery if configured.
		if spec.MeasureRecovery && len(active) > 0 {
			recStart := time.Now()
			s.deleteAllBestEffort(client, active)
			active = nil
			s.waitDrainAndRecovery(ctx, client, sampler)
			recSec := time.Since(recStart).Seconds()
			row.RecoveryTimeSeconds = recSec
			recoveryByTier[fmt.Sprintf("%.2fx", tier)] = recSec
			if recSec > maxRecoverySec {
				maxRecoverySec = recSec
			}
		}

		densityFrontier = append(densityFrontier, row)
	}

	durationSec := time.Since(startedAt).Seconds()
	latencies := make(map[string]report.LatencyDist, len(rec.latencies))
	for k, vals := range rec.latencies {
		if len(vals) > 0 {
			latencies[k] = report.ComputeLatencyDist(vals)
		}
	}

	status := "passed"
	if rec.totalOps == 0 || rec.failedOps > 0 {
		status = "failed"
	}
	// Any host/cgroup OOM kill fails the stability gate (OSEP-0034 §4.6).
	for _, row := range densityFrontier {
		if row.OOMKills > 0 {
			status = "failed"
			break
		}
	}

	var throughput float64
	if durationSec > 0 && rec.succeededOps > 0 {
		throughput = float64(rec.succeededOps) / durationSec
	}

	extra := map[string]any{
		"density_frontier":      densityFrontier,
		"recovery_time_seconds": maxRecoverySec,
		"recovery_by_tier":      recoveryByTier,
		"node_memory_mib":       nodeMemMiB,
		"template_memory_mib":   templateMemMiB,
	}
	if latestLoadProfiles != nil {
		lpCopy := *latestLoadProfiles
		lpCopy.DensityFrontier = densityFrontier
		extra["load_profiles"] = &lpCopy
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
	return res, rec.samples, nil
}

func (s *HighDensitySoakSuite) scaleUp(ctx context.Context, client *scaleClient, prof *profile.BenchmarkProfile, rec *scaleRecorder, count int, tier float64) ([]soakActiveSandbox, int) {
	var (
		mu      sync.Mutex
		created []soakActiveSandbox
		denials int
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, 16)
	opName := fmt.Sprintf("soak_scaleup_%.2fx", tier)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			req := scaleBuildBaseCreateRequest(prof, scaleDefaultTimeoutSec, nil)
			t0 := time.Now()
			info, err := client.lifecycle.CreateSandbox(ctx, req)
			ackMs := scaleElapsedMs(t0)
			if err != nil {
				if scaleIsHTTPStatus(err, http.StatusTooManyRequests) || scaleIsHTTPStatus(err, http.StatusServiceUnavailable) {
					mu.Lock()
					denials++
					mu.Unlock()
				}
				rec.recordOp(OperationSample{
					Operation: opName,
					LatencyMs: ackMs,
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
				return
			}
			readyCtx, cancel := context.WithTimeout(ctx, scaleDefaultReadyTimeout)
			epURL, epHeaders, err := client.waitSandboxRunningAndPing(readyCtx, info.ID, s.pollInterval)
			cancel()
			pingMs := scaleElapsedMs(t0)
			if err != nil {
				client.deleteSandboxBestEffort(info.ID)
				rec.recordOp(OperationSample{
					Operation: opName,
					SandboxID: info.ID,
					LatencyMs: pingMs,
					Status:    "error",
					AckState:  string(info.Status.State),
					ErrorCode: scaleErrorCode(err),
				})
				return
			}

			rec.addLatency("t_api_ack", ackMs)
			rec.addLatency("t_first_ping", pingMs)
			rec.recordOp(OperationSample{
				Operation: opName,
				SandboxID: info.ID,
				LatencyMs: pingMs,
				Status:    "ok",
				AckState:  string(info.Status.State),
			})
			mu.Lock()
			created = append(created, soakActiveSandbox{
				id:        info.ID,
				epURL:     epURL,
				epHeaders: epHeaders,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return created, denials
}

func (s *HighDensitySoakSuite) resolveEgressProbeURL(spec *profile.HighDensitySoakSuiteSpec, rc RunContext) string {
	if spec != nil && strings.TrimSpace(spec.EgressProbeURL) != "" {
		return strings.TrimSpace(spec.EgressProbeURL)
	}
	if strings.TrimSpace(s.egressProbeURL) != "" {
		return strings.TrimSpace(s.egressProbeURL)
	}
	if strings.TrimSpace(rc.SDKConfig.Domain) != "" {
		return strings.TrimRight(rc.SDKConfig.GetBaseURL(), "/") + "/health"
	}
	return "https://example.com"
}

// buildEgressProbeCommand constructs a POSIX-shell probe command that tries
// curl, wget, python3, and bash /dev/tcp in order so it works on both rich
// and minimal guest images.
func buildEgressProbeCommand(targetURL string) string {
	quoted := "'" + strings.ReplaceAll(targetURL, "'", "'\"'\"'") + "'"
	return fmt.Sprintf(
		"URL=%s; "+
			"if command -v curl >/dev/null 2>&1; then "+
			"curl -sf --max-time 5 -o /dev/null \"$URL\"; "+
			"elif command -v wget >/dev/null 2>&1; then "+
			"wget -q -T 5 -O /dev/null \"$URL\"; "+
			"elif command -v python3 >/dev/null 2>&1; then "+
			"python3 -c 'import sys,urllib.request; urllib.request.urlopen(sys.argv[1], timeout=5).read(1)' \"$URL\"; "+
			"elif command -v bash >/dev/null 2>&1; then "+
			"bash -c 'u=${1#*://}; hp=${u%%%%/*}; p=/${u#*/}; [ \"$p\" = \"/$u\" ] && p=/; "+
			"h=${hp%%%%:*}; port=${hp#*:}; [ \"$port\" = \"$hp\" ] && { case \"$1\" in https://*) port=443;; *) port=80;; esac; }; "+
			"exec 3<>/dev/tcp/\"$h\"/\"$port\" && printf \"GET %%s HTTP/1.0\\r\\nHost: %%s\\r\\n\\r\\n\" \"$p\" \"$h\" >&3 && head -n 1 <&3 | grep -q \"^HTTP/\"' _ \"$URL\"; "+
			"else echo \"no http probe tool available\" >&2; exit 127; fi",
		quoted,
	)
}

func (s *HighDensitySoakSuite) runTierWorkload(
	ctx context.Context,
	client *scaleClient,
	prof *profile.BenchmarkProfile,
	rec *scaleRecorder,
	active []soakActiveSandbox,
	duration time.Duration,
	tier float64,
	egressProbeURL string,
) []float64 {
	if len(active) == 0 {
		return nil
	}

	tierCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var execLatencies []float64
	tick := s.tickInterval
	if tick > duration/2 && duration > 20*time.Millisecond {
		tick = duration / 2
	}
	if tick <= 0 {
		tick = 10 * time.Millisecond
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	rrIdx := 0
	runOnce := func() {
		target := active[rrIdx%len(active)]
		rrIdx++

		// (1) Periodic in-sandbox exec.
		execStart := time.Now()
		_, err := client.runCommand(tierCtx, target.epURL, target.epHeaders, "echo soak-ok")
		execMs := scaleElapsedMs(execStart)
		if err == nil {
			execLatencies = append(execLatencies, execMs)
			rec.addLatency("soak_exec", execMs)
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("soak_exec_%.2fx", tier),
				SandboxID: target.id,
				LatencyMs: execMs,
				Status:    "ok",
			})
		} else if tierCtx.Err() == nil {
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("soak_exec_%.2fx", tier),
				SandboxID: target.id,
				LatencyMs: execMs,
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
		}

		// (2) Outbound egress HTTP/TLS probe via in-sandbox exec (skipped when egressProbeURL is "none"/"disabled").
		if egressProbeURL != "" && !strings.EqualFold(egressProbeURL, "none") && !strings.EqualFold(egressProbeURL, "disabled") {
			egressStart := time.Now()
			egressCmd := buildEgressProbeCommand(egressProbeURL)
			_, err = client.runCommand(tierCtx, target.epURL, target.epHeaders, egressCmd)
			egressMs := scaleElapsedMs(egressStart)
			if err == nil {
				rec.addLatency("soak_egress_probe", egressMs)
				rec.recordOp(OperationSample{
					Operation: fmt.Sprintf("soak_egress_%.2fx", tier),
					SandboxID: target.id,
					LatencyMs: egressMs,
					Status:    "ok",
				})
			} else if tierCtx.Err() == nil {
				rec.recordOp(OperationSample{
					Operation: fmt.Sprintf("soak_egress_%.2fx", tier),
					SandboxID: target.id,
					LatencyMs: egressMs,
					Status:    "error",
					ErrorCode: scaleErrorCode(err),
				})
			}
		}

		// (3) Ingress proxy request.
		pingURL := strings.TrimRight(target.epURL, "/") + "/ping"
		statusCode, _, ingressMs, err := client.probeURL(tierCtx, pingURL, target.epHeaders)
		if err == nil && statusCode == http.StatusOK {
			rec.addLatency("soak_ingress_probe", ingressMs)
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("soak_ingress_%.2fx", tier),
				SandboxID: target.id,
				LatencyMs: ingressMs,
				Status:    "ok",
			})
		} else if tierCtx.Err() == nil {
			code := scaleErrorCode(err)
			if err == nil {
				code = fmt.Sprintf("http_%d", statusCode)
			}
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("soak_ingress_%.2fx", tier),
				SandboxID: target.id,
				LatencyMs: ingressMs,
				Status:    "error",
				ErrorCode: code,
			})
		}

		// (4) Background churn: create -> ping -> delete a short-lived sandbox.
		// Skip starting a new churn sandbox if the tier window has already elapsed, and
		// use the parent ctx for CreateSandbox so a tier-deadline expiry mid-POST cannot
		// orphan a newly created sandbox without returning its ID for deletion.
		if tierCtx.Err() != nil {
			return
		}
		churnStart := time.Now()
		churnReq := scaleBuildBaseCreateRequest(prof, scaleDefaultTimeoutSec, nil)
		churnCreateCtx, churnCreateCancel := context.WithTimeout(ctx, scaleDefaultReadyTimeout)
		churnInfo, err := client.lifecycle.CreateSandbox(churnCreateCtx, churnReq)
		churnCreateCancel()
		if err == nil {
			_, _, readyErr := client.waitSandboxRunningAndPing(tierCtx, churnInfo.ID, s.pollInterval)
			client.deleteSandboxBestEffort(churnInfo.ID)
			churnMs := scaleElapsedMs(churnStart)
			if readyErr == nil {
				rec.addLatency("soak_churn", churnMs)
				rec.recordOp(OperationSample{
					Operation: fmt.Sprintf("soak_churn_%.2fx", tier),
					SandboxID: churnInfo.ID,
					LatencyMs: churnMs,
					Status:    "ok",
					AckState:  string(churnInfo.Status.State),
				})
			} else if tierCtx.Err() == nil {
				rec.recordOp(OperationSample{
					Operation: fmt.Sprintf("soak_churn_%.2fx", tier),
					SandboxID: churnInfo.ID,
					LatencyMs: churnMs,
					Status:    "error",
					ErrorCode: scaleErrorCode(readyErr),
				})
			}
		} else if tierCtx.Err() == nil {
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("soak_churn_%.2fx", tier),
				LatencyMs: scaleElapsedMs(churnStart),
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
		}
	}

	// Execute at least one iteration immediately, then continue on ticker until tierCtx expires.
	runOnce()
	for {
		select {
		case <-tierCtx.Done():
			return execLatencies
		case <-ticker.C:
			runOnce()
		}
	}
}

func (s *HighDensitySoakSuite) deleteAllBestEffort(client *scaleClient, sandboxes []soakActiveSandbox) {
	var wg sync.WaitGroup
	for _, sbx := range sandboxes {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			client.deleteSandboxBestEffort(id)
		}(sbx.id)
	}
	wg.Wait()
}

func (s *HighDensitySoakSuite) waitDrainAndRecovery(ctx context.Context, client *scaleClient, sampler SoakLoadSampler) {
	drainCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	for {
		slotsDrained := true
		if sampler != nil {
			if sm, err := sampler.Sample(drainCtx); err == nil && sm != nil && sm.DeletingSlots > 0 {
				slotsDrained = false
			}
		}
		listDrained := true
		if resp, err := client.lifecycle.ListSandboxes(drainCtx, opensandbox.ListOptions{}); err == nil && resp != nil {
			for _, item := range resp.Items {
				if item.Status.State != opensandbox.StateTerminated && item.Status.State != opensandbox.StateFailed {
					listDrained = false
					break
				}
			}
		}
		if slotsDrained && listDrained {
			return
		}
		select {
		case <-drainCtx.Done():
			return
		case <-time.After(s.pollInterval):
		}
	}
}
