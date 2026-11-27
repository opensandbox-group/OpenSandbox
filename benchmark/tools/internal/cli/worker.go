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

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/collector"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/suite"
)

type workerRunOptions struct {
	ProfilePath       string
	SuitesFlag        string
	OutputDir         string
	ServerDomain      string
	APIKey            string
	APIKeyFile        string
	RunID             string
	Cycles            int
	LeakAuditGrace    time.Duration
	LeakAuditGraceSet bool
	OpenSandboxCommit string
	OpenSandboxTag    string
	FastSandboxCommit string
	FastSandboxTag    string
	BenchGitCommit    string
}

func defaultInClusterCollectorClients(log *slog.Logger) (*collector.Clients, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return collector.NewClientsFromConfig(restCfg, log)
}

func (a *App) runWorker(ctx context.Context, opts workerRunOptions) error {
	cycles := opts.Cycles
	if cycles == 0 {
		cycles = 1
	}
	if cycles < 1 {
		return fmt.Errorf("--cycles must be >= 1, got %d", cycles)
	}

	prof, err := profile.Load(opts.ProfilePath)
	if err != nil {
		return err
	}

	gracePeriod := opts.LeakAuditGrace
	if !opts.LeakAuditGraceSet {
		if prof.Environment.LeakAuditGraceSeconds != nil {
			gracePeriod = time.Duration(*prof.Environment.LeakAuditGraceSeconds) * time.Second
		} else if gracePeriod == 0 {
			gracePeriod = collector.DefaultLeakAuditGracePeriod
		}
	}
	if gracePeriod < 0 {
		return fmt.Errorf("--leak-audit-grace must be >= 0, got %s", gracePeriod)
	}

	selected, err := a.Suites.Resolve(splitCSV(opts.SuitesFlag), prof.Runtime)
	if err != nil {
		return err
	}

	resolvedAPIKey := opts.APIKey
	if opts.APIKeyFile != "" {
		rawKey, err := os.ReadFile(opts.APIKeyFile)
		if err != nil {
			return fmt.Errorf("read api-key-file %q: %w", opts.APIKeyFile, err)
		}
		resolvedAPIKey = strings.TrimSpace(string(rawKey))
	}

	resolvedServer := opts.ServerDomain
	if resolvedServer == "" {
		resolvedServer = defaultServerDomain(prof)
	}

	runID := opts.RunID
	if strings.TrimSpace(runID) == "" {
		runID = fmt.Sprintf("bench-%s", time.Now().UTC().Format("20060102-150405"))
	}

	var (
		clients     *collector.Clients
		collCfg     collector.Config
		clusterOps  *collector.ClusterOps
		promScraper *collector.PrometheusScraper
		obdProbe    *collector.OverlayBDProbe
		leakAuditor *collector.LeakAuditor
		hooks       suite.BenchHooks
	)

	baseEnv := report.EnvironmentFingerprint{
		PoolMode:        prof.Stack.FastSandbox.Pool.Mode,
		ReadinessPollMs: 10,
	}

	if a.NewCollectorClients != nil {
		c, cErr := a.NewCollectorClients()
		if cErr != nil {
			a.Logger.Info("in-cluster kubernetes client unavailable; running in SDK-only mode", "reason", cErr)
		} else if c != nil {
			clients = c
			collCfg = collector.ConfigFromProfile(prof)
			clusterOps = collector.NewClusterOps(collCfg, clients)
			promScraper = collector.NewPrometheusScraper(collCfg, clients)
			obdProbe = collector.NewOverlayBDProbe(collCfg, clients)
			leakAuditor = collector.NewLeakAuditor(collCfg, clients)

			populateWorkerEnvironment(ctx, clients, &baseEnv)
			hooks = buildClusterBenchHooks(ctx, collCfg, clients, clusterOps, promScraper)
		}
	}

	sdkCfg := opensandbox.ConnectionConfig{
		Domain:   resolvedServer,
		Protocol: "http",
		APIKey:   resolvedAPIKey,
	}

	if prof.Runtime == profile.RuntimeFastSandboxOverlayBD {
		tplID, tplErr := ensureWorkerTemplate(ctx, prof, sdkCfg, a.Logger, 2*time.Second)
		if tplErr != nil {
			return fmt.Errorf("ensure overlaybd template in server catalog: %w", tplErr)
		}
		if tplID != "" {
			hooks.TemplateID = tplID
			if prof.Extensions == nil {
				prof.Extensions = make(map[string]any)
			}
			prof.Extensions["templateId"] = tplID
			a.Logger.Info("using server catalog template for benchmark suites", "templateId", tplID)
		}
	}

	runCycle := func(cycleRunID, cycleOutDir string) (*report.Report, error) {
		rep := report.New(report.Provenance{
			RunID:             cycleRunID,
			ProfileName:       prof.Metadata.Name,
			Runtime:           prof.Runtime,
			OpenSandboxTag:    opts.OpenSandboxTag,
			OpenSandboxCommit: opts.OpenSandboxCommit,
			FastSandboxTag:    opts.FastSandboxTag,
			FastSandboxCommit: opts.FastSandboxCommit,
			BenchGitCommit:    opts.BenchGitCommit,
			Timestamp:         time.Now().UTC(),
		})
		rep.Environment = baseEnv

		var (
			baseHostOOM   map[string]int64
			baseCgroupOOM int64
		)
		if leakAuditor != nil {
			baseCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			baseHostOOM, baseCgroupOOM = leakAuditor.CaptureBaseline(baseCtx)
			cancel()
		}

		rc := suite.RunContext{
			RunID:      cycleRunID,
			Profile:    prof,
			SDKConfig:  sdkCfg,
			Logger:     a.Logger,
			OutputDir:  cycleOutDir,
			WarmupRuns: 2,
		}

		var firstErr error
		for idx, s := range selected {
			suiteName := s.Name()
			suiteStart := time.Now().UTC()
			a.Logger.Info("starting benchmark suite", "suite", suiteName, "index", idx+1, "total", len(selected))

			var (
				promBefore  *collector.PromSnapshot
				obdBefore   *collector.OverlayBDSnapshot
				loadSampler *collector.LoadSampler
			)

			suiteHooks := hooks
			if clients != nil {
				if snap, sErr := promScraper.Snapshot(ctx); sErr == nil {
					promBefore = snap
				}
				if snap, oErr := obdProbe.Snapshot(ctx); oErr == nil {
					obdBefore = snap
				}
				if shouldRunLoadSampler(suiteName) {
					interval := loadSamplerInterval(prof)
					loadSampler = collector.NewLoadSampler(collCfg, clients, interval)
					loadSampler.Start(ctx)
					if suiteName == "high-density-soak" {
						suiteHooks.SoakLoadSampler = newCollectorSoakSampler(loadSampler, promScraper)
					}
				}
			}

			suiteCtx := suite.WithBenchHooks(ctx, suiteHooks)
			res, opSamples, runErr := s.Run(suiteCtx, rc)

			var sampledProfiles *report.LoadProfiles
			if loadSampler != nil {
				sampledProfiles = loadSampler.Stop()
			}

			if res == nil && runErr != nil {
				res = &report.SuiteResult{
					Name:            suiteName,
					Status:          "failed",
					StartedAt:       suiteStart,
					DurationSeconds: time.Since(suiteStart).Seconds(),
					Errors:          map[string]int{"suite_error": 1},
					Extra:           map[string]any{"error": runErr.Error()},
				}
			}

			if res != nil {
				if clients != nil {
					if promAfter, sErr := promScraper.Snapshot(ctx); sErr == nil {
						if deltas := collector.ComputeDeltas(promBefore, promAfter); len(deltas) > 0 {
							if res.PromMetricDeltas == nil {
								res.PromMetricDeltas = deltas
							} else {
								for k, v := range deltas {
									if _, exists := res.PromMetricDeltas[k]; !exists {
										res.PromMetricDeltas[k] = v
									}
								}
							}
						}
					}
					if obdAfter, oErr := obdProbe.Snapshot(ctx); oErr == nil && res.OverlayBDMetrics == nil {
						peakActive := resolvePeakActiveSandboxes(res, opSamples, loadSampler, obdAfter)
						res.OverlayBDMetrics = collector.ComputeOverlayBDMetrics(obdBefore, obdAfter, -1, peakActive)
					}
				}
				rep.Suites[suiteName] = *res
				mergeLoadProfiles(rep, sampledProfiles, res)
			} else if sampledProfiles != nil {
				mergeLoadProfiles(rep, sampledProfiles, nil)
			}

			if runErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("suite %q: %w", suiteName, runErr)
				}
			} else if res != nil && res.Status == "failed" {
				if firstErr == nil {
					firstErr = fmt.Errorf("suite %q failed (%d/%d ops failed)", suiteName, res.FailedOps, res.TotalOps)
				}
			}

			if res != nil {
				a.Logger.Info("completed benchmark suite",
					"suite", suiteName,
					"status", res.Status,
					"durationSeconds", math.Round(res.DurationSeconds*100)/100,
					"succeededOps", res.SucceededOps,
					"failedOps", res.FailedOps,
					"error", runErr,
				)
			} else {
				a.Logger.Info("completed benchmark suite", "suite", suiteName, "error", runErr)
			}

			if clients != nil && idx < len(selected)-1 && ctx.Err() == nil {
				gcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if gcErr := clusterOps.TriggerRegistryGC(gcCtx, prof.Environment.BenchNamespace); gcErr != nil {
					a.Logger.Debug("best-effort registry GC skipped", "suite", suiteName, "error", gcErr)
				}
				cancel()
			}

			if ctx.Err() != nil {
				break
			}
		}

		if clients != nil && leakAuditor != nil {
			var minStateRootFreeGiB float64
			if prof != nil && prof.Suites.HighDensitySoak != nil && prof.Suites.HighDensitySoak.MinStateRootFreeGiB > 0 {
				minStateRootFreeGiB = float64(prof.Suites.HighDensitySoak.MinStateRootFreeGiB)
			}
			auditTimeout := gracePeriod + 45*time.Second
			auditCtx, cancel := context.WithTimeout(context.Background(), auditTimeout)
			audit, auditErr := leakAuditor.Audit(auditCtx, collector.LeakAuditOptions{
				MinStateRootFreeGiB:    minStateRootFreeGiB,
				BaselineHostOOMKills:   baseHostOOM,
				BaselineCgroupOOMKills: baseCgroupOOM,
				GracePeriod:            gracePeriod,
			})
			cancel()
			if auditErr != nil {
				a.Logger.Warn("post-run leak audit encountered an error", "error", auditErr)
				if firstErr == nil {
					firstErr = fmt.Errorf("post-run leak audit: %w", auditErr)
				}
			} else if audit != nil {
				rep.LeakAudit = audit
				if !audit.Passed && firstErr == nil {
					firstErr = fmt.Errorf("post-run leak audit failed (%d failure(s))", len(audit.Failures))
				}
			}
		}

		if cycleOutDir != "" {
			if writeErr := rep.WriteBundle(cycleOutDir); writeErr != nil {
				if firstErr == nil {
					return rep, writeErr
				}
				a.Logger.Warn("failed to write partial report bundle", "outputDir", cycleOutDir, "error", writeErr)
			}
		}

		return rep, firstErr
	}

	if cycles == 1 {
		_, err := runCycle(runID, opts.OutputDir)
		return err
	}

	cycleInputs := make([]report.CycleRunInput, 0, cycles)
	var (
		firstCycleErr error
		failedCycles  int
	)
	for c := 1; c <= cycles; c++ {
		if ctx.Err() != nil {
			break
		}
		cycleRunID := fmt.Sprintf("%s-cycle-%d", runID, c)
		var cycleOutDir string
		if opts.OutputDir != "" {
			cycleOutDir = filepath.Join(opts.OutputDir, fmt.Sprintf("cycle-%d", c))
		}
		a.Logger.Info("starting benchmark cycle", "cycle", c, "totalCycles", cycles, "runID", cycleRunID)
		rep, cycleErr := runCycle(cycleRunID, cycleOutDir)
		if cycleErr != nil {
			failedCycles++
			if firstCycleErr == nil {
				firstCycleErr = fmt.Errorf("cycle %d: %w", c, cycleErr)
			}
		}
		cycleInputs = append(cycleInputs, report.CycleRunInput{
			Cycle:  c,
			Report: rep,
			Err:    cycleErr,
		})
	}

	agg := report.AggregateCycles(runID, prof.Metadata.Name, prof.Runtime, cycleInputs)
	if opts.OutputDir != "" {
		if writeErr := agg.WriteBundle(opts.OutputDir); writeErr != nil {
			if firstCycleErr == nil {
				return writeErr
			}
			a.Logger.Warn("failed to write multi-cycle aggregate bundle", "outputDir", opts.OutputDir, "error", writeErr)
		}
	}
	if failedCycles > 0 {
		return fmt.Errorf("%d of %d cycles failed: %w", failedCycles, len(cycleInputs), firstCycleErr)
	}
	return nil
}

