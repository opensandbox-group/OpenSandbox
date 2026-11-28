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
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	rootfsSourceProbeInterval = 20 * time.Millisecond
	rootfsSourceProbeTimeout  = 5 * time.Second
)

// CacheEvictorFunc evicts OverlayBD and local OCI caches on a target node before
// measuring a cold-cache restore.
type CacheEvictorFunc func(ctx context.Context, nodeName string) error

// RootfsOnlyRestoreOption configures optional hooks for RootfsOnlyRestoreSuite.
type RootfsOnlyRestoreOption func(*RootfsOnlyRestoreSuite)

// WithRootfsNodeIsolator injects a node-isolation hook used when CrossNode is
// enabled in the profile to force placement onto a different node.
func WithRootfsNodeIsolator(fn NodeIsolatorFunc) RootfsOnlyRestoreOption {
	return func(s *RootfsOnlyRestoreSuite) {
		s.nodeIsolator = fn
	}
}

// WithRootfsCacheEvictor injects a cache-eviction hook used before the cold
// restore measurement so cold restores measure real uncached OverlayBD block fetch
// rather than hitting freshly committed local cache.
func WithRootfsCacheEvictor(fn CacheEvictorFunc) RootfsOnlyRestoreOption {
	return func(s *RootfsOnlyRestoreSuite) {
		s.cacheEvictor = fn
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
// cold-boot sandbox creation/forking across dirty-upper tiers, source-sandbox
// capture stall, warm vs. cache-evicted cold restores, concurrent fan-out,
// cross-node portability, and OSEP-0028 resource/entrypoint overrides (OSEP-0034 §3, §4.1).
type RootfsOnlyRestoreSuite struct {
	nodeIsolator NodeIsolatorFunc
	cacheEvictor CacheEvictorFunc
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
	return "RootfsOnly snapshot create/fork across source stall, warm/cold cache, fan-out, cross-node, and OSEP-0028 overrides"
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
	entrypointOK   bool
	observedVCPUs  int
	observedMemMiB float64
	err            error
}

type sourceStallMeasurement struct {
	maxStallMs     float64
	baselinePingMs float64
	probes         int
	failedProbes   int
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
	evictForCold := spec.EvictCacheForCold == nil || *spec.EvictCacheForCold

	client := newScaleClient(rc)
	rec := newScaleRecorder()

	dirtySummaries := make([]map[string]any, 0, len(dirtyLadder))
	fanoutSummaries := make([]map[string]any, 0, len(fanoutLevels))
	var crossNodeSummary map[string]any
	overrideSummaries := make([]map[string]any, 0, len(spec.ResourceOverrides))
	prevTierEvicted := false
	prevTierCleaned := true

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
				Operation:  fmt.Sprintf("rootfs_base_create_%dmib", dirtyMiB),
				Status:     "error",
				ErrorCode:  scaleErrorCode(err),
				StderrHead: scaleErrorStderrHead(err, 200),
			})
			continue
		}
		baseID := baseInfo.ID
		beforeNode := scaleExtractNode(baseInfo)
		baseDeleted := false
		baseCleaned := true
		deleteBase := func() {
			if !baseDeleted && baseID != "" {
				baseDeleted = true
				client.deleteSandboxBestEffort(baseID)
				baseCleaned = waitSandboxDeleted(ctx, client, baseID, s.pollInterval, 15*time.Second)
			}
		}

		baseEP, baseHeaders, err := client.waitSandboxRunningAndPing(ctx, baseID, s.pollInterval)
		if err != nil {
			deleteBase()
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_base_ready_%dmib", dirtyMiB),
				SandboxID:  baseID,
				Status:     "error",
				ErrorCode:  scaleErrorCode(err),
				StderrHead: scaleErrorStderrHead(err, 200),
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

		// 2. Write disk marker and optional dirty upper data on the persistent rootfs
		// (/var/tmp, not /tmp which is mounted as tmpfs and wiped on RootfsOnly cold boot).
		writeCmd := fmt.Sprintf("mkdir -p /var/tmp && printf '%%s' %q > /var/tmp/bench-rootfs-marker", markerToken)
		if dirtyMiB > 0 {
			writeCmd = fmt.Sprintf("mkdir -p /var/tmp && dd if=/dev/urandom of=/var/tmp/bench-dirty.bin bs=1M count=%d status=none && printf '%%s' %q > /var/tmp/bench-rootfs-marker && sync", dirtyMiB, markerToken)
		} else {
			writeCmd = writeCmd + " && sync"
		}
		if _, err := client.runCommand(ctx, baseEP, baseHeaders, writeCmd); err != nil {
			deleteBase()
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_write_state_%dmib", dirtyMiB),
				SandboxID:  baseID,
				Status:     "error",
				ErrorCode:  scaleErrorCode(err),
				StderrHead: scaleErrorStderrHead(err, 200),
			})
			continue
		}

		// 3. Capture RootfsOnly snapshot (FSEP-0001) while probing the live source
		// sandbox to quantify the FIFREEZE->FITHAW pause interruption window.
		waitRunCtx, waitRunCancel := context.WithTimeout(ctx, 15*time.Second)
		_, _ = client.waitSandboxState(waitRunCtx, baseID, opensandbox.StateRunning, 100*time.Millisecond)
		waitRunCancel()

		stopStallProbe := startSourceStallProbe(ctx, client, baseEP, baseHeaders, rootfsSourceProbeInterval)

		snapStart := time.Now()
		snapName := fmt.Sprintf("bench-rootfs-%dmib-%d", dirtyMiB, time.Now().UnixNano())
		var snapInfo *opensandbox.SnapshotInfo
		for attempt := 0; attempt < 5; attempt++ {
			snapInfo, err = client.lifecycle.CreateSnapshot(ctx, baseID, opensandbox.CreateSnapshotRequest{
				Name: snapName,
				Mode: opensandbox.SnapshotModeRootfsOnly,
			})
			if err == nil || !strings.Contains(err.Error(), "409") {
				break
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			if ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			_ = stopStallProbe()
			deleteBase()
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_snapshot_create_%dmib", dirtyMiB),
				SandboxID:  baseID,
				LatencyMs:  scaleElapsedMs(snapStart),
				Status:     "error",
				ErrorCode:  scaleErrorCode(err),
				StderrHead: scaleErrorStderrHead(err, 200),
			})
			continue
		}
		snapID := snapInfo.ID

		snapCtx, snapCancel := context.WithTimeout(ctx, scaleDefaultSnapTimeout)
		_, err = client.waitSnapshotReady(snapCtx, snapID, s.pollInterval)
		snapCancel()
		snapMs := scaleElapsedMs(snapStart)
		stallMeas := stopStallProbe()
		if err != nil {
			client.deleteSnapshotBestEffort(snapID)
			deleteBase()
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_snapshot_ready_%dmib", dirtyMiB),
				SandboxID:  baseID,
				LatencyMs:  snapMs,
				Status:     "error",
				ErrorCode:  scaleErrorCode(err),
				StderrHead: scaleErrorStderrHead(err, 200),
			})
			continue
		}
		rec.addLatency("snapshot_create", snapMs)
		if stallMeas.probes > 0 {
			rec.addLatency("source_pause_stall", stallMeas.maxStallMs)
		}
		rec.recordOp(OperationSample{
			Operation: fmt.Sprintf("rootfs_snapshot_create_%dmib", dirtyMiB),
			SandboxID: baseID,
			LatencyMs: snapMs,
			Status:    "ok",
		})

		// Verify the live source sandbox resumed cleanly after RootfsOnly capture.
		postCaptureOK := false
		postOut, postErr := client.runCommand(ctx, baseEP, baseHeaders, "cat /var/tmp/bench-rootfs-marker")
		if postErr == nil && strings.TrimSpace(postOut) == markerToken {
			postCaptureOK = true
		} else {
			if postErr == nil {
				postErr = fmt.Errorf("post-capture source marker mismatch: got %q, want %q", strings.TrimSpace(postOut), markerToken)
			}
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_source_post_capture_%dmib", dirtyMiB),
				SandboxID:  baseID,
				Status:     "error",
				ErrorCode:  scaleErrorCode(postErr),
				StderrHead: scaleErrorStderrHead(postErr, 200),
			})
		}

		// Delete the source sandbox before any cache-eviction step: CacheEvictor
		// restarts overlaybd-ublkd on the target node, which would sever live ublk
		// devices of any running sandbox on that node.
		deleteBase()

		// (a) Warm local-cache restore first (reusing freshly committed OverlayBD
		// upper/blob cache), followed by cold restore (explicitly evicting local
		// OverlayBD/OCI caches across all runtime nodes when CacheEvictor is wired
		// and enabled so multi-node scheduling never routes Cold to a warm node).
		// When WarmupRuns > 0 on the first tier or after a prior all-node eviction,
		// run WarmupRuns concurrent unmeasured cold-boot warmups (capped at 4) so
		// multi-node OverlayBD block caches hold the cold-boot blocks before timing
		// Warm, then wait for warmup sandboxes to finish tearing down.
		// Note: after fast-sandbox#258, RootfsOnly carries manifest metadata on the
		// rootfs OCI image itself without attaching a _snapfiles ublk device.
		warmupCleaned := true
		warmupOK := true
		if rc.WarmupRuns > 0 && (idx == 0 || prevTierEvicted) {
			warmups := rc.WarmupRuns
			if warmups > 4 {
				warmups = 4
			}
			warmOuts := s.runFanout(ctx, client, snapID, markerToken, warmups)
			for _, wOut := range warmOuts {
				if wOut.err != nil {
					warmupOK = false
					rec.recordOp(OperationSample{
						Operation:  fmt.Sprintf("rootfs_warmup_%dmib", dirtyMiB),
						SandboxID:  wOut.sandboxID,
						LatencyMs:  wOut.pingMs,
						Status:     "error",
						ErrorCode:  scaleErrorCode(wOut.err),
						StderrHead: scaleErrorStderrHead(wOut.err, 200),
					})
				}
				if wOut.sandboxID != "" && !waitSandboxDeleted(ctx, client, wOut.sandboxID, s.pollInterval, 15*time.Second) {
					warmupCleaned = false
				}
			}
		}
		var warmOut rootfsRestoreOutcome
		warmCleaned := true
		if !warmupCleaned {
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_warmup_teardown_%dmib", dirtyMiB),
				Status:     "error",
				ErrorCode:  "teardown_timeout",
				StderrHead: "warmup sandbox teardown timed out before measured warm restore",
			})
		} else if !warmupOK {
			rec.recordOp(OperationSample{
				Operation:  fmt.Sprintf("rootfs_restore_warm_%dmib", dirtyMiB),
				Status:     "error",
				ErrorCode:  "warmup_failed",
				StderrHead: "skipped measured warm restore because warmup restore failed",
			})
		} else {
			warmOut = s.restoreFromSnapshot(ctx, client, snapID, markerToken, nil, nil, 0)
			s.recordRestoreOutcome(rec, fmt.Sprintf("rootfs_restore_warm_%dmib", dirtyMiB), "warm_materialize_first_ping", warmOut)
			if warmOut.sandboxID != "" {
				warmCleaned = waitSandboxDeleted(ctx, client, warmOut.sandboxID, s.pollInterval, 15*time.Second)
			}
		}

		// Run concurrent fan-out on the primary snapshot BEFORE all-node cache
		// eviction so c=1 / c=8 / c=16 measure consistent warm-cache fan-out
		// scaling without c=1 landing on a half-evicted cold node, then wait
		// for all fan-out sandboxes to finish tearing down before CacheEvictor.
		fanoutCleaned := true
		if idx == 0 {
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
					if out.sandboxID != "" && !waitSandboxDeleted(ctx, client, out.sandboxID, s.pollInterval, 15*time.Second) {
						fanoutCleaned = false
					}
				}
				fanoutSummaries = append(fanoutSummaries, map[string]any{
					"concurrency":    concurrency,
					"succeeded":      okCount,
					"total":          concurrency,
					"t_first_ping":   report.ComputeLatencyDist(pingSamples),
					"guest_boot_gap": report.ComputeLatencyDist(gapSamples),
				})
			}
		}

		coldCacheEvicted := false
		if evictForCold {
			if !prevTierCleaned || !baseCleaned || !warmupCleaned || !warmCleaned || !fanoutCleaned {
				rec.recordOp(OperationSample{
					Operation:  fmt.Sprintf("rootfs_evict_cache_skipped_%dmib", dirtyMiB),
					Status:     "error",
					ErrorCode:  "teardown_timeout",
					StderrHead: fmt.Sprintf("skipped cache eviction because prior sandbox teardown timed out (prev_tier=%t base=%t warmup=%t warm=%t fanout=%t)", prevTierCleaned, baseCleaned, warmupCleaned, warmCleaned, fanoutCleaned),
				})
			} else {
				evictor := s.cacheEvictor
				if evictor == nil {
					if hooks := BenchHooksFromContext(ctx); hooks.CacheEvictor != nil {
						evictor = hooks.CacheEvictor.EvictOverlayBDCaches
					}
				}
				if evictor != nil {
					// Pass empty nodeName to evict all runtime nodes so multi-node
					// FastPath placement cannot route the Cold restore onto another
					// node with a warm OverlayBD cache.
					if evErr := evictor(ctx, ""); evErr != nil {
						rec.recordOp(OperationSample{
							Operation:  fmt.Sprintf("rootfs_evict_cache_%dmib", dirtyMiB),
							Status:     "error",
							ErrorCode:  scaleErrorCode(evErr),
							StderrHead: scaleErrorStderrHead(evErr, 200),
						})
					} else {
						coldCacheEvicted = true
					}
				}
			}
		}
		prevTierEvicted = coldCacheEvicted

		coldOut := s.restoreFromSnapshot(ctx, client, snapID, markerToken, nil, nil, 0)
		s.recordRestoreOutcome(rec, fmt.Sprintf("rootfs_restore_cold_%dmib", dirtyMiB), "cold_materialize_first_ping", coldOut)
		tierCleaned := true
		if coldOut.sandboxID != "" && !waitSandboxDeleted(ctx, client, coldOut.sandboxID, s.pollInterval, 15*time.Second) {
			tierCleaned = false
		}

		dirtyRow := map[string]any{
			"dirty_upper_mib":        dirtyMiB,
			"snapshot_id":            snapID,
			"source_node":            beforeNode,
			"snapshot_create_ms":     snapMs,
			"source_pause_stall_ms":  stallMeas.maxStallMs,
			"post_capture_source_ok": postCaptureOK,
			"warm_node":              warmOut.node,
			"warm_first_ping_ms":     warmOut.pingMs,
			"warm_boot_gap_ms":       warmOut.bootGapMs,
			"warm_marker_verified":   warmOut.markerOK,
			"cold_cache_evicted":     coldCacheEvicted,
			"cold_node":              coldOut.node,
			"cold_first_ping_ms":     coldOut.pingMs,
			"cold_boot_gap_ms":       coldOut.bootGapMs,
			"cold_marker_verified":   coldOut.markerOK,
		}
		dirtySummaries = append(dirtySummaries, dirtyRow)

		// Run cross-node and OSEP-0028 override matrices against the primary
		// snapshot (first tier in DirtyUpperMiB) so multi-tier runs stay bounded
		// while exercising every restore path.
		if idx == 0 {
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
						Operation:  "rootfs_cross_node_isolate",
						SandboxID:  baseID,
						Status:     "error",
						ErrorCode:  "cross_node_precondition",
						StderrHead: reason,
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
							Operation:  "rootfs_cross_node_isolate",
							SandboxID:  baseID,
							Status:     "error",
							ErrorCode:  scaleErrorCode(isoErr),
							StderrHead: scaleErrorStderrHead(isoErr, 200),
						})
						crossNodeSummary = map[string]any{
							"before_node":   beforeNode,
							"cross_node_ok": false,
							"error":         isoErr.Error(),
						}
					} else {
						cnOut := s.restoreFromSnapshot(ctx, client, snapID, markerToken, nil, nil, 0)
						if cnOut.sandboxID != "" && !waitSandboxDeleted(ctx, client, cnOut.sandboxID, s.pollInterval, 15*time.Second) {
							tierCleaned = false
						}
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

			// (d) OSEP-0028 CPU/memory/entrypoint override create.
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
				if len(ov.Entrypoint) > 0 {
					opName += "_ep"
				}
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
				if len(ov.Entrypoint) > 0 {
					row["entrypoint"] = ov.Entrypoint
					row["entrypoint_verified"] = ovOut.entrypointOK
				}
				if ovOut.err != nil {
					row["error"] = ovOut.err.Error()
				}
				overrideSummaries = append(overrideSummaries, row)
				if ovOut.sandboxID != "" && !waitSandboxDeleted(ctx, client, ovOut.sandboxID, s.pollInterval, 15*time.Second) {
					tierCleaned = false
				}
			}
		}

		client.deleteSnapshotBestEffort(snapID)
		deleteBase()
		prevTierCleaned = tierCleaned && baseCleaned && warmupCleaned && warmCleaned && fanoutCleaned
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
	if len(rec.errorsByOp) > 0 {
		res.ErrorsByOperation = rec.errorsByOp
	}
	if len(rec.errorSamples) > 0 {
		res.ErrorSamples = rec.errorSamples
	}
	return res, rec.samples, nil
}

