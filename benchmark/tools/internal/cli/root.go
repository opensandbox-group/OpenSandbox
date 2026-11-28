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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/collector"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/plot"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/provisioner"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/suite"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/trace"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/version"
)

// App holds the extensible registries used by the CLI.
// Callers can register additional Provisioner or Suite implementations before executing.
type App struct {
	Suites              *suite.Registry
	Provisioners        *provisioner.Registry
	Logger              *slog.Logger
	NewRemoteRunner     func(kubeconfig string) (runner.RemoteRunner, *runner.KubeClients, error)
	NewCollectorClients func() (*collector.Clients, error)
}

func NewDefaultApp() *App {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return &App{
		Suites:       suite.DefaultRegistry(),
		Provisioners: provisioner.DefaultRegistry(),
		Logger:       log,
		NewRemoteRunner: func(kubeconfig string) (runner.RemoteRunner, *runner.KubeClients, error) {
			kc, err := runner.LoadKubeClients(kubeconfig)
			if err != nil {
				return nil, nil, err
			}
			return runner.NewK8sRunner(kc, log), kc, nil
		},
		NewCollectorClients: func() (*collector.Clients, error) {
			return defaultInClusterCollectorClients(log)
		},
	}
}

func NewRootCmd() *cobra.Command {
	return NewDefaultApp().RootCmd()
}

func (a *App) RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "opensandbox-bench",
		Short:         "Standalone OpenSandbox benchmark harness (OSEP-0034)",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(
		a.newPreflightCmd(),
		a.newEnvCmd(),
		a.newRunCmd(),
		a.newTraceCmd(),
		a.newCompareCmd(),
		a.newPlotCmd(),
		a.newProfileCmd(),
		a.newSuitesCmd(),
		a.newWorkerCmd(),
		a.newVersionCmd(),
	)
	return root
}

func (a *App) newPreflightCmd() *cobra.Command {
	var kubeconfig, profilePath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Check cluster and node readiness (KVM, ublk_drv, XFS reflink, spill dir, CPU homogeneity)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prof, err := profile.Load(profilePath)
			if err != nil {
				return err
			}
			prov, err := a.Provisioners.Get(prof.Runtime)
			if err != nil {
				return err
			}
			kc, err := runner.LoadKubeClients(kubeconfig)
			if err != nil {
				return err
			}
			env, err := prov.Preflight(cmd.Context(), prof, provisioner.Options{
				Kubeconfig: kubeconfig,
				Client:     kc.Clientset,
				RestConfig: kc.RestConfig,
				Logger:     a.Logger,
			})
			if err != nil {
				return err
			}
			return renderPreflightSummary(cmd.OutOrStdout(), env, asJSON)
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&profilePath, "profile", "", "Path to BenchmarkProfile YAML (defaults to embedded reference profile)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print EnvironmentFingerprint as JSON")
	return cmd
}

func (a *App) newEnvCmd() *cobra.Command {
	envCmd := &cobra.Command{
		Use:   "env",
		Short: "Provision or tear down the benchmark stack",
	}

	var upKubeconfig, upProfile string
	var prepareNodes bool
	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Provision full OverlayBD benchmark stack (registry, Redis, Helm charts, template, pool)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prof, err := profile.Load(upProfile)
			if err != nil {
				return err
			}
			prov, err := a.Provisioners.Get(prof.Runtime)
			if err != nil {
				return err
			}
			kc, err := runner.LoadKubeClients(upKubeconfig)
			if err != nil {
				return err
			}
			return prov.Up(cmd.Context(), prof, provisioner.Options{
				Kubeconfig:   upKubeconfig,
				PrepareNodes: prepareNodes,
				Client:       kc.Clientset,
				RestConfig:   kc.RestConfig,
				Logger:       a.Logger,
			})
		},
	}
	upCmd.Flags().StringVar(&upKubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	upCmd.Flags().StringVar(&upProfile, "profile", "", "Path to BenchmarkProfile YAML")
	upCmd.Flags().BoolVar(&prepareNodes, "prepare-nodes", false, "Opt-in host node preparation (XFS reflink StateRoot / modprobe ublk_drv)")

	var downKubeconfig, downProfile string
	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Tear down benchmark-provisioned resources and run post-teardown leak check",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prof, err := profile.Load(downProfile)
			if err != nil {
				return err
			}
			prov, err := a.Provisioners.Get(prof.Runtime)
			if err != nil {
				return err
			}
			kc, err := runner.LoadKubeClients(downKubeconfig)
			if err != nil {
				return err
			}
			audit, err := prov.Down(cmd.Context(), prof, provisioner.Options{
				Kubeconfig: downKubeconfig,
				Client:     kc.Clientset,
				RestConfig: kc.RestConfig,
				Logger:     a.Logger,
			})
			if audit != nil {
				renderLeakAuditSummary(cmd.OutOrStdout(), audit)
			}
			if err != nil {
				return err
			}
			if audit != nil && !audit.Passed {
				return fmt.Errorf("post-teardown leak audit failed (%d failure(s))", len(audit.Failures))
			}
			return nil
		},
	}
	downCmd.Flags().StringVar(&downKubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	downCmd.Flags().StringVar(&downProfile, "profile", "", "Path to BenchmarkProfile YAML")

	envCmd.AddCommand(upCmd, downCmd)
	return envCmd
}

