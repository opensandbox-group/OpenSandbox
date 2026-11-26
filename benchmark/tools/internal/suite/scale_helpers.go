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
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

const (
	scaleDefaultPollInterval = 10 * time.Millisecond
	scaleDefaultTimeoutSec   = 600
	scaleDefaultReadyTimeout = 120 * time.Second
	scaleDefaultSnapTimeout  = 180 * time.Second
	scaleDefaultPauseTimeout = 180 * time.Second
)

// NodeIsolatorFunc isolates a source node's fastlet from FastPath placement
// before a cross-node create/restore test, returning a cleanup function to
// restore placement afterward (OSEP-0034 §3).
type NodeIsolatorFunc func(ctx context.Context, nodeName string) (cleanup func(context.Context) error, err error)

// scaleClient wraps the OpenSandbox Go SDK and a raw HTTP client for
// black-box timing of lifecycle, snapshot, ingress, and execd operations.
type scaleClient struct {
	cfg       opensandbox.ConnectionConfig
	lifecycle *opensandbox.LifecycleClient
	http      *http.Client
	logger    *slog.Logger
}

func newScaleClient(rc RunContext) *scaleClient {
	cfg := rc.SDKConfig
	// Disable endpoint caching so cross-node restores and wake-storm probes
	// always observe fresh endpoints.
	cfg.EndpointCacheDisabled = true

	baseURL := cfg.GetBaseURL() + "/" + opensandbox.APIVersion
	var opts []opensandbox.Option
	if cfg.AuthHeader != "" {
		opts = append(opts, opensandbox.WithAuthHeader(cfg.AuthHeader))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, opensandbox.WithHTTPClient(cfg.HTTPClient))
	}
	if t := cfg.GetRequestTimeout(); t > 0 {
		opts = append(opts, opensandbox.WithTimeout(t))
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, opensandbox.WithHeaders(cfg.Headers))
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: cfg.GetRequestTimeout(),
			Transport: &http.Transport{
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 256,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	logger := rc.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &scaleClient{
		cfg:       cfg,
		lifecycle: opensandbox.NewLifecycleClient(baseURL, cfg.GetAPIKey(), opts...),
		http:      httpClient,
		logger:    logger,
	}
}

// buildBaseCreateRequest constructs a CreateSandboxRequest from the profile's
// template/image configuration. If profile.Extensions["templateId"] is set,
// template mode is used; otherwise image mode with resourceLimits is used.
func scaleBuildBaseCreateRequest(prof *profile.BenchmarkProfile, timeoutSec int, lifecycle *opensandbox.SandboxLifecycle) opensandbox.CreateSandboxRequest {
	if timeoutSec <= 0 {
		timeoutSec = scaleDefaultTimeoutSec
	}
	t := timeoutSec

	if prof != nil && prof.Extensions != nil {
		if tid, ok := prof.Extensions["templateId"].(string); ok && strings.TrimSpace(tid) != "" {
			return opensandbox.CreateSandboxRequest{
				TemplateID: strings.TrimSpace(tid),
				Timeout:    &t,
				Lifecycle:  lifecycle,
			}
		}
	}

	baseImage := "python:3.12-bookworm"
	vcpus := 1
	memMiB := 2048
	if prof != nil {
		if prof.Stack.Template.BaseImage != "" {
			baseImage = prof.Stack.Template.BaseImage
		}
		if prof.Stack.Template.VCPUs > 0 {
			vcpus = prof.Stack.Template.VCPUs
		}
		if prof.Stack.Template.MemoryMiB > 0 {
			memMiB = prof.Stack.Template.MemoryMiB
		}
	}

	return opensandbox.CreateSandboxRequest{
		Image:      &opensandbox.ImageSpec{URI: baseImage},
		Entrypoint: opensandbox.DefaultEntrypoint,
		Timeout:    &t,
		ResourceLimits: opensandbox.ResourceLimits{
			"cpu":    strconv.Itoa(vcpus),
			"memory": fmt.Sprintf("%dMi", memMiB),
		},
		Lifecycle: lifecycle,
	}
}