func shouldRunLoadSampler(suiteName string) bool {
	switch suiteName {
	case "high-density-soak", "schedule-burst", "steady-churn", "density-ramp":
		return true
	default:
		return false
	}
}

func loadSamplerInterval(prof *profile.BenchmarkProfile) time.Duration {
	if prof != nil && prof.Suites.HighDensitySoak != nil && prof.Suites.HighDensitySoak.SamplerIntervalSeconds > 0 {
		return time.Duration(prof.Suites.HighDensitySoak.SamplerIntervalSeconds) * time.Second
	}
	return collector.DefaultSamplerInterval
}

func mergeLoadProfiles(rep *report.Report, sampled *report.LoadProfiles, res *report.SuiteResult) {
	if rep == nil {
		return
	}
	if sampled != nil {
		if rep.LoadProfiles == nil {
			cp := *sampled
			rep.LoadProfiles = &cp
		} else {
			if rep.LoadProfiles.Host == nil {
				rep.LoadProfiles.Host = sampled.Host
			}
			if rep.LoadProfiles.Fastlet == nil {
				rep.LoadProfiles.Fastlet = sampled.Fastlet
			}
			if rep.LoadProfiles.Egress == nil {
				rep.LoadProfiles.Egress = sampled.Egress
			}
			if rep.LoadProfiles.FastletProxy == nil {
				rep.LoadProfiles.FastletProxy = sampled.FastletProxy
			}
			if rep.LoadProfiles.FirecrackerRuntime == nil {
				rep.LoadProfiles.FirecrackerRuntime = sampled.FirecrackerRuntime
			}
			if rep.LoadProfiles.OpenSandboxServer == nil {
				rep.LoadProfiles.OpenSandboxServer = sampled.OpenSandboxServer
			}
			if rep.LoadProfiles.IngressGateway == nil {
				rep.LoadProfiles.IngressGateway = sampled.IngressGateway
			}
		}
	}
	if res == nil || res.Extra == nil {
		return
	}
	if lp, ok := res.Extra["load_profiles"].(*report.LoadProfiles); ok && lp != nil {
		if rep.LoadProfiles == nil {
			cp := *lp
			rep.LoadProfiles = &cp
		} else if len(lp.DensityFrontier) > 0 && len(rep.LoadProfiles.DensityFrontier) == 0 {
			rep.LoadProfiles.DensityFrontier = lp.DensityFrontier
		}
	}
	if df, ok := res.Extra["density_frontier"].([]report.DensityTierRow); ok && len(df) > 0 {
		if rep.LoadProfiles == nil {
			rep.LoadProfiles = &report.LoadProfiles{}
		}
		rep.LoadProfiles.DensityFrontier = df
	}
}

