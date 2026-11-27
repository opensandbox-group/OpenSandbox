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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const (
	defaultReadinessPollInterval = 10 * time.Millisecond
	defaultReadyTimeout          = 60 * time.Second
	defaultPauseTimeout          = 120 * time.Second
	defaultSandboxTTLSeconds     = 600
	defaultCleanupTimeout        = 15 * time.Second
	postResumeExecMaxAttempts    = 5
	postResumeExecRetryBackoff   = 150 * time.Millisecond
)

// CacheEvictor evicts node-side OverlayBD caches (fsb-vm blob-cache, manifest-cache,
// shared_snapfiles, and overlaybd-ublkd block cache) and re-warms /v1/pool to poolLow
// so ublk device cold-create is not conflated with storage cache misses (OSEP-0034 §3).
type CacheEvictor interface {
	EvictOverlayBDCaches(ctx context.Context, nodeName string) error
}

// NodeIsolator isolates the source node's fastlet(s) from FastPath placement so
// cross-node resume deterministically lands on a different node (OSEP-0034 §3).
type NodeIsolator interface {
	IsolateNodeFastlets(ctx context.Context, nodeName string) (restore func(context.Context) error, err error)
}

// NodeResolver resolves the Kubernetes node name where a sandbox is scheduled/running.
type NodeResolver interface {
	SandboxNode(ctx context.Context, sandboxID string) (string, error)
}

// MetricsSnapshotter captures before/after white-box OverlayBD and Prometheus metric deltas.
type MetricsSnapshotter interface {
	BeforeSuite(ctx context.Context, suiteName string) error
	AfterSuite(ctx context.Context, suiteName string) (*report.OverlayBDMetrics, map[string]float64, error)
}

// StepTelemetrySnapshot holds a synchronized point-in-time host memory,
// Firecracker smaps_rollup PSS/RSS, daemon RSS, and CPU/Load snapshot.
type StepTelemetrySnapshot struct {
	MemTotalMiB          float64 `json:"mem_total_mib,omitempty"`
	MemAvailableMiB      float64 `json:"mem_available_mib,omitempty"`
	CachedMiB            float64 `json:"cached_mib,omitempty"`
	BuffersMiB           float64 `json:"buffers_mib,omitempty"`
	SlabMiB              float64 `json:"slab_mib,omitempty"`
	SReclaimableMiB      float64 `json:"sreclaimable_mib,omitempty"`
	SUnreclaimMiB        float64 `json:"sunreclaim_mib,omitempty"`
	FCPIDCount           int     `json:"fc_pid_count,omitempty"`
	FCPssAvgMiB          float64 `json:"fc_pss_avg_mib,omitempty"`
	FCRssAvgMiB          float64 `json:"fc_rss_avg_mib,omitempty"`
	FastletRSSMiB        float64 `json:"fastlet_rss_mib,omitempty"`
	EgressRSSMiB         float64 `json:"egress_rss_mib,omitempty"`
	UblkdRSSMiB          float64 `json:"ublkd_rss_mib,omitempty"`
	ProxyRSSMiB          float64 `json:"proxy_rss_mib,omitempty"`
	CPUUtilPct           float64 `json:"cpu_util_pct,omitempty"`
	Load1m               float64 `json:"load_1m,omitempty"`
	CPUPressureSomeAvg60 float64 `json:"cpu_psi_some_avg60,omitempty"`
	MemPressureFullAvg60 float64 `json:"mem_psi_full_avg60,omitempty"`
}

// StepTelemetrySampler captures synchronous host memory and process snapshots
// (used by density-ramp baseline + per-step telemetry and high-density-soak).
type StepTelemetrySampler interface {
	Snapshot(ctx context.Context) (*StepTelemetrySnapshot, error)
}

// StepTelemetrySamplerFunc adapts a function to StepTelemetrySampler.
type StepTelemetrySamplerFunc func(ctx context.Context) (*StepTelemetrySnapshot, error)