func (c *scaleClient) resolveExecdEndpoint(ctx context.Context, sandboxID string) (string, map[string]string, error) {
	ep, err := c.lifecycle.GetEndpoint(ctx, sandboxID, opensandbox.DefaultExecdPort, nil)
	if err != nil {
		return "", nil, err
	}
	raw := c.cfg.RewriteEndpointURL(strings.TrimSpace(ep.Endpoint))
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = c.cfg.GetProtocol() + "://" + raw
	}
	raw = strings.TrimRight(raw, "/")
	return raw, ep.Headers, nil
}

// waitPing polls GET <endpointURL>/ping until it receives HTTP 200.
func (c *scaleClient) waitPing(ctx context.Context, endpointURL string, headers map[string]string, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		pollInterval = scaleDefaultPollInterval
	}
	pingURL := strings.TrimRight(endpointURL, "/") + "/ping"
	for {
		status, _, _, err := c.probeURL(ctx, pingURL, headers)
		if err == nil && status == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("wait ping %s: %w (last err: %v)", pingURL, ctx.Err(), err)
			}
			return fmt.Errorf("wait ping %s: %w (last status: %d)", pingURL, ctx.Err(), status)
		case <-time.After(pollInterval):
		}
	}
}

// probeURL performs a single HTTP GET without automatic retries so callers can
// observe exact status codes (200, 429, 503 + Retry-After, etc.) and latency.
func (c *scaleClient) probeURL(ctx context.Context, targetURL string, headers map[string]string) (int, http.Header, float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		return 0, nil, elapsedMs, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, resp.Header.Clone(), elapsedMs, nil
}