func (a *App) newRunCmd() *cobra.Command {
	var kubeconfig, profilePath, suitesFlag, outputDir string
	var workerBinary, moduleDir, serverDomain, apiKey, runID string
	var provision, teardown, keepRunner, allowDegradedTelemetry bool
	var cycles int
	var leakAuditGrace time.Duration

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Inject static worker binary into in-cluster Runner Pod and execute benchmark suites",
		RunE: func(cmd *cobra.Command, _ []string) (retErr error) {
			ctx := cmd.Context()
			if cycles < 1 {
				return fmt.Errorf("--cycles must be >= 1, got %d", cycles)
			}
			if leakAuditGrace < 0 {
				return fmt.Errorf("--leak-audit-grace must be >= 0, got %s", leakAuditGrace)
			}
			prof, profileBytes, err := loadProfileWithRaw(profilePath)
			if err != nil {
				return err
			}
			selected, err := a.Suites.Resolve(splitCSV(suitesFlag), prof.Runtime)
			if err != nil {
				return err
			}
			suiteNames := make([]string, 0, len(selected))
			for _, s := range selected {
				suiteNames = append(suiteNames, s.Name())
			}

			rem, kc, err := a.NewRemoteRunner(kubeconfig)
			if err != nil {
				return err
			}

			if provision || teardown {
				prov, err := a.Provisioners.Get(prof.Runtime)
				if err != nil {
					return err
				}
				provOpts := provisioner.Options{
					Kubeconfig: kubeconfig,
					Logger:     a.Logger,
				}
				if kc != nil {
					provOpts.Client = kc.Clientset
					provOpts.RestConfig = kc.RestConfig
				}
				if teardown {
					defer func() {
						// Use an independent bounded context so cancellation/timeout still tears down the environment.
						downCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer cancel()
						audit, downErr := prov.Down(downCtx, prof, provOpts)
						if audit != nil {
							renderLeakAuditSummary(cmd.OutOrStdout(), audit)
						}
						if downErr != nil {
							if retErr == nil {
								retErr = fmt.Errorf("env down: %w", downErr)
							} else {
								a.Logger.Warn("failed to tear down environment after run error", "error", downErr)
							}
							return
						}
						if audit != nil && !audit.Passed {
							if retErr == nil {
								retErr = fmt.Errorf("post-teardown leak audit failed (%d failure(s))", len(audit.Failures))
							} else {
								a.Logger.Warn("post-teardown leak audit failed after run error", "failures", len(audit.Failures))
							}
						}
					}()
				}
				if provision {
					if _, err := prov.Preflight(ctx, prof, provOpts); err != nil {
						return fmt.Errorf("preflight: %w", err)
					}
					if err := prov.Up(ctx, prof, provOpts); err != nil {
						return fmt.Errorf("env up: %w", err)
					}
				}
			}

			ns := prof.Environment.BenchNamespace
			podName, err := rem.EnsureRunnerPod(ctx, prof)
			if err != nil {
				return err
			}
			if !keepRunner {
				defer func() {
					// Use an independent bounded context so Ctrl-C or worker timeout still cleans up the Runner Pod.
					delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if delErr := rem.DeleteRunnerPod(delCtx, ns, podName); delErr != nil {
						a.Logger.Warn("failed to delete runner pod", "namespace", ns, "pod", podName, "error", delErr)
					}
				}()
			}

			binPath := workerBinary
			if binPath == "" {
				arch, err := rem.DetectClusterArch(ctx, ns, podName)
				if err != nil {
					return err
				}
				resolvedDir, err := resolveToolsModuleDir(moduleDir)
				if err != nil {
					return err
				}
				tmpDir, err := os.MkdirTemp("", "opensandbox-bench-worker-*")
				if err != nil {
					return fmt.Errorf("create temp build dir: %w", err)
				}
				defer os.RemoveAll(tmpDir)
				binPath = filepath.Join(tmpDir, "opensandbox-bench-linux-"+arch)
				a.Logger.Info("cross-compiling static linux worker binary", "arch", arch, "moduleDir", resolvedDir)
				if err := runner.CrossCompileLinuxBinary(ctx, resolvedDir, arch, binPath); err != nil {
					return err
				}
			}

			if err := rem.InjectBinary(ctx, ns, podName, binPath, runner.DefaultRemoteBinaryPath); err != nil {
				return err
			}
			if err := rem.InjectBytes(ctx, ns, podName, profileBytes, runner.DefaultRemoteProfilePath, 0o644); err != nil {
				return err
			}

			effectiveServer := serverDomain
			if effectiveServer == "" {
				effectiveServer = defaultServerDomain(prof)
			}
			workerArgs := []string{
				"worker", "run",
				"--profile", runner.DefaultRemoteProfilePath,
				"--suite", strings.Join(suiteNames, ","),
				"--output", runner.DefaultRemoteBundleDir,
				"--server", effectiveServer,
			}
			if apiKey != "" {
				if err := rem.InjectBytes(ctx, ns, podName, []byte(apiKey), runner.DefaultRemoteAPIKeyPath, 0o600); err != nil {
					return err
				}
				workerArgs = append(workerArgs, "--api-key-file", runner.DefaultRemoteAPIKeyPath)
			}
			if runID != "" {
				workerArgs = append(workerArgs, "--run-id", runID)
			}
			if cycles > 1 {
				workerArgs = append(workerArgs, "--cycles", strconv.Itoa(cycles))
			}
			if cmd.Flags().Changed("leak-audit-grace") {
				workerArgs = append(workerArgs, "--leak-audit-grace", leakAuditGrace.String())
			}
			if allowDegradedTelemetry {
				workerArgs = append(workerArgs, "--allow-degraded-telemetry")
			}
			localProv := DetectLocalProvenance("")
			if localProv.OpenSandboxCommit != "" {
				workerArgs = append(workerArgs, "--opensandbox-commit", localProv.OpenSandboxCommit)
			}
			if localProv.OpenSandboxTag != "" {
				workerArgs = append(workerArgs, "--opensandbox-tag", localProv.OpenSandboxTag)
			}
			if localProv.FastSandboxCommit != "" {
				workerArgs = append(workerArgs, "--fast-sandbox-commit", localProv.FastSandboxCommit)
			}
			if localProv.FastSandboxTag != "" {
				workerArgs = append(workerArgs, "--fast-sandbox-tag", localProv.FastSandboxTag)
			}
			if localProv.BenchGitCommit != "" {
				workerArgs = append(workerArgs, "--bench-git-commit", localProv.BenchGitCommit)
			}

			execErr := rem.ExecWorker(ctx, ns, podName, runner.DefaultRemoteBinaryPath, workerArgs, cmd.OutOrStdout(), cmd.ErrOrStderr())

			if outputDir != "" {
				// Use an independent bounded context so partial bundles are still pulled on caller cancellation/timeout.
				pullCtx, pullCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				pullErr := rem.PullBundle(pullCtx, ns, podName, runner.DefaultRemoteBundleDir, outputDir)
				pullCancel()
				if pullErr != nil {
					if execErr == nil {
						return pullErr
					}
					a.Logger.Warn("failed to pull partial bundle after worker error", "error", pullErr)
				}
			} else {
				a.Logger.Warn("no --output directory specified; run bundle inside runner pod will not be pulled locally")
			}
			return execErr
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&profilePath, "profile", "", "Path to BenchmarkProfile YAML")
	cmd.Flags().StringVar(&suitesFlag, "suite", "all", "Comma-separated suite names or 'all'")
	cmd.Flags().StringVar(&outputDir, "output", "", "Output directory for run bundle (report.json, summary.md, traces/)")
	cmd.Flags().StringVar(&workerBinary, "worker-binary", "", "Optional pre-built static Linux opensandbox-bench binary (skips local cross-compilation)")
	cmd.Flags().StringVar(&moduleDir, "module-dir", "", "Path to benchmark/tools Go module when auto cross-compiling")
	cmd.Flags().StringVar(&serverDomain, "server", "", "Override in-cluster opensandbox-server host:port passed to worker")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "OpenSandbox API key passed to worker")
	cmd.Flags().StringVar(&runID, "run-id", "", "Benchmark run identifier passed to worker")
	cmd.Flags().BoolVar(&provision, "provision", false, "Run preflight + env up before executing suites")
	cmd.Flags().BoolVar(&teardown, "teardown", false, "Run env down after executing suites (preserving bundle on failure)")
	cmd.Flags().BoolVar(&keepRunner, "keep-runner", false, "Keep the in-cluster Runner Pod after execution for debugging")
	cmd.Flags().IntVar(&cycles, "cycles", 1, "Number of consecutive benchmark cycles to run and aggregate")
	cmd.Flags().DurationVar(&leakAuditGrace, "leak-audit-grace", collector.DefaultLeakAuditGracePeriod, "Grace period to wait for async CR finalizer cleanup during LeakAudit (e.g. 30s, 0s)")
	cmd.Flags().BoolVar(&allowDegradedTelemetry, "allow-degraded-telemetry", false, "Allow benchmark execution when Telemetry Preflight detects missing RBAC or runtime pods (marks report telemetry_status=degraded)")
	return cmd
}