func (f StepTelemetrySamplerFunc) Snapshot(ctx context.Context) (*StepTelemetrySnapshot, error) {
	return f(ctx)
}

// BenchHooks carries optional cluster-level hooks injected by the worker/integrator.
// All fields are optional; suites degrade gracefully or report when a hook is absent.
type BenchHooks struct {
	CacheEvictor          CacheEvictor
	NodeIsolator          NodeIsolator
	NodeResolver          NodeResolver
	MetricsSnapshotter    MetricsSnapshotter
	SoakLoadSampler       SoakLoadSampler
	StepTelemetrySampler  StepTelemetrySampler
	ActiveSandboxReporter func(count int)
	SoakNodeMemoryMiB     int
	SoakNodeMemorySource  string
	RequireNodeMemory     bool
	WakeResumeCounter     func(ctx context.Context) (int64, error)
	TemplateID            string
}

type benchHooksContextKey struct{}

// WithBenchHooks attaches optional BenchHooks to ctx for suites to consume without
// requiring RunContext schema changes.
func WithBenchHooks(ctx context.Context, hooks BenchHooks) context.Context {
	return context.WithValue(ctx, benchHooksContextKey{}, hooks)
}

// BenchHooksFromContext extracts BenchHooks previously attached via WithBenchHooks.
func BenchHooksFromContext(ctx context.Context) BenchHooks {
	if ctx == nil {
		return BenchHooks{}
	}
	if h, ok := ctx.Value(benchHooksContextKey{}).(BenchHooks); ok {
		return h
	}
	return BenchHooks{}
}

// SuiteOption configures optional suite dependencies (such as BenchHooks for tests or worker wiring).
type SuiteOption func(*suiteConfig)

type suiteConfig struct {
	hooks BenchHooks
}

// WithSuiteHooks supplies BenchHooks directly to a suite constructor.
func WithSuiteHooks(hooks BenchHooks) SuiteOption {
	return func(c *suiteConfig) {
		c.hooks = hooks
	}
}

func resolveHooks(ctx context.Context, cfg suiteConfig) BenchHooks {
	merged := BenchHooksFromContext(ctx)
	if cfg.hooks.CacheEvictor != nil {
		merged.CacheEvictor = cfg.hooks.CacheEvictor
	}
	if cfg.hooks.NodeIsolator != nil {
		merged.NodeIsolator = cfg.hooks.NodeIsolator
	}
	if cfg.hooks.NodeResolver != nil {
		merged.NodeResolver = cfg.hooks.NodeResolver
	}
	if cfg.hooks.MetricsSnapshotter != nil {
		merged.MetricsSnapshotter = cfg.hooks.MetricsSnapshotter
	}
	if cfg.hooks.SoakLoadSampler != nil {
		merged.SoakLoadSampler = cfg.hooks.SoakLoadSampler
	}
	if cfg.hooks.StepTelemetrySampler != nil {
		merged.StepTelemetrySampler = cfg.hooks.StepTelemetrySampler
	}
	if cfg.hooks.ActiveSandboxReporter != nil {
		merged.ActiveSandboxReporter = cfg.hooks.ActiveSandboxReporter
	}
	if cfg.hooks.SoakNodeMemoryMiB > 0 {
		merged.SoakNodeMemoryMiB = cfg.hooks.SoakNodeMemoryMiB
	}
	if cfg.hooks.SoakNodeMemorySource != "" {
		merged.SoakNodeMemorySource = cfg.hooks.SoakNodeMemorySource
	}
	if cfg.hooks.RequireNodeMemory {
		merged.RequireNodeMemory = true
	}
	if cfg.hooks.WakeResumeCounter != nil {
		merged.WakeResumeCounter = cfg.hooks.WakeResumeCounter
	}
	if cfg.hooks.TemplateID != "" {
		merged.TemplateID = cfg.hooks.TemplateID
	}
	return merged
}

