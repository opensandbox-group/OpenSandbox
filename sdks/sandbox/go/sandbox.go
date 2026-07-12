// Copyright 2026 Alibaba Group Holding Ltd.
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

package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SandboxCreateOptions configures sandbox creation.
type SandboxCreateOptions struct {
	// Image is the container image URI (required).
	Image string
	// SnapshotID restores the sandbox from a previously created snapshot.
	SnapshotID string

	// Entrypoint is the command to run. Defaults to DefaultEntrypoint.
	Entrypoint []string

	// Resource limits (e.g. {"cpu": "500m", "memory": "256Mi"}).
	// Defaults to DefaultResourceLimits.
	ResourceLimits ResourceLimits

	// ResourceRequests sets Kubernetes resource requests (guaranteed minimums).
	// When set, enables Burstable QoS (requests < limits).
	// When nil, limits are used for both limits and requests (Guaranteed QoS).
	ResourceRequests ResourceLimits

	// TimeoutSeconds is the sandbox TTL. Nil means use DefaultTimeoutSeconds.
	TimeoutSeconds *int

	// Env variables injected into the sandbox.
	Env map[string]string

	// SecureAccess enables secured access for sandbox endpoints.
	SecureAccess bool

	// Metadata for filtering and tagging.
	Metadata map[string]string

	// NetworkPolicy for egress control.
	NetworkPolicy *NetworkPolicy

	// CredentialProxy enables Credential Vault transparent proxy support.
	CredentialProxy *CredentialProxyConfig

	// Volumes to mount.
	Volumes []Volume

	// ImageAuth provides registry credentials for private images.
	ImageAuth *ImageAuth

	// ManualCleanup, when true, creates the sandbox with no TTL so it stays
	// alive until explicitly killed. The timeout field is omitted from the
	// request (nil), causing the server to treat it as infinite.
	ManualCleanup bool

	// Extensions for provider-specific parameters.
	Extensions map[string]string

	// Platform selects the target OS/arch for the sandbox (e.g. {"os":
	// "windows", "arch": "amd64"}). When nil the server applies its default.
	Platform *PlatformSpec

	// SkipHealthCheck skips the WaitUntilReady call after creation.
	SkipHealthCheck bool

	// ReadyTimeout overrides DefaultReadyTimeoutSeconds.
	ReadyTimeout time.Duration

	// HealthCheckInterval overrides DefaultHealthCheckPollingInterval.
	HealthCheckInterval time.Duration

	// HealthCheck is a custom health check function. If nil, execd /ping is used.
	HealthCheck func(ctx context.Context, sb *Sandbox) (bool, error)
}

// Sandbox is the high-level object wrapping lifecycle, execd, and egress clients.
// Use CreateSandbox or ConnectSandbox to obtain an instance.
type Sandbox struct {
	id     string
	config *ConnectionConfig

	lifecycle *LifecycleClient
	execd     *ExecdClient
	egress    *EgressClient
	mu        sync.Mutex
}

// ID returns the sandbox identifier.
func (s *Sandbox) ID() string { return s.id }

// CreateSandbox creates a new sandbox and waits for it to be ready.
func CreateSandbox(ctx context.Context, config ConnectionConfig, opts SandboxCreateOptions) (*Sandbox, error) {
	if (opts.Image == "") == (opts.SnapshotID == "") {
		return nil, &InvalidArgumentError{Field: "Image/SnapshotID", Message: "exactly one of image or snapshotID is required"}
	}

	entrypoint := opts.Entrypoint
	if len(entrypoint) == 0 {
		entrypoint = DefaultEntrypoint
	}
	limits := opts.ResourceLimits
	if limits == nil {
		limits = DefaultResourceLimits
	}
	var timeout *int
	if opts.ManualCleanup {
		// nil timeout — omitted from JSON via omitempty, server treats as no TTL.
	} else if opts.TimeoutSeconds != nil {
		timeout = opts.TimeoutSeconds
	} else {
		t := DefaultTimeoutSeconds
		timeout = &t
	}

	lc := config.lifecycleClient()

	req := CreateSandboxRequest{
		Image:            nil,
		SnapshotID:       opts.SnapshotID,
		Entrypoint:       entrypoint,
		ResourceLimits:   limits,
		ResourceRequests: opts.ResourceRequests,
		Timeout:          timeout,
		Env:              opts.Env,
		SecureAccess:     opts.SecureAccess,
		Metadata:         opts.Metadata,
		NetworkPolicy:    opts.NetworkPolicy,
		CredentialProxy:  opts.CredentialProxy,
		Volumes:          opts.Volumes,
		Extensions:       opts.Extensions,
		Platform:         opts.Platform,
	}
	if opts.Image != "" {
		req.Image = &ImageSpec{URI: opts.Image, Auth: opts.ImageAuth}
	}

	created, err := lc.CreateSandbox(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: create sandbox: %w", err)
	}

	sb := &Sandbox{
		id:        created.ID,
		config:    &config,
		lifecycle: lc,
	}

	if err := sb.waitForRunning(ctx, opts.ReadyTimeout); err != nil {
		// Best-effort cleanup
		_ = lc.DeleteSandbox(context.Background(), created.ID)
		return nil, err
	}

	if err := sb.resolveExecd(ctx); err != nil {
		_ = lc.DeleteSandbox(context.Background(), created.ID)
		return nil, fmt.Errorf("opensandbox: resolve execd: %w", err)
	}

	if !opts.SkipHealthCheck {
		readyOpts := ReadyOptions{
			Timeout:         opts.ReadyTimeout,
			PollingInterval: opts.HealthCheckInterval,
			HealthCheck:     opts.HealthCheck,
		}
		if err := sb.WaitUntilReady(ctx, readyOpts); err != nil {
			_ = lc.DeleteSandbox(context.Background(), created.ID)
			return nil, err
		}
	}

	return sb, nil
}

