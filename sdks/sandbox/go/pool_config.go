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
	"fmt"
	"time"
)

// PoolState represents the lifecycle state of a SandboxPool.
type PoolState int

const (
	PoolStateCreated  PoolState = iota
	PoolStateRunning
	PoolStateDegraded
	PoolStateDraining
	PoolStateStopped
)

func (s PoolState) String() string {
	switch s {
	case PoolStateCreated:
		return "CREATED"
	case PoolStateRunning:
		return "RUNNING"
	case PoolStateDegraded:
		return "DEGRADED"
	case PoolStateDraining:
		return "DRAINING"
	case PoolStateStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// AcquirePolicy defines behavior when no idle sandbox is available.
type AcquirePolicy int

const (
	// DirectCreate creates a new sandbox on-demand if the pool is empty.
	DirectCreate AcquirePolicy = iota
	// FailFast returns an error immediately if no idle sandbox is available.
	FailFast
)

// Pool configuration defaults.
const (
	DefaultReconcileInterval         = 30 * time.Second
	DefaultDegradedThreshold         = 3
	DefaultIdleTimeout               = 24 * time.Hour
	DefaultDrainTimeout              = 30 * time.Second
	DefaultAcquireReadyTimeout       = 30 * time.Second
	DefaultAcquireHealthCheckInterval = 200 * time.Millisecond
	DefaultPrimaryLockTTL            = 60 * time.Second
)

// PoolConfig configures a SandboxPool.
type PoolConfig struct {
	// PoolName identifies this pool (required).
	PoolName string
	// MaxIdle is the target number of idle sandboxes to maintain (required).
	MaxIdle int
	// ConnectionConfig for communicating with the OpenSandbox server (required).
	ConnectionConfig ConnectionConfig
	// CreationSpec is the template for creating pool sandboxes (required).
	CreationSpec PoolCreationSpec
	// StateStore manages idle sandbox state (required).
	StateStore PoolStateStore

	// OwnerID uniquely identifies this pool instance for leader election.
	// Auto-generated if empty.
	OwnerID string
	// WarmupConcurrency limits parallel sandbox creation during reconcile.
	// Default: max(1, ceil(MaxIdle * 0.2))
	WarmupConcurrency int
	// ReconcileInterval between background reconciliation ticks.
	ReconcileInterval time.Duration
	// DegradedThreshold is the number of consecutive warmup failures before
	// entering DEGRADED backoff.
	DegradedThreshold int
	// IdleTimeout is the TTL for idle sandboxes in the pool.
	IdleTimeout time.Duration
	// DrainTimeout is the maximum time to wait for in-flight operations on shutdown.
	DrainTimeout time.Duration
	// AcquireReadyTimeout is the timeout for health-checking an acquired sandbox.
	AcquireReadyTimeout time.Duration
	// AcquireHealthCheckInterval is the polling interval for acquire health checks.
	AcquireHealthCheckInterval time.Duration
	// AcquireMinRemainingTTL skips idle sandboxes whose remaining TTL is below this.
	// Default: min(60s, IdleTimeout/2)
	AcquireMinRemainingTTL time.Duration
	// PrimaryLockTTL is the TTL for the leader election lock.
	PrimaryLockTTL time.Duration

	// HealthCheck is an optional custom health check function.
	// If nil, the default execd /ping check is used.
	HealthCheck func(ctx context.Context, sb *Sandbox) (bool, error)
	// SandboxPreparer is an optional hook called after a sandbox is created
	// during warmup (e.g., to pre-install packages).
	SandboxPreparer func(ctx context.Context, sb *Sandbox) error
}

func (c *PoolConfig) validate() error {
	if c.PoolName == "" {
		return &InvalidArgumentError{Field: "PoolName", Message: "pool name is required"}
	}
	if c.MaxIdle < 0 {
		return &InvalidArgumentError{Field: "MaxIdle", Message: "must be non-negative"}
	}
	if c.CreationSpec.Image == "" {
		return &InvalidArgumentError{Field: "CreationSpec.Image", Message: "image is required"}
	}
	if c.StateStore == nil {
		return &InvalidArgumentError{Field: "StateStore", Message: "state store is required"}
	}
	return nil
}

func (c *PoolConfig) applyDefaults() {
	if c.WarmupConcurrency <= 0 {
		wc := (c.MaxIdle + 4) / 5 // ceil(MaxIdle * 0.2)
		if wc < 1 {
			wc = 1
		}
		c.WarmupConcurrency = wc
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = DefaultReconcileInterval
	}
	if c.DegradedThreshold <= 0 {
		c.DegradedThreshold = DefaultDegradedThreshold
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.AcquireReadyTimeout <= 0 {
		c.AcquireReadyTimeout = DefaultAcquireReadyTimeout
	}
	if c.AcquireHealthCheckInterval <= 0 {
		c.AcquireHealthCheckInterval = DefaultAcquireHealthCheckInterval
	}
	if c.AcquireMinRemainingTTL <= 0 {
		minTTL := 60 * time.Second
		half := c.IdleTimeout / 2
		if half < minTTL {
			minTTL = half
		}
		c.AcquireMinRemainingTTL = minTTL
	}
	if c.PrimaryLockTTL <= 0 {
		c.PrimaryLockTTL = DefaultPrimaryLockTTL
	}
	if c.OwnerID == "" {
		c.OwnerID = fmt.Sprintf("pool-%s-%d", c.PoolName, time.Now().UnixNano())
	}
}

// PoolCreationSpec is the template for creating sandboxes in the pool.
type PoolCreationSpec struct {
	Image            string
	Timeout          int
	Entrypoint       []string
	ResourceLimits   ResourceLimits
	ResourceRequests ResourceLimits
	Env              map[string]string
	Metadata         map[string]string
	NetworkPolicy    *NetworkPolicy
	Volumes          []Volume
	Extensions       map[string]string
	ImageAuth        *ImageAuth
}

// PoolSnapshot contains a point-in-time view of pool state.
type PoolSnapshot struct {
	State         PoolState
	IdleCount     int
	InFlight      int32
	FailureCount  int
	LastError     string
	BackoffActive bool
}

// acquireOpts holds options for a single Acquire call.
type acquireOpts struct {
	sandboxTimeout time.Duration
	policy         AcquirePolicy
}

// AcquireOption configures an Acquire call.
type AcquireOption func(*acquireOpts)

// WithSandboxTimeout sets the TTL to renew the acquired sandbox to.
func WithSandboxTimeout(d time.Duration) AcquireOption {
	return func(o *acquireOpts) { o.sandboxTimeout = d }
}

// WithAcquirePolicy sets the fallback behavior when no idle sandbox is available.
func WithAcquirePolicy(p AcquirePolicy) AcquireOption {
	return func(o *acquireOpts) { o.policy = p }
}