func populateWorkerEnvironment(ctx context.Context, clients *collector.Clients, env *report.EnvironmentFingerprint) {
	if clients == nil || clients.Clientset == nil || env == nil {
		return
	}
	if sv, err := clients.Clientset.Discovery().ServerVersion(); err == nil && sv != nil {
		env.KubernetesVersion = sv.GitVersion
	}
	nodeList, err := clients.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	nodes := make([]report.NodeInfo, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if env.ClusterArch == "" {
			if arch := n.Labels[corev1.LabelArchStable]; arch != "" {
				env.ClusterArch = arch
			} else if n.Status.NodeInfo.Architecture != "" {
				env.ClusterArch = n.Status.NodeInfo.Architecture
			}
		}
		nodes = append(nodes, report.NodeInfo{
			Name:             n.Name,
			KernelVersion:    n.Status.NodeInfo.KernelVersion,
			OSImage:          n.Status.NodeInfo.OSImage,
			ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
			CPUCapacity:      n.Status.Capacity.Cpu().String(),
			MemoryCapacity:   n.Status.Capacity.Memory().String(),
		})
	}
	env.Nodes = nodes
}

func buildClusterBenchHooks(
	ctx context.Context,
	cfg collector.Config,
	clients *collector.Clients,
	ops *collector.ClusterOps,
	scraper *collector.PrometheusScraper,
) suite.BenchHooks {
	hooks := suite.BenchHooks{
		CacheEvictor: &collectorCacheEvictor{ops: ops, poolLow: cfg.PoolLow},
		NodeIsolator: &collectorNodeIsolator{ops: ops},
		NodeResolver: &collectorNodeResolver{clients: clients, resourceNS: cfg.ResourceNamespace},
		WakeResumeCounter: func(rCtx context.Context) (int64, error) {
			snap, err := scraper.Snapshot(rCtx)
			if err != nil || snap == nil || snap.Merged == nil {
				return 0, fmt.Errorf("scrape ingress wake flights counter: %w", err)
			}
			// Query real OSEP-0010 ingress OTLP wake-flights counter exported via OTel Collector (:8889/metrics).
			triggeredFilter := map[string]string{"outcome": "triggered"}
			for _, metricName := range []string{"ingress_wake_flights_total", "ingress_wake_flights"} {
				if len(snap.Merged.Samples[metricName]) > 0 {
					return int64(math.Round(snap.Merged.Sum(metricName, triggeredFilter))), nil
				}
			}
			return 0, fmt.Errorf("ingress wake flights metric unavailable")
		},
		SoakNodeMemoryMiB: detectSoakNodeMemoryMiB(ctx, clients),
	}
	return hooks
}