// startSourceStallProbe polls the live source sandbox's /ping endpoint in the
// background while RootfsOnly capture runs and returns a stop function that
// reports the maximum observed pause/unavailability stall window.
func startSourceStallProbe(
	ctx context.Context,
	client *scaleClient,
	endpointURL string,
	headers map[string]string,
	interval time.Duration,
) func() sourceStallMeasurement {
	if interval <= 0 {
		interval = rootfsSourceProbeInterval
	}
	pingURL := strings.TrimRight(endpointURL, "/") + "/ping"

	// Capture baseline ping before starting the snapshot (with brief retries so a
	// single transient blip does not leave baselineMs at 0).
	var (
		baselineMs  float64
		hasBaseline bool
	)
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			break
		}
		baseCtx, baseCancel := context.WithTimeout(ctx, rootfsSourceProbeTimeout)
		status, _, elapsedMs, err := client.probeURL(baseCtx, pingURL, headers)
		baseCancel()
		if err == nil && status == http.StatusOK {
			baselineMs = elapsedMs
			hasBaseline = true
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	probeCtx, cancel := context.WithCancel(ctx)
	done := make(chan sourceStallMeasurement, 1)

	go func() {
		meas := sourceStallMeasurement{baselinePingMs: baselineMs}
		var (
			inStall    bool
			stallStart time.Time
			refBaseMs  = baselineMs
			haveRef    = hasBaseline
		)
		recordStall := func(ms float64) {
			if ms > meas.maxStallMs {
				meas.maxStallMs = ms
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-probeCtx.Done():
				if inStall {
					recordStall(scaleElapsedMs(stallStart))
				}
				done <- meas
				return
			case <-ticker.C:
				pCtx, pCancel := context.WithTimeout(probeCtx, rootfsSourceProbeTimeout)
				reqStart := time.Now()
				status, _, elapsedMs, err := client.probeURL(pCtx, pingURL, headers)
				pCancel()
				if probeCtx.Err() != nil {
					if inStall {
						recordStall(scaleElapsedMs(stallStart))
					} else {
						// Account for an in-flight probe only when it had already been
						// blocked past the noise floor (a real VM freeze), not a routine
						// sub-threshold /ping interrupted by normal probe shutdown.
						inFlightMs := scaleElapsedMs(reqStart)
						noiseFloorMs := 3.0 * refBaseMs
						if noiseFloorMs < 20.0 {
							noiseFloorMs = 20.0
						}
						if inFlightMs >= noiseFloorMs {
							meas.probes++
							if err != nil || status != http.StatusOK {
								meas.failedProbes++
							}
							if delta := inFlightMs - refBaseMs; delta > 0 {
								recordStall(delta)
							} else {
								recordStall(inFlightMs)
							}
						}
					}
					done <- meas
					return
				}
				meas.probes++
				if err == nil && status == http.StatusOK {
					if inStall {
						recordStall(scaleElapsedMs(stallStart))
						inStall = false
					}
					// If initial baseline probes all failed, establish a reference floor
					// from the minimum observed healthy RTT before attributing stalls.
					if !haveRef {
						refBaseMs = elapsedMs
						haveRef = true
						meas.baselinePingMs = refBaseMs
						continue
					}
					if elapsedMs < refBaseMs {
						refBaseMs = elapsedMs
						meas.baselinePingMs = refBaseMs
					}
					// Gate elevated-latency stall attribution on a noise floor
					// max(3x baseline, 20ms) so routine I/O jitter is not reported as a pause.
					noiseFloorMs := 3.0 * refBaseMs
					if noiseFloorMs < 20.0 {
						noiseFloorMs = 20.0
					}
					if elapsedMs >= noiseFloorMs {
						if delta := elapsedMs - refBaseMs; delta > 0 {
							recordStall(delta)
						}
					}
				} else {
					meas.failedProbes++
					if !inStall {
						inStall = true
						stallStart = reqStart
					}
				}
			}
		}
	}()

	return func() sourceStallMeasurement {
		cancel()
		return <-done
	}
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
	if override != nil && len(override.Entrypoint) > 0 {
		req.Entrypoint = append([]string(nil), override.Entrypoint...)
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

	// Verify disk marker persistence and fresh execd responsiveness BEFORE any
	// auxiliary K8s node metadata lookups so client-go calls never skew t_first_exec.
	stdout, err := client.runCommand(ctx, epURL, epHeaders, "cat /var/tmp/bench-rootfs-marker")
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

	// Verify OSEP-0028 guest CPU/memory/entrypoint overrides when requested.
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
			maxExpectedMiB := float64(override.MemoryMiB) * 1.10
			if out.observedMemMiB < minExpectedMiB {
				out.err = fmt.Errorf("OSEP-0028 memory override mismatch: guest MemTotal=%.1fMiB < 75%% of requested %dMiB", out.observedMemMiB, override.MemoryMiB)
				return out
			}
			if out.observedMemMiB > maxExpectedMiB {
				out.err = fmt.Errorf("OSEP-0028 memory override mismatch: guest MemTotal=%.1fMiB > 110%% of requested %dMiB", out.observedMemMiB, override.MemoryMiB)
				return out
			}
			if baseMemMiB > 0 && override.MemoryMiB > baseMemMiB && out.observedMemMiB <= float64(baseMemMiB) {
				out.err = fmt.Errorf("OSEP-0028 memory scale-up not observed: guest MemTotal=%.1fMiB <= base %dMiB (requested %dMiB)", out.observedMemMiB, baseMemMiB, override.MemoryMiB)
				return out
			}
			if baseMemMiB > 0 && override.MemoryMiB < baseMemMiB && out.observedMemMiB >= float64(baseMemMiB)*0.95 {
				out.err = fmt.Errorf("OSEP-0028 memory scale-down not observed: guest MemTotal=%.1fMiB >= 95%% of base %dMiB (requested %dMiB)", out.observedMemMiB, baseMemMiB, override.MemoryMiB)
				return out
			}
		}
		out.overrideOK = true
	}

	if override != nil && len(override.Entrypoint) > 0 {
		psOut, psErr := client.runCommand(ctx, epURL, epHeaders, "ps -eo args 2>/dev/null || for f in /proc/[0-9]*/cmdline; do tr '\\0' ' ' < \"$f\" 2>/dev/null; echo; done")
		if psErr != nil {
			out.err = fmt.Errorf("verify OSEP-0028 entrypoint override exec: %w", psErr)
			return out
		}
		if !matchGuestEntrypoint(psOut, override.Entrypoint) {
			expected := strings.TrimSpace(strings.Join(override.Entrypoint, " "))
			out.err = fmt.Errorf("OSEP-0028 entrypoint override not observed in guest process list: want %q", expected)
			return out
		}
		out.entrypointOK = true
		if override.VCPUs == 0 && override.MemoryMiB == 0 {
			out.overrideOK = true
		}
	}
	return out
}