func loadProfileWithRaw(path string) (*profile.BenchmarkProfile, []byte, error) {
	if strings.TrimSpace(path) == "" {
		raw, err := profile.EmbeddedRaw(profile.DefaultProfileName)
		if err != nil {
			return nil, nil, err
		}
		p, err := profile.Parse(raw)
		return p, raw, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read profile %q: %w", path, err)
	}
	p, err := profile.Parse(raw)
	return p, raw, err
}

func resolveToolsModuleDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, candidate := range []string{".", filepath.Join("benchmark", "tools")} {
		if _, err := os.Stat(filepath.Join(candidate, "cmd", "opensandbox-bench", "main.go")); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not locate benchmark/tools module directory; pass --module-dir or --worker-binary")
}

func (a *App) newTraceCmd() *cobra.Command {
	var traceID string
	var topSlow int

	cmd := &cobra.Command{
		Use:   "trace <run-bundle-dir>",
		Short: "Inspect saved OTel trace waterfalls offline from a run bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundleDir := args[0]
			if traceID != "" {
				t, err := trace.LoadTraceFile(filepath.Join(bundleDir, "traces", traceID+".json"))
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), trace.RenderWaterfall(t))
				return nil
			}
			traces, err := trace.LoadBundleTraces(bundleDir)
			if err != nil {
				return err
			}
			if len(traces) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No saved traces found in bundle.")
				return nil
			}
			limit := topSlow
			if limit <= 0 || limit > len(traces) {
				limit = len(traces)
			}
			for i := 0; i < limit; i++ {
				if i > 0 {
					fmt.Fprintln(cmd.OutOrStdout())
				}
				fmt.Fprint(cmd.OutOrStdout(), trace.RenderWaterfall(traces[i]))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&traceID, "trace-id", "", "Specific trace ID to render")
	cmd.Flags().IntVar(&topSlow, "top-slow", 5, "Number of slowest traces to render when --trace-id is omitted")
	return cmd
}