func detectSoakNodeMemoryMiB(ctx context.Context, clients *collector.Clients) int {
	if clients == nil || clients.Clientset == nil {
		return 0
	}
	nodes, err := clients.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return 0
	}
	var fcMemBytes, allMemBytes int64
	for i := range nodes.Items {
		n := &nodes.Items[i]
		mem := n.Status.Allocatable.Memory().Value()
		if mem <= 0 {
			mem = n.Status.Capacity.Memory().Value()
		}
		allMemBytes += mem
		if n.Labels["fast-sandbox.io/firecracker-node"] == "true" {
			fcMemBytes += mem
		}
	}
	target := fcMemBytes
	if target <= 0 {
		target = allMemBytes
	}
	if target <= 0 {
		return 0
	}
	return int(target / (1024 * 1024))
}

type collectorCacheEvictor struct {
	ops     *collector.ClusterOps
	poolLow int
}

func (e *collectorCacheEvictor) EvictOverlayBDCaches(ctx context.Context, nodeName string) error {
	return e.ops.EvictOverlayBDCaches(ctx, collector.EvictCacheOptions{
		NodeName: nodeName,
		PoolLow:  e.poolLow,
	})
}

type collectorNodeIsolator struct {
	ops *collector.ClusterOps
}

func (i *collectorNodeIsolator) IsolateNodeFastlets(ctx context.Context, nodeName string) (func(context.Context) error, error) {
	if _, err := i.ops.IsolateFastletNode(ctx, nodeName); err != nil {
		return nil, err
	}
	return func(restoreCtx context.Context) error {
		return i.ops.RestoreFastletNode(restoreCtx, nodeName)
	}, nil
}