// runCommand executes a shell command against execd and returns combined stdout.
func (c *scaleClient) runCommand(ctx context.Context, endpointURL string, headers map[string]string, command string) (string, error) {
	opts := []opensandbox.Option{
		opensandbox.WithHTTPClient(c.http),
	}
	if len(headers) > 0 {
		opts = append(opts, opensandbox.WithHeaders(headers))
	}
	execd := opensandbox.NewExecdClient(endpointURL, "", opts...)
	var out strings.Builder
	var cmdErr string
	err := execd.RunCommand(ctx, opensandbox.RunCommandRequest{
		Command: command,
	}, func(ev opensandbox.StreamEvent) error {
		var payload struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Err  struct {
				Name  string `json:"ename"`
				Value string `json:"evalue"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(ev.Data), &payload) == nil && payload.Type != "" {
			switch payload.Type {
			case "stdout":
				out.WriteString(payload.Text)
			case "error":
				cmdErr = fmt.Sprintf("%s: %s", payload.Err.Name, payload.Err.Value)
			}
			return nil
		}
		if ev.Event == "stdout" || ev.Event == "" {
			out.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		return out.String(), err
	}
	if cmdErr != "" {
		return out.String(), fmt.Errorf("execd command error: %s", cmdErr)
	}
	return out.String(), nil
}

func (c *scaleClient) waitSnapshotReady(ctx context.Context, snapshotID string, pollInterval time.Duration) (*opensandbox.SnapshotInfo, error) {
	if pollInterval <= 0 {
		pollInterval = scaleDefaultPollInterval
	}
	for {
		info, err := c.lifecycle.GetSnapshot(ctx, snapshotID)
		if err != nil {
			return nil, err
		}
		switch info.Status.State {
		case opensandbox.SnapshotStateReady:
			return info, nil
		case opensandbox.SnapshotStateFailed:
			return info, fmt.Errorf("snapshot %s failed: %s (%s)", snapshotID, info.Status.Message, info.Status.Reason)
		}
		select {
		case <-ctx.Done():
			return info, fmt.Errorf("wait snapshot %s ready: %w (last state: %s)", snapshotID, ctx.Err(), info.Status.State)
		case <-time.After(pollInterval):
		}
	}
}

func (c *scaleClient) waitSandboxState(ctx context.Context, sandboxID string, target opensandbox.SandboxState, pollInterval time.Duration) (*opensandbox.SandboxInfo, error) {
	if pollInterval <= 0 {
		pollInterval = scaleDefaultPollInterval
	}
	for {
		info, err := c.lifecycle.GetSandbox(ctx, sandboxID)
		if err != nil {
			return nil, err
		}
		if info.Status.State == target {
			return info, nil
		}
		if info.Status.State == opensandbox.StateFailed || info.Status.State == opensandbox.StateTerminated {
			return info, fmt.Errorf("sandbox %s entered terminal state %s while waiting for %s: %s", sandboxID, info.Status.State, target, info.Status.Message)
		}
		select {
		case <-ctx.Done():
			return info, fmt.Errorf("wait sandbox %s state %s: %w (last state: %s)", sandboxID, target, ctx.Err(), info.Status.State)
		case <-time.After(pollInterval):
		}
	}
}

// waitSandboxRunningAndPing waits until the sandbox is reachable via execd /ping,
// resolving the endpoint as soon as available.
func (c *scaleClient) waitSandboxRunningAndPing(ctx context.Context, sandboxID string, pollInterval time.Duration) (string, map[string]string, error) {
	if pollInterval <= 0 {
		pollInterval = scaleDefaultPollInterval
	}
	var epURL string
	var epHeaders map[string]string
	for {
		var err error
		epURL, epHeaders, err = c.resolveExecdEndpoint(ctx, sandboxID)
		if err == nil && epURL != "" {
			break
		}
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("resolve endpoint for sandbox %s: %w (last err: %v)", sandboxID, ctx.Err(), err)
		case <-time.After(pollInterval):
		}
	}
	if err := c.waitPing(ctx, epURL, epHeaders, pollInterval); err != nil {
		return epURL, epHeaders, err
	}
	return epURL, epHeaders, nil
}

func (c *scaleClient) deleteSandboxBestEffort(sandboxID string) {
	if sandboxID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.lifecycle.DeleteSandbox(ctx, sandboxID)
}

func (c *scaleClient) deleteSnapshotBestEffort(snapshotID string) {
	if snapshotID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.lifecycle.DeleteSnapshot(ctx, snapshotID)
}

func scaleExtractNode(info *opensandbox.SandboxInfo) string {
	if info == nil {
		return ""
	}
	keys := []string{"node", "nodeName", "fast-sandbox.io/node", "kubernetes.io/hostname"}
	for _, k := range keys {
		if v := strings.TrimSpace(info.Metadata[k]); v != "" {
			return v
		}
	}
	for _, k := range keys {
		if v := strings.TrimSpace(info.Extensions[k]); v != "" {
			return v
		}
	}
	return ""
}

func scaleParseRetryAfter(h http.Header, fallback, maxCap time.Duration) time.Duration {
	if h == nil {
		return fallback
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return fallback
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil && secs >= 0 {
		d := time.Duration(secs * float64(time.Second))
		if maxCap > 0 && d > maxCap {
			return maxCap
		}
		return d
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		if maxCap > 0 && d > maxCap {
			return maxCap
		}
		return d
	}
	return fallback
}

func scaleIsHTTPStatus(err error, code int) bool {
	var apiErr *opensandbox.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == code
	}
	return false
}

func scaleErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *opensandbox.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("http_%d", apiErr.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "error"
}

func scaleElapsedMs(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

// scaleRecorder safely aggregates operation samples, named latency vectors,
// and error counts across concurrent goroutines.
type scaleRecorder struct {
	mu              sync.Mutex
	samples         []OperationSample
	latencies       map[string][]float64
	errors          map[string]int
	totalOps        int
	succeededOps    int
	failedOps       int
	pendingFallback int
}

func newScaleRecorder() *scaleRecorder {
	return &scaleRecorder{
		latencies: make(map[string][]float64),
		errors:    make(map[string]int),
	}
}

func (r *scaleRecorder) addLatency(name string, ms float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latencies[name] = append(r.latencies[name], ms)
}

func (r *scaleRecorder) recordOp(sample OperationSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, sample)
	r.totalOps++
	if sample.AckState == string(opensandbox.StatePending) {
		r.pendingFallback++
	}
	if sample.Status == "ok" {
		r.succeededOps++
	} else {
		r.failedOps++
		code := sample.ErrorCode
		if code == "" {
			code = "error"
		}
		r.errors[code]++
	}
}