// ClientOption configures a BenchClient.
type ClientOption func(*BenchClient)

// WithClientHooks attaches BenchHooks to a BenchClient.
func WithClientHooks(hooks BenchHooks) ClientOption {
	return func(c *BenchClient) {
		c.hooks = hooks
	}
}

// WithClientLogger sets the structured logger on a BenchClient.
func WithClientLogger(logger *slog.Logger) ClientOption {
	return func(c *BenchClient) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// BenchClient wraps sdks/sandbox/go lifecycle & execd clients with benchmark-grade
// timing (exact t_api_ack with Running vs Pending classification, 429/Retry-After detection,
// 10ms guest /ping polling for t_first_ping, and first-exec verification).
type BenchClient struct {
	cfg       opensandbox.ConnectionConfig
	lifecycle *opensandbox.LifecycleClient
	hooks     BenchHooks
	logger    *slog.Logger
}

// NewBenchClient constructs a BenchClient from an opensandbox.ConnectionConfig.
func NewBenchClient(cfg opensandbox.ConnectionConfig, opts ...ClientOption) *BenchClient {
	clientOpts := buildSDKClientOpts(cfg, true, cfg.Headers)
	baseURL := cfg.GetBaseURL() + "/" + opensandbox.APIVersion
	// Disable SDK endpoint cache so post-resume endpoint lookups never observe stale routes.
	lc := opensandbox.NewLifecycleClientWithCache(baseURL, cfg.GetAPIKey(), nil, clientOpts...)

	bc := &BenchClient{
		cfg:       cfg,
		lifecycle: lc,
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(bc)
	}
	return bc
}

// Lifecycle returns the underlying SDK LifecycleClient.
func (c *BenchClient) Lifecycle() *opensandbox.LifecycleClient {
	return c.lifecycle
}

// Hooks returns the configured BenchHooks.
func (c *BenchClient) Hooks() BenchHooks {
	return c.hooks
}

// CreateOptions configures a single benchmark sandbox creation + readiness probe.
type CreateOptions struct {
	TemplateID            string
	Image                 string
	TimeoutSeconds        int
	ResourceLimits        opensandbox.ResourceLimits
	Metadata              map[string]string
	Extensions            map[string]string
	ReadinessPollInterval time.Duration
	ReadyTimeout          time.Duration
	FirstExecCommand      string
	SkipFirstExec         bool
}

// CreateResult captures detailed black-box lifecycle timings for a sandbox create.
type CreateResult struct {
	SandboxID         string
	AckState          string // "Running" or "Pending"
	APIAckMs          float64
	EndpointResolveMs float64
	FirstPingMs       float64
	FirstExecMs       float64
	ExecMs            float64
	ExecOutput        string
	Execd             *opensandbox.ExecdClient
	StatusCode        int
	Is429             bool
	RetryAfter        time.Duration
	ErrorCode         string
}

// ExecResult captures command execution output and latency.
type ExecResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	HasExit    bool
	DurationMs float64
}

// PauseResult captures timings for an async Pause operation.
type PauseResult struct {
	APIAckMs float64
	TotalMs  float64
}

// ResumeResult captures timings for an async Resume operation.
type ResumeResult struct {
	APIAckMs    float64
	FirstPingMs float64
	Execd       *opensandbox.ExecdClient
}