type collectorNodeResolver struct {
	clients    *collector.Clients
	resourceNS string
}

func (r *collectorNodeResolver) SandboxNode(ctx context.Context, sandboxID string) (string, error) {
	if r.clients == nil || r.clients.DynamicClient == nil {
		return "", fmt.Errorf("dynamic client unavailable")
	}
	for _, gvr := range []struct {
		res func() (*unstructured.Unstructured, error)
	}{
		{res: func() (*unstructured.Unstructured, error) {
			return r.clients.DynamicClient.Resource(collector.SandboxGVRV1Alpha2).Namespace(r.resourceNS).Get(ctx, sandboxID, metav1.GetOptions{})
		}},
		{res: func() (*unstructured.Unstructured, error) {
			return r.clients.DynamicClient.Resource(collector.SandboxGVRV1Alpha1).Namespace(r.resourceNS).Get(ctx, sandboxID, metav1.GetOptions{})
		}},
	} {
		u, err := gvr.res()
		if err != nil {
			continue
		}
		if node := extractNodeNameFromSandbox(u); node != "" {
			return node, nil
		}
		if podName := extractFastletPodNameFromSandbox(u); podName != "" && r.clients.Clientset != nil {
			podNS := r.resourceNS
			if podNS == "" {
				podNS = u.GetNamespace()
			}
			pod, pErr := r.clients.Clientset.CoreV1().Pods(podNS).Get(ctx, podName, metav1.GetOptions{})
			if pErr == nil && pod != nil && strings.TrimSpace(pod.Spec.NodeName) != "" {
				return strings.TrimSpace(pod.Spec.NodeName), nil
			}
		}
	}
	return "", fmt.Errorf("could not resolve node for sandbox %q", sandboxID)
}