func (a *App) newCompareCmd() *cobra.Command {
	var failOnRegression bool
	th := report.DefaultCompareThresholds()

	cmd := &cobra.Command{
		Use:   "compare <baseline.json> <candidate.json>",
		Short: "Compare two benchmark reports offline and detect regressions",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := report.Load(args[0])
			if err != nil {
				return err
			}
			cand, err := report.Load(args[1])
			if err != nil {
				return err
			}
			res := report.Compare(base, cand, th)
			fmt.Fprint(cmd.OutOrStdout(), res.MarkdownSummary)
			if failOnRegression && res.HasRegression {
				return fmt.Errorf("detected %d benchmark regression(s)", len(res.Regressions))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&failOnRegression, "fail-on-regression", false, "Exit non-zero if any regression threshold is breached")
	cmd.Flags().Float64Var(&th.MaxLatencyRegressionPct, "max-latency-regression-pct", th.MaxLatencyRegressionPct, "Max allowed P95/P99 latency increase (%)")
	cmd.Flags().Float64Var(&th.MaxThroughputDropPct, "max-throughput-drop-pct", th.MaxThroughputDropPct, "Max allowed throughput drop (%)")
	cmd.Flags().Float64Var(&th.MaxPerSandboxOverheadPct, "max-overhead-regression-pct", th.MaxPerSandboxOverheadPct, "Max allowed per-sandbox memory overhead increase (%)")
	return cmd
}

func (a *App) newPlotCmd() *cobra.Command {
	var outputPath, title, hostMemoryPath string
	var baselinePath, baselineHostMemoryPath, baselineLabel, candidateLabel string

	cmd := &cobra.Command{
		Use:   "plot <report.json> [candidate.json]",
		Short: "Render a self-contained interactive HTML report (single run or two-version comparison)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 && strings.TrimSpace(baselinePath) != "" {
				return fmt.Errorf("cannot specify both two positional report files and --baseline")
			}

			// Two-version comparison mode:
			//   plot <baseline.json> <candidate.json> --output compare.html
			//   plot <candidate.json> --baseline <baseline.json> --output compare.html
			if len(args) == 2 || strings.TrimSpace(baselinePath) != "" {
				var baseFile, candFile string
				if len(args) == 2 {
					baseFile, candFile = args[0], args[1]
				} else {
					baseFile, candFile = baselinePath, args[0]
				}
				baseRep, err := report.Load(baseFile)
				if err != nil {
					return err
				}
				candRep, err := report.Load(candFile)
				if err != nil {
					return err
				}
				cmpOpts := plot.CompareOptions{
					Title:                   title,
					BaselineLabel:           baselineLabel,
					CandidateLabel:          candidateLabel,
					BaselineHostMemoryPath:  baselineHostMemoryPath,
					CandidateHostMemoryPath: hostMemoryPath,
				}
				if outputPath == "-" {
					htmlDoc, err := plot.RenderCompareHTML(baseRep, candRep, cmpOpts)
					if err != nil {
						return err
					}
					_, err = fmt.Fprint(cmd.OutOrStdout(), htmlDoc)
					return err
				}
				if strings.TrimSpace(outputPath) == "" {
					outputPath = filepath.Join(filepath.Dir(candFile), "compare.html")
				}
				if err := plot.WriteCompareHTML(baseRep, candRep, outputPath, cmpOpts); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote comparison HTML report to %s\n", outputPath)
				return nil
			}

			rep, err := report.Load(args[0])
			if err != nil {
				return err
			}
			opts := plot.Options{
				Title:          title,
				HostMemoryPath: hostMemoryPath,
			}
			if outputPath == "-" {
				htmlDoc, err := plot.RenderHTML(rep, opts)
				if err != nil {
					return err
				}
				_, err = fmt.Fprint(cmd.OutOrStdout(), htmlDoc)
				return err
			}
			if strings.TrimSpace(outputPath) == "" {
				outputPath = filepath.Join(filepath.Dir(args[0]), "report.html")
			}
			if err := plot.WriteHTML(rep, outputPath, opts); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote HTML report to %s\n", outputPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Output HTML path (defaults to report.html / compare.html; '-' for stdout)")
	cmd.Flags().StringVar(&title, "title", "", "Custom title for the generated HTML report")
	cmd.Flags().StringVar(&hostMemoryPath, "host-memory", "", "Optional path to host-memory-profile.json (candidate in comparison mode)")
	cmd.Flags().StringVar(&baselinePath, "baseline", "", "Optional baseline report.json path to render a two-version comparison report")
	cmd.Flags().StringVar(&baselineHostMemoryPath, "baseline-host-memory", "", "Optional baseline host-memory-profile.json path in comparison mode")
	cmd.Flags().StringVar(&baselineLabel, "baseline-label", "", "Custom display label for baseline version (defaults to provenance tag/commit)")
	cmd.Flags().StringVar(&candidateLabel, "candidate-label", "", "Custom display label for candidate version (defaults to provenance tag/commit)")
	return cmd
}