// ResolveTemplateID determines the golden template ID to use for sandbox creation.
// Priority: BenchHooks.TemplateID -> profile.Extensions["templateId"] -> first Succeeded
// template from GET /v1/templates (preferring a baseImage match). Returns "" if none exists
// so callers can fall back to direct image creation.
func (c *BenchClient) ResolveTemplateID(ctx context.Context, prof *profile.BenchmarkProfile) (string, error) {
	if c.hooks.TemplateID != "" {
		return c.hooks.TemplateID, nil
	}
	if prof != nil && prof.Extensions != nil {
		if v, ok := prof.Extensions["templateId"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}

	resp, err := c.lifecycle.ListTemplates(ctx, opensandbox.ListTemplatesOptions{PageSize: 50})
	if err != nil {
		// Servers without /v1/templates or direct-image test servers may return 404/405.
		var apiErr *opensandbox.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) {
			return "", nil
		}
		return "", err
	}

	var fallback string
	wantImage := ""
	if prof != nil {
		wantImage = prof.Stack.Template.BaseImage
	}
	for _, tpl := range resp.Items {
		if tpl.Status.Phase != opensandbox.TemplatePhaseSucceeded {
			continue
		}
		if wantImage != "" && tpl.Image == wantImage {
			return tpl.TemplateID, nil
		}
		if fallback == "" {
			fallback = tpl.TemplateID
		}
	}
	return fallback, nil
}

// CreateSandbox executes POST /v1/sandboxes, records exact t_api_ack and Running/Pending
// ack state, polls guest /ping at ReadinessPollInterval for t_first_ping, and optionally
// runs a first-exec command for t_first_exec.
func (c *BenchClient) CreateSandbox(ctx context.Context, opts CreateOptions) (*CreateResult, error) {
	res := &CreateResult{}

	timeoutSec := opts.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = defaultSandboxTTLSeconds
	}
	pollInterval := opts.ReadinessPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultReadinessPollInterval
	}
	readyTimeout := opts.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = defaultReadyTimeout
	}

	req := opensandbox.CreateSandboxRequest{
		Timeout:    &timeoutSec,
		Metadata:   opts.Metadata,
		Extensions: opts.Extensions,
	}
	if opts.TemplateID != "" {
		req.TemplateID = opts.TemplateID
	} else {
		image := opts.Image
		if image == "" {
			image = "ubuntu:22.04"
		}
		req.Image = &opensandbox.ImageSpec{URI: image}
		req.Entrypoint = opensandbox.DefaultEntrypoint
		if len(opts.ResourceLimits) > 0 {
			req.ResourceLimits = opts.ResourceLimits
		} else {
			req.ResourceLimits = opensandbox.DefaultResourceLimits
		}
	}

	tStart := time.Now()
	info, err := c.lifecycle.CreateSandbox(ctx, req)
	res.APIAckMs = msSince(tStart)
	if err != nil {
		res.ErrorCode, res.StatusCode, res.Is429, res.RetryAfter = ClassifyError(err)
		return res, err
	}

	res.SandboxID = info.ID
	if info.Status.State == opensandbox.StateRunning {
		res.AckState = string(opensandbox.StateRunning)
	} else {
		res.AckState = string(opensandbox.StatePending)
	}

	execd, resolveAt, err := c.waitFirstPingTimed(ctx, info.ID, pollInterval, readyTimeout)
	if !resolveAt.IsZero() {
		res.EndpointResolveMs = float64(resolveAt.Sub(tStart).Microseconds()) / 1000.0
	}
	if err != nil {
		res.ErrorCode, res.StatusCode, res.Is429, res.RetryAfter = ClassifyError(err)
		return res, fmt.Errorf("wait first ping for sandbox %s: %w", info.ID, err)
	}
	res.FirstPingMs = msSince(tStart)
	res.Execd = execd

	if !opts.SkipFirstExec {
		cmd := opts.FirstExecCommand
		if cmd == "" {
			cmd = "echo opensandbox-bench-ok"
		}
		execRes, err := c.ExecCommand(ctx, execd, cmd)
		if err != nil {
			res.ErrorCode, res.StatusCode, res.Is429, res.RetryAfter = ClassifyError(err)
			return res, fmt.Errorf("first exec for sandbox %s: %w", info.ID, err)
		}
		res.FirstExecMs = msSince(tStart)
		res.ExecMs = execRes.DurationMs
		res.ExecOutput = execRes.Stdout
	}

	return res, nil
}

