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
	"strconv"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// RootfsOnlyRestoreOption configures optional hooks for RootfsOnlyRestoreSuite.
type RootfsOnlyRestoreOption func(*RootfsOnlyRestoreSuite)

// WithRootfsNodeIsolator injects a node-isolation hook used when CrossNode is
// enabled in the profile to force placement onto a different node.
func WithRootfsNodeIsolator(fn NodeIsolatorFunc) RootfsOnlyRestoreOption {
	return func(s *RootfsOnlyRestoreSuite) {
		s.nodeIsolator = fn
	}
}

// WithRootfsPollInterval overrides the readiness/snapshot polling interval.
func WithRootfsPollInterval(d time.Duration) RootfsOnlyRestoreOption {
	return func(s *RootfsOnlyRestoreSuite) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// RootfsOnlyRestoreSuite benchmarks FSEP-0001 RootfsOnly snapshot creation and
// cold-boot sandbox creation/forking across dirty-upper tiers, warm vs. cold
// materialized snapfiles, concurrent fan-out, cross-node portability, and
// OSEP-0028 CPU/memory overrides (OSEP-0034 §3, §4.1).
type RootfsOnlyRestoreSuite struct {
	nodeIsolator NodeIsolatorFunc
	pollInterval time.Duration
}

// NewRootfsOnlyRestoreSuite constructs the Phase 1 rootfs-only-restore suite.
func NewRootfsOnlyRestoreSuite(opts ...RootfsOnlyRestoreOption) *RootfsOnlyRestoreSuite {
	s := &RootfsOnlyRestoreSuite{
		pollInterval: scaleDefaultPollInterval,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *RootfsOnlyRestoreSuite) Name() string { return "rootfs-only-restore" }

func (s *RootfsOnlyRestoreSuite) Description() string {
	return "RootfsOnly snapshot create/fork across warm/cold materialize, fan-out, cross-node, and OSEP-0028 overrides"
}

func (s *RootfsOnlyRestoreSuite) SupportedRuntimes() []string {
	return []string{profile.RuntimeFastSandboxOverlayBD}
}

type rootfsRestoreOutcome struct {
	sandboxID      string
	ackState       string
	node           string
	ackMs          float64
	pingMs         float64
	bootGapMs      float64
	execMs         float64
	markerOK       bool
	overrideOK     bool
	observedVCPUs  int
	observedMemMiB float64
	err            error
}

func (s *RootfsOnlyRestoreSuite) Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error) {
	startedAt := time.Now().UTC()
	spec := &profile.RootfsOnlyRestoreSuiteSpec{
		DirtyUpperMiB:       []int{0},
		FanoutConcurrencies: []int{1},
	}
	if rc.Profile != nil && rc.Profile.Suites.RootfsOnlyRestore != nil {
		spec = rc.Profile.Suites.RootfsOnlyRestore
	}
	dirtyLadder := spec.DirtyUpperMiB
	if len(dirtyLadder) == 0 {
		dirtyLadder = []int{0}
	}
	fanoutLevels := spec.FanoutConcurrencies
	if len(fanoutLevels) == 0 {
		fanoutLevels = []int{1}
	}

	client := newScaleClient(rc)
	rec := newScaleRecorder()

	dirtySummaries := make([]map[string]any, 0, len(dirtyLadder))
	fanoutSummaries := make([]map[string]any, 0, len(fanoutLevels))
	var crossNodeSummary map[string]any
	overrideSummaries := make([]map[string]any, 0, len(spec.ResourceOverrides))

	for idx, dirtyMiB := range dirtyLadder {
		if err := ctx.Err(); err != nil {
			break
		}

		markerToken := fmt.Sprintf("bench-rootfs-%s-%d-%d", rc.RunID, dirtyMiB, time.Now().UnixNano())
		baseReq := scaleBuildBaseCreateRequest(rc.Profile, scaleDefaultTimeoutSec, nil)

		// 1. Create base sandbox and wait for readiness.
		baseInfo, err := client.lifecycle.CreateSandbox(ctx, baseReq)
		if err != nil {
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("rootfs_base_create_%dmib", dirtyMiB),
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
			continue
		}
		baseID := baseInfo.ID
		beforeNode := scaleExtractNode(baseInfo)

		baseEP, baseHeaders, err := client.waitSandboxRunningAndPing(ctx, baseID, s.pollInterval)
		if err != nil {
			client.deleteSandboxBestEffort(baseID)
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("rootfs_base_ready_%dmib", dirtyMiB),
				SandboxID: baseID,
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
			continue
		}
		if beforeNode == "" {
			if refreshed, gErr := client.lifecycle.GetSandbox(ctx, baseID); gErr == nil {
				beforeNode = scaleExtractNode(refreshed)
			}
		}
		if beforeNode == "" {
			if hooks := BenchHooksFromContext(ctx); hooks.NodeResolver != nil {
				if resolved, rErr := hooks.NodeResolver.SandboxNode(ctx, baseID); rErr == nil && resolved != "" {
					beforeNode = resolved
				}
			}
		}

		// 2. Write disk marker and optional dirty upper data.
		writeCmd := fmt.Sprintf("printf '%%s' %q > /tmp/bench-rootfs-marker", markerToken)
		if dirtyMiB > 0 {
			writeCmd = fmt.Sprintf("dd if=/dev/urandom of=/tmp/bench-dirty.bin bs=1M count=%d status=none && %s && sync", dirtyMiB, writeCmd)
		} else {
			writeCmd = writeCmd + " && sync"
		}
		if _, err := client.runCommand(ctx, baseEP, baseHeaders, writeCmd); err != nil {
			client.deleteSandboxBestEffort(baseID)
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("rootfs_write_state_%dmib", dirtyMiB),
				SandboxID: baseID,
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
			continue
		}

		// 3. Capture RootfsOnly snapshot (FSEP-0001).
		snapStart := time.Now()
		snapName := fmt.Sprintf("bench-rootfs-%dmib-%d", dirtyMiB, time.Now().UnixNano())
		snapInfo, err := client.lifecycle.CreateSnapshot(ctx, baseID, opensandbox.CreateSnapshotRequest{
			Name: snapName,
			Mode: opensandbox.SnapshotModeRootfsOnly,
		})
		if err != nil {
			client.deleteSandboxBestEffort(baseID)
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("rootfs_snapshot_create_%dmib", dirtyMiB),
				SandboxID: baseID,
				LatencyMs: scaleElapsedMs(snapStart),
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
			continue
		}
		snapID := snapInfo.ID

		snapCtx, snapCancel := context.WithTimeout(ctx, scaleDefaultSnapTimeout)
		_, err = client.waitSnapshotReady(snapCtx, snapID, s.pollInterval)
		snapCancel()
		snapMs := scaleElapsedMs(snapStart)
		if err != nil {
			client.deleteSnapshotBestEffort(snapID)
			client.deleteSandboxBestEffort(baseID)
			rec.recordOp(OperationSample{
				Operation: fmt.Sprintf("rootfs_snapshot_ready_%dmib", dirtyMiB),
				SandboxID: baseID,
				LatencyMs: snapMs,
				Status:    "error",
				ErrorCode: scaleErrorCode(err),
			})
			continue
		}
		rec.addLatency("snapshot_create", snapMs)
		rec.recordOp(OperationSample{
			Operation: fmt.Sprintf("rootfs_snapshot_create_%dmib", dirtyMiB),
			SandboxID: baseID,
			LatencyMs: snapMs,
			Status:    "ok",
		})

		// (a) Cold materialize vs. warm materialized shared_snapfiles create.
		coldOut := s.restoreFromSnapshot(ctx, client, snapID, markerToken, nil, nil, 0)
		s.recordRestoreOutcome(rec, fmt.Sprintf("rootfs_restore_cold_%dmib", dirtyMiB), "cold_materialize_first_ping", coldOut)

		warmOut := s.restoreFromSnapshot(ctx, client, snapID, markerToken, nil, nil, 0)
		s.recordRestoreOutcome(rec, fmt.Sprintf("rootfs_restore_warm_%dmib", dirtyMiB), "warm_materialize_first_ping", warmOut)

		dirtySummaries = append(dirtySummaries, map[string]any{
			"dirty_upper_mib":         dirtyMiB,
			"snapshot_id":             snapID,
			"snapshot_create_ms":      snapMs,
			"cold_first_ping_ms":      coldOut.pingMs,
			"cold_boot_gap_ms":        coldOut.bootGapMs,
			"cold_marker_verified":    coldOut.markerOK,
			"warm_first_ping_ms":      warmOut.pingMs,
			"warm_boot_gap_ms":        warmOut.bootGapMs,
			"warm_marker_verified":    warmOut.markerOK,
		})

		// Run fan-out, cross-node, and OSEP-0028 override matrices against the
		// primary snapshot (first tier in DirtyUpperMiB) so multi-tier runs
		// stay bounded while exercising every restore path.
		if idx == 0 {
			// (b) Concurrent fan-out from one RootfsOnly snapshot.
			for _, concurrency := range fanoutLevels {
				if concurrency <= 0 {
					continue
				}
				outcomes := s.runFanout(ctx, client, snapID, markerToken, concurrency)
				var pingSamples []float64
				var gapSamples []float64
				okCount := 0
				for _, out := range outcomes {
					s.recordRestoreOutcome(rec, fmt.Sprintf("rootfs_fanout_c%d", concurrency), "", out)
					if out.err == nil {
						okCount++
						pingSamples = append(pingSamples, out.pingMs)
						gapSamples = append(gapSamples, out.bootGapMs)
					}
				}
				fanoutSummaries = append(fanoutSummaries, map[string]any{
					"concurrency":      concurrency,
					"succeeded":        okCount,
					"total":            concurrency,
					"t_first_ping":     report.ComputeLatencyDist(pingSamples),
					"guest_boot_gap":   report.ComputeLatencyDist(gapSamples),
				})
			}

			// (c) Cross-node create (via required node-isolation hook).
			if spec.CrossNode {
				isolator := s.nodeIsolator
				if isolator == nil {
					if hooks := BenchHooksFromContext(ctx); hooks.NodeIsolator != nil {
						isolator = hooks.NodeIsolator.IsolateNodeFastlets
					}
				}
				if isolator == nil || beforeNode == "" {
					reason := "cross-node mode requires NodeIsolator hook (in-cluster collector unavailable)"
					if isolator != nil {
						reason = "cross-node mode failed to resolve source sandbox node"
					}
					rec.recordOp(OperationSample{
						Operation: "rootfs_cross_node_isolate",
						SandboxID: baseID,
						Status:    "error",
						ErrorCode: "cross_node_precondition",
					})
					crossNodeSummary = map[string]any{
						"before_node":   beforeNode,
						"cross_node_ok": false,
						"error":         reason,
					}
				} else {
					cleanup, isoErr := isolator(ctx, beforeNode)
					if isoErr != nil {
						rec.recordOp(OperationSample{
							Operation: "rootfs_cross_node_isolate",
							SandboxID: baseID,
							Status:    "error",
							ErrorCode: scaleErrorCode(isoErr),
						})
						crossNodeSummary = map[string]any{
							"before_node":   beforeNode,
							"cross_node_ok": false,
							"error":         isoErr.Error(),
						}
					} else {
						cnOut := s.restoreFromSnapshot(ctx, client, snapID, markerToken, nil, nil, 0)
						if cleanup != nil {
							_ = cleanup(context.Background())
						}
						if cnOut.err == nil && (cnOut.node == "" || cnOut.node == beforeNode) {
							cnOut.err = fmt.Errorf("cross-node invariant violated: before_node=%q after_node=%q", beforeNode, cnOut.node)
						}
						s.recordRestoreOutcome(rec, "rootfs_restore_cross_node", "cross_node_first_ping", cnOut)
						crossNodeSummary = map[string]any{
							"before_node":     beforeNode,
							"after_node":      cnOut.node,
							"cross_node_ok":   cnOut.err == nil && beforeNode != "" && cnOut.node != "" && beforeNode != cnOut.node,
							"first_ping_ms":   cnOut.pingMs,
							"boot_gap_ms":     cnOut.bootGapMs,
							"marker_verified": cnOut.markerOK,
						}
						if cnOut.err != nil {
							crossNodeSummary["error"] = cnOut.err.Error()
						}
					}
				}
			}

			// (d) OSEP-0028 CPU/memory override create.
			baseMemMiB := 0
			if rc.Profile != nil {
				baseMemMiB = rc.Profile.Stack.Template.MemoryMiB
			}
			for _, ov := range spec.ResourceOverrides {
				ovCopy := ov
				limits := opensandbox.ResourceLimits{}
				if ov.VCPUs > 0 {
					limits["cpu"] = strconv.Itoa(ov.VCPUs)
				}
				if ov.MemoryMiB > 0 {
					limits["memory"] = fmt.Sprintf("%dMi", ov.MemoryMiB)
				}
				ovOut := s.restoreFromSnapshot(ctx, client, snapID, markerToken, limits, &ovCopy, baseMemMiB)
				opName := fmt.Sprintf("rootfs_restore_override_%dvcpu_%dmib", ov.VCPUs, ov.MemoryMiB)
				s.recordRestoreOutcome(rec, opName, "override_first_ping", ovOut)
				row := map[string]any{
					"vcpus":               ov.VCPUs,
					"memory_mib":          ov.MemoryMiB,
					"observed_vcpus":      ovOut.observedVCPUs,
					"observed_memory_mib": ovOut.observedMemMiB,
					"first_ping_ms":       ovOut.pingMs,
					"boot_gap_ms":         ovOut.bootGapMs,
					"marker_verified":     ovOut.markerOK,
					"override_verified":   ovOut.overrideOK,
					"ok":                  ovOut.err == nil,
				}
				if ovOut.err != nil {
					row["error"] = ovOut.err.Error()
				}
				overrideSummaries = append(overrideSummaries, row)
			}
		}

		client.deleteSnapshotBestEffort(snapID)
		client.deleteSandboxBestEffort(baseID)
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
	var throughput float64
	if durationSec > 0 && rec.succeededOps > 0 {
		throughput = float64(rec.succeededOps) / durationSec
	}

	extra := map[string]any{
		"dirty_upper_tiers":  dirtySummaries,
		"fanout":             fanoutSummaries,
		"resource_overrides": overrideSummaries,
	}
	if crossNodeSummary != nil {
		extra["cross_node"] = crossNodeSummary
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

func (s *RootfsOnlyRestoreSuite) runFanout(ctx context.Context, client *scaleClient, snapshotID, markerToken string, concurrency int) []rootfsRestoreOutcome {
	outcomes := make([]rootfsRestoreOutcome, concurrency)
	startCh := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startCh
			outcomes[idx] = s.restoreFromSnapshot(ctx, client, snapshotID, markerToken, nil, nil, 0)
		}(i)
	}
	close(startCh)
	wg.Wait()
	return outcomes
}

func (s *RootfsOnlyRestoreSuite) restoreFromSnapshot(
	ctx context.Context,
	client *scaleClient,
	snapshotID, markerToken string,
	resourceLimits opensandbox.ResourceLimits,
	override *profile.ResourceOverrideSpec,
	baseMemMiB int,
) rootfsRestoreOutcome {
	var out rootfsRestoreOutcome
	timeoutSec := scaleDefaultTimeoutSec
	req := opensandbox.CreateSandboxRequest{
		SnapshotID:     snapshotID,
		Timeout:        &timeoutSec,
		ResourceLimits: resourceLimits,
	}

	t0 := time.Now()
	info, err := client.lifecycle.CreateSandbox(ctx, req)
	out.ackMs = scaleElapsedMs(t0)
	if err != nil {
		out.err = err
		return out
	}
	out.sandboxID = info.ID
	out.ackState = string(info.Status.State)
	out.node = scaleExtractNode(info)
	defer client.deleteSandboxBestEffort(out.sandboxID)

	readyCtx, cancel := context.WithTimeout(ctx, scaleDefaultReadyTimeout)
	epURL, epHeaders, err := client.waitSandboxRunningAndPing(readyCtx, out.sandboxID, s.pollInterval)
	cancel()
	out.pingMs = scaleElapsedMs(t0)
	if out.pingMs >= out.ackMs {
		out.bootGapMs = out.pingMs - out.ackMs
	}
	if err != nil {
		out.err = err
		return out
	}

	if out.node == "" {
		if refreshed, gErr := client.lifecycle.GetSandbox(ctx, out.sandboxID); gErr == nil {
			out.node = scaleExtractNode(refreshed)
		}
	}
	if out.node == "" {
		if hooks := BenchHooksFromContext(ctx); hooks.NodeResolver != nil {
			if resolved, rErr := hooks.NodeResolver.SandboxNode(ctx, out.sandboxID); rErr == nil && resolved != "" {
				out.node = resolved
			}
		}
	}

	// Verify disk marker persistence and fresh execd responsiveness.
	stdout, err := client.runCommand(ctx, epURL, epHeaders, "cat /tmp/bench-rootfs-marker")
	out.execMs = scaleElapsedMs(t0)
	if err != nil {
		out.err = fmt.Errorf("verify disk marker exec: %w", err)
		return out
	}
	if strings.TrimSpace(stdout) != markerToken {
		out.err = fmt.Errorf("disk marker mismatch: got %q, want %q", strings.TrimSpace(stdout), markerToken)
		return out
	}
	out.markerOK = true

	// Verify OSEP-0028 guest CPU/memory overrides when requested.
	if override != nil && (override.VCPUs > 0 || override.MemoryMiB > 0) {
		probeCmd := `printf 'NPROC=%s MEM_KB=%s\n' "$(nproc)" "$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"`
		probeOut, probeErr := client.runCommand(ctx, epURL, epHeaders, probeCmd)
		if probeErr != nil {
			out.err = fmt.Errorf("verify OSEP-0028 resource override exec: %w", probeErr)
			return out
		}
		vcpus, memKiB, parseErr := parseGuestResourceProbe(probeOut)
		if parseErr != nil {
			out.err = fmt.Errorf("parse OSEP-0028 resource override probe (%q): %w", strings.TrimSpace(probeOut), parseErr)
			return out
		}
		out.observedVCPUs = vcpus
		out.observedMemMiB = float64(memKiB) / 1024.0
		if override.VCPUs > 0 && vcpus != override.VCPUs {
			out.err = fmt.Errorf("OSEP-0028 vCPU override mismatch: guest nproc=%d, want %d", vcpus, override.VCPUs)
			return out
		}
		if override.MemoryMiB > 0 {
			minExpectedMiB := float64(override.MemoryMiB) * 0.75
			if out.observedMemMiB < minExpectedMiB {
				out.err = fmt.Errorf("OSEP-0028 memory override mismatch: guest MemTotal=%.1fMiB < 75%% of requested %dMiB", out.observedMemMiB, override.MemoryMiB)
				return out
			}
			if baseMemMiB > 0 && override.MemoryMiB > baseMemMiB && out.observedMemMiB <= float64(baseMemMiB) {
				out.err = fmt.Errorf("OSEP-0028 memory scale-up not observed: guest MemTotal=%.1fMiB <= base %dMiB (requested %dMiB)", out.observedMemMiB, baseMemMiB, override.MemoryMiB)
				return out
			}
		}
		out.overrideOK = true
	}
	return out
}

func parseGuestResourceProbe(out string) (int, int64, error) {
	var (
		vcpus   int
		memKiB  int64
		gotCPU  bool
		gotMem  bool
	)
	for _, tok := range strings.Fields(out) {
		switch {
		case strings.HasPrefix(tok, "NPROC="):
			v, err := strconv.Atoi(strings.TrimPrefix(tok, "NPROC="))
			if err != nil || v <= 0 {
				return 0, 0, fmt.Errorf("invalid NPROC token %q", tok)
			}
			vcpus = v
			gotCPU = true
		case strings.HasPrefix(tok, "MEM_KB="):
			v, err := strconv.ParseInt(strings.TrimPrefix(tok, "MEM_KB="), 10, 64)
			if err != nil || v <= 0 {
				return 0, 0, fmt.Errorf("invalid MEM_KB token %q", tok)
			}
			memKiB = v
			gotMem = true
		}
	}
	if !gotCPU || !gotMem {
		return 0, 0, fmt.Errorf("missing NPROC or MEM_KB in probe output")
	}
	return vcpus, memKiB, nil
}

func (s *RootfsOnlyRestoreSuite) recordRestoreOutcome(rec *scaleRecorder, opName, extraLatencyKey string, out rootfsRestoreOutcome) {
	if out.err != nil {
		rec.recordOp(OperationSample{
			Operation: opName,
			SandboxID: out.sandboxID,
			LatencyMs: out.pingMs,
			Status:    "error",
			AckState:  out.ackState,
			ErrorCode: scaleErrorCode(out.err),
		})
		return
	}
	rec.addLatency("t_api_ack", out.ackMs)
	rec.addLatency("t_first_ping", out.pingMs)
	rec.addLatency("t_first_exec", out.execMs)
	rec.addLatency("guest_boot_to_first_ping_gap", out.bootGapMs)
	if extraLatencyKey != "" {
		rec.addLatency(extraLatencyKey, out.pingMs)
	}
	rec.recordOp(OperationSample{
		Operation: opName,
		SandboxID: out.sandboxID,
		LatencyMs: out.pingMs,
		Status:    "ok",
		AckState:  out.ackState,
	})
}