// ConnectSandbox connects to an existing running sandbox by ID.
func ConnectSandbox(ctx context.Context, config ConnectionConfig, sandboxID string, opts ...ReadyOptions) (*Sandbox, error) {
	if sandboxID == "" {
		return nil, &InvalidArgumentError{Field: "sandboxID", Message: "sandbox ID is required"}
	}
	if len(opts) > 1 {
		return nil, &InvalidArgumentError{
			Field:   "opts",
			Message: "at most one ReadyOptions is supported",
		}
	}

	lc := config.lifecycleClient()

	sb := &Sandbox{
		id:        sandboxID,
		config:    &config,
		lifecycle: lc,
	}

	if err := sb.resolveExecd(ctx); err != nil {
		return nil, fmt.Errorf("opensandbox: resolve execd: %w", err)
	}

	if len(opts) > 0 {
		if err := sb.WaitUntilReady(ctx, opts[0]); err != nil {
			return nil, err
		}
	}

	return sb, nil
}

// ResumeSandbox resumes a paused sandbox and reconnects to it.
func ResumeSandbox(ctx context.Context, config ConnectionConfig, sandboxID string, opts ...ReadyOptions) (*Sandbox, error) {
	lc := config.lifecycleClient()
	if err := lc.ResumeSandbox(ctx, sandboxID); err != nil {
		return nil, fmt.Errorf("opensandbox: resume sandbox: %w", err)
	}
	return ConnectSandbox(ctx, config, sandboxID, opts...)
}

// Resume resumes this sandbox if it was paused and reconnects to it.
func (s *Sandbox) Resume(ctx context.Context, opts ...ReadyOptions) (*Sandbox, error) {
	return ResumeSandbox(ctx, *s.config, s.id, opts...)
}

// Kill terminates the sandbox. This is irreversible.
func (s *Sandbox) Kill(ctx context.Context) error {
	if s.lifecycle.cache != nil {
		s.lifecycle.cache.Invalidate(s.id)
	}
	return s.lifecycle.DeleteSandbox(ctx, s.id)
}

// Close releases local HTTP resources. Does NOT terminate the sandbox.
func (s *Sandbox) Close() error {
	// No-op for now — Go's http.Client doesn't need explicit close.
	// Placeholder for future transport pooling.
	return nil
}

// Pause pauses the sandbox while preserving its state.
// Endpoint cache is invalidated because endpoints may change across pause/resume.
func (s *Sandbox) Pause(ctx context.Context) error {
	if s.lifecycle.cache != nil {
		s.lifecycle.cache.Invalidate(s.id)
	}
	return s.lifecycle.PauseSandbox(ctx, s.id)
}

// GetInfo returns the sandbox's current info (status, metadata, image, etc.).
func (s *Sandbox) GetInfo(ctx context.Context) (*SandboxInfo, error) {
	return s.lifecycle.GetSandbox(ctx, s.id)
}

// PatchMetadata patches this sandbox's metadata. Non-nil values add or replace
// keys. Nil values delete keys.
func (s *Sandbox) PatchMetadata(ctx context.Context, patch MetadataPatch) (*SandboxInfo, error) {
	return s.lifecycle.PatchSandboxMetadata(ctx, s.id, patch)
}

// IsHealthy checks whether the sandbox's execd service is responsive.
func (s *Sandbox) IsHealthy(ctx context.Context) bool {
	if s.execd == nil {
		return false
	}
	return s.execd.Ping(ctx) == nil
}

// Ping checks if the execd service is responsive.
func (s *Sandbox) Ping(ctx context.Context) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.Ping(ctx)
}

// Renew extends the sandbox's expiration by the given duration from now.
func (s *Sandbox) Renew(ctx context.Context, duration time.Duration) (*RenewExpirationResponse, error) {
	return s.lifecycle.RenewExpiration(ctx, s.id, time.Now().Add(duration))
}

// CreateSnapshot creates a persistent snapshot from this sandbox.
func (s *Sandbox) CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	return s.lifecycle.CreateSnapshot(ctx, s.id, req)
}