// WaitFirstPing resolves the sandbox execd endpoint and polls GET /ping every pollInterval
// until a 200 OK is observed or timeout expires.
func (c *BenchClient) WaitFirstPing(ctx context.Context, sandboxID string, pollInterval, timeout time.Duration) (*opensandbox.ExecdClient, error) {
	execd, _, err := c.waitFirstPingTimed(ctx, sandboxID, pollInterval, timeout)
	return execd, err
}

func (c *BenchClient) waitFirstPingTimed(ctx context.Context, sandboxID string, pollInterval, timeout time.Duration) (*opensandbox.ExecdClient, time.Time, error) {
	if pollInterval <= 0 {
		pollInterval = defaultReadinessPollInterval
	}
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		execd     *opensandbox.ExecdClient
		resolveAt time.Time
		lastErr   error
	)
	for {
		if err := waitCtx.Err(); err != nil {
			if lastErr != nil {
				return nil, resolveAt, fmt.Errorf("readiness timeout (%w): last error: %v", err, lastErr)
			}
			return nil, resolveAt, err
		}

		if execd == nil {
			resolved, err := c.ResolveExecd(waitCtx, sandboxID)
			if err != nil {
				lastErr = err
				if !isRetryableReadinessError(err) {
					return nil, resolveAt, err
				}
				if err := sleepWithContext(waitCtx, pollInterval); err != nil {
					return nil, resolveAt, fmt.Errorf("readiness timeout (%w): last error: %v", err, lastErr)
				}
				continue
			}
			execd = resolved
			if resolveAt.IsZero() {
				resolveAt = time.Now()
			}
		}

		if err := execd.Ping(waitCtx); err == nil {
			return execd, resolveAt, nil
		} else {
			lastErr = err
			// If the endpoint itself returned 404/410 or route error, re-resolve on next tick.
			var apiErr *opensandbox.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusGone) {
				execd = nil
			}
		}

		if err := sleepWithContext(waitCtx, pollInterval); err != nil {
			return nil, resolveAt, fmt.Errorf("readiness timeout (%w): last error: %v", err, lastErr)
		}
	}
}

// ResolveExecd fetches the execd endpoint for sandboxID from opensandbox-server and
// constructs a direct (or server-proxied) ExecdClient.
func (c *BenchClient) ResolveExecd(ctx context.Context, sandboxID string) (*opensandbox.ExecdClient, error) {
	useProxy := c.cfg.UseServerProxy
	ep, err := c.lifecycle.GetEndpoint(ctx, sandboxID, opensandbox.DefaultExecdPort, &useProxy)
	if err != nil {
		return nil, err
	}

	execdURL := c.cfg.RewriteEndpointURL(ep.Endpoint)
	if !strings.HasPrefix(execdURL, "http://") && !strings.HasPrefix(execdURL, "https://") {
		execdURL = c.cfg.GetProtocol() + "://" + execdURL
	}

	headers := mergeDataPlaneHeaders(c.cfg, ep.Headers)
	opts := buildSDKClientOpts(c.cfg, false, headers)
	return opensandbox.NewExecdClient(execdURL, "", opts...), nil
}

// ExecCommand executes a foreground shell command via execd and collects stdout, stderr, and exit code.
func (c *BenchClient) ExecCommand(ctx context.Context, execd *opensandbox.ExecdClient, command string) (*ExecResult, error) {
	return c.runCommand(ctx, execd, opensandbox.RunCommandRequest{Command: command})
}

// ExecCommandWithRetry runs ExecCommand up to maxAttempts times, retrying with
// backoff on transient HTTP 503 / upstream-unavailable route-settle errors.
func (c *BenchClient) ExecCommandWithRetry(
	ctx context.Context,
	execd *opensandbox.ExecdClient,
	command string,
	maxAttempts int,
	backoff time.Duration,
) (*ExecResult, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var (
		res *ExecResult
		err error
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err = c.ExecCommand(ctx, execd, command)
		if err == nil {
			return res, nil
		}
		if attempt == maxAttempts || !isTransientExecError(err) {
			return res, err
		}
		if backoff > 0 {
			if sleepErr := sleepWithContext(ctx, backoff); sleepErr != nil {
				return res, err
			}
		}
	}
	return res, err
}