func (a *App) newProfileCmd() *cobra.Command {
	profileCmd := &cobra.Command{
		Use:   "profile",
		Short: "Inspect, export, or validate BenchmarkProfile YAML files",
	}

	var initName, initOut string
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write an embedded reference BenchmarkProfile YAML to stdout or a file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := profile.EmbeddedRaw(initName)
			if err != nil {
				return err
			}
			if initOut == "" || initOut == "-" {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			return os.WriteFile(initOut, raw, 0o644)
		},
	}
	initCmd.Flags().StringVar(&initName, "name", profile.DefaultProfileName, "Embedded profile name")
	initCmd.Flags().StringVarP(&initOut, "output", "o", "-", "Output path ('-' for stdout)")

	var validatePath string
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a BenchmarkProfile YAML against OSEP-0034 invariants",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prof, err := profile.Load(validatePath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Profile %q (runtime=%s) is valid.\n", prof.Metadata.Name, prof.Runtime)
			return nil
		},
	}
	validateCmd.Flags().StringVar(&validatePath, "profile", "", "Path to BenchmarkProfile YAML (defaults to embedded reference profile)")

	profileCmd.AddCommand(initCmd, validateCmd)
	return profileCmd
}

func (a *App) newSuitesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suites",
		Short: "List registered benchmark suites and supported runtimes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, name := range a.Suites.Names() {
				s, _ := a.Suites.Get(name)
				rts := strings.Join(s.SupportedRuntimes(), ",")
				if rts == "" {
					rts = "all"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-22s [%s]  %s\n", s.Name(), rts, s.Description())
			}
			return nil
		},
	}
}