// matchGuestEntrypoint checks whether any guest process line in ps output matches
// the requested entrypoint executable and arguments, skipping the probe command itself
// and avoiding substring false positives (such as "sh" matching "bash").
func matchGuestEntrypoint(psOut string, entrypoint []string) bool {
	if len(entrypoint) == 0 {
		return true
	}
	wantBin := strings.TrimSpace(entrypoint[0])
	if wantBin == "" {
		return true
	}
	wantBinBase := path.Base(wantBin)
	wantArgs := strings.TrimSpace(strings.Join(entrypoint[1:], " "))

	for _, rawLine := range strings.Split(psOut, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.EqualFold(line, "COMMAND") || strings.EqualFold(line, "ARGS") {
			continue
		}
		// Skip the shell/ps probe process launched to inspect guest processes.
		if strings.Contains(line, "ps -eo args") || strings.Contains(line, "/proc/*/cmdline") || strings.Contains(line, "/proc/[0-9]*/cmdline") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		procBin := strings.TrimLeft(fields[0], "-")
		if procBin != wantBin && path.Base(procBin) != wantBinBase {
			continue
		}
		if wantArgs == "" {
			return true
		}
		procArgs := strings.Join(fields[1:], " ")
		if procArgs == wantArgs || strings.HasPrefix(procArgs, wantArgs+" ") {
			return true
		}
	}
	return false
}