func isTransientExecError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *opensandbox.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "503") || strings.Contains(lower, "service unavailable")
}

// ExecBackground starts a background shell command via execd.
func (c *BenchClient) ExecBackground(ctx context.Context, execd *opensandbox.ExecdClient, command string) (*ExecResult, error) {
	return c.runCommand(ctx, execd, opensandbox.RunCommandRequest{Command: command, Background: true})
}

func (c *BenchClient) runCommand(ctx context.Context, execd *opensandbox.ExecdClient, req opensandbox.RunCommandRequest) (*ExecResult, error) {
	if execd == nil {
		return nil, fmt.Errorf("execd client is nil")
	}

	tStart := time.Now()
	var (
		stdoutLines []string
		stderrLines []string
		exitCode    int
		hasExit     bool
		execErrMsg  string
		completed   bool
	)

	err := execd.RunCommand(ctx, req, func(event opensandbox.StreamEvent) error {
		if event.Data == "" {
			return nil
		}
		var ev struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ExitCode *int   `json:"exit_code,omitempty"`
			Error    *struct {
				EName    string `json:"ename,omitempty"`
				EValue   string `json:"evalue,omitempty"`
				ExitCode *int   `json:"exit_code,omitempty"`
			} `json:"error,omitempty"`
			EName  string `json:"ename,omitempty"`
			EValue string `json:"evalue,omitempty"`
		}
		if json.Unmarshal([]byte(event.Data), &ev) != nil {
			stdoutLines = append(stdoutLines, event.Data)
			return nil
		}
		switch ev.Type {
		case "stdout":
			stdoutLines = append(stdoutLines, ev.Text)
		case "stderr":
			stderrLines = append(stderrLines, ev.Text)
		case "execution_complete":
			completed = true
			if ev.ExitCode != nil {
				exitCode = *ev.ExitCode
				hasExit = true
			}
		case "error":
			if ev.Error != nil {
				execErrMsg = strings.TrimSpace(ev.Error.EName + ": " + ev.Error.EValue)
				if ev.Error.ExitCode != nil {
					exitCode = *ev.Error.ExitCode
					hasExit = true
				}
			} else {
				execErrMsg = strings.TrimSpace(ev.EName + ": " + ev.EValue)
			}
		}
		return nil
	})

	res := &ExecResult{
		Stdout:     strings.Join(stdoutLines, "\n"),
		Stderr:     strings.Join(stderrLines, "\n"),
		ExitCode:   exitCode,
		HasExit:    hasExit,
		DurationMs: msSince(tStart),
	}
	if err != nil {
		return res, err
	}
	if execErrMsg != "" {
		return res, fmt.Errorf("execd command error: %s", execErrMsg)
	}
	if !req.Background && !completed && len(stdoutLines) == 0 && len(stderrLines) == 0 {
		return res, opensandbox.ErrIncompleteCommand
	}
	if hasExit && exitCode != 0 {
		return res, fmt.Errorf("command exited with code %d (stderr=%q)", exitCode, res.Stderr)
	}
	return res, nil
}