// GetEndpoint retrieves the public access endpoint for a service port.
func (s *Sandbox) GetEndpoint(ctx context.Context, port int) (*Endpoint, error) {
	useProxy := s.config.UseServerProxy
	return s.lifecycle.GetEndpoint(ctx, s.id, port, &useProxy)
}

// GetSignedEndpoint retrieves a signed endpoint URL with an OSEP-0011 route
// token that expires at the given Unix epoch timestamp (seconds).
func (s *Sandbox) GetSignedEndpoint(ctx context.Context, port int, expires int64) (*Endpoint, error) {
	return s.lifecycle.GetSignedEndpoint(ctx, s.id, port, expires)
}

// ReadyOptions configures WaitUntilReady behavior.
type ReadyOptions struct {
	Timeout         time.Duration
	PollingInterval time.Duration
	HealthCheck     func(ctx context.Context, sb *Sandbox) (bool, error)
}

// WaitUntilReady polls until the sandbox is ready or the timeout expires.
// By default it checks execd /ping; if HealthCheck is provided, it uses that instead.
func (s *Sandbox) WaitUntilReady(ctx context.Context, opts ReadyOptions) error {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = time.Duration(DefaultReadyTimeoutSeconds) * time.Second
	}
	interval := opts.PollingInterval
	if interval == 0 {
		interval = DefaultHealthCheckPollingInterval
	}

	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var healthy bool
		if opts.HealthCheck != nil {
			var err error
			healthy, err = opts.HealthCheck(ctx, s)
			if err != nil {
				lastErr = err
			}
		} else {
			err := s.execd.Ping(ctx)
			healthy = err == nil
			if err != nil {
				lastErr = err
			}
		}

		if healthy {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	return &SandboxReadyTimeoutError{
		SandboxID: s.id,
		Elapsed:   timeout.String(),
		LastErr:   lastErr,
	}
}

// waitForRunning polls the lifecycle API until the sandbox reaches Running state.
func (s *Sandbox) waitForRunning(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Duration(DefaultReadyTimeoutSeconds) * time.Second
	}

	waitCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	start := time.Now()
	for {
		if err := waitCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return &SandboxRunningTimeoutError{
					SandboxID: s.id,
					Elapsed:   time.Since(start).String(),
					LastErr:   err,
				}
			}
			return fmt.Errorf("opensandbox: sandbox %s did not reach Running state: %w", s.id, err)
		}

		info, err := s.lifecycle.GetSandbox(waitCtx, s.id)
		if err != nil {
			return fmt.Errorf("opensandbox: get sandbox status: %w", err)
		}
		if info.Status.State == StateRunning {
			return nil
		}
		if info.Status.State == StateFailed || info.Status.State == StateTerminated {
			return fmt.Errorf("opensandbox: sandbox %s entered terminal state: %s (%s)",
				s.id, info.Status.State, info.Status.Reason)
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// resolveExecd resolves the execd endpoint and creates the ExecdClient.
// Safe for concurrent use — uses mutex for one-time lazy initialization.
func (s *Sandbox) resolveExecd(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execd != nil {
		return nil
	}

	useProxy := s.config.UseServerProxy
	endpoint, err := s.lifecycle.GetEndpoint(ctx, s.id, DefaultExecdPort, &useProxy)
	if err != nil {
		return err
	}

	execdURL := s.config.RewriteEndpointURL(endpoint.Endpoint)
	if !strings.HasPrefix(execdURL, "http") {
		execdURL = s.config.GetProtocol() + "://" + execdURL
	}

	headers := make(map[string]string, len(endpoint.Headers)+1)
	for k, v := range endpoint.Headers {
		headers[k] = v
	}
	if s.config.UseServerProxy {
		if _, ok := headers[execdAuthHeader]; !ok {
			if apiKey := s.config.GetAPIKey(); apiKey != "" {
				headers[execdAuthHeader] = apiKey
			}
		}
	}

	s.execd = s.config.execdClient(execdURL, headers)
	return nil
}

// resolveEgress resolves the egress endpoint and creates the EgressClient.
// Safe for concurrent use — uses mutex for one-time lazy initialization.
func (s *Sandbox) resolveEgress(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.egress != nil {
		return nil
	}

	useProxy := s.config.UseServerProxy
	endpoint, err := s.lifecycle.GetEndpoint(ctx, s.id, DefaultEgressPort, &useProxy)
	if err != nil {
		return err
	}

	egressURL := s.config.RewriteEndpointURL(endpoint.Endpoint)
	if !strings.HasPrefix(egressURL, "http") {
		egressURL = s.config.GetProtocol() + "://" + egressURL
	}

	headers := make(map[string]string, len(endpoint.Headers)+1)
	for k, v := range endpoint.Headers {
		headers[k] = v
	}
	if s.config.UseServerProxy {
		if _, ok := headers[egressAuthHeader]; !ok {
			if apiKey := s.config.GetAPIKey(); apiKey != "" {
				headers[egressAuthHeader] = apiKey
			}
		}
	}

	s.egress = s.config.egressClient(egressURL, headers)
	return nil
}
