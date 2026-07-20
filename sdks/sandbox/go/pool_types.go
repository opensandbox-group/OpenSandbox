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
	"time"
)

// PoolLifecycleState represents the lifecycle state of a sandbox pool.
type PoolLifecycleState int

const (
	PoolLifecycleNotStarted PoolLifecycleState = iota
	PoolLifecycleStarting
	PoolLifecycleRunning
	PoolLifecycleDraining
	PoolLifecycleStopped
)

func (s PoolLifecycleState) String() string {
	switch s {
	case PoolLifecycleNotStarted:
		return "NOT_STARTED"
	case PoolLifecycleStarting:
		return "STARTING"
	case PoolLifecycleRunning:
		return "RUNNING"
	case PoolLifecycleDraining:
		return "DRAINING"
	case PoolLifecycleStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// PoolHealthState represents the health state of a sandbox pool.
type PoolHealthState int

const (
	PoolHealthy PoolHealthState = iota
	PoolDegraded
)

func (s PoolHealthState) String() string {
	switch s {
	case PoolHealthy:
		return "HEALTHY"
	case PoolDegraded:
		return "DEGRADED"
	default:
		return "UNKNOWN"
	}
}

// AcquirePolicy determines behavior when the pool is empty.
type AcquirePolicy int

const (
	AcquirePolicyDirectCreate AcquirePolicy = iota
	AcquirePolicyFailFast
)

func (p AcquirePolicy) String() string {
	switch p {
	case AcquirePolicyDirectCreate:
		return "DIRECT_CREATE"
	case AcquirePolicyFailFast:
		return "FAIL_FAST"
	default:
		return "UNKNOWN"
	}
}

// IdleEntry represents a sandbox in the idle pool.
type IdleEntry struct {
	SandboxID string
	ExpiresAt time.Time
}

// StoreCounters contains pool state store counters for observability.
type StoreCounters struct {
	IdleCount int
}

// TakeIdleResult is the result of taking an idle sandbox from the store.
type TakeIdleResult struct {
	SandboxID                string
	DiscardedAliveSandboxIDs []string
}

// ReapResult holds the result of a min-TTL-aware expired idle reap.
type ReapResult struct {
	DiscardedAliveSandboxIDs []string
}

// PoolSnapshot is a point-in-time snapshot of the pool state.
type PoolSnapshot struct {
	LifecycleState     PoolLifecycleState
	HealthState        PoolHealthState
	IdleCount          int
	MaxIdle            int
	FailureCount       int
	BackoffActive      bool
	LastError          string
	InFlightOperations int
}

// PoolCreationSpec defines the sandbox creation parameters used by the pool.
type PoolCreationSpec struct {
	Image           string
	SnapshotID      string
	Entrypoint      []string
	ResourceLimits  ResourceLimits
	TimeoutSeconds  *int
	Env             map[string]string
	Metadata        map[string]string
	NetworkPolicy   *NetworkPolicy
	Volumes         []Volume
	Extensions      map[string]string
	Platform        *PlatformSpec
	ManualCleanup   bool
	SecureAccess    bool
	CredentialProxy *CredentialProxyConfig
	ImageAuth       *ImageAuth
}

// PooledSandboxCreator allows custom sandbox creation logic.
type PooledSandboxCreator interface {
	Create(ctx context.Context, createCtx PooledSandboxCreateContext) (*Sandbox, error)
}

// PooledSandboxCreateReason indicates why a sandbox is being created.
type PooledSandboxCreateReason int

const (
	CreateReasonWarmup PooledSandboxCreateReason = iota
	CreateReasonAcquire
)

func (r PooledSandboxCreateReason) String() string {
	switch r {
	case CreateReasonWarmup:
		return "WARMUP"
	case CreateReasonAcquire:
		return "ACQUIRE"
	default:
		return "UNKNOWN"
	}
}

// PooledSandboxCreateContext carries pool metadata and creation parameters
// to a PooledSandboxCreator.
type PooledSandboxCreateContext struct {
	PoolName                   string
	OwnerID                    string
	IdleTimeout                time.Duration
	Reason                     PooledSandboxCreateReason
	ReadyTimeout               time.Duration
	HealthCheckPollingInterval time.Duration
	SkipHealthCheck            bool
	HealthCheck                func(ctx context.Context, sb *Sandbox) error
	ConnectionConfig           ConnectionConfig
	CreationSpec               PoolCreationSpec
}

// PoolLogger is the logging interface for pool operations.
// The default implementation is a no-op. Users can inject their own
// implementation (e.g., wrapping log/slog) via the builder.
type PoolLogger interface {
	Info(msg string, keysAndValues ...interface{})
	Warn(msg string, keysAndValues ...interface{})
	Debug(msg string, keysAndValues ...interface{})
}

// noopPoolLogger is the default logger that discards all output.
type noopPoolLogger struct{}

func (noopPoolLogger) Info(_ string, _ ...interface{})  {}
func (noopPoolLogger) Warn(_ string, _ ...interface{})  {}
func (noopPoolLogger) Debug(_ string, _ ...interface{}) {}

// PoolConfig holds the configuration for a sandbox pool.
type PoolConfig struct {
	PoolName          string
	OwnerID           string
	MaxIdle           int
	WarmupConcurrency int
	PrimaryLockTTL    time.Duration
	ReconcileInterval time.Duration
	DegradedThreshold int
	EmptyBehavior     AcquirePolicy
	StateStore        PoolStateStore
	ConnectionConfig  ConnectionConfig
	CreationSpec      PoolCreationSpec

	AcquireReadyTimeout               time.Duration
	WarmupReadyTimeout                time.Duration
	AcquireHealthCheckPollingInterval time.Duration
	WarmupHealthCheckPollingInterval  time.Duration

	AcquireHealthCheck    func(ctx context.Context, sb *Sandbox) error
	WarmupHealthCheck     func(ctx context.Context, sb *Sandbox) error
	WarmupSandboxPreparer func(ctx context.Context, sb *Sandbox) error
	SandboxCreator        PooledSandboxCreator

	WarmupSkipHealthCheck  bool
	AcquireMinRemainingTTL time.Duration
	IdleTimeout            time.Duration
	DrainTimeout           time.Duration
	Logger                 PoolLogger
}

// AcquireOptions configures a single Acquire call.
type AcquireOptions struct {
	SandboxTimeout  time.Duration
	Policy          *AcquirePolicy
	SkipHealthCheck bool
	MinRemainingTTL time.Duration
}

// DefaultIdleTimeout is the default TTL for idle pool entries (24 hours, per OSEP-0005).
const DefaultIdleTimeout = 24 * time.Hour