// Pause issues POST /v1/sandboxes/{id}/pause and polls GET /v1/sandboxes/{id} until state == Paused.
func (c *BenchClient) Pause(ctx context.Context, sandboxID string, pollInterval, timeout time.Duration) (*PauseResult, error) {
	if pollInterval <= 0 {
		pollInterval = defaultReadinessPollInterval
	}
	if timeout <= 0 {
		timeout = defaultPauseTimeout
	}

	tStart := time.Now()
	if err := c.lifecycle.PauseSandbox(ctx, sandboxID); err != nil {
		return &PauseResult{APIAckMs: msSince(tStart)}, err
	}
	apiAckMs := msSince(tStart)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		info, err := c.lifecycle.GetSandbox(waitCtx, sandboxID)
		if err != nil {
			return &PauseResult{APIAckMs: apiAckMs, TotalMs: msSince(tStart)}, err
		}
		switch info.Status.State {
		case opensandbox.StatePaused:
			return &PauseResult{APIAckMs: apiAckMs, TotalMs: msSince(tStart)}, nil
		case opensandbox.StateFailed, opensandbox.StateTerminated:
			return &PauseResult{APIAckMs: apiAckMs, TotalMs: msSince(tStart)},
				fmt.Errorf("sandbox %s entered terminal state %s while pausing (%s)", sandboxID, info.Status.State, info.Status.Reason)
		}
		if err := sleepWithContext(waitCtx, pollInterval); err != nil {
			return &PauseResult{APIAckMs: apiAckMs, TotalMs: msSince(tStart)}, fmt.Errorf("pause timeout for sandbox %s: %w", sandboxID, err)
		}
	}
}

// Resume issues POST /v1/sandboxes/{id}/resume and polls guest /ping at pollInterval
// until the restored sandbox answers 200 OK (authoritative t_first_ping).
func (c *BenchClient) Resume(ctx context.Context, sandboxID string, pollInterval, timeout time.Duration) (*ResumeResult, error) {
	if pollInterval <= 0 {
		pollInterval = defaultReadinessPollInterval
	}
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}

	tStart := time.Now()
	if err := c.lifecycle.ResumeSandbox(ctx, sandboxID); err != nil {
		return &ResumeResult{APIAckMs: msSince(tStart)}, err
	}
	apiAckMs := msSince(tStart)

	execd, err := c.WaitFirstPing(ctx, sandboxID, pollInterval, timeout)
	if err != nil {
		return &ResumeResult{APIAckMs: apiAckMs, FirstPingMs: msSince(tStart)}, err
	}
	return &ResumeResult{
		APIAckMs:    apiAckMs,
		FirstPingMs: msSince(tStart),
		Execd:       execd,
	}, nil
}

// Delete terminates a sandbox.
func (c *BenchClient) Delete(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return nil
	}
	return c.lifecycle.DeleteSandbox(ctx, sandboxID)
}

// BestEffortDelete terminates a sandbox with an independent bounded timeout so
// cleanup still runs if the caller context was canceled.
func (c *BenchClient) BestEffortDelete(sandboxID string) {
	if sandboxID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()
	if err := c.lifecycle.DeleteSandbox(ctx, sandboxID); err != nil && c.logger != nil {
		c.logger.Warn("best-effort sandbox delete failed", "sandbox_id", sandboxID, "error", err)
	}
}

// GetSandboxNode returns the node where the sandbox is currently placed.
// Prefers BenchHooks.NodeResolver when available, falling back to well-known
// metadata/extension keys returned by GET /v1/sandboxes/{id}.
func (c *BenchClient) GetSandboxNode(ctx context.Context, sandboxID string) (string, error) {
	if c.hooks.NodeResolver != nil {
		node, err := c.hooks.NodeResolver.SandboxNode(ctx, sandboxID)
		if err == nil && node != "" {
			return node, nil
		}
		if err != nil {
			return "", err
		}
	}

	info, err := c.lifecycle.GetSandbox(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	for _, m := range []map[string]string{info.Extensions, info.Metadata} {
		for _, key := range []string{"fast-sandbox.io/node", "nodeName", "node"} {
			if v := strings.TrimSpace(m[key]); v != "" {
				return v, nil
			}
		}
	}
	return "", nil
}

// ClassifyError maps an error into a low-cardinality benchmark error code plus HTTP metadata.
func ClassifyError(err error) (code string, statusCode int, is429 bool, retryAfter time.Duration) {
	if err == nil {
		return "", 0, false, 0
	}
	var apiErr *opensandbox.APIError
	if errors.As(err, &apiErr) {
		statusCode = apiErr.StatusCode
		retryAfter = apiErr.RetryAfter
		if apiErr.StatusCode == http.StatusTooManyRequests {
			return "http_429", statusCode, true, retryAfter
		}
		if apiErr.Response.Code != "" {
			return fmt.Sprintf("http_%d_%s", apiErr.StatusCode, sanitizeErrorCode(apiErr.Response.Code)), statusCode, false, retryAfter
		}
		return fmt.Sprintf("http_%d", apiErr.StatusCode), statusCode, false, retryAfter
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", 0, false, 0
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", 0, false, 0
	}
	return "client_error", 0, false, 0
}

func sanitizeErrorCode(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "::", "_")
	return s
}