func extractNodeNameFromSandbox(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	for _, path := range [][]string{
		{"status", "nodeName"},
		{"status", "assignment", "nodeName"},
		{"status", "assignedNode"},
		{"spec", "nodeName"},
	} {
		if v, found, _ := unstructured.NestedString(u.Object, path...); found && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func extractFastletPodNameFromSandbox(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	for _, path := range [][]string{
		{"status", "placement", "fastletName"},
		{"status", "assignment", "fastletPod"},
		{"status", "assignment", "fastletName"},
		{"status", "fastletName"},
		{"status", "podName"},
		{"status", "assignedPod"},
	} {
		if v, found, _ := unstructured.NestedString(u.Object, path...); found && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func resolvePeakActiveSandboxes(res *report.SuiteResult, samples []suite.OperationSample, loadSampler *collector.LoadSampler, obdAfter *collector.OverlayBDSnapshot) int {
	peak := 0
	if loadSampler != nil {
		if p := loadSampler.PeakActiveSandboxes(); p > peak {
			peak = p
		}
	}
	if res != nil && res.Extra != nil {
		if df, ok := res.Extra["density_frontier"].([]report.DensityTierRow); ok {
			for _, row := range df {
				if row.ActiveSandboxes > peak {
					peak = row.ActiveSandboxes
				}
			}
		}
		if fs, ok := res.Extra["fleet_size"].(int); ok && fs > peak {
			peak = fs
		}
		if pa, ok := res.Extra["peak_active_sandboxes"].(int); ok && pa > peak {
			peak = pa
		}
	}
	if peak > 0 {
		return peak
	}
	if obdAfter != nil {
		inUse := 0
		for _, n := range obdAfter.Nodes {
			if n.Pool != nil {
				inUse += n.Pool.InUse
			}
		}
		if inUse > peak {
			peak = inUse
		}
	}
	if peak > 0 {
		return peak
	}
	if len(samples) > 0 {
		uniq := make(map[string]struct{})
		for _, sm := range samples {
			if sm.SandboxID != "" {
				uniq[sm.SandboxID] = struct{}{}
			}
		}
		if len(uniq) > peak {
			peak = len(uniq)
		}
	}
	return peak
}

func newCollectorSoakSampler(sampler *collector.LoadSampler, scraper *collector.PrometheusScraper) suite.SoakLoadSampler {
	return suite.SoakLoadSamplerFunc(func(ctx context.Context) (*suite.SoakLoadSample, error) {
		out := &suite.SoakLoadSample{
			LoadProfiles: sampler.Aggregate(),
		}
		if host, ok := sampler.LatestHostSnapshot(); ok {
			out.CPUPressureSomeAvg60 = host.CPUPSI.SomeAvg60
			out.MemPressureFullAvg60 = host.MemoryPSI.FullAvg60
			out.MemAvailableMiB = host.MemAvailableMiB
			out.StateRootFreeGiB = host.StateRootFreeGiB
			out.OOMKills = int(host.OOMKillTotal)
		}
		out.OOMKills += int(sampler.LatestPodCgroupOOMDelta())
		if out.LoadProfiles != nil && out.LoadProfiles.Fastlet != nil {
			out.ResidentMiBPerSandbox = out.LoadProfiles.Fastlet.PerSandboxResidentMiB
		}
		if deleting, ok := sampler.LatestDeletingSlots(); ok {
			out.DeletingSlots = deleting
		} else if scraper != nil {
			if snap, err := scraper.Snapshot(ctx); err == nil && snap != nil && snap.Merged != nil {
				out.DeletingSlots = int(math.Round(snap.Merged.Sum("fast_sandbox_fastlet_admission_slots", map[string]string{"state": "deleting"})))
			}
		}
		return out, nil
	})
}

// ensureWorkerTemplate resolves a Succeeded OverlayBD template from opensandbox-server's
// catalog (honoring prof.Extensions["templateId"] when already set). If the server is
// reachable and has no Succeeded template yet, it creates one via POST /v1/templates and
// polls until Succeeded so all suites launch fast-sandbox microVMs rather than falling back
// to BatchSandbox container pods.
func ensureWorkerTemplate(
	ctx context.Context,
	prof *profile.BenchmarkProfile,
	sdkCfg opensandbox.ConnectionConfig,
	log *slog.Logger,
	pollInterval time.Duration,
) (string, error) {
	if prof == nil {
		return "", nil
	}
	if log == nil {
		log = slog.Default()
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	if prof.Extensions != nil {
		if existing, ok := prof.Extensions["templateId"].(string); ok && strings.TrimSpace(existing) != "" {
			return strings.TrimSpace(existing), nil
		}
	}

	baseURL := sdkCfg.GetBaseURL() + "/" + opensandbox.APIVersion
	var opts []opensandbox.Option
	if sdkCfg.HTTPClient != nil {
		opts = append(opts, opensandbox.WithHTTPClient(sdkCfg.HTTPClient))
	}
	lc := opensandbox.NewLifecycleClient(baseURL, sdkCfg.GetAPIKey(), opts...)

	listResp, err := lc.ListTemplates(ctx, opensandbox.ListTemplatesOptions{PageSize: 50})
	if err != nil {
		var apiErr *opensandbox.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) {
			log.Info("server does not expose /v1/templates; skipping automatic template resolution", "status", apiErr.StatusCode)
			return "", nil
		}
		// Degrade gracefully when running offline/unit tests without a live server.
		log.Warn("failed to list server templates; skipping automatic template creation", "error", err)
		return "", nil
	}

	if id := pickSucceededTemplate(listResp.Items, prof.Stack.Template.BaseImage); id != "" {
		return id, nil
	}

	publish := strings.TrimSpace(prof.Stack.Template.Publish)
	if publish == "" {
		benchNS := prof.Environment.BenchNamespace
		if benchNS == "" {
			benchNS = "opensandbox-bench"
		}
		publish = fmt.Sprintf("oci://opensandbox-bench-registry.%s.svc:5000/opensandbox/templates", benchNS)
	}
	vcpus := prof.Stack.Template.VCPUs
	if vcpus <= 0 {
		vcpus = 1
	}
	memMiB := prof.Stack.Template.MemoryMiB
	if memMiB <= 0 {
		memMiB = 2048
	}
	diskGiB := prof.Stack.Template.DiskGiB
	if diskGiB <= 0 {
		diskGiB = prof.Stack.FastSandbox.Runtime.OCI.PoolSizeGB
	}
	if diskGiB <= 0 {
		diskGiB = 2
	}

	req := opensandbox.CreateTemplateRequest{
		Image:   prof.Stack.Template.BaseImage,
		Publish: publish,
		Format:  opensandbox.TemplateFormatOverlayBD,
		ResourceLimits: opensandbox.ResourceLimits{
			"cpu":    strconv.Itoa(vcpus),
			"memory": fmt.Sprintf("%dMi", memMiB),
			"disk":   fmt.Sprintf("%dGi", diskGiB),
		},
		Metadata: map[string]string{
			"app.kubernetes.io/managed-by": "opensandbox-bench",
		},
	}

	log.Info("no Succeeded template in server catalog; creating OverlayBD template via /v1/templates",
		"image", req.Image, "publish", req.Publish)
	created, err := lc.CreateTemplate(ctx, req)
	if err != nil {
		var apiErr *opensandbox.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) {
			log.Info("server does not support POST /v1/templates; skipping", "status", apiErr.StatusCode)
			return "", nil
		}
		return "", fmt.Errorf("create template: %w", err)
	}
	if created.Status.Phase == opensandbox.TemplatePhaseSucceeded {
		return created.TemplateID, nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("timed out waiting for template %q to reach Succeeded: %w", created.TemplateID, waitCtx.Err())
		case <-ticker.C:
			info, gErr := lc.GetTemplate(waitCtx, created.TemplateID)
			if gErr != nil {
				continue
			}
			switch info.Status.Phase {
			case opensandbox.TemplatePhaseSucceeded:
				log.Info("server catalog template build succeeded", "templateId", info.TemplateID, "manifestRef", info.Status.ManifestRef)
				return info.TemplateID, nil
			case opensandbox.TemplatePhaseFailed:
				return "", fmt.Errorf("template %q build failed: %s", info.TemplateID, info.Status.Message)
			}
		}
	}
}

func pickSucceededTemplate(items []opensandbox.TemplateInfo, preferredImage string) string {
	var fallbackOverlayBD, fallbackAny string
	for _, tpl := range items {
		if tpl.Status.Phase != opensandbox.TemplatePhaseSucceeded || tpl.TemplateID == "" {
			continue
		}
		if tpl.Format == opensandbox.TemplateFormatOverlayBD {
			if preferredImage != "" && tpl.Image == preferredImage {
				return tpl.TemplateID
			}
			if fallbackOverlayBD == "" {
				fallbackOverlayBD = tpl.TemplateID
			}
		}
		if fallbackAny == "" {
			fallbackAny = tpl.TemplateID
		}
	}
	if fallbackOverlayBD != "" {
		return fallbackOverlayBD
	}
	return fallbackAny
}