func (a *App) newWorkerCmd() *cobra.Command {
	workerCmd := &cobra.Command{
		Use:   "worker",
		Short: "In-cluster worker subcommands executed inside the Bench Runner Pod",
	}

	var opts workerRunOptions
	runWorkerCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute benchmark suites directly inside the cluster network",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.LeakAuditGraceSet = cmd.Flags().Changed("leak-audit-grace")
			return a.runWorker(cmd.Context(), opts)
		},
	}
	runWorkerCmd.Flags().StringVar(&opts.ProfilePath, "profile", "", "Path to BenchmarkProfile YAML")
	runWorkerCmd.Flags().StringVar(&opts.SuitesFlag, "suite", "all", "Comma-separated suite names or 'all'")
	runWorkerCmd.Flags().StringVar(&opts.OutputDir, "output", "/tmp/run-bundle", "Output directory inside Runner Pod")
	runWorkerCmd.Flags().StringVar(&opts.ServerDomain, "server", "", "In-cluster opensandbox-server host:port (defaults to opensandbox-server.<systemNamespace>.svc:80)")
	runWorkerCmd.Flags().StringVar(&opts.APIKey, "api-key", "", "OpenSandbox API key")
	runWorkerCmd.Flags().StringVar(&opts.APIKeyFile, "api-key-file", "", "Path to file containing OpenSandbox API key (preferred over --api-key)")
	runWorkerCmd.Flags().StringVar(&opts.RunID, "run-id", "", "Benchmark run identifier")
	runWorkerCmd.Flags().IntVar(&opts.Cycles, "cycles", 1, "Number of consecutive benchmark cycles to execute")
	runWorkerCmd.Flags().DurationVar(&opts.LeakAuditGrace, "leak-audit-grace", collector.DefaultLeakAuditGracePeriod, "Grace period to wait for async CR finalizer cleanup during LeakAudit")
	runWorkerCmd.Flags().BoolVar(&opts.AllowDegradedTelemetry, "allow-degraded-telemetry", false, "Allow degraded telemetry when RBAC or runtime pod probes are unavailable")
	runWorkerCmd.Flags().StringVar(&opts.OpenSandboxCommit, "opensandbox-commit", "", "OpenSandbox git commit SHA stamped into report provenance")
	runWorkerCmd.Flags().StringVar(&opts.OpenSandboxTag, "opensandbox-tag", "", "OpenSandbox git tag stamped into report provenance")
	runWorkerCmd.Flags().StringVar(&opts.FastSandboxCommit, "fast-sandbox-commit", "", "fast-sandbox git commit SHA stamped into report provenance")
	runWorkerCmd.Flags().StringVar(&opts.FastSandboxTag, "fast-sandbox-tag", "", "fast-sandbox git tag stamped into report provenance")
	runWorkerCmd.Flags().StringVar(&opts.BenchGitCommit, "bench-git-commit", "", "Benchmark harness git commit SHA stamped into report provenance")

	workerCmd.AddCommand(runWorkerCmd)
	return workerCmd
}

func (a *App) newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print opensandbox-bench build version metadata",
		Run: func(cmd *cobra.Command, _ []string) {
			version.Print(cmd.OutOrStdout(), "opensandbox-bench")
		},
	}
}

func defaultServerDomain(prof *profile.BenchmarkProfile) string {
	ns := prof.Environment.SystemNamespace
	if ns == "" {
		ns = "opensandbox-system"
	}
	return fmt.Sprintf("opensandbox-server.%s.svc:80", ns)
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