func isRetryableReadinessError(err error) bool {
	var apiErr *opensandbox.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func buildSDKClientOpts(cfg opensandbox.ConnectionConfig, includeAuthHeader bool, headers map[string]string) []opensandbox.Option {
	var opts []opensandbox.Option
	if includeAuthHeader && cfg.AuthHeader != "" {
		opts = append(opts, opensandbox.WithAuthHeader(cfg.AuthHeader))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, opensandbox.WithHTTPClient(cfg.HTTPClient))
	} else if cfg.Transport != nil {
		opts = append(opts, opensandbox.WithHTTPClient(&http.Client{
			Transport: cfg.Transport.NewTransport(),
		}))
	}
	if t := cfg.GetRequestTimeout(); t > 0 {
		opts = append(opts, opensandbox.WithTimeout(t))
	}
	if len(headers) > 0 {
		opts = append(opts, opensandbox.WithHeaders(headers))
	}
	if cfg.Retry != nil {
		opts = append(opts, opensandbox.WithRetry(*cfg.Retry))
	}
	return opts
}

func mergeDataPlaneHeaders(cfg opensandbox.ConnectionConfig, endpointHeaders map[string]string) map[string]string {
	headers := make(map[string]string, len(cfg.Headers)+len(endpointHeaders)+1)
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	for k, v := range endpointHeaders {
		for existing := range headers {
			if strings.EqualFold(existing, k) {
				delete(headers, existing)
			}
		}
		headers[k] = v
	}
	if cfg.UseServerProxy {
		hasAuth := false
		for existing := range headers {
			if strings.EqualFold(existing, "OPEN-SANDBOX-API-KEY") {
				hasAuth = true
				break
			}
		}
		if !hasAuth && cfg.GetAPIKey() != "" {
			headers["OPEN-SANDBOX-API-KEY"] = cfg.GetAPIKey()
		}
	} else {
		for existing := range headers {
			if strings.EqualFold(existing, "OPEN-SANDBOX-API-KEY") {
				delete(headers, existing)
			}
		}
	}
	return headers
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

func newOperationSample(op, sandboxID string, latencyMs float64, ackState string, err error) OperationSample {
	s := OperationSample{
		Operation: op,
		SandboxID: sandboxID,
		TraceID:   "",
		Sampled:   false,
		LatencyMs: latencyMs,
		Status:    "ok",
		AckState:  ackState,
	}
	if err != nil {
		s.Status = "error"
		s.ErrorCode, _, _, _ = ClassifyError(err)
	}
	return s
}

func loggerFromRunContext(rc RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}

func templateResourceLimits(prof *profile.BenchmarkProfile) opensandbox.ResourceLimits {
	if prof == nil {
		return opensandbox.DefaultResourceLimits
	}
	limits := opensandbox.ResourceLimits{}
	if prof.Stack.Template.VCPUs > 0 {
		limits["cpu"] = fmt.Sprintf("%d", prof.Stack.Template.VCPUs)
	}
	if prof.Stack.Template.MemoryMiB > 0 {
		limits["memory"] = fmt.Sprintf("%dMi", prof.Stack.Template.MemoryMiB)
	}
	if len(limits) == 0 {
		return opensandbox.DefaultResourceLimits
	}
	return limits
}