func parseGuestResourceProbe(out string) (int, int64, error) {
	var (
		vcpus  int
		memKiB int64
		gotCPU bool
		gotMem bool
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

// waitSandboxDeleted polls GetSandbox until the sandbox disappears (404) or
// enters Terminated, preventing async ublk/rootfs teardown from colliding with
// subsequent warm restores or CacheEvictor overlaybd-ublkd restarts. It returns
// true only when teardown was observed before timeout (plus a short settle pause
// after 404 to allow async runtime teardown to drain).
func waitSandboxDeleted(ctx context.Context, client *scaleClient, sandboxID string, pollInterval, timeout time.Duration) bool {
	if sandboxID == "" {
		return true
	}
	if pollInterval <= 0 {
		pollInterval = 50 * time.Millisecond
	}
	if pollInterval < 25*time.Millisecond {
		pollInterval = 25 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	settle := pollInterval * 2
	if settle > 200*time.Millisecond {
		settle = 200 * time.Millisecond
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		info, err := client.lifecycle.GetSandbox(waitCtx, sandboxID)
		if err != nil {
			if scaleIsHTTPStatus(err, http.StatusNotFound) {
				select {
				case <-waitCtx.Done():
					return false
				case <-time.After(settle):
					return true
				}
			}
		} else if info != nil && info.Status.State == opensandbox.StateTerminated {
			return true
		}
		select {
		case <-waitCtx.Done():
			return false
		case <-time.After(pollInterval):
		}
	}
}

func (s *RootfsOnlyRestoreSuite) recordRestoreOutcome(rec *scaleRecorder, opName, extraLatencyKey string, out rootfsRestoreOutcome) {
	if out.err != nil {
		rec.recordOp(OperationSample{
			Operation:  opName,
			SandboxID:  out.sandboxID,
			LatencyMs:  out.pingMs,
			Status:     "error",
			AckState:   out.ackState,
			ErrorCode:  scaleErrorCode(out.err),
			StderrHead: scaleErrorStderrHead(out.err, 200),
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
